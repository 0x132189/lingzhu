// Package provider defines the model adapter interface and the OpenAI implementation.
package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"lingzhu/internal/model"
)

type Chunk struct {
	Text      string
	ToolCalls []ToolCallDelta
	// Usage is set on the final chunk when the provider reports it.
	Usage *Usage
}

// Usage is what the provider measured for one model call. Input excludes
// cached tokens, so Input + CacheRead is the whole prompt.
type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
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

// ErrContextOverflow marks a provider rejection caused by a prompt that is too
// long. Callers compact and retry instead of failing the run.
var ErrContextOverflow = errors.New("context overflow")

// IsContextOverflow reports whether err is a too-long prompt rejection.
func IsContextOverflow(err error) bool {
	return errors.Is(err, ErrContextOverflow)
}

var overflowHints = []string{
	"context_length_exceeded",
	"maximum context length",
	"context window",
	"too many tokens",
	"prompt is too long",
	"input is too long",
	"exceeds the maximum",
}

func classifyError(status string, body string) error {
	text := strings.ToLower(status + " " + body)
	for _, hint := range overflowHints {
		if strings.Contains(text, hint) {
			return fmt.Errorf("%w: %s %s", ErrContextOverflow, status, strings.TrimSpace(body))
		}
	}
	return fmt.Errorf("provider %s: %s", status, strings.TrimSpace(body))
}
