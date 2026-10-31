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
	store, err := session.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return New(agent.New(store, staticProvider{}, tool.NewRegistry(), "test-model", 2), store)
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
			Role    string `json:"Role"`
			Content string `json:"Content"`
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
