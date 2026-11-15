// Package provider defines the model adapter interface and the OpenAI implementation.
package provider

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"lingzhu/internal/model"
)

type Chunk struct {
	Text      string
	Reasoning string
	// ReasoningSignature and ReasoningRedacted carry Anthropic's proof for a
	// thinking block. A replayed block without its signature is rejected, so
	// the values travel with the reasoning text.
	ReasoningSignature string
	ReasoningRedacted  string
	// ReasoningSource names the API that issued the proof above, so a replay
	// never hands one provider's blob to another. Empty means Anthropic.
	ReasoningSource string
	ToolCalls       []ToolCallDelta
	// FinishReason is "stop", "length", "tool_calls" and so on when reported.
	FinishReason string
	// Usage is set on the final chunk when the provider reports it.
	Usage *Usage
	// Compaction carries a provider-side context window replacement, such as
	// an Anthropic compaction block. It is empty for ordinary chunks.
	Compaction string
}

// Usage is what the provider measured for one model call. Input excludes
// cached tokens, so Input + CacheRead is the whole prompt.
type Usage struct {
	Input      int
	Output     int
	CacheRead  int
	CacheWrite int
}

// ReasoningSourceAnthropic and ReasoningSourceResponses name the two APIs that
// hand out reasoning proofs. A proof is only valid for the API that issued it.
const (
	ReasoningSourceAnthropic = "messages"
	ReasoningSourceResponses = "responses"
)

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

// ErrIncompleteStream marks a response that ended without a finish reason and
// without any output. A provider that stops mid-answer reports this instead of
// looking like a deliberately empty reply.
var ErrIncompleteStream = errors.New("provider response ended without a finish reason")

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
	// RetryAfter is the provider's suggested wait, when it sent one.
	RetryAfter time.Duration
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
	if IsIdleTimeout(err) {
		return true
	}
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

// IsUnreachable reports whether the failure means this machine has no network
// path to the provider host. These retries get their own one-hour budget.
func IsUnreachable(err error) bool {
	if err == nil {
		return false
	}
	var dns *net.DNSError
	if errors.As(err, &dns) {
		return true
	}
	text := strings.ToLower(err.Error())
	for _, code := range []string{"enotfound", "eai_again", "enetunreach", "ehostunreach", "enetdown"} {
		if strings.Contains(text, code) {
			return true
		}
	}
	return false
}

// IsTimeout reports whether a provider call timed out rather than failed with
// a retryable protocol error.
func IsTimeout(err error) bool {
	if err == nil {
		return false
	}
	// An idle watchdog trip is a stalled stream, not a slow one: it must not
	// spend the timeout budget that exists for genuinely hung endpoints.
	if IsIdleTimeout(err) {
		return false
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "timeout") || strings.Contains(text, "timed out") || errors.Is(err, context.DeadlineExceeded)
}

func classifyError(code int, status string, body string) error {
	return classifyErrorAfter(code, status, body, 0)
}

func classifyErrorAfter(code int, status string, body string, retryAfter time.Duration) error {
	text := strings.ToLower(status + " " + body)
	overflow := false
	for _, hint := range overflowHints {
		if strings.Contains(text, hint) {
			overflow = true
			break
		}
	}
	return &StatusError{
		Code:       code,
		Message:    fmt.Sprintf("provider %s: %s", status, strings.TrimSpace(body)),
		Overflow:   overflow,
		RetryAfter: retryAfter,
	}
}

// ParseRetryAfter reads the seconds or HTTP-date form of Retry-After.
func ParseRetryAfter(value string) time.Duration {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.Atoi(value); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if at, err := http.ParseTime(value); err == nil {
		if wait := time.Until(at); wait > 0 {
			return wait
		}
	}
	return 0
}
