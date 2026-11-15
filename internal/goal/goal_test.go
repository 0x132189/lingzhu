package goal

import (
	"strings"
	"testing"
)

func TestNewStartsActive(t *testing.T) {
	state := New("  ship the release  ", "boot-1")
	if state.Status != Active {
		t.Fatalf("want active, got %q", state.Status)
	}
	if state.Objective != "ship the release" {
		t.Fatalf("objective not trimmed: %q", state.Objective)
	}
	if state.MaxRounds != MaxRounds || state.Boot != "boot-1" || state.ID == "" {
		t.Fatalf("unexpected state: %+v", state)
	}
}

func TestParseVerdict(t *testing.T) {
	cases := []struct {
		text string
		want string
		ok   bool
	}{
		{`{"verdict":"done","reason":"tests pass"}`, "done", true},
		{"Here you go:\n```json\n{\"verdict\":\"continue\",\"reason\":\"missing tests\",\"next_step\":\"add them\"}\n```", "continue", true},
		{`{"verdict":"blocked","reason":"needs credentials"}`, "blocked", true},
		{`{"verdict":"paused","reason":"user stopped"}`, "paused", true},
		{`{"verdict":"maybe","reason":"unsure"}`, "", false},
		{"no json here", "", false},
	}
	for _, test := range cases {
		verdict, ok := ParseVerdict(test.text)
		if ok != test.ok || verdict.Verdict != test.want {
			t.Fatalf("ParseVerdict(%q) = %q,%v want %q,%v", test.text, verdict.Verdict, ok, test.want, test.ok)
		}
	}
}

func TestSameReason(t *testing.T) {
	if !SameReason("Missing  tests", "missing tests") {
		t.Fatal("normalized reasons should match")
	}
	if SameReason("", "") {
		t.Fatal("empty reasons never stall")
	}
	if SameReason("one", "two") {
		t.Fatal("different reasons must not match")
	}
}

func TestRenderEscapesObjective(t *testing.T) {
	text := Render(Kickoff, map[string]string{"objective": Escape("a <b> & c")})
	if !strings.Contains(text, "a &lt;b&gt; &amp; c") {
		t.Fatalf("objective not escaped:\n%s", text)
	}
	if strings.Contains(text, "{{objective}}") {
		t.Fatal("placeholder not replaced")
	}
}

func TestObjectiveBlock(t *testing.T) {
	block := ObjectiveBlock("do <it>")
	if !strings.HasPrefix(block, "<untrusted_objective>") || !strings.Contains(block, "do &lt;it&gt;") {
		t.Fatalf("unexpected block: %q", block)
	}
}

func TestTruncate(t *testing.T) {
	if got := Truncate("a\n b   c", 10); got != "a b c" {
		t.Fatalf("want collapsed text, got %q", got)
	}
	long := strings.Repeat("x", 100)
	got := Truncate(long, 10)
	if len([]rune(got)) != 10 || !strings.HasSuffix(got, "…") {
		t.Fatalf("want truncated label, got %q", got)
	}
}

func TestPromptsCarryRequiredRules(t *testing.T) {
	for _, want := range []string{"untrusted_objective", "reviewer", "evidence"} {
		if !strings.Contains(Kickoff, want) {
			t.Fatalf("kickoff missing %q", want)
		}
	}
	for _, want := range []string{"untrusted_objective", "Do not repeat work"} {
		if !strings.Contains(Continuation, want) {
			t.Fatalf("continuation missing %q", want)
		}
	}
	for _, want := range []string{`"verdict":"done"`, `"verdict":"continue"`, `"verdict":"blocked"`, `"verdict":"paused"`, "Do not continue the work"} {
		if !strings.Contains(Judge, want) {
			t.Fatalf("judge missing %q", want)
		}
	}
	if !strings.Contains(Reminder, "<system-reminder>") {
		t.Fatal("reminder must be a system reminder")
	}
}
