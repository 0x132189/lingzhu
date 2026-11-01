package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func newTestServer(t *testing.T) *Server {
	t.Helper()
	return newServerWith(t, staticProvider{})
}

func newServerWith(t *testing.T, p provider.Provider) *Server {
	t.Helper()
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
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
