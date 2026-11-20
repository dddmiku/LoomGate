// Package inference is the transport-independent provider boundary.
package inference

import (
	"context"
	"errors"
	"time"
)

type Model struct {
	ID, Name      string
	ContextWindow int64
	Incremental   bool
	MultiTurn     bool
	// Credits is the upstream billing multiplier ("0.65x", "免费"). Empty means
	// the provider does not publish one.
	Credits string
}
type Result struct {
	Text, AccountRef, RequestDigest   string
	Reasoning                         string // Buffered reasoning, separate from the answer.
	Attempted, Acknowledged, Complete bool
	Events, Attempts                  int
	Usage                             TokenUsage
	UsageReported                     bool
	PromptTokens                      int
	CompletionTokens                  int
	CachedTokens                      int
	AttemptRecords                    []Attempt
	ConversationRef                   string
	// FinishReason is the upstream's own termination cause (stop, length,
	// content_filter, tool_calls). Empty means the provider does not report
	// one, in which case the caller falls back to "stop".
	FinishReason string
	// ToolCalls is set when the model answered by requesting tools instead of
	// (or in addition to) prose. Empty means a plain text answer.
	ToolCalls []ToolCall
	// MessageID is private attribution metadata for a bound work task. It must
	// never appear in a public provider response.
	MessageID string `json:"-"`
	Recovery  []byte `json:"-"`
}

// Message is one conversation turn.
//
// Content is the plain text. ToolCalls is set on an assistant turn that asked
// the caller to run tools, and ToolCallID identifies the call this turn answers
// when Role is "tool". Providers that cannot carry tools simply ignore both.
type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
}

// ToolCall is one requested function invocation, in OpenAI shape.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function ToolCallFunction `json:"function"`
	Index    *int             `json:"index,omitempty"`
}

// ToolCallFunction carries the name and the JSON-encoded arguments.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// ToolDefinition is one tool the caller offers, in OpenAI shape. It is kept as
// raw JSON because the provider forwards it verbatim: rewriting a schema it
// does not interpret only risks breaking the caller's contract.
type ToolDefinition map[string]any
type Dialogue struct {
	Model, Owner, ConversationRef string
	AccountID, AccountRevision    string
	Messages                      []Message
	IdempotencyKey                string
	// Tools is the caller's tool catalog, forwarded verbatim to providers that
	// support tool calling. Empty means a plain text request.
	Tools      []ToolDefinition
	ToolChoice any
}

func (Dialogue) String() string   { return "inference.Dialogue[redacted]" }
func (Dialogue) GoString() string { return "inference.Dialogue[redacted]" }

type DialogueProvider interface {
	Provider
	CompleteDialogue(context.Context, Dialogue, func(string) error) (Result, error)
}

// ReasoningProvider is an optional capability for a model that thinks before
// it answers. The upstream streams those thinking deltas first and stays
// otherwise silent, so a client that only receives answer content sees nothing
// for the entire thinking phase — which looks like a hang when the model thinks
// for a long time. Providers that can surface reasoning implement this.
//
// The reasoning text never becomes part of Result.Text: it is a separate
// channel, and a client that ignores it still receives the identical answer.
type ReasoningProvider interface {
	DialogueProvider
	CompleteDialogueReasoning(context.Context, Dialogue, func(string) error, func(string) error) (Result, error)
}

// ToolProvider is an optional capability for a model that can call caller
// supplied tools. The transcript then has to be forwarded verbatim — an
// assistant turn that requested a tool and the tool turn answering it lose
// their meaning when flattened to text — and the answer can come back as tool
// calls instead of prose.
type ToolProvider interface {
	DialogueProvider
	SupportsTools() bool
}

// ToolCallDelta is one incremental tool-call fragment as the upstream streams
// it. The first fragment for an index carries the id and function name; later
// ones append to Arguments. Consumers reassemble by Index.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

// ToolCapableProvider streams tool-call fragments in addition to text. A
// provider that answers with tool calls emits no text for that turn.
//
// Tools and reasoning coexist: a thinking model reasons first even when the
// turn ends in a tool call. Both sinks are therefore always supplied, and a
// provider forwards whichever deltas it receives.
type ToolCapableProvider interface {
	ToolProvider
	CompleteDialogueTools(context.Context, Dialogue, func(string) error, func(string) error, func(ToolCallDelta) error) (Result, error)
}

// TokenUsage preserves the difference between an omitted metric and a reported zero.
type TokenUsage struct {
	Prompt, Completion, Cached *int
	Credit                     *float64
}

func (u TokenUsage) Reported() bool {
	return u.Prompt != nil || u.Completion != nil || u.Cached != nil || u.Credit != nil
}

func (u TokenUsage) Complete() bool { return u.Prompt != nil && u.Completion != nil }

type Attempt struct {
	// AccountRef identifies the provider account used for this upstream call.
	AccountRef            string
	StartedAt, FinishedAt time.Time
	// HTTPStatus is zero if no response headers arrived.
	HTTPStatus   int
	ErrorCode    string
	Usage        TokenUsage
	FinishReason string
}

func (Result) String() string   { return "inference.Result[redacted]" }
func (Result) GoString() string { return "inference.Result[redacted]" }

type Provider interface {
	Models(context.Context) ([]Model, error)
	Complete(context.Context, string, string) (Result, error)
}

var ErrStreamRevised = errors.New("provider_stream_revised")
var ErrStreamContract = errors.New("provider_stream_contract_invalid")

// StreamingProvider emits acknowledged, ordered text deltas synchronously.
// The callback provides backpressure and its error must cancel upstream work.
// Emission never implies completion: only a successful final Result does.
type StreamingProvider interface {
	Provider
	Stream(context.Context, string, string, func(string) error) (Result, error)
}
