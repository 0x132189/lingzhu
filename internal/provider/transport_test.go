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

	"lingzhu/internal/model"
)

func sseBody(events ...string) string {
	var b strings.Builder
	for _, event := range events {
		b.WriteString(event)
		b.WriteString("\n\n")
	}
	return b.String()
}

func TestAnthropicStreamTranslatesMessagesAndTools(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "key" || r.Header.Get("anthropic-version") == "" {
			t.Fatalf("missing anthropic headers: %v", r.Header)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody(
			`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10,"cache_read_input_tokens":3,"cache_creation_input_tokens":2}}}`,
			`event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tool_1","name":"echo"}}`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
			`event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
			`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`,
			`event: message_stop
data: {"type":"message_stop"}`,
		)))
	}))
	defer server.Close()

	p := NewAnthropic(server.URL, "key")
	stream, err := p.Stream(context.Background(), model.Request{
		Model: "claude-test",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "be terse"},
			{Role: model.RoleUser, Content: "read"},
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "call_0", Name: "read", Arguments: `{"path":"a"}`}}},
			{Role: model.RoleTool, ToolCallID: "call_0", Content: "content"},
		},
		Tools:      []model.ToolDef{{Name: "read", Description: "read a file", Schema: []byte(`{"type":"object"}`)}},
		ToolChoice: "none",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	if body["system"] != "be terse" {
		t.Fatalf("system prompt not translated: %+v", body)
	}
	messages, _ := body["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("unexpected message turns: %+v", messages)
	}
	last, _ := messages[2].(map[string]any)
	blocks, _ := last["content"].([]any)
	if len(blocks) != 1 || blocks[0].(map[string]any)["type"] != "tool_result" {
		t.Fatalf("tool_result block missing: %+v", last)
	}
	if body["tool_choice"].(map[string]any)["type"] != "none" {
		t.Fatalf("tool choice not translated: %+v", body["tool_choice"])
	}

	var text string
	var calls []ToolCallDelta
	var usage *Usage
	var finish string
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
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
	}
	if text != "hi" {
		t.Fatalf("unexpected text: %q", text)
	}
	if len(calls) != 2 || calls[0].ID != "tool_1" || calls[1].Arguments != `{"a":1}` {
		t.Fatalf("tool calls not merged: %+v", calls)
	}
	if finish != "tool_calls" {
		t.Fatalf("unexpected finish: %q", finish)
	}
	if usage == nil || usage.Input != 10 || usage.Output != 5 || usage.CacheRead != 3 || usage.CacheWrite != 2 {
		t.Fatalf("usage not accumulated: %+v", usage)
	}
}

func TestResponsesStreamTranslatesItemsAndEvents(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody(
			`data: {"type":"response.output_item.added","item":{"type":"function_call","id":"item_1","call_id":"call_1","name":"echo"}}`,
			`data: {"type":"response.output_text.delta","delta":"hi"}`,
			`data: {"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"{\"a\":"}`,
			`data: {"type":"response.function_call_arguments.delta","item_id":"item_1","delta":"1}"}`,
			`data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":12,"output_tokens":4,"input_tokens_details":{"cached_tokens":2}}}}`,
		)))
	}))
	defer server.Close()

	p := NewOpenAIResponses(server.URL, "key")
	stream, err := p.Stream(context.Background(), model.Request{
		Model: "gpt-test",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "go"},
			{Role: model.RoleAssistant, ToolCalls: []model.ToolCall{{ID: "call_1", Name: "echo", Arguments: `{}`}}},
			{Role: model.RoleTool, ToolCallID: "call_1", Content: "done"},
		},
		Tools:   []model.ToolDef{{Name: "echo", Description: "echo", Schema: []byte(`{"type":"object"}`)}},
		Options: map[string]any{"max_tokens": 2048},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	input, _ := body["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("unexpected input items: %+v", input)
	}
	if input[1].(map[string]any)["type"] != "function_call" || input[2].(map[string]any)["type"] != "function_call_output" {
		t.Fatalf("tool items not translated: %+v", input)
	}
	if body["tools"].([]any)[0].(map[string]any)["type"] != "function" {
		t.Fatalf("tool schema not translated: %+v", body["tools"])
	}
	if body["max_output_tokens"] != float64(2048) {
		t.Fatalf("max_tokens should map to max_output_tokens: %+v", body)
	}

	var text string
	var args string
	var usage *Usage
	var finish string
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		text += chunk.Text
		for _, call := range chunk.ToolCalls {
			args += call.Arguments
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
		}
	}
	if text != "hi" || args != `{"a":1}` {
		t.Fatalf("unexpected stream: text=%q args=%q", text, args)
	}
	if finish != "stop" {
		t.Fatalf("unexpected finish: %q", finish)
	}
	if usage == nil || usage.Input != 10 || usage.Output != 4 || usage.CacheRead != 2 {
		t.Fatalf("usage not normalized: %+v", usage)
	}
}

func TestCatalogSelectsTransportByAPI(t *testing.T) {
	catalog, err := NewCatalog([]ProviderSpec{{
		ID:      "p",
		BaseURL: "https://example.com",
		APIKey:  "key",
		API:     "messages",
		Models:  []ModelSpec{{ID: "claude"}},
	}}, "p", "claude")
	if err != nil {
		t.Fatal(err)
	}
	target, err := catalog.Resolve(Selection{Provider: "p", Model: "claude"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := target.Provider.(*Anthropic); !ok {
		t.Fatalf("messages provider not selected: %T", target.Provider)
	}
}

func TestCatalogSelectsModelLevelTransport(t *testing.T) {
	catalog, err := NewCatalog([]ProviderSpec{{
		ID:      "p",
		BaseURL: "https://example.com",
		APIKey:  "key",
		Models:  []ModelSpec{{ID: "chat"}, {ID: "gpt-5", API: "responses"}},
	}}, "p", "chat")
	if err != nil {
		t.Fatal(err)
	}
	target, err := catalog.Resolve(Selection{Provider: "p", Model: "gpt-5"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := target.Provider.(*OpenAIResponses); !ok {
		t.Fatalf("model api override not applied: %T", target.Provider)
	}
}

func TestCatalogRejectsUnknownAPI(t *testing.T) {
	_, err := NewCatalog([]ProviderSpec{{
		ID:      "p",
		BaseURL: "https://example.com",
		APIKey:  "key",
		API:     "websocket",
		Models:  []ModelSpec{{ID: "m"}},
	}}, "p", "m")
	if err == nil || !strings.Contains(err.Error(), "unsupported api") {
		t.Fatalf("unknown transport should be rejected: %v", err)
	}
	_, err = NewCatalog([]ProviderSpec{{
		ID:      "p",
		BaseURL: "https://example.com",
		APIKey:  "key",
		Models:  []ModelSpec{{ID: "m", API: "sse"}},
	}}, "p", "m")
	if err == nil || !strings.Contains(err.Error(), "unsupported api") {
		t.Fatalf("unknown model transport should be rejected: %v", err)
	}
	if _, err := NewCatalog([]ProviderSpec{{
		ID:      "p",
		BaseURL: "https://example.com",
		APIKey:  "key",
		API:     "anthropic",
		Models:  []ModelSpec{{ID: "m", API: "openai-responses"}},
	}}, "p", "m"); err != nil {
		t.Fatalf("aliases should be accepted: %v", err)
	}
}
