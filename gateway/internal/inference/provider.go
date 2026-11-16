// Package inference is the transport-independent provider boundary.
package inference

import (
	"context"
	"time"
)

type Model struct {
	ID, Name      string
	ContextWindow int64
}
type Result struct {
	Text, AccountRef, RequestDigest   string
	Attempted, Acknowledged, Complete bool
	Events, Attempts                  int
	// Missing usage remains unknown; providers must not estimate billed tokens.
	UsageReported  bool
	AttemptRecords []Attempt
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
