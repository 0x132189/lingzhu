package session

import (
	"strings"
	"testing"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
)

func TestBuildMessageRecordsKeepsCreatedTimes(t *testing.T) {
	first := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	answer := first.Add(2 * time.Second)
	toolCall := answer.Add(time.Second)
	result := toolCall.Add(time.Second)
	events := []event.Event{
		{Type: event.UserMessage, Seq: 1, At: first, Data: map[string]any{
			"text": "look", "agent": "build", "provider": "openai", "model": "gpt-5",
		}},
		{Type: event.AssistantMessage, Seq: 2, At: answer, Data: map[string]any{"text": "reading"}},
		{Type: event.ToolCall, Seq: 3, At: toolCall, Data: map[string]any{"id": "call_1", "name": "read", "arguments": "{}"}},
		{Type: event.ToolResult, Seq: 4, At: result, Data: map[string]any{"id": "call_1", "output": "ok"}},
	}

	records := BuildMessageRecords(events)
	if len(records) != 3 {
		t.Fatalf("unexpected records: %+v", records)
	}
	if !records[0].Created.Equal(first) || !records[1].Created.Equal(answer) || !records[2].Created.Equal(result) {
		t.Fatalf("unexpected created times: %+v", records)
	}
	if records[0].Agent != "build" || records[0].Provider != "openai" || records[0].Model != "gpt-5" {
		t.Fatalf("user turn meta missing: %+v", records[0])
	}
	if len(records[1].ToolCalls) != 1 || records[1].ToolCalls[0].ID != "call_1" {
		t.Fatalf("tool call should extend the assistant record: %+v", records[1])
	}
}

func TestBuildMessageRecordsKeepsReferences(t *testing.T) {
	ev := event.New(event.UserMessage, map[string]any{
		"text": "look",
		"references": []model.Reference{
			{Name: "a.go", Mime: "text/plain", Path: "/ws/a.go"},
		},
	})
	records := BuildMessageRecords([]event.Event{ev})
	if len(records) != 1 || len(records[0].References) != 1 {
		t.Fatalf("references missing: %+v", records)
	}
	if records[0].References[0].Path != "/ws/a.go" || records[0].References[0].Name != "a.go" {
		t.Fatalf("unexpected reference: %+v", records[0].References[0])
	}
}

func TestBuildMessageRecordsKeepsToolOutcomeAndInstructionPaths(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	events := []event.Event{
		{Type: event.ToolResult, Seq: 1, At: at, Data: map[string]any{
			"id": "call_1", "name": "grep", "output": "boom", "failed": true,
		}},
		{Type: event.Synthetic, Seq: 2, At: at.Add(time.Second), Data: map[string]any{
			"text": "Instructions from: AGENTS.md", "description": "Loaded AGENTS.md",
			"paths": []any{"AGENTS.md", "sub/AGENTS.md"},
		}},
	}

	records := BuildMessageRecords(events)
	if len(records) != 2 {
		t.Fatalf("unexpected records: %+v", records)
	}
	if !records[0].Failed {
		t.Fatalf("failed tool result should stay failed in history: %+v", records[0])
	}
	if len(records[1].SyntheticPaths) != 2 || records[1].SyntheticPaths[0] != "AGENTS.md" {
		t.Fatalf("instruction paths missing: %+v", records[1])
	}
}

func TestBuildMessageRecordsKeepsSwitchRowsOutOfTheModelContext(t *testing.T) {
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	events := []event.Event{
		{Type: event.UserMessage, Seq: 1, At: at, Data: map[string]any{"text": "go"}},
		{Type: event.AgentSelected, Seq: 2, At: at.Add(time.Second), Data: map[string]any{
			"agent": "plan", "previous": "build",
		}},
		{Type: event.ModelSelected, Seq: 3, At: at.Add(2 * time.Second), Data: map[string]any{
			"provider": "anthropic", "model": "claude",
			"previous": map[string]any{"provider": "openai", "model": "gpt"},
		}},
		{Type: event.UserMessage, Seq: 4, At: at.Add(3 * time.Second), Data: map[string]any{"text": "again"}},
	}

	records := BuildMessageRecords(events)
	if len(records) != 4 {
		t.Fatalf("transcript should keep both switch rows: %+v", records)
	}
	if records[1].Switch == nil || records[1].Switch.Kind != "agent" || records[1].Switch.Agent != "plan" {
		t.Fatalf("agent switch row missing: %+v", records[1])
	}
	if records[2].Switch == nil || records[2].Switch.Kind != "model" ||
		records[2].Switch.PreviousProvider != "openai" || records[2].Switch.PreviousModel != "gpt" {
		t.Fatalf("model switch row missing: %+v", records[2])
	}

	// The model never receives a switch row: it carries no prompt text.
	messages := BuildMessages(events)
	if len(messages) != 2 {
		t.Fatalf("switch rows must not reach the provider: %+v", messages)
	}
	for _, message := range messages {
		if message.Switch != nil {
			t.Fatalf("switch row leaked into the model context: %+v", message)
		}
	}
}

func TestBuildMessagesMergesStreamedAssistantAfterEarlyToolCall(t *testing.T) {
	events := []event.Event{
		{Type: event.ToolCall, Seq: 1, Data: map[string]any{"id": "call_1", "name": "read", "arguments": `{}`}},
		{Type: event.AssistantMessage, Seq: 2, Data: map[string]any{"text": "reading"}},
		{Type: event.ToolResult, Seq: 3, Data: map[string]any{"id": "call_1", "name": "read", "output": "ok"}},
	}
	messages := BuildMessages(events)
	if len(messages) != 2 {
		t.Fatalf("unexpected messages: %+v", messages)
	}
	if messages[0].Role != model.RoleAssistant || messages[0].Content != "reading" || len(messages[0].ToolCalls) != 1 {
		t.Fatalf("assistant text should merge into the early tool-call message: %+v", messages[0])
	}
	if messages[1].Role != model.RoleTool || messages[1].Content != "ok" {
		t.Fatalf("tool result should follow the assistant message: %+v", messages[1])
	}
}

func TestInstructionUpdatePersistsAsSyntheticNotice(t *testing.T) {
	events := []event.Event{
		event.InstructionUpdatedEvent("The harness instructions changed.", []string{"agents", "environment"}),
	}
	messages := BuildMessages(events)
	if len(messages) != 1 {
		t.Fatalf("unexpected messages: %+v", messages)
	}
	if messages[0].SyntheticSource != "instructions" || messages[0].SyntheticDescription != "Instructions updated" {
		t.Fatalf("instruction notice metadata missing: %+v", messages[0])
	}
	if messages[0].SyntheticState != "" {
		t.Fatalf("an instruction update must not carry a completion state, got %q", messages[0].SyntheticState)
	}
	if len(messages[0].SyntheticSources) != 2 || messages[0].SyntheticSources[0] != "agents" {
		t.Fatalf("instruction sources missing: %+v", messages[0].SyntheticSources)
	}
	if !strings.Contains(messages[0].Content, "harness instructions changed") {
		t.Fatalf("model-facing text missing: %+v", messages[0])
	}
}

func TestJobNoticeKeepsTheJobID(t *testing.T) {
	// The transcript keys a background report by this id. Without it every
	// report collapses onto one row key and the rows stack on each other.
	events := []event.Event{
		event.JobCompletedEvent("job_1_1", "npm run dev", "completed", "/tmp/out.log", "[Background shell completed]", 0),
		event.JobCompletedEvent("job_1_2", "npm test", "failed", "/tmp/out2.log", "[Background shell failed]", -1),
	}
	messages := BuildMessages(events)
	if len(messages) != 2 {
		t.Fatalf("unexpected messages: %+v", messages)
	}
	if messages[0].SyntheticID != "job_1_1" {
		t.Fatalf("a job notice must carry its job id, got %q", messages[0].SyntheticID)
	}
	if messages[1].SyntheticID != "job_1_2" {
		t.Fatalf("each report keeps its own id, got %q", messages[1].SyntheticID)
	}
	if messages[1].SyntheticState != "error" {
		t.Fatalf("a failed job must read as an error, got %q", messages[1].SyntheticState)
	}
}

func TestSubagentNoticeKeepsTheChildID(t *testing.T) {
	events := []event.Event{
		event.SubagentCompletedEvent("sub_1", "explore the repo", "completed", "[Subagent finished]", ""),
	}
	messages := BuildMessages(events)
	if len(messages) != 1 {
		t.Fatalf("unexpected messages: %+v", messages)
	}
	if messages[0].SyntheticID != "sub_1" {
		t.Fatalf("a subagent notice must carry its child id, got %q", messages[0].SyntheticID)
	}
}

func TestBuildMessagesKeepsAssistantParts(t *testing.T) {
	events := []event.Event{
		{Type: event.AssistantMessage, Seq: 1, Data: map[string]any{
			"text": "answer",
			"parts": []model.MessagePart{
				{Type: "reasoning", Text: "think", Created: "2026-01-01T00:00:00Z", Completed: "2026-01-01T00:00:02Z"},
				{Type: "text", Text: "answer", Created: "2026-01-01T00:00:02Z", Completed: "2026-01-01T00:00:03Z"},
			},
		}},
	}

	messages := BuildMessages(events)
	if len(messages) != 1 {
		t.Fatalf("unexpected messages: %+v", messages)
	}
	parts := messages[0].Parts
	if len(parts) != 2 || parts[0].Type != "reasoning" || parts[0].Text != "think" || parts[1].Text != "answer" {
		t.Fatalf("assistant parts missing: %+v", parts)
	}
	if parts[0].Created != "2026-01-01T00:00:00Z" || parts[0].Completed != "2026-01-01T00:00:02Z" {
		t.Fatalf("part timestamps missing: %+v", parts[0])
	}
}

func TestBuildMessagesHidesStagedRevert(t *testing.T) {
	events := []event.Event{
		{Type: event.UserMessage, Seq: 1, Data: map[string]any{"text": "first"}},
		{Type: event.AssistantMessage, Seq: 2, Data: map[string]any{"text": "first answer"}},
		{Type: event.UserMessage, Seq: 3, Data: map[string]any{"text": "second"}},
		{Type: event.AssistantMessage, Seq: 4, Data: map[string]any{"text": "second answer"}},
		{Type: event.SessionReverted, Seq: 5, Data: map[string]any{
			"turnSeq":  float64(3),
			"snapshot": "aaa",
			"current":  "bbb",
		}},
		{Type: event.UserMessage, Seq: 6, Data: map[string]any{"text": "new branch"}},
	}
	messages := BuildMessages(events)
	if len(messages) != 3 {
		t.Fatalf("unexpected messages: %+v", messages)
	}
	if messages[0].Content != "first" || messages[1].Content != "first answer" || messages[2].Content != "new branch" {
		t.Fatalf("unexpected hidden boundary: %+v", messages)
	}

	events = append(events, event.Event{Type: event.SessionReapplied, Seq: 7, Data: map[string]any{"turnSeq": float64(3)}})
	messages = BuildMessages(events)
	if len(messages) != 5 {
		t.Fatalf("reapply should restore the hidden turn: %+v", messages)
	}
	if messages[3].Content != "second answer" || messages[4].Content != "new branch" {
		t.Fatalf("unexpected reapplied order: %+v", messages)
	}
}

func TestBuildMessageRecordsDropsInstructionUpdatesBeforeCheckpoint(t *testing.T) {
	checkpoint := event.CompactionEvent(Checkpoint("## Objective\n- resume", "[User]: latest"), 1)
	checkpoint.Seq = 5
	before := event.InstructionUpdatedEvent("The harness instructions changed.", []string{"agents"})
	before.Seq = 1
	after := event.InstructionUpdatedEvent("The harness instructions changed.", []string{"environment"})
	after.Seq = 6
	events := []event.Event{
		before,
		event.User("old"),
		checkpoint,
		after,
	}

	records := BuildMessageRecords(events)
	if len(records) != 3 {
		t.Fatalf("only stale instruction updates should disappear: %+v", records)
	}
	if records[0].Content != "old" || !records[1].Compaction {
		t.Fatalf("history around the checkpoint was changed: %+v", records)
	}
	if records[2].SyntheticSource != "instructions" || len(records[2].SyntheticSources) != 1 {
		t.Fatalf("the update after the checkpoint must stay: %+v", records[2])
	}
}

func TestBuildMessagesRevertOnlyHidesUpToItself(t *testing.T) {
	events := []event.Event{
		{Type: event.UserMessage, Seq: 1, Data: map[string]any{"text": "keep"}},
		{Type: event.UserMessage, Seq: 2, Data: map[string]any{"text": "undo"}},
		{Type: event.SessionReverted, Seq: 3, Data: map[string]any{"turnSeq": float64(2)}},
		{Type: event.UserMessage, Seq: 4, Data: map[string]any{"text": "new"}},
		{Type: event.SessionReverted, Seq: 5, Data: map[string]any{"turnSeq": float64(4)}},
	}
	messages := BuildMessages(events)
	if len(messages) != 1 || messages[0].Content != "keep" {
		t.Fatalf("latest revert should win: %+v", messages)
	}
}

func TestBuildMessageRecordsKeepsHistoryBeforeCompaction(t *testing.T) {
	checkpoint := Checkpoint("## Objective\n- resume", "[User]: latest")
	events := []event.Event{
		event.User("old"),
		event.Assistant("older"),
		event.CompactionEvent(checkpoint, 2),
		event.User("new"),
	}

	records := BuildMessageRecords(events)
	if len(records) != 4 {
		t.Fatalf("transcript should keep every turn plus the checkpoint: %+v", records)
	}
	if records[0].Content != "old" || records[1].Content != "older" {
		t.Fatalf("history before the checkpoint was dropped: %+v", records)
	}
	if !records[2].Compaction || records[2].Content != checkpoint || records[2].CompactionCovered != 2 {
		t.Fatalf("checkpoint divider missing: %+v", records[2])
	}
	if records[3].Content != "new" {
		t.Fatalf("turn after the checkpoint missing: %+v", records[3])
	}
	if messages := BuildMessages(events); len(messages) != 2 || messages[0].Content != checkpoint {
		t.Fatalf("model context must restart at the checkpoint: %+v", messages)
	}
}

func TestBuildMessagesKeepsUserAttachments(t *testing.T) {
	events := []event.Event{
		event.UserWithAttachments("look", []model.Attachment{
			{Mime: "image/png", Name: "pixel.png", Data: "AAA"},
		}),
		{Type: event.AssistantMessage, Seq: 2, Data: map[string]any{"text": "seen"}},
	}
	messages := BuildMessages(events)
	if len(messages) != 2 {
		t.Fatalf("unexpected messages: %+v", messages)
	}
	if len(messages[0].Attachments) != 1 {
		t.Fatalf("user attachments missing: %+v", messages[0])
	}
	attachment := messages[0].Attachments[0]
	if attachment.Mime != "image/png" || attachment.Name != "pixel.png" || attachment.Data != "AAA" {
		t.Fatalf("unexpected attachment: %+v", attachment)
	}
}
