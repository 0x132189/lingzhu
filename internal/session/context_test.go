package session

import (
	"strings"
	"testing"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
)

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
