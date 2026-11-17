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
}
type Result struct {
	Text, AccountRef, RequestDigest   string
	Attempted, Acknowledged, Complete bool
	Events, Attempts                  int
	// Missing usage remains unknown; providers must not estimate billed tokens.
	UsageReported   bool
	AttemptRecords  []Attempt
	ConversationRef string
	Recovery        []byte `json:"-"`
}

// Dialogue contains caller roles, never a flattened prompt. Owner is an opaque
// authenticated caller scope; upstream conversation identifiers are not public.
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type Dialogue struct {
	Model, Owner, ConversationRef string
	AccountID, AccountRevision    string
	Messages                      []Message
	IdempotencyKey                string
}

func (Dialogue) String() string   { return "inference.Dialogue[redacted]" }
func (Dialogue) GoString() string { return "inference.Dialogue[redacted]" }

type DialogueProvider interface {
	Provider
	CompleteDialogue(context.Context, Dialogue, func(string) error) (Result, error)
}
type Attempt struct {
	StartedAt, FinishedAt time.Time
	HTTPStatus            int
	ErrorCode             string
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
