package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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

func TestStreamReportsReasoningOnlyChunks(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"step \"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"one\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"answer\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewOpenAI(server.URL, "key")
	stream, err := p.Stream(context.Background(), model.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var reasoning, text string
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		reasoning += chunk.Reasoning
		text += chunk.Text
	}
	if reasoning != "step one" {
		t.Fatalf("want reasoning %q, got %q", "step one", reasoning)
	}
	if text != "answer" {
		t.Fatalf("want answer %q, got %q", "answer", text)
	}
}

func TestStreamMergesModelOptions(t *testing.T) {
	var got map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := decodeJSON(r, &got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewOpenAI(server.URL, "key")
	stream, err := p.Stream(context.Background(), model.Request{
		Model:    "m",
		Messages: []model.Message{{Role: model.RoleUser, Content: "hi"}},
		Options:  map[string]any{"reasoningEffort": "high", "textVerbosity": "low", "thinking": map[string]any{"type": "enabled"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if got["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", got["reasoning_effort"])
	}
	if got["verbosity"] != "low" {
		t.Fatalf("verbosity = %v", got["verbosity"])
	}
	thinking, _ := got["thinking"].(map[string]any)
	if thinking["type"] != "enabled" {
		t.Fatalf("thinking = %v", got["thinking"])
	}
	if got["model"] != "m" {
		t.Fatalf("model = %v", got["model"])
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

func TestChatRequestLowersAttachments(t *testing.T) {
	out := toChatRequest(model.Request{Messages: []model.Message{
		{
			Role:       model.RoleTool,
			Content:    "read media",
			ToolCallID: "call_1",
			Attachments: []model.Attachment{
				{Mime: "image/png", Name: "pixel.png", Data: "AAA"},
				{Mime: "application/pdf", Name: "doc.pdf", Data: "BBB"},
			},
		},
	}})
	if len(out.Messages) != 2 {
		t.Fatalf("want tool message plus media message, got %+v", out.Messages)
	}
	if out.Messages[0].Role != "tool" || out.Messages[0].Content != "read media" {
		t.Fatalf("unexpected tool message: %+v", out.Messages[0])
	}
	parts, ok := out.Messages[1].Content.([]chatContentPart)
	if !ok || len(parts) != 2 {
		t.Fatalf("unexpected media content: %#v", out.Messages[1].Content)
	}
	if parts[0].Type != "image_url" || parts[0].ImageURL == nil || parts[0].ImageURL.URL != "data:image/png;base64,AAA" {
		t.Fatalf("unexpected image part: %+v", parts[0])
	}
	if parts[1].Type != "file" || parts[1].File == nil || parts[1].File.Filename != "doc.pdf" || parts[1].File.FileData != "data:application/pdf;base64,BBB" {
		t.Fatalf("unexpected PDF part: %+v", parts[1])
	}
}

func TestStreamUsesTokenSource(t *testing.T) {
	header := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		header = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewOpenAI(server.URL, "static-key")
	p.TokenSource = func(context.Context) (string, error) { return "dynamic", nil }
	stream, err := p.Stream(context.Background(), model.Request{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	if _, err := stream.Recv(); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if header != "Bearer dynamic" {
		t.Fatalf("token source not used: %q", header)
	}
}

func TestChatRequestInlinesTextAttachments(t *testing.T) {
	out := toChatRequest(model.Request{Messages: []model.Message{{
		Role:    model.RoleUser,
		Content: "read this",
		Attachments: []model.Attachment{
			{Mime: "text/plain", Name: "notes.txt", Data: "aGVsbG8="},
		},
	}}})
	if len(out.Messages) != 2 {
		t.Fatalf("want user message plus media message, got %+v", out.Messages)
	}
	parts, ok := out.Messages[1].Content.([]chatContentPart)
	if !ok || len(parts) != 1 || parts[0].Type != "text" {
		t.Fatalf("unexpected text attachment: %#v", out.Messages[1].Content)
	}
	if !strings.Contains(parts[0].Text, "notes.txt") || !strings.Contains(parts[0].Text, "hello") {
		t.Fatalf("unexpected text part: %q", parts[0].Text)
	}
}

func TestChatRequestSkipsEmptyBodyForAttachmentOnlyPrompt(t *testing.T) {
	out := toChatRequest(model.Request{Messages: []model.Message{{
		Role: model.RoleUser,
		Attachments: []model.Attachment{
			{Mime: "image/png", Name: "pixel.png", Data: "AAA"},
		},
	}}})
	if len(out.Messages) != 1 {
		t.Fatalf("want only the media message, got %+v", out.Messages)
	}
	if out.Messages[0].Role != "user" {
		t.Fatalf("unexpected role: %+v", out.Messages[0])
	}
	parts, ok := out.Messages[0].Content.([]chatContentPart)
	if !ok || len(parts) != 1 || parts[0].Type != "image_url" {
		t.Fatalf("unexpected media content: %#v", out.Messages[0].Content)
	}
}

func TestChatRequestLabelsTextRangeAndDirectory(t *testing.T) {
	out := toChatRequest(model.Request{Messages: []model.Message{{
		Role:    model.RoleUser,
		Content: "read this",
		Attachments: []model.Attachment{
			{Mime: "text/plain", Name: "notes.txt", Data: "aGVsbG8=", Kind: "text", Start: 2, End: 4},
			{Mime: "text/plain", Name: "src/", Data: "bGlzdGluZw==", Kind: "directory"},
		},
	}}})
	parts, ok := out.Messages[1].Content.([]chatContentPart)
	if !ok || len(parts) != 2 {
		t.Fatalf("unexpected text attachments: %#v", out.Messages[1].Content)
	}
	if !strings.Contains(parts[0].Text, "notes.txt L2-L4") {
		t.Fatalf("missing range label: %q", parts[0].Text)
	}
	if !strings.Contains(parts[1].Text, "directory src/") {
		t.Fatalf("missing directory label: %q", parts[1].Text)
	}
}

func TestToChatRequestIncludesToolChoice(t *testing.T) {
	out := toChatRequest(model.Request{
		Model:      "m",
		ToolChoice: "none",
		Tools:      []model.ToolDef{{Name: "echo", Description: "d", Schema: []byte(`{"type":"object"}`)}},
	})
	if out.ToolChoice != "none" || len(out.Tools) != 1 {
		t.Fatalf("tool choice must keep the cached tool definitions: %+v", out)
	}
}

func TestParseRetryAfter(t *testing.T) {
	if got := ParseRetryAfter("3"); got != 3*time.Second {
		t.Fatalf("seconds form = %v", got)
	}
	if got := ParseRetryAfter(""); got != 0 {
		t.Fatalf("empty form = %v", got)
	}
	if got := ParseRetryAfter("not-a-date"); got != 0 {
		t.Fatalf("invalid form = %v", got)
	}
}

func TestStreamSendsRequestHeaders(t *testing.T) {
	var seen string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = r.Header.Get("X-Agent")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	p := NewOpenAI(server.URL, "key")
	stream, err := p.Stream(context.Background(), model.Request{
		Model:   "m",
		Headers: map[string]string{"X-Agent": "demo"},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		_, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	if seen != "demo" {
		t.Fatalf("request header was not sent: %q", seen)
	}
}
