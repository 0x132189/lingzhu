package server

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
)

type staticProvider struct{}

func (staticProvider) Stream(context.Context, model.Request) (provider.Stream, error) {
	return &staticStream{chunks: []provider.Chunk{{Text: "hi"}, {Text: "!"}}}, nil
}

type staticStream struct {
	chunks []provider.Chunk
	i      int
}

func (s *staticStream) Recv() (provider.Chunk, error) {
	if s.i >= len(s.chunks) {
		return provider.Chunk{}, io.EOF
	}
	chunk := s.chunks[s.i]
	s.i++
	return chunk, nil
}

func (s *staticStream) Close() error { return nil }

func testStore(t *testing.T) *session.Store {
	t.Helper()
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newServerWith(t, staticProvider{})
}

func newServerWith(t *testing.T, p provider.Provider) *Server {
	t.Helper()
	store := testStore(t)
	catalog := provider.Static("test", "Test", p, []provider.ModelSpec{{ID: "test-model", Name: "test-model"}}, "test-model")
	return New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2}), store)
}

// blockProvider never answers until its run is cancelled.
type blockProvider struct {
	started chan struct{}
}

func (p *blockProvider) Stream(ctx context.Context, _ model.Request) (provider.Stream, error) {
	select {
	case p.started <- struct{}{}:
	default:
	}
	return &blockStream{ctx: ctx}, nil
}

type blockStream struct {
	ctx context.Context
}

func (s *blockStream) Recv() (provider.Chunk, error) {
	<-s.ctx.Done()
	return provider.Chunk{}, s.ctx.Err()
}

func (s *blockStream) Close() error { return nil }

func prompt(t *testing.T, url, session, text string) *http.Response {
	t.Helper()
	resp, err := http.Post(url+"/sessions/"+session+"/prompt", "application/json", strings.NewReader(`{"text":"`+text+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestPromptStreamsSSE(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json",
		strings.NewReader(`{"text":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("want SSE content type, got %q", got)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	text := string(body)
	for _, want := range []string{"event: text.delta", "event: run.end", `"reason":"done"`} {
		if !strings.Contains(text, want) {
			t.Fatalf("stream missing %q:\n%s", want, text)
		}
	}

	messages, err := http.Get(srv.URL + "/sessions/s1/messages")
	if err != nil {
		t.Fatal(err)
	}
	defer messages.Body.Close()
	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(messages.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Messages) != 2 || payload.Messages[1].Content != "hi!" {
		t.Fatalf("unexpected messages: %+v", payload.Messages)
	}
}

// gateProvider pauses the first response until the test releases it, so input
// can be queued while the turn is active.
type gateProvider struct {
	mu      sync.Mutex
	calls   int
	started chan struct{}
	release chan struct{}
}

func (p *gateProvider) Stream(context.Context, model.Request) (provider.Stream, error) {
	p.mu.Lock()
	p.calls++
	call := p.calls
	p.mu.Unlock()
	if call == 1 {
		select {
		case p.started <- struct{}{}:
		default:
		}
		return &gateStream{release: p.release}, nil
	}
	return &staticStream{chunks: []provider.Chunk{{Text: "second"}}}, nil
}

type gateStream struct {
	release <-chan struct{}
	sent    bool
}

func (s *gateStream) Recv() (provider.Chunk, error) {
	if s.sent {
		return provider.Chunk{}, io.EOF
	}
	<-s.release
	s.sent = true
	return provider.Chunk{Text: "first"}, nil
}

func (s *gateStream) Close() error { return nil }

func TestInputQueuesWhileRunIsActive(t *testing.T) {
	p := &gateProvider{started: make(chan struct{}, 1), release: make(chan struct{})}
	store := testStore(t)
	catalog := provider.Static("test", "Test", p, []provider.ModelSpec{{ID: "m", Name: "m"}}, "m")
	srv := httptest.NewServer(New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 4}), store).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(`{"text":"start"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	select {
	case <-p.started:
	case <-time.After(2 * time.Second):
		t.Fatal("run did not start")
	}
	running, err := http.Get(srv.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	var runningInfo struct {
		Running bool `json:"running"`
	}
	if err := json.NewDecoder(running.Body).Decode(&runningInfo); err != nil {
		t.Fatal(err)
	}
	running.Body.Close()
	if !runningInfo.Running {
		t.Fatal("session should report running while the model is active")
	}

	queued := post(t, srv.URL+"/sessions/s1/input", `{"text":"queued"}`)
	if queued.StatusCode != http.StatusAccepted {
		t.Fatalf("want 202 for queued input, got %d", queued.StatusCode)
	}
	close(p.release)
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	running, err = http.Get(srv.URL + "/sessions/s1")
	if err != nil {
		t.Fatal(err)
	}
	if err := json.NewDecoder(running.Body).Decode(&runningInfo); err != nil {
		t.Fatal(err)
	}
	running.Body.Close()
	if runningInfo.Running {
		t.Fatal("session should be idle after run.end")
	}

	messages, err := store.Messages("s1")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"start", "first", "queued", "second"}
	if len(messages) != len(want) {
		t.Fatalf("want %d messages, got %+v", len(want), messages)
	}
	for index, text := range want {
		if messages[index].Content != text {
			t.Fatalf("message %d = %q, want %q", index, messages[index].Content, text)
		}
	}
}

func TestInputRequiresActiveRun(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	resp := post(t, srv.URL+"/sessions/s1/input", `{"text":"late"}`)
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("want 409 without a run, got %d", resp.StatusCode)
	}
}

func TestGlobalEventsTagSessions(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL+"/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("want 200, got %d", resp.StatusCode)
	}

	lines := make(chan string, 128)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
		close(lines)
	}()
	go func() {
		response, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(`{"text":"hello"}`))
		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			response.Body.Close()
		}
	}()

	timeout := time.After(3 * time.Second)
	for {
		select {
		case line, ok := <-lines:
			if !ok {
				t.Fatal("global event stream closed")
			}
			if strings.HasPrefix(line, "data: ") &&
				strings.Contains(line, `"sessionId":"s1"`) &&
				strings.Contains(line, `"type":"run.end"`) {
				return
			}
		case <-timeout:
			t.Fatal("global event stream did not report run.end")
		}
	}
}

func TestListSessionsEmptyIsAnArray(t *testing.T) {
	srv := newTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	var payload struct {
		Sessions []session.Info `json:"sessions"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Sessions == nil {
		t.Fatalf("empty session list must encode as [], got %s", rec.Body.String())
	}
}

func TestPromptRejectsEmptyText(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400, got %d", resp.StatusCode)
	}
}

func TestReplyPermissionValidatesInput(t *testing.T) {
	srv := httptest.NewServer(newTestServer(t).Handler())
	defer srv.Close()

	bad := post(t, srv.URL+"/sessions/s1/permission/per_1", `{"decision":"maybe"}`)
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("want 400 for unknown decision, got %d", bad.StatusCode)
	}
	missing := post(t, srv.URL+"/sessions/s1/permission/per_missing", `{"decision":"once"}`)
	if missing.StatusCode != http.StatusNotFound {
		t.Fatalf("want 404 for unknown request, got %d", missing.StatusCode)
	}
}

func post(t *testing.T, url, body string) *http.Response {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	return resp
}

type recordingProvider struct {
	model string
}

func (p *recordingProvider) Stream(_ context.Context, req model.Request) (provider.Stream, error) {
	p.model = req.Model
	return &staticStream{chunks: []provider.Chunk{{Text: "ok"}}}, nil
}

func TestSessionModelSelectionRoundTrip(t *testing.T) {
	store := testStore(t)
	recorder := &recordingProvider{}
	catalog := provider.Static("p1", "P1", recorder, []provider.ModelSpec{
		{ID: "m1", ContextWindow: 1000},
		{ID: "m2", ContextWindow: 2000},
	}, "m1")
	srv := httptest.NewServer(New(agent.New(store, catalog, tool.NewRegistry(), nil, agent.Options{MaxSteps: 2}), store).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/sessions/s1/prompt", "application/json",
		strings.NewReader(`{"text":"hello","provider":"p1","model":"m2"}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if recorder.model != "m2" {
		t.Fatalf("prompt used %q, want m2", recorder.model)
	}
	if info, err := store.Info("s1"); err != nil || info.Provider != "p1" || info.Model != "m2" {
		t.Fatalf("selection was not stored: %+v %v", info, err)
	}

	req, err := http.NewRequest(http.MethodPatch, srv.URL+"/sessions/s1", strings.NewReader(`{"model":"m1"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	patch, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	patch.Body.Close()
	if patch.StatusCode != http.StatusNoContent {
		t.Fatalf("want 204, got %d", patch.StatusCode)
	}

	contextResp, err := http.Get(srv.URL + "/sessions/s1/context")
	if err != nil {
		t.Fatal(err)
	}
	defer contextResp.Body.Close()
	var contextPayload struct {
		Provider string `json:"provider"`
		Model    string `json:"model"`
		Window   int    `json:"window"`
	}
	if err := json.NewDecoder(contextResp.Body).Decode(&contextPayload); err != nil {
		t.Fatal(err)
	}
	if contextPayload.Provider != "p1" || contextPayload.Model != "m1" || contextPayload.Window != 1000 {
		t.Fatalf("unexpected context: %+v", contextPayload)
	}
}
