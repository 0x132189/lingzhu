package provider

import (
	"bufio"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/model"
)

func TestAnthropicBodyMarksCacheBreakpoints(t *testing.T) {
	body, err := anthropicBody(model.Request{
		Model:    "claude-test",
		CacheTTL: time.Hour,
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "base"},
			{Role: model.RoleUser, Content: "hello"},
		},
		Tools: []model.ToolDef{{Name: "read", Description: "read", Schema: []byte(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	system, ok := body["system"].([]map[string]any)
	if !ok || len(system) != 1 {
		t.Fatalf("system should be a cacheable block: %#v", body["system"])
	}
	control, _ := system[0]["cache_control"].(map[string]any)
	if control["type"] != "ephemeral" || control["ttl"] != "1h" {
		t.Fatalf("unexpected system cache control: %#v", control)
	}
	tools, _ := body["tools"].([]map[string]any)
	if len(tools) != 1 || tools[0]["cache_control"] == nil {
		t.Fatalf("last tool should carry a cache breakpoint: %#v", body["tools"])
	}
	messages, _ := body["messages"].([]map[string]any)
	last := messages[len(messages)-1]
	blocks, _ := last["content"].([]map[string]any)
	if blocks[len(blocks)-1]["cache_control"] == nil {
		t.Fatalf("tail message should carry a cache breakpoint: %#v", last)
	}
}

func TestAnthropicStreamReadsCompactionDelta(t *testing.T) {
	stream := &anthropicStream{reader: bufio.NewReader(strings.NewReader(
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"compaction_delta\",\"content\":\"summary window\"}}\n",
	))}
	chunk, err := stream.Recv()
	if err != nil {
		t.Fatal(err)
	}
	if chunk.Compaction != "summary window" {
		t.Fatalf("compaction delta was not parsed: %+v", chunk)
	}
}

func TestAnthropicBodyLeavesCacheOffByDefault(t *testing.T) {
	body, err := anthropicBody(model.Request{
		Model:    "claude-test",
		Messages: []model.Message{{Role: model.RoleSystem, Content: "base"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := body["system"].(string); !ok {
		t.Fatalf("system should stay a string without a cache TTL: %#v", body["system"])
	}
}
