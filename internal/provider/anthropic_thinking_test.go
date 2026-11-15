package provider

import (
	"bufio"
	"strings"
	"testing"

	"lingzhu/internal/model"
)

// A reasoning part that carries a signature must be replayed as a thinking
// block; Anthropic rejects a continuation that drops the proof.
func TestAnthropicReplaysSignedThinking(t *testing.T) {
	body, err := anthropicBody(model.Request{
		Model: "claude",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "question"},
			{
				Role:    model.RoleAssistant,
				Content: "answer",
				Parts: []model.MessagePart{
					{Type: "reasoning", Text: "weighing options", Signature: "sig_abc"},
					{Type: "text", Text: "answer"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages, _ := body["messages"].([]map[string]any)
	if len(messages) < 2 {
		t.Fatalf("expected two messages, got %d", len(messages))
	}
	content, _ := messages[1]["content"].([]map[string]any)
	if len(content) == 0 || content[0]["type"] != "thinking" {
		t.Fatalf("expected a leading thinking block: %+v", content)
	}
	if content[0]["signature"] != "sig_abc" || content[0]["thinking"] != "weighing options" {
		t.Fatalf("thinking block lost its text or signature: %+v", content[0])
	}
}

// A redacted block is replayed encrypted, never as text.
func TestAnthropicReplaysRedactedThinking(t *testing.T) {
	body, err := anthropicBody(model.Request{
		Model: "claude",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "question"},
			{
				Role:  model.RoleAssistant,
				Parts: []model.MessagePart{{Type: "reasoning", Redacted: "encrypted_blob"}},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages, _ := body["messages"].([]map[string]any)
	content, _ := messages[1]["content"].([]map[string]any)
	if len(content) != 1 || content[0]["type"] != "redacted_thinking" {
		t.Fatalf("expected one redacted block: %+v", content)
	}
	if content[0]["data"] != "encrypted_blob" {
		t.Fatalf("redacted payload lost: %+v", content[0])
	}
}

// A reasoning part without proof cannot be replayed; sending it as plain text
// would be rejected by the API, so it is skipped.
func TestAnthropicSkipsUnsignedReasoning(t *testing.T) {
	body, err := anthropicBody(model.Request{
		Model: "claude",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "question"},
			{
				Role:    model.RoleAssistant,
				Content: "answer",
				Parts: []model.MessagePart{
					{Type: "reasoning", Text: "no proof here"},
					{Type: "text", Text: "answer"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages, _ := body["messages"].([]map[string]any)
	content, _ := messages[1]["content"].([]map[string]any)
	for _, block := range content {
		if block["type"] == "thinking" || block["type"] == "redacted_thinking" {
			t.Fatalf("unsigned reasoning must not be replayed: %+v", block)
		}
	}
	if len(content) != 1 || content[0]["type"] != "text" {
		t.Fatalf("expected only the text block: %+v", content)
	}
}

// A proof issued by another API is meaningless here, so it is skipped rather
// than sent to a server that did not sign it.
func TestAnthropicSkipsForeignReasoningProof(t *testing.T) {
	body, err := anthropicBody(model.Request{
		Model: "claude",
		Messages: []model.Message{
			{Role: model.RoleUser, Content: "question"},
			{
				Role:    model.RoleAssistant,
				Content: "answer",
				Parts: []model.MessagePart{
					{Type: "reasoning", Text: "openai chain", Signature: "sig_openai", Source: ReasoningSourceResponses},
					{Type: "reasoning", Text: "openai blob", Redacted: "enc_openai", Source: ReasoningSourceResponses},
					{Type: "text", Text: "answer"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	messages, _ := body["messages"].([]map[string]any)
	content, _ := messages[1]["content"].([]map[string]any)
	for _, block := range content {
		if block["type"] == "thinking" || block["type"] == "redacted_thinking" {
			t.Fatalf("foreign proof must not be replayed: %+v", block)
		}
	}
	if len(content) != 1 || content[0]["type"] != "text" {
		t.Fatalf("expected only the text block: %+v", content)
	}
}

// The stream must surface a signature that arrives on its own, because it is
// the only proof the thinking block came from this provider.
func TestAnthropicStreamSurfacesSignature(t *testing.T) {
	stream := `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig_xyz"}}

event: message_stop
data: {"type":"message_stop"}

`
	reader := &anthropicStream{body: nil, reader: newReader(stream)}
	chunk, err := reader.Recv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk.ReasoningSignature != "sig_xyz" {
		t.Fatalf("signature lost: %+v", chunk)
	}
	if chunk.ReasoningSource != ReasoningSourceAnthropic {
		t.Fatalf("signature must record its source: %+v", chunk)
	}
}

// A redacted block arrives whole in its start event.
func TestAnthropicStreamSurfacesRedacted(t *testing.T) {
	stream := `event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"redacted_thinking","data":"blob"}}

`
	reader := &anthropicStream{body: nil, reader: newReader(stream)}
	chunk, err := reader.Recv()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if chunk.ReasoningRedacted != "blob" {
		t.Fatalf("redacted payload lost: %+v", chunk)
	}
}

func newReader(text string) *bufio.Reader {
	return bufio.NewReaderSize(strings.NewReader(text), 64*1024)
}
