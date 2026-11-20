package autoclaw

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"unicode/utf8"

	"workbuddy2api/internal/inference"
	"workbuddy2api/internal/providerretry"
)

const (
	maxSSELineBytes       = 1 << 20
	maxSSEWireBytes       = 64 << 20
	maxSSEOutputBytes     = 8 << 20
	maxSSEToolCalls       = 64
	maxSSEIdentifierBytes = 256
)

type streamContractError struct {
	code  string
	cause error
}

func (e *streamContractError) Error() string                   { return e.code }
func (e *streamContractError) PublicCode() string              { return e.code }
func (e *streamContractError) RetryClass() providerretry.Class { return providerretry.Fatal }
func (e *streamContractError) Unwrap() error                   { return e.cause }
func invalidStream() error                                     { return &streamContractError{code: "autoclaw_stream_invalid"} }
func streamLimit() error                                       { return &streamContractError{code: "autoclaw_stream_limit"} }
func incompleteStream(cause error) error {
	return &streamContractError{code: "autoclaw_stream_incomplete", cause: cause}
}

type sseToolDelta struct {
	Index    *int                       `json:"index"`
	ID       string                     `json:"id"`
	Type     string                     `json:"type"`
	Function inference.ToolCallFunction `json:"function"`
}
type sseDelta struct {
	Content   string         `json:"content"`
	Reasoning string         `json:"reasoning_content"`
	Tools     []sseToolDelta `json:"tool_calls"`
}
type sseChoice struct {
	Index        *int      `json:"index"`
	Delta        *sseDelta `json:"delta"`
	FinishReason string    `json:"finish_reason"`
}
type sseChunk struct {
	Choices []sseChoice     `json:"choices"`
	Usage   json.RawMessage `json:"usage"`
	Error   json.RawMessage `json:"error"`
}

type sseDecoder struct {
	text                strings.Builder
	usage               streamUsage
	tools               []inference.ToolCall
	events, outputBytes int
	reasoningSeen       bool
	terminal            bool
	emit, reasoning     func(string) error
	tool                func(inference.ToolCallDelta) error
}

func (d *sseDecoder) reserve(size int) error {
	if size < 0 || size > maxSSEOutputBytes-d.outputBytes {
		return streamLimit()
	}
	d.outputBytes += size
	return nil
}

// Usage fields are cumulative snapshots, not increments. A malformed metric
// cannot erase previously observed fields or manufacture a negative token count.
func checkedSSEUsage(raw json.RawMessage, previous streamUsage) (streamUsage, error) {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return previous, nil
	}
	object := bytes.TrimSpace(raw)
	if len(object) == 0 || object[0] != '{' {
		return previous, invalidStream()
	}
	var fields struct {
		Prompt     *int `json:"prompt_tokens"`
		Completion *int `json:"completion_tokens"`
		Cached     *int `json:"prompt_cache_hit_tokens"`
		Details    *struct {
			Cached *int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	}
	if json.Unmarshal(object, &fields) != nil {
		return previous, invalidStream()
	}
	for _, metric := range []*int{fields.Prompt, fields.Completion, fields.Cached} {
		if metric != nil && *metric < 0 {
			return previous, invalidStream()
		}
	}
	if fields.Details != nil && fields.Details.Cached != nil && *fields.Details.Cached < 0 {
		return previous, invalidStream()
	}
	next := parseUsage(object, previous)
	if next.PromptSeen && next.CachedSeen && next.Cached > next.Prompt {
		return previous, invalidStream()
	}
	return next, nil
}

func (d *sseDecoder) finish() error {
	if !d.terminal {
		return incompleteStream(io.ErrUnexpectedEOF)
	}
	if d.text.Len() == 0 && len(d.tools) == 0 && !d.reasoningSeen && d.usage.FinishReason != "content_filter" {
		return &emptyAnswerError{}
	}
	for _, call := range d.tools {
		if call.ID == "" || call.Function.Name == "" {
			return invalidStream()
		}
	}
	if d.usage.FinishReason == "tool_calls" && len(d.tools) == 0 {
		return invalidStream()
	}
	return nil
}

// AutoClaw sends one JSON object per data line. Empty separators, SSE comments
// and metadata are not model output. A JSON/error data frame is never skipped.
func (d *sseDecoder) line(line []byte) (done bool, err error) {
	line = bytes.TrimSuffix(line, []byte{'\n'})
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if !bytes.HasPrefix(line, []byte("data:")) {
		return false, nil
	}
	payload := bytes.TrimSpace(line[len("data:"):])
	if len(payload) == 0 {
		return false, nil
	}
	if bytes.Equal(payload, []byte("[DONE]")) {
		d.terminal = true
		return true, d.finish()
	}
	if !utf8.Valid(payload) || payload[0] != '{' {
		return false, invalidStream()
	}
	var chunk sseChunk
	if json.Unmarshal(payload, &chunk) != nil {
		return false, invalidStream()
	}
	if chunk.Choices == nil && (len(chunk.Usage) == 0 || bytes.Equal(bytes.TrimSpace(chunk.Usage), []byte("null"))) {
		return false, invalidStream()
	}
	usage, err := checkedSSEUsage(chunk.Usage, d.usage)
	if err != nil {
		return false, err
	}
	d.usage = usage
	if len(chunk.Error) > 0 && !bytes.Equal(bytes.TrimSpace(chunk.Error), []byte("null")) {
		return false, invalidStream()
	}
	// The request carries a single completion; interleaving choices would mix
	// independent answers, tool-call indices and termination reasons.
	if len(chunk.Choices) > 1 {
		return false, invalidStream()
	}
	for _, choice := range chunk.Choices {
		if choice.Index != nil && *choice.Index != 0 {
			return false, invalidStream()
		}
		delta := choice.Delta
		if delta == nil {
			if choice.FinishReason == "" {
				return false, invalidStream()
			}
			delta = &sseDelta{}
		}
		if d.terminal && (delta.Content != "" || delta.Reasoning != "" || len(delta.Tools) > 0) {
			return false, invalidStream()
		}
		if choice.FinishReason != "" {
			switch choice.FinishReason {
			case "stop", "length", "content_filter", "tool_calls":
			default:
				return false, invalidStream()
			}
			if d.usage.FinishReason != "" && choice.FinishReason != d.usage.FinishReason {
				return false, invalidStream()
			}
		}
		for _, fragment := range delta.Tools {
			index := 0
			if fragment.Index != nil {
				index = *fragment.Index
			}
			if index < 0 || index >= maxSSEToolCalls || index > len(d.tools) {
				return false, invalidStream()
			}
			if fragment.Type != "" && fragment.Type != "function" || len(fragment.ID) > maxSSEIdentifierBytes {
				return false, invalidStream()
			}
			if index == len(d.tools) {
				d.tools = append(d.tools, inference.ToolCall{Type: "function"})
			}
			call := &d.tools[index]
			if fragment.ID != "" {
				if call.ID != "" && call.ID != fragment.ID {
					return false, invalidStream()
				}
				for i, other := range d.tools {
					if i != index && other.ID == fragment.ID {
						return false, invalidStream()
					}
				}
			}
			if len(call.Function.Name)+len(fragment.Function.Name) > maxSSEIdentifierBytes {
				return false, streamLimit()
			}
			if err := d.reserve(len(fragment.ID) + len(fragment.Function.Name) + len(fragment.Function.Arguments)); err != nil {
				return false, err
			}
			if fragment.ID != "" {
				call.ID = fragment.ID
			}
			call.Function.Name += fragment.Function.Name
			call.Function.Arguments += fragment.Function.Arguments
			if d.tool != nil {
				if err := d.tool(inference.ToolCallDelta{Index: index, ID: fragment.ID, Name: fragment.Function.Name, Arguments: fragment.Function.Arguments}); err != nil {
					return false, err
				}
			}
			d.events++
		}
		if delta.Reasoning != "" {
			if err := d.reserve(len(delta.Reasoning)); err != nil {
				return false, err
			}
			if d.reasoning != nil {
				if err := d.reasoning(delta.Reasoning); err != nil {
					return false, err
				}
			}
			d.reasoningSeen = true
			d.events++
		}
		if delta.Content != "" {
			if err := d.reserve(len(delta.Content)); err != nil {
				return false, err
			}
			if d.emit != nil {
				if err := d.emit(delta.Content); err != nil {
					return false, err
				}
			}
			d.text.WriteString(delta.Content)
			d.events++
		}
		if choice.FinishReason != "" {
			d.usage.FinishReason, d.terminal = choice.FinishReason, true
		}
	}
	return false, nil
}

func decodeSSE(body io.Reader, emit, reasoning func(string) error, tool func(inference.ToolCallDelta) error) (string, int, streamUsage, []inference.ToolCall, error) {
	d := &sseDecoder{emit: emit, reasoning: reasoning, tool: tool}
	reader := bufio.NewReaderSize(body, 8192)
	var pending []byte
	wireBytes := 0
	result := func(err error) (string, int, streamUsage, []inference.ToolCall, error) {
		return d.text.String(), d.events, d.usage, d.tools, err
	}
	for {
		fragment, readErr := reader.ReadSlice('\n')
		if len(fragment) > maxSSELineBytes-len(pending) || len(fragment) > maxSSEWireBytes-wireBytes {
			return result(streamLimit())
		}
		wireBytes += len(fragment)
		if len(pending) > 0 || errors.Is(readErr, bufio.ErrBufferFull) {
			pending = append(pending, fragment...)
			if errors.Is(readErr, bufio.ErrBufferFull) {
				continue
			}
			fragment = pending
		}
		if len(fragment) > 0 {
			done, parseErr := d.line(fragment)
			if parseErr != nil {
				return result(parseErr)
			}
			if done {
				return result(nil)
			}
		}
		pending = pending[:0]
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return result(d.finish())
			}
			return result(incompleteStream(readErr))
		}
	}
}
