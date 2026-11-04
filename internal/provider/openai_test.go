package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"lingzhu/internal/model"
)

func decodeJSON(r *http.Request, v any) error {
	return json.NewDecoder(r.Body).Decode(v)
}

func TestStreamParsesSSE(t *testing.T) {
	var gotBody chatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := decodeJSON(r, &gotBody); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"echo\",\"arguments\":\"{\\\"a\\\":1}\"}}]}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewOpenAI(server.URL, "key")
	stream, err := p.Stream(context.Background(), model.Request{
		Model:    "m",
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
		Tools:    []model.ToolDef{{Name: "echo", Description: "d", Schema: []byte(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var text string
	var calls []ToolCallDelta
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		text += chunk.Text
		calls = append(calls, chunk.ToolCalls...)
	}
	if text != "hello" {
		t.Fatalf("want hello, got %q", text)
	}
	if len(calls) != 1 || calls[0].Name != "echo" || calls[0].ID != "call_1" {
		t.Fatalf("unexpected tool calls: %+v", calls)
	}
	if gotBody.Model != "m" || !gotBody.Stream || len(gotBody.Tools) != 1 {
		t.Fatalf("unexpected request body: %+v", gotBody)
	}
	if gotBody.StreamOptions == nil || !gotBody.StreamOptions.IncludeUsage {
		t.Fatalf("usage was not requested: %+v", gotBody.StreamOptions)
	}
	if gotBody.Messages[0].Role != "user" || gotBody.Messages[0].Content != "hi" {
		t.Fatalf("unexpected messages: %+v", gotBody.Messages)
	}
}

func TestStreamReportsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad key", http.StatusUnauthorized)
	}))
	defer server.Close()

	p := NewOpenAI(server.URL, "key")
	if _, err := p.Stream(context.Background(), model.Request{Model: "m"}); err == nil {
		t.Fatal("want error on non-200 response")
	}
}

func TestStreamReportsFinishReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"truncated\"},\"finish_reason\":\"length\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewOpenAI(server.URL, "key")
	stream, err := p.Stream(context.Background(), model.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	chunk, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Text != "truncated" || chunk.FinishReason != "length" {
		t.Fatalf("unexpected chunk: %+v", chunk)
	}
}

func TestStreamNormalizesUsage(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"prompt_tokens_details\":{\"cached_tokens\":40}}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewOpenAI(server.URL, "key")
	stream, err := p.Stream(context.Background(), model.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var usage *Usage
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
	}
	if usage == nil {
		t.Fatal("usage chunk not reported")
	}
	if usage.Input != 60 || usage.CacheRead != 40 || usage.Output != 7 {
		t.Fatalf("unexpected usage: %+v", usage)
	}
}

func TestIsRetryable(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"rate limit", &StatusError{Code: 429}, true},
		{"server error", &StatusError{Code: 503}, true},
		{"bad key", &StatusError{Code: 401}, false},
		{"bad request", &StatusError{Code: 400}, false},
		{"truncated stream", io.ErrUnexpectedEOF, true},
		{"plain error", errors.New("bad body"), false},
	}
	for _, tc := range cases {
		if got := IsRetryable(tc.err); got != tc.want {
			t.Fatalf("%s: IsRetryable = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestStreamClassifiesOverflow(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":{"message":"This model's maximum context length is 8192 tokens"}}`, http.StatusBadRequest)
	}))
	defer server.Close()

	p := NewOpenAI(server.URL, "key")
	_, err := p.Stream(context.Background(), model.Request{Model: "m"})
	if !IsContextOverflow(err) {
		t.Fatalf("want context overflow, got %v", err)
	}
}
