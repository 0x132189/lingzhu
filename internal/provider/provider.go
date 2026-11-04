// Package provider defines the model adapter interface and the OpenAI implementation.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"

	"lingzhu/internal/model"
)

type Chunk struct {
	Text      string
	ToolCalls []ToolCallDelta
	// FinishReason is "stop", "length", "tool_calls" and so on when reported.
	FinishReason string
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

// StatusError is a non-2xx response or an in-stream provider error. Code is
// zero when the failure happened after the response started.
type StatusError struct {
	Code     int
	Message  string
	Overflow bool
}

func (e *StatusError) Error() string { return e.Message }

func (e *StatusError) Unwrap() error {
	if e.Overflow {
		return ErrContextOverflow
	}
	return nil
}

// IsRetryable reports whether the same request may succeed after a short wait.
// Only transport and rate-limit failures qualify; authentication, invalid
// request and content errors do not.
func IsRetryable(err error) bool {
	var status *StatusError
	if errors.As(err, &status) {
		return status.Code == 408 || status.Code == 429 || status.Code >= 500
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.ErrClosedPipe) ||
		errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ECONNREFUSED)
}

func classifyError(code int, status string, body string) error {
	text := strings.ToLower(status + " " + body)
	overflow := false
	for _, hint := range overflowHints {
		if strings.Contains(text, hint) {
			overflow = true
			break
		}
	}
	return &StatusError{
		Code:     code,
		Message:  fmt.Sprintf("provider %s: %s", status, strings.TrimSpace(body)),
		Overflow: overflow,
	}
}
