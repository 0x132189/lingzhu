// Package provider defines the model adapter interface and the OpenAI implementation.
package provider

import (
	"context"

	"lingzhu/internal/model"
)

type Chunk struct {
	Text      string
	ToolCalls []ToolCallDelta
}

// ToolCallDelta is one streamed tool-call fragment, merged by Index.
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

type Stream interface {
	Recv() (Chunk, error)
	Close() error
}

type Provider interface {
	Stream(ctx context.Context, req model.Request) (Stream, error)
}
