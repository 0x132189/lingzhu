package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"lingzhu/internal/rpc"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// errStreamUnsupported marks a server that does not offer the standalone SSE
// stream the Streamable HTTP transport may open after initialize.
var (
	errStreamUnsupported = errors.New("mcp: server does not offer a GET stream")
	errSessionExpired    = errors.New("mcp: HTTP 404: session expired")
)

// TokenFunc returns a bearer token for one configured oauth endpoint. Force
// asks the resolver to refresh a token the server has already rejected.
type TokenFunc func(ctx context.Context, endpoint string, force bool) (string, error)

// httpTransport speaks the Streamable HTTP transport: every JSON-RPC message is
// one POST, the answer arrives as a JSON body or an SSE stream on the same
// response, and a standalone GET stream carries server-initiated messages.
type httpTransport struct {
	name      string
	endpoint  string
	headers   map[string]string
	client    *http.Client
	onMessage func(rpc.Message)
	onClose   func()
	token     func(context.Context, bool) (string, error)

	mu              sync.Mutex
	sessionID       string
	protocolVersion string
	closed          bool
	streamCancel    context.CancelFunc
	streamOnce      sync.Once
}

func startHTTP(name string, cfg Server, token func(context.Context, bool) (string, error), onMessage func(rpc.Message), onClose func()) (*httpTransport, error) {
	endpoint := strings.TrimSpace(expandEnv(cfg.URL))
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("mcp: server %q needs an http(s) url", name)
	}
	return &httpTransport{
		name:      name,
		endpoint:  endpoint,
		headers:   expandHeaders(cfg.Headers),
		client:    &http.Client{},
		onMessage: onMessage,
		onClose:   onClose,
		token:     token,
	}, nil
}

// SetProtocolVersion records the version negotiated during initialize and sends
// it on later requests, as required by the Streamable HTTP transport.
func (t *httpTransport) SetProtocolVersion(version string) {
	t.mu.Lock()
	t.protocolVersion = version
	t.mu.Unlock()
}

// modernRequestHeaders mirrors the method and name headers required by the
// 2026-07-28 revision so servers can validate the request before parsing it.
func modernRequestHeaders(message []byte) (string, string) {
	var frame struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if err := json.Unmarshal(message, &frame); err != nil {
		return "", ""
	}
	return frame.Method, frame.Params.Name
}

func (t *httpTransport) Send(ctx context.Context, message []byte, expectID int64) error {
	err := t.send(ctx, message, expectID, false)
	var authErr *AuthRequiredError
	if t.token == nil || !errors.As(err, &authErr) || hasAuthHeader(t.headers) {
		return err
	}
	// The cached token was rejected before the request ran, so replaying the
	// same message once after a forced refresh cannot duplicate side effects.
	if retryErr := t.send(ctx, message, expectID, true); retryErr == nil {
		return nil
	}
	return err
}

func (t *httpTransport) send(ctx context.Context, message []byte, expectID int64, force bool) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return errClosed
	}
	session := t.sessionID
	protocol := t.protocolVersion
	t.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint, bytes.NewReader(message))
	if err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	if protocol == modernProtocolVersion {
		if method, name := modernRequestHeaders(message); method != "" {
			req.Header.Set("Mcp-Method", method)
			if name != "" {
				req.Header.Set("Mcp-Name", name)
			}
		}
	}
	for key, value := range t.headers {
		req.Header.Set(key, value)
	}
	if err := t.authorize(ctx, req, force); err != nil {
		return err
	}

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("mcp: request failed: %w", err)
	}
	defer resp.Body.Close()
	if id := resp.Header.Get("Mcp-Session-Id"); id != "" {
		t.mu.Lock()
		t.sessionID = id
		t.mu.Unlock()
	}
	// A legacy Streamable HTTP server answers 404 when it no longer knows this
	// session. Mark the connection closed so the manager can show a failed
	// server and the user can retry instead of every later call failing.
	if resp.StatusCode == http.StatusNotFound && session != "" {
		t.mu.Lock()
		if t.sessionID == session {
			t.sessionID = ""
		}
		t.mu.Unlock()
		return errSessionExpired
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return authRequired(t.name, resp.Header.Get("WWW-Authenticate"))
	}

	switch {
	case resp.StatusCode == http.StatusAccepted || resp.StatusCode == http.StatusNoContent:
		return nil
	case resp.StatusCode >= http.StatusBadRequest:
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := strings.TrimSpace(string(body))
		if detail == "" {
			return fmt.Errorf("mcp: HTTP %d", resp.StatusCode)
		}
		return fmt.Errorf("mcp: HTTP %d: %s", resp.StatusCode, detail)
	}

	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		// A notification has no answer to wait for; do not block on a stream
		// the server may keep open.
		if expectID == 0 {
			return nil
		}
		return readEventStream(ctx, resp.Body, func(message rpc.Message) bool {
			t.onMessage(message)
			return messageIDEquals(message.ID, expectID)
		})
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return fmt.Errorf("mcp: read response: %w", err)
	}
	messages, err := decodeMessages(body)
	if err != nil {
		return fmt.Errorf("mcp: invalid response: %w", err)
	}
	for _, message := range messages {
		t.onMessage(message)
	}
	return nil
}

// StartStream opens the optional standalone GET stream. Servers that reject it
// with 404/405/501 simply keep the POST-only behaviour.
func (t *httpTransport) StartStream(ctx context.Context) {
	t.streamOnce.Do(func() {
		t.mu.Lock()
		if t.closed {
			t.mu.Unlock()
			return
		}
		streamCtx, cancel := context.WithCancel(ctx)
		t.streamCancel = cancel
		t.mu.Unlock()
		go t.streamLoop(streamCtx)
	})
}

func (t *httpTransport) streamLoop(ctx context.Context) {
	delay := 500 * time.Millisecond
	for {
		err := t.openStream(ctx)
		var authErr *AuthRequiredError
		if ctx.Err() != nil || t.isClosed() || errors.Is(err, errStreamUnsupported) || errors.As(err, &authErr) {
			return
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		if delay < 10*time.Second {
			delay *= 2
		}
	}
}

func (t *httpTransport) openStream(ctx context.Context) error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return errClosed
	}
	session := t.sessionID
	protocol := t.protocolVersion
	t.mu.Unlock()
	if session == "" {
		return errStreamUnsupported
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, t.endpoint, nil)
	if err != nil {
		return fmt.Errorf("mcp: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Mcp-Session-Id", session)
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	for key, value := range t.headers {
		req.Header.Set(key, value)
	}
	if err := t.authorize(ctx, req, false); err != nil {
		return err
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("mcp: stream request failed: %w", err)
	}
	defer resp.Body.Close()
	switch resp.StatusCode {
	case http.StatusNotFound, http.StatusMethodNotAllowed, http.StatusNotImplemented:
		return errStreamUnsupported
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return authRequired(t.name, resp.Header.Get("WWW-Authenticate"))
	}
	if resp.StatusCode >= http.StatusBadRequest && resp.StatusCode < http.StatusInternalServerError {
		return errStreamUnsupported
	}
	if resp.StatusCode >= http.StatusInternalServerError {
		return fmt.Errorf("mcp: stream HTTP %d", resp.StatusCode)
	}
	if contentType := strings.ToLower(resp.Header.Get("Content-Type")); !strings.Contains(contentType, "text/event-stream") {
		return errStreamUnsupported
	}
	return readEventStream(ctx, resp.Body, func(message rpc.Message) bool {
		t.onMessage(message)
		return false
	})
}

// authorizeRequest adds the OAuth bearer token unless a configured header
// already supplies an Authorization value. Both HTTP transports share it.
func authorizeRequest(ctx context.Context, req *http.Request, headers map[string]string, token func(context.Context, bool) (string, error), force bool) error {
	if req.Header.Get("Authorization") != "" || hasAuthHeader(headers) {
		return nil
	}
	if token == nil {
		return nil
	}
	value, err := token(ctx, force)
	if err != nil {
		return fmt.Errorf("mcp: oauth: %w", err)
	}
	if strings.TrimSpace(value) != "" {
		req.Header.Set("Authorization", "Bearer "+value)
	}
	return nil
}

func hasAuthHeader(headers map[string]string) bool {
	for key := range headers {
		if strings.EqualFold(key, "Authorization") {
			return true
		}
	}
	return false
}

// authorize adds the OAuth bearer token unless a configured header already
// supplies an Authorization value.
func (t *httpTransport) authorize(ctx context.Context, req *http.Request, force bool) error {
	return authorizeRequest(ctx, req, t.headers, t.token, force)
}

func (t *httpTransport) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// shutdown marks the transport dead exactly once and notifies the client. An
// intentional close also terminates the server session, as the Streamable HTTP
// transport requires; an expired session is already gone.
func (t *httpTransport) shutdown(terminate bool) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	t.closed = true
	session := t.sessionID
	protocol := t.protocolVersion
	cancel := t.streamCancel
	t.mu.Unlock()
	if terminate && session != "" {
		t.terminate(session, protocol)
	}
	if cancel != nil {
		cancel()
	}
	if t.onClose != nil {
		t.onClose()
	}
}

func (t *httpTransport) terminate(session, protocol string) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, t.endpoint, nil)
	if err != nil {
		return
	}
	req.Header.Set("Mcp-Session-Id", session)
	if protocol != "" {
		req.Header.Set("MCP-Protocol-Version", protocol)
	}
	for key, value := range t.headers {
		req.Header.Set(key, value)
	}
	if err := t.authorize(ctx, req, false); err != nil {
		return
	}
	resp, err := t.client.Do(req)
	if err != nil {
		return
	}
	_ = resp.Body.Close()
}

func (t *httpTransport) Close() error {
	t.shutdown(true)
	return nil
}
