package session

import (
	"strings"
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
)

func TestEstimateCountsCJKPerCharacter(t *testing.T) {
	if got := Estimate("你好世界"); got < 4 {
		t.Fatalf("CJK should not be estimated at four characters per token: %d", got)
	}
	if got := Estimate("hello world"); got > 4 {
		t.Fatalf("ASCII estimate regressed: %d", got)
	}
}

func TestBuildContextUsesMeasuredUsageAsAnchor(t *testing.T) {
	events := []event.Event{
		event.User("hello"),
		event.UsageEvent(1000, 10, 200, 0),
		event.Assistant("hi"),
	}
	got := BuildContext(events)
	if len(got.Messages) != 2 {
		t.Fatalf("want 2 messages, got %d", len(got.Messages))
	}
	want := 1200 + Estimate("hi")
	if got.Tokens != want {
		t.Fatalf("want %d tokens, got %d", want, got.Tokens)
	}
}

func TestBuildMessagesResetsAtCompaction(t *testing.T) {
	checkpoint := Checkpoint("## Objective\n- resume", "[User]: latest")
	events := []event.Event{
		event.User("old"),
		event.Assistant("older"),
		event.CompactionEvent(checkpoint, 2),
		event.User("new"),
	}
	messages := BuildMessages(events)
	if len(messages) != 2 {
		t.Fatalf("want checkpoint plus new message, got %+v", messages)
	}
	if messages[0].Role != model.RoleUser || messages[0].Content != checkpoint {
		t.Fatalf("unexpected checkpoint message: %+v", messages[0])
	}
	if messages[1].Content != "new" {
		t.Fatalf("unexpected tail: %+v", messages[1])
	}
}

func TestBuildMessagesMarksJobNoticeSynthetic(t *testing.T) {
	events := []event.Event{
		event.User("hello"),
		event.JobCompletedEvent("job_1", "make", "completed", "out.log", "[Background shell completed]", 0),
		event.SubagentCompletedEvent("sub_1", "child task", "completed", "[Subagent completed]", ""),
	}
	messages := BuildMessages(events)
	if len(messages) != 3 {
		t.Fatalf("want 3 messages, got %+v", messages)
	}
	if messages[1].Role != model.RoleUser || !messages[1].Synthetic {
		t.Fatalf("job notice should be a synthetic user message: %+v", messages[1])
	}
	if messages[2].Role != model.RoleUser || !messages[2].Synthetic {
		t.Fatalf("subagent notice should be a synthetic user message: %+v", messages[2])
	}
	if messages[1].Content != "[Background shell completed]" || messages[2].Content != "[Subagent completed]" {
		t.Fatalf("unexpected notice text: %q / %q", messages[1].Content, messages[2].Content)
	}
	if messages[1].SyntheticSource != "shell" || messages[1].SyntheticDescription != "make" || messages[1].SyntheticState != "completed" {
		t.Fatalf("unexpected shell notice metadata: %+v", messages[1])
	}
	if messages[2].SyntheticSource != "subagent" || messages[2].SyntheticDescription != "child task" || messages[2].SyntheticState != "completed" {
		t.Fatalf("unexpected subagent notice metadata: %+v", messages[2])
	}
}

func TestCeilingKeepsReserve(t *testing.T) {
	cases := []struct{ window, want int }{
		{0, 0},
		{100_000, 84_000},
		{200_000, 170_000},
	}
	for _, c := range cases {
		if got := Ceiling(c.window); got != c.want {
			t.Fatalf("Ceiling(%d) = %d, want %d", c.window, got, c.want)
		}
	}
}

func TestCheckpointShape(t *testing.T) {
	text := Checkpoint("summary", "")
	if !strings.Contains(text, "<summary>\nsummary\n</summary>") {
		t.Fatalf("unexpected checkpoint: %q", text)
	}
	if strings.Contains(text, "<recent-context>") {
		t.Fatalf("empty recent context should be omitted: %q", text)
	}
}

func TestBuildMessagesKeepsToolMetadata(t *testing.T) {
	events := []event.Event{
		event.ToolCallEvent("call_1", "execute", `{"code":"await tool(\"read\", {path:\"a.ts\"})"}`),
		event.ToolResultWithAttachments("call_1", "execute", "hello", "", false, nil, map[string]any{
			"toolCalls": []any{map[string]any{"tool": "read", "status": "completed"}},
		}),
	}
	messages := BuildMessages(events)
	if len(messages) != 2 {
		t.Fatalf("want assistant plus tool message, got %+v", messages)
	}
	calls, ok := messages[1].Metadata["toolCalls"].([]any)
	if !ok || len(calls) != 1 {
		t.Fatalf("tool metadata missing: %+v", messages[1].Metadata)
	}
}

func TestBuildMessagesKeepsToolAttachments(t *testing.T) {
	events := []event.Event{
		event.User("look"),
		event.ToolCallEvent("call_1", "read", `{"path":"x.png"}`),
		event.ToolResultWithAttachments("call_1", "read", "attached", "", false, []model.Attachment{
			{Mime: "image/png", Name: "x.png", Data: "AAA"},
		}),
	}
	messages := BuildMessages(events)
	if len(messages) != 3 || len(messages[2].Attachments) != 1 {
		t.Fatalf("attachment missing from tool message: %+v", messages)
	}
	if messages[2].Attachments[0].Mime != "image/png" || messages[2].Attachments[0].Data != "AAA" {
		t.Fatalf("unexpected attachment: %+v", messages[2].Attachments[0])
	}
}
