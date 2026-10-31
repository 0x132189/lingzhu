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
