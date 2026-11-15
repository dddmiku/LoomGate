// Package stream decodes the official Doubao completion/async event protocol.
// It is transport machinery, not an assertion that signing or API tools work.
package stream

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strconv"
	"strings"
	"unicode/utf8"
)

var (
	ErrMalformed    = errors.New("doubao_stream_malformed")
	ErrLimit        = errors.New("doubao_stream_limit")
	ErrIncomplete   = errors.New("doubao_stream_incomplete")
	ErrUnsupported  = errors.New("doubao_stream_unsupported_event")
	ErrNativeAction = errors.New("doubao_native_action_required")
	ErrUpstream     = errors.New("doubao_stream_upstream_error")
)

type Limits struct{ EventBytes, TotalBytes, StateBytes, Messages, Blocks int }

func limits(l Limits) Limits {
	if l.EventBytes <= 0 {
		l.EventBytes = 1 << 20
	}
	if l.TotalBytes <= 0 {
		l.TotalBytes = 32 << 20
	}
	if l.StateBytes <= 0 {
		l.StateBytes = 8 << 20
	}
	if l.Messages <= 0 {
		l.Messages = 128
	}
	if l.Blocks <= 0 {
		l.Blocks = 2048
	}
	return l
}

type Event struct {
	Name  string
	ID    int64
	HasID bool
	Data  json.RawMessage
}

func (Event) String() string   { return "doubao.stream.Event[redacted]" }
func (Event) GoString() string { return "doubao.stream.Event[redacted]" }

type Reader struct {
	source *bufio.Reader
	limits Limits
	first  bool
}

// byteBudget belongs to an entire Run, including all resumed and async bodies.
// Charge actual source reads, including read-ahead, CRLF and comments.
type byteBudget struct{ remaining int }
type budgetReader struct {
	source io.Reader
	budget *byteBudget
}

func (r budgetReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.budget.remaining <= 0 {
		// A one-byte sentinel distinguishes exact EOF from an over-budget byte.
		// Never return that byte to the parser or grant a new stream a fresh budget.
		var probe [1]byte
		n, err := r.source.Read(probe[:])
		if n > 0 {
			return 0, ErrLimit
		}
		return 0, err
	}
	if len(p) > r.budget.remaining {
		p = p[:r.budget.remaining]
	}
	n, err := r.source.Read(p)
	r.budget.remaining -= n
	return n, err
}

func NewReader(source io.Reader, l Limits) *Reader {
	l = limits(l)
	return newReader(source, l, &byteBudget{remaining: l.TotalBytes})
}
func newReader(source io.Reader, l Limits, budget *byteBudget) *Reader {
	return &Reader{source: bufio.NewReaderSize(budgetReader{source: source, budget: budget}, 4096), limits: limits(l), first: true}
}
func (r *Reader) line() (string, error) {
	var line []byte
	for {
		b, err := r.source.ReadByte()
		if err != nil {
			if err == io.EOF && len(line) > 0 {
				return "", ErrIncomplete
			}
			return "", err
		}
		if b == '\n' {
			break
		}
		if b == '\r' {
			next, err := r.source.Peek(1)
			if err != nil && err != io.EOF {
				return "", err
			}
			if err == nil && next[0] == '\n' {
				r.source.ReadByte()
			}
			break
		}
		line = append(line, b)
		if len(line) > r.limits.EventBytes {
			return "", ErrLimit
		}
	}
	if !utf8.Valid(line) {
		return "", ErrMalformed
	}
	if r.first {
		r.first = false
		line = bytes.TrimPrefix(line, []byte{0xef, 0xbb, 0xbf})
	}
	return string(line), nil
}
func (r *Reader) Next(ctx context.Context) (Event, error) {
	e := Event{}
	var data strings.Builder
	size := 0
	fields := false
	for {
		if ctx.Err() != nil {
			return Event{}, ctx.Err()
		}
		line, err := r.line()
		if err != nil {
			if err == io.EOF && fields {
				return Event{}, ErrIncomplete
			}
			return Event{}, err
		}
		if line == "" {
			if data.Len() == 0 {
				e = Event{}
				fields = false
				size = 0
				continue
			}
			e.Data = json.RawMessage(strings.TrimSuffix(data.String(), "\n"))
			if e.Name == "" {
				return Event{}, ErrMalformed
			}
			return e, nil
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		fields = true
		size += len(line)
		if size > r.limits.EventBytes {
			return Event{}, ErrLimit
		}
		name, value, found := strings.Cut(line, ":")
		if !found {
			value = ""
		}
		value = strings.TrimPrefix(value, " ")
		switch name {
		case "event":
			e.Name = value
		case "id":
			if value == "" {
				e.HasID = false
				continue
			}
			n, err := strconv.ParseInt(value, 10, 64)
			if err != nil || n < 0 {
				return Event{}, ErrMalformed
			}
			e.ID = n
			e.HasID = true
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
}

type Block struct {
	ID       string                     `json:"block_id"`
	Type     int                        `json:"block_type"`
	ParentID string                     `json:"parent_id,omitempty"`
	Patch    int                        `json:"patch_type,omitempty"`
	Content  map[string]json.RawMessage `json:"content,omitempty"`
	Finished bool                       `json:"is_finish,omitempty"`
}
type Message struct {
	ID       string  `json:"message_id"`
	UserType int     `json:"user_type"`
	Blocks   []Block `json:"content_block"`
}
type Update struct {
	Kind, MessageID, BlockID, Text string
	BlockType, Operation           int
}

func (Update) String() string   { return "doubao.stream.Update[redacted]" }
func (Update) GoString() string { return "doubao.stream.Update[redacted]" }

type Handoff struct {
	Key       string          `json:"fetch_key"`
	Type      int             `json:"fetch_type"`
	Scene     json.RawMessage `json:"append_scene"`
	MessageID string          `json:"msg_id"`
}

func (Handoff) String() string   { return "doubao.stream.Handoff[redacted]" }
func (Handoff) GoString() string { return "doubao.stream.Handoff[redacted]" }

type Cursor struct {
	LastID                       int64
	Seen, Terminal, Acknowledged bool
	latest                       *deltaTarget
	owned                        map[string]bool
}
type deltaTarget struct {
	message, block, key string
	blockType           int
}
type State struct {
	Messages                  map[string]Message
	Order                     []string
	Handoffs                  []Handoff
	NativeContent             bool
	Events, Replayed, Unknown int
	limits                    Limits
}

func (State) String() string   { return "doubao.stream.State[redacted]" }
func (State) GoString() string { return "doubao.stream.State[redacted]" }
func NewState(l Limits) *State { return &State{Messages: map[string]Message{}, limits: limits(l)} }
func decode(raw []byte, value any) error {
	if json.Unmarshal(raw, value) != nil {
		return ErrMalformed
	}
	return nil
}

func (s *State) Apply(cursor *Cursor, event Event, emit func(Update) error) error {
	if event.Name == "SSE_HEARTBEAT" {
		return nil
	}
	if event.HasID && cursor.Seen && event.ID <= cursor.LastID {
		s.Replayed++
		return nil
	}
	if cursor.Terminal {
		return ErrMalformed
	}
	if !json.Valid(event.Data) {
		return ErrMalformed
	}
	if emit == nil {
		emit = func(Update) error { return nil }
	}
	var err error
	switch event.Name {
	case "SSE_ACK":
		var ack struct {
			Queries []json.RawMessage          `json:"query_list"`
			Meta    map[string]json.RawMessage `json:"ack_client_meta"`
		}
		if decode(event.Data, &ack) != nil || len(ack.Queries) == 0 || len(ack.Meta) == 0 {
			return ErrMalformed
		}
		cursor.Acknowledged = true
	case "STREAM_TIMEOUT_CONTROL": // Remote timing cannot extend the caller's deadline.
	case "STREAM_MSG_NOTIFY":
		var row struct {
			Content struct {
				Blocks []Block `json:"content_block"`
			} `json:"content"`
			Meta Message `json:"meta"`
		}
		if decode(event.Data, &row) != nil {
			return ErrMalformed
		}
		row.Meta.Blocks = row.Content.Blocks
		if cursor.latest != nil && cursor.latest.message == row.Meta.ID {
			cursor.latest = nil
		}
		err = s.replaceMessage(row.Meta, emit)
		if err == nil {
			cursor.own(row.Meta)
		}
	case "FULL_MSG_NOTIFY":
		var row struct {
			Message Message `json:"message"`
		}
		if decode(event.Data, &row) != nil {
			return ErrMalformed
		}
		if cursor.latest != nil && cursor.latest.message == row.Message.ID {
			cursor.latest = nil
		}
		err = s.replaceMessage(row.Message, emit)
		if err == nil {
			cursor.own(row.Message)
		}
	case "STREAM_CHUNK":
		err = s.chunk(cursor, event.Data, emit)
	case "CHUNK_DELTA":
		if cursor.latest == nil {
			return ErrMalformed
		}
		target := cursor.latest
		msg, ok := s.Messages[target.message]
		if !ok || !target.exists(msg) {
			cursor.latest = nil
			return ErrMalformed
		}
		block := Block{ID: target.block, Type: target.blockType, Patch: 1, Content: map[string]json.RawMessage{target.key: event.Data}}
		err = s.applyBlocks(&msg, []Block{block}, 1, emit)
		if err == nil {
			s.Messages[msg.ID] = msg
		}
	case "SSE_REPLY_END":
		var end struct {
			Type int `json:"end_type"`
		}
		if decode(event.Data, &end) != nil || end.Type < 1 || end.Type > 3 {
			return ErrMalformed
		}
		if end.Type == 3 {
			cursor.Terminal = true
		}
	case "FETCH_STREAM":
		var h Handoff
		if decode(event.Data, &h) != nil || h.Key == "" || len(h.Key) > 4096 || h.Type != 2 || len(h.Scene) == 0 {
			return ErrUnsupported
		}
		for _, old := range s.Handoffs {
			if old.Key == h.Key {
				if string(old.Scene) != string(h.Scene) {
					return ErrMalformed
				}
				return nil
			}
		}
		if len(s.Handoffs) >= 16 {
			return ErrLimit
		}
		s.Handoffs = append(s.Handoffs, h)
	case "ASYNC_CHUNK_SNAPSHOT":
		var snapshot struct {
			Seq      *int64 `json:"snapshot_seq"`
			Next     *int64 `json:"next_seq"`
			Messages []struct {
				Meta    Message `json:"meta"`
				Content struct {
					Blocks []Block `json:"content_block"`
				} `json:"content"`
			} `json:"message_snapshots"`
		}
		if decode(event.Data, &snapshot) != nil || snapshot.Seq == nil || snapshot.Next == nil || *snapshot.Seq < 0 || *snapshot.Next < *snapshot.Seq {
			return ErrMalformed
		}
		cursor.latest = nil
		// Snapshot replaces this stream's known messages without erasing other
		// task streams. Its next_seq, not its event envelope ID, is the resume cursor.
		for id := range cursor.owned {
			if old, ok := s.Messages[id]; ok {
				if err = s.applyBlocks(&old, nil, 3, emit); err != nil {
					return err
				}
			}
			delete(s.Messages, id)
		}
		cursor.owned = map[string]bool{}
		order := s.Order[:0]
		for _, id := range s.Order {
			if _, ok := s.Messages[id]; ok {
				order = append(order, id)
			}
		}
		s.Order = order
		for _, row := range snapshot.Messages {
			row.Meta.Blocks = row.Content.Blocks
			if err = s.replaceMessage(row.Meta, emit); err != nil {
				return err
			}
			cursor.own(row.Meta)
		}
		event.ID = *snapshot.Next
		event.HasID = true
	case "DOWNLINK_CMD":
		s.NativeContent = true
		return ErrNativeAction
	case "STREAM_ERROR", "gateway-error":
		return ErrUpstream
	default:
		s.Unknown++
		return ErrUnsupported
	}
	if err != nil {
		return err
	}
	if err = s.checkLimit(); err != nil {
		return err
	}
	s.Events++
	if event.HasID {
		cursor.LastID = event.ID
		cursor.Seen = true
	}
	return nil
}
func (c *Cursor) own(m Message) {
	if m.UserType == 2 {
		if c.owned == nil {
			c.owned = map[string]bool{}
		}
		c.owned[m.ID] = true
	}
}
func (s *State) replaceMessage(msg Message, emit func(Update) error) error {
	if msg.ID == "" || len(msg.ID) > 512 || msg.UserType < 1 || msg.UserType > 3 {
		return ErrMalformed
	}
	if _, exists := s.Messages[msg.ID]; !exists {
		s.Order = append(s.Order, msg.ID)
	}
	if msg.UserType != 2 {
		msg.Blocks = nil
		s.Messages[msg.ID] = msg
		return nil
	}
	if old, ok := s.Messages[msg.ID]; ok && old.UserType != 2 {
		return ErrMalformed
	}
	blocks := msg.Blocks
	msg.Blocks = s.Messages[msg.ID].Blocks
	if err := s.applyBlocks(&msg, blocks, 2, emit); err != nil {
		return err
	}
	s.Messages[msg.ID] = msg
	return nil
}
func (s *State) chunk(cursor *Cursor, data []byte, emit func(Update) error) error {
	// A content-block operation supersedes the previous delta context; metadata,
	// TTS and legacy operations between text chunks do not.
	touchedBlock := false
	var next *deltaTarget
	var chunk struct {
		ID  string `json:"message_id"`
		Ops []struct {
			Object int             `json:"patch_object"`
			Type   int             `json:"patch_type"`
			Value  json.RawMessage `json:"patch_value"`
		} `json:"patch_op"`
	}
	if decode(data, &chunk) != nil || chunk.ID == "" {
		return ErrMalformed
	}
	msg, ok := s.Messages[chunk.ID]
	if !ok {
		return ErrMalformed
	}
	if msg.UserType != 2 {
		return nil
	}
	for _, op := range chunk.Ops {
		if op.Type < 1 || op.Type > 3 {
			return ErrMalformed
		}
		switch op.Object {
		case 1:
			touchedBlock = true
			cursor.latest = nil
			var value struct {
				Blocks []Block `json:"content_block"`
			}
			if op.Type != 3 && decode(op.Value, &value) != nil {
				return ErrMalformed
			}
			if len(chunk.Ops) == 1 && len(value.Blocks) == 1 && op.Type != 3 && value.Blocks[0].Patch != 3 && !value.Blocks[0].Finished {
				b := value.Blocks[0]
				if len(b.Content) == 1 {
					for key := range b.Content {
						next = &deltaTarget{message: msg.ID, block: b.ID, key: key, blockType: b.Type}
					}
				} else {
					cursor.latest = nil
				}
			}
			if err := s.applyBlocks(&msg, value.Blocks, op.Type, emit); err != nil {
				return err
			}
		case 2, 3, 4, 5, 50, 100, 101, 102, 103, 110, 111, 112: // Known metadata/legacy fields; content-block text remains authoritative.
		default:
			return ErrUnsupported
		}
	}
	s.Messages[msg.ID] = msg
	if touchedBlock && next != nil && next.exists(msg) {
		cursor.latest = next
	}
	return nil
}
func (t *deltaTarget) exists(msg Message) bool {
	if msg.UserType != 2 {
		return false
	}
	for _, b := range msg.Blocks {
		if b.ID == t.block {
			return b.Type == t.blockType && !b.Finished && len(b.Content) == 1 && b.Content[t.key] != nil
		}
	}
	return false
}
func (s *State) applyBlocks(msg *Message, blocks []Block, outer int, emit func(Update) error) error {
	if outer == 2 || outer == 3 {
		for _, old := range msg.Blocks {
			if err := emit(Update{Kind: "block", MessageID: msg.ID, BlockID: old.ID, BlockType: old.Type, Operation: 3}); err != nil {
				return err
			}
		}
		msg.Blocks = nil
	}
	if outer == 3 {
		return nil
	}
	for _, b := range blocks {
		if b.ID == "" || len(b.ID) > 512 {
			return ErrMalformed
		}
		operation := b.Patch
		if operation == 0 {
			operation = 2
		}
		if outer == 2 {
			operation = 2
		}
		if operation < 1 || operation > 3 {
			return ErrMalformed
		}
		if b.Type == 10016 || b.Type == 10024 {
			s.NativeContent = true
			return ErrNativeAction
		}
		if b.Type == 10019 {
			s.NativeContent = true
		}
		index := -1
		for i, old := range msg.Blocks {
			if old.ID == b.ID {
				index = i
				break
			}
		}
		if operation == 3 {
			if index >= 0 {
				msg.Blocks = append(msg.Blocks[:index], msg.Blocks[index+1:]...)
			}
			if err := emit(Update{Kind: "block", MessageID: msg.ID, BlockID: b.ID, Operation: 3}); err != nil {
				return err
			}
			continue
		}
		if operation == 1 && index >= 0 {
			old := msg.Blocks[index]
			if old.Type != b.Type {
				return ErrMalformed
			}
			for key, raw := range b.Content {
				var incoming, previous map[string]json.RawMessage
				if decode(raw, &incoming) != nil {
					return ErrMalformed
				}
				if prior := old.Content[key]; prior != nil {
					if decode(prior, &previous) != nil {
						return ErrMalformed
					}
				}
				if previous == nil {
					previous = map[string]json.RawMessage{}
				}
				for field, value := range incoming {
					if field == "text" || field == "code" {
						var before, after string
						if prev := previous[field]; prev != nil && decode(prev, &before) != nil {
							return ErrMalformed
						}
						if decode(value, &after) != nil {
							return ErrMalformed
						}
						value, _ = json.Marshal(before + after)
					}
					previous[field] = value
				}
				merged, _ := json.Marshal(previous)
				if old.Content == nil {
					old.Content = map[string]json.RawMessage{}
				}
				old.Content[key] = merged
			}
			old.Finished = b.Finished
			msg.Blocks[index] = old
		} else {
			if index >= 0 {
				msg.Blocks[index] = b
			} else {
				msg.Blocks = append(msg.Blocks, b)
			}
		}
		key, field := "", ""
		if b.Type == 10000 {
			key = "text_block"
			field = "text"
		}
		if b.Type == 10008 {
			key = "code_block"
			field = "code"
		}
		if key != "" {
			var value map[string]json.RawMessage
			if raw := b.Content[key]; raw != nil {
				if decode(raw, &value) != nil {
					return ErrMalformed
				}
				var text string
				if raw := value[field]; raw != nil {
					if decode(raw, &text) != nil {
						return ErrMalformed
					}
					if err := emit(Update{Kind: "text", MessageID: msg.ID, BlockID: b.ID, BlockType: b.Type, Text: text, Operation: operation}); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}
func (s *State) checkLimit() error {
	if len(s.Messages) > s.limits.Messages {
		return ErrLimit
	}
	count, size := 0, 0
	for _, m := range s.Messages {
		count += len(m.Blocks)
		for _, b := range m.Blocks {
			for _, raw := range b.Content {
				size += len(raw)
			}
		}
	}
	if count > s.limits.Blocks || size > s.limits.StateBytes {
		return ErrLimit
	}
	return nil
}
