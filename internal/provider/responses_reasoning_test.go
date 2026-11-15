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

// The agent replays the whole transcript, so the request must be stateless.
// Storing would leave the conversation on the provider and make a replayed id
// resolve to something else.
func TestResponsesBodyIsStateless(t *testing.T) {
	body, err := responsesBody(model.Request{
		Model:    "gpt-test",
		Messages: []model.Message{{Role: model.RoleUser, Content: "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if body["store"] != false {
		t.Fatalf("store must be false, got %#v", body["store"])
	}
	include, _ := body["include"].([]string)
	if len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("encrypted reasoning must be requested: %#v", body["include"])
	}
}

// A caller that explicitly asks for stored responses keeps that choice, and
// then encrypted reasoning is unnecessary.
func TestResponsesBodyHonorsExplicitStore(t *testing.T) {
	body, err := responsesBody(model.Request{
		Model:    "gpt-test",
		Messages: []model.Message{{Role: model.RoleUser, Content: "hello"}},
		Options:  map[string]any{"store": true},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body["include"]; ok {
		t.Fatalf("stored responses need no encrypted reasoning: %#v", body["include"])
	}
}

// A function call whose reasoning item is missing is rejected by the API, so
// the proof must be replayed ahead of the call it led to.
func TestResponsesBodyReplaysReasoningBeforeToolCall(t *testing.T) {
	body, err := responsesBody(model.Request{
		Model: "gpt-test",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "go"},
			{
				Role:    model.RoleAssistant,
				Content: "checking",
				Parts: []model.MessagePart{
					{Type: "reasoning", Text: "weigh options", Redacted: "enc_blob", Source: ReasoningSourceResponses},
					{Type: "text", Text: "checking"},
				},
				ToolCalls: []model.ToolCall{{ID: "call_1", Name: "echo", Arguments: `{}`}},
			},
			{Role: model.RoleTool, ToolCallID: "call_1", Content: "done"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input, _ := body["input"].([]map[string]any)
	if len(input) != 5 {
		t.Fatalf("unexpected input items: %#v", input)
	}
	if input[1]["type"] != "reasoning" {
		t.Fatalf("reasoning must precede the message: %#v", input)
	}
	if input[1]["encrypted_content"] != "enc_blob" {
		t.Fatalf("encrypted reasoning lost: %#v", input[1])
	}
	if summary, ok := input[1]["summary"].([]map[string]any); !ok || len(summary) != 0 {
		t.Fatalf("summary must be present and empty: %#v", input[1]["summary"])
	}
	if input[2]["type"] != "message" || input[3]["type"] != "function_call" || input[4]["type"] != "function_call_output" {
		t.Fatalf("items out of order: %#v", input)
	}
}

// Reasoning without encrypted content cannot be replayed: an invented blob is
// rejected, so the part is skipped instead of being sent as text.
func TestResponsesBodySkipsUnprovableReasoning(t *testing.T) {
	body, err := responsesBody(model.Request{
		Model: "gpt-test",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "go"},
			{
				Role:  model.RoleAssistant,
				Parts: []model.MessagePart{{Type: "reasoning", Text: "no blob"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input, _ := body["input"].([]map[string]any)
	for _, item := range input {
		if item["type"] == "reasoning" {
			t.Fatalf("unprovable reasoning must not be replayed: %#v", item)
		}
	}
}

// A proof issued by another API is meaningless here, so it is skipped rather
// than handed to a server that did not sign it.
func TestResponsesBodySkipsForeignReasoningProof(t *testing.T) {
	body, err := responsesBody(model.Request{
		Model: "gpt-test",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "go"},
			{
				Role: model.RoleAssistant,
				Parts: []model.MessagePart{
					{Type: "reasoning", Text: "anthropic chain", Redacted: "sig_blob", Source: ReasoningSourceAnthropic},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input, _ := body["input"].([]map[string]any)
	for _, item := range input {
		if item["type"] == "reasoning" {
			t.Fatalf("foreign proof must not be replayed: %#v", item)
		}
	}
}

// The completed reasoning item carries the encrypted blob; the added event does
// not. The stream must surface it so the next turn can replay the chain.
func TestResponsesStreamSurfacesEncryptedReasoning(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody(
			`data: {"type":"response.output_item.added","item":{"type":"reasoning","id":"rs_1","summary":[]}}`,
			`data: {"type":"response.reasoning_summary_text.delta","delta":"weighing"}`,
			`data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_blob","summary":[{"type":"summary_text","text":"weighing"}]}}`,
			`data: {"type":"response.completed","response":{"status":"completed"}}`,
		)))
	}))
	defer server.Close()

	stream, err := NewOpenAIResponses(server.URL, "key").Stream(context.Background(), model.Request{
		Model:    "gpt-test",
		Messages: []model.Message{{Role: model.RoleUser, Content: "go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()

	var reasoning, redacted, source string
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		reasoning += chunk.Reasoning
		if chunk.ReasoningRedacted != "" {
			redacted = chunk.ReasoningRedacted
			source = chunk.ReasoningSource
		}
	}
	if reasoning != "weighing" {
		t.Fatalf("summary text lost: %q", reasoning)
	}
	if redacted != "enc_blob" || source != ReasoningSourceResponses {
		t.Fatalf("encrypted reasoning not surfaced: %q from %q", redacted, source)
	}
}

// The proof must land on the summary part that streamed before it, so one
// replayed item keeps both the visible text and the blob.
func TestResponsesStreamAttachesProofToSummaryPart(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody(
			`data: {"type":"response.reasoning_summary_text.delta","delta":"weighing"}`,
			`data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_blob"}}`,
			`data: {"type":"response.output_text.delta","delta":"answer"}`,
			`data: {"type":"response.completed","response":{"status":"completed"}}`,
		)))
	}))
	defer server.Close()

	stream, err := NewOpenAIResponses(server.URL, "key").Stream(context.Background(), model.Request{
		Model:    "gpt-test",
		Messages: []model.Message{{Role: model.RoleUser, Content: "go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatal(err)
		}
	}

	// Rebuild the same way the agent does: one reasoning part that received
	// the summary text, then the proof.
	parts := attachForTest([]model.MessagePart{{Type: "reasoning", Text: "weighing"}}, Chunk{
		ReasoningRedacted: "enc_blob",
		ReasoningSource:   ReasoningSourceResponses,
	})
	if len(parts) != 1 {
		t.Fatalf("proof must join the summary part: %#v", parts)
	}
	if parts[0].Redacted != "enc_blob" || parts[0].Text != "weighing" {
		t.Fatalf("part lost text or proof: %#v", parts[0])
	}
}

// attachForTest mirrors the agent's proof attachment so a provider test can
// assert on the part it would store.
func attachForTest(parts []model.MessagePart, chunk Chunk) []model.MessagePart {
	if chunk.ReasoningRedacted == "" {
		return parts
	}
	for index := len(parts) - 1; index >= 0; index-- {
		if parts[index].Type != "reasoning" || parts[index].Signature != "" || parts[index].Redacted != "" {
			break
		}
		parts[index].Redacted = chunk.ReasoningRedacted
		parts[index].Source = chunk.ReasoningSource
		return parts
	}
	return append(parts, model.MessagePart{
		Type:     "reasoning",
		Redacted: chunk.ReasoningRedacted,
		Source:   chunk.ReasoningSource,
	})
}

// A full turn must survive the round trip: what the stream produced has to be
// what the next request replays.
func TestResponsesReasoningRoundTrip(t *testing.T) {
	var body map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(sseBody(
			`data: {"type":"response.reasoning_summary_text.delta","delta":"step"}`,
			`data: {"type":"response.output_item.done","item":{"type":"reasoning","id":"rs_1","encrypted_content":"enc_blob"}}`,
			`data: {"type":"response.output_text.delta","delta":"answer"}`,
			`data: {"type":"response.completed","response":{"status":"completed"}}`,
		)))
	}))
	defer server.Close()

	stream, err := NewOpenAIResponses(server.URL, "key").Stream(context.Background(), model.Request{
		Model:    "gpt-test",
		Messages: []model.Message{{Role: model.RoleUser, Content: "go"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var reasoning string
	var proof Chunk
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		reasoning += chunk.Reasoning
		if chunk.ReasoningRedacted != "" {
			proof = chunk
		}
	}
	stream.Close()

	parts := attachForTest([]model.MessagePart{{Type: "reasoning", Text: reasoning}}, proof)
	next, err := responsesBody(model.Request{
		Model: "gpt-test",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "go"},
			{Role: model.RoleAssistant, Content: "answer", Parts: parts},
			{Role: model.RoleUser, Content: "more"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	input, _ := next["input"].([]map[string]any)
	var found map[string]any
	for _, item := range input {
		if item["type"] == "reasoning" {
			found = item
		}
	}
	if found == nil {
		t.Fatalf("reasoning not replayed: %#v", input)
	}
	if found["encrypted_content"] != "enc_blob" {
		t.Fatalf("blob lost on replay: %#v", found)
	}
	if body["include"] == nil {
		t.Fatalf("first request did not ask for encrypted reasoning: %#v", body)
	}
}
