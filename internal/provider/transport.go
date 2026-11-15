package provider

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"
)

// Transport bounds every model request. Without these limits a stream that
// stops mid-response holds its turn forever, because the agent has no other
// deadline.
const (
	// DefaultHeaderTimeout bounds the wait for response headers after the
	// request is written.
	DefaultHeaderTimeout = 60 * time.Second
	// DefaultIdleTimeout bounds the wait for the next stream chunk. A model
	// that reasons for longer than this without sending a byte is treated as a
	// stalled connection rather than a slow one.
	DefaultIdleTimeout = 15 * time.Minute
	// dialTimeout bounds establishing the connection.
	dialTimeout = 30 * time.Second
)

// Timeouts are the per-request limits of one provider transport.
type Timeouts struct {
	Header time.Duration
	Idle   time.Duration
}

// DefaultTimeouts returns the limits used unless configuration overrides them.
func DefaultTimeouts() Timeouts {
	return Timeouts{Header: DefaultHeaderTimeout, Idle: DefaultIdleTimeout}
}

// NewTransport builds the shared HTTP transport. A non-positive value in
// timeouts falls back to the default for that field.
func NewTransport(timeouts Timeouts) *http.Transport {
	header := timeouts.Header
	if header <= 0 {
		header = DefaultHeaderTimeout
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   15 * time.Second,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: header,
	}
}

// NewClient builds the HTTP client used by the model adapters. The client has
// no overall timeout, because a streaming response legitimately outlives any
// fixed limit; the transport's header timeout and the per-chunk idle watchdog
// bound the call instead.
func NewClient(timeouts Timeouts) *http.Client {
	return &http.Client{Transport: NewTransport(timeouts)}
}

// ErrIdleTimeout marks a stream that produced no data for the idle limit.
var ErrIdleTimeout = errors.New("provider stream idle timeout")

// idleReader fails a read once no byte arrived within the idle limit. A
// streaming body is wrapped with it so a stalled provider cannot hang a turn.
type idleReader struct {
	reader  readCloser
	timeout time.Duration
	timer   *time.Timer
	stalled atomic.Bool
}

type readCloser interface {
	Read([]byte) (int, error)
	Close() error
}

// WatchIdle wraps a stream body with an idle deadline. The caller must close
// the returned reader.
func WatchIdle(body readCloser, timeout time.Duration) readCloser {
	if timeout <= 0 || body == nil {
		return body
	}
	wrapped := &idleReader{reader: body, timeout: timeout}
	wrapped.timer = time.AfterFunc(timeout, func() {
		wrapped.stalled.Store(true)
		_ = body.Close()
	})
	return wrapped
}

func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	if err != nil && r.stalled.Load() {
		return n, fmt.Errorf("%w after %s", ErrIdleTimeout, r.timeout)
	}
	return n, err
}

func (r *idleReader) Close() error {
	r.timer.Stop()
	return r.reader.Close()
}

// IsIdleTimeout reports whether a stream failure was the idle watchdog. It is
// retryable: the request reached the provider, so the prompt itself was fine.
func IsIdleTimeout(err error) bool {
	return errors.Is(err, ErrIdleTimeout)
}
