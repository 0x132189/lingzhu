package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"lingzhu/internal/rpc"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// sseTransport speaks the legacy HTTP+SSE transport: a GET stream announces the
// POST endpoint, requests are posted there, and all responses arrive on the
// stream.
type sseTransport struct {
	streamURL *url.URL
	headers   map[string]string
	client    *http.Client
	onMessage func(rpc.Message)
	onClose   func()
	token     func(context.Context) (string, error)

	mu        sync.Mutex
	endpoint  string
	body      io.ReadCloser
	err       error
	cancel    context.CancelFunc
	ready     chan struct{}
	readyOnce sync.Once
	closeOnce sync.Once
}

func startSSE(ctx context.Context, name string, cfg Server, token func(context.Context) (string, error), onMessage func(rpc.Message), onClose func()) (*sseTransport, error) {
	raw := strings.TrimSpace(expandEnv(cfg.URL))
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("mcp: server %q needs an http(s) url", name)
	}
	streamCtx, cancel := context.WithCancel(context.Background())
	t := &sseTransport{
		streamURL: parsed,
		headers:   expandHeaders(cfg.Headers),
		client:    &http.Client{},
		onMessage: onMessage,
		onClose:   onClose,
		token:     token,
		cancel:    cancel,
		ready:     make(chan struct{}),
	}
	go t.connect(streamCtx)

	select {
	case <-t.ready:
		t.mu.Lock()
		err := t.err
		t.mu.Unlock()
		if err != nil {
			cancel()
			return nil, fmt.Errorf("mcp: connect %q: %w", name, err)
		}
		return t, nil
	case <-ctx.Done():
		cancel()
		return nil, fmt.Errorf("mcp: connect %q: %w", name, ctx.Err())
	}
}

func (t *sseTransport) connect(ctx context.Context) {
	defer t.finish()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.streamURL.String(), nil)
	if err != nil {
		t.fail(err)
		return
	}
	req.Header.Set("Accept", "text/event-stream")
	for key, value := range t.headers {
		req.Header.Set(key, value)
	}
	if err := t.authorize(ctx, req); err != nil {
		t.fail(err)
		return
	}
	resp, err := t.client.Do(req)
	if err != nil {
		t.fail(err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.fail(fmt.Errorf("HTTP %d", resp.StatusCode))
		return
	}
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "text/event-stream") {
		t.fail(fmt.Errorf("unexpected content type %q", resp.Header.Get("Content-Type")))
		return
	}

	t.mu.Lock()
	t.body = resp.Body
	t.mu.Unlock()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	var data strings.Builder
	event := ""
	flush := func() {
		payload := strings.TrimSpace(data.String())
		data.Reset()
		current := event
		event = ""
		if payload == "" {
			return
		}
		if current == "endpoint" {
			t.setEndpoint(payload)
			return
		}
		messages, err := decodeMessages([]byte(payload))
		if err != nil {
			return
		}
		for _, message := range messages {
			t.onMessage(message)
		}
	}

	for scanner.Scan() {
		line := strings.TrimSuffix(scanner.Text(), "\r")
		if line == "" {
			flush()
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, _ := strings.Cut(line, ":")
		value = strings.TrimPrefix(value, " ")
		switch field {
		case "event":
			event = value
		case "data":
			data.WriteString(value)
			data.WriteByte('\n')
		}
	}
	flush()
	if err := scanner.Err(); err != nil && ctx.Err() == nil {
		t.fail(err)
	}
}

func (t *sseTransport) setEndpoint(raw string) {
	resolved, err := t.streamURL.Parse(raw)
	if err != nil {
		t.fail(fmt.Errorf("invalid endpoint %q: %w", raw, err))
		return
	}
	t.mu.Lock()
	t.endpoint = resolved.String()
	t.mu.Unlock()
	t.readyOnce.Do(func() { close(t.ready) })
}

func (t *sseTransport) fail(err error) {
	t.mu.Lock()
	if t.err == nil {
		t.err = err
	}
	t.mu.Unlock()
	t.readyOnce.Do(func() { close(t.ready) })
}

func (t *sseTransport) finish() {
	t.mu.Lock()
	if t.err == nil && t.endpoint == "" {
		t.err = errors.New("stream closed before the endpoint was announced")
	}
	t.mu.Unlock()
	t.readyOnce.Do(func() { close(t.ready) })
	t.onClose()
}

func (t *sseTransport) Send(ctx context.Context, message []byte, expectID int64) error {
	t.mu.Lock()
	endpoint := t.endpoint
	t.mu.Unlock()
	if endpoint == "" {
		return errClosed
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(message))
	if err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	for key, value := range t.headers {
		req.Header.Set(key, value)
	}
	if err := t.authorize(ctx, req); err != nil {
		return err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("mcp: request failed: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("mcp: HTTP %d", resp.StatusCode)
	}
	return nil
}

func (t *sseTransport) Close() error {
	t.closeOnce.Do(func() {
		t.cancel()
		t.mu.Lock()
		body := t.body
		t.body = nil
		t.mu.Unlock()
		if body != nil {
			_ = body.Close()
		}
	})
	return nil
}

func (t *sseTransport) authorize(ctx context.Context, req *http.Request) error {
	return authorizeRequest(ctx, req, t.headers, t.token)
}
