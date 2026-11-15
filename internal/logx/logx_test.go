package logx

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func capture(t *testing.T) *[]string {
	t.Helper()
	var lines []string
	SetSink(func(line string) { lines = append(lines, line) })
	t.Cleanup(func() { SetSink(nil) })
	return &lines
}

func TestRecordCarriesSortedFields(t *testing.T) {
	lines := capture(t)
	SetLevel(LevelInfo)
	Info("turn failed", Session("s1"), Component("agent"), Err(errors.New("boom")))

	if len(*lines) != 1 {
		t.Fatalf("expected one line, got %d", len(*lines))
	}
	line := (*lines)[0]
	if !strings.HasPrefix(line, "INFO turn failed ") {
		t.Fatalf("unexpected header: %q", line)
	}
	// Fields are sorted, so a reader can rely on the order.
	component := strings.Index(line, "component=agent")
	errorAt := strings.Index(line, "error=boom")
	session := strings.Index(line, "session=s1")
	if component < 0 || errorAt < 0 || session < 0 {
		t.Fatalf("missing fields: %q", line)
	}
	if !(component < errorAt && errorAt < session) {
		t.Fatalf("fields are not sorted: %q", line)
	}
}

func TestLevelFiltersRecords(t *testing.T) {
	lines := capture(t)
	SetLevel(LevelWarn)
	t.Cleanup(func() { SetLevel(LevelInfo) })

	Debug("hidden")
	Info("hidden")
	Warn("shown")
	Error("shown too")
	if len(*lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %v", len(*lines), *lines)
	}
	if !strings.HasPrefix((*lines)[0], "WARN ") || !strings.HasPrefix((*lines)[1], "ERROR ") {
		t.Fatalf("unexpected levels: %v", *lines)
	}
}

func TestValuesAreQuotedWhenAmbiguous(t *testing.T) {
	lines := capture(t)
	SetLevel(LevelInfo)
	Info("values",
		F("plain", "simple"),
		F("spaced", "two words"),
		F("empty", ""),
		F("count", 42),
		F("ok", true),
	)
	line := (*lines)[0]
	for _, want := range []string{
		"plain=simple",
		`spaced="two words"`,
		`empty=""`,
		"count=42",
		"ok=true",
	} {
		if !strings.Contains(line, want) {
			t.Fatalf("missing %q in %q", want, line)
		}
	}
}

func TestNilErrorFieldIsDropped(t *testing.T) {
	lines := capture(t)
	SetLevel(LevelInfo)
	Info("clean", Err(nil), Session("s1"))
	if strings.Contains((*lines)[0], "error=") {
		t.Fatalf("a nil error must not add a field: %q", (*lines)[0])
	}
}

func TestParseLevel(t *testing.T) {
	for input, want := range map[string]Level{
		"debug":    LevelDebug,
		"":         LevelInfo,
		"info":     LevelInfo,
		"warn":     LevelWarn,
		"warning":  LevelWarn,
		"error":    LevelError,
		"nonsense": LevelInfo,
	} {
		if got := ParseLevel(input); got != want {
			t.Fatalf("ParseLevel(%q) = %v, want %v", input, got, want)
		}
	}
}

func TestContextCarriesSession(t *testing.T) {
	ctx := NewContext(t.Context(), "s1")
	if got := FromContext(ctx); got != "s1" {
		t.Fatalf("FromContext = %q, want s1", got)
	}
	if got := FromContext(NewContext(t.Context(), "")); got != "" {
		t.Fatalf("an empty session must not be attached, got %q", got)
	}
}

func TestTimingRendersADuration(t *testing.T) {
	lines := capture(t)
	SetLevel(LevelInfo)
	Info("done", Timing(time.Now().Add(-1500*time.Millisecond)))
	if !strings.Contains((*lines)[0], "elapsed=") {
		t.Fatalf("missing elapsed: %q", (*lines)[0])
	}
	if strings.Contains((*lines)[0], "elapsed=\"") {
		t.Fatalf("a duration should not be quoted: %q", (*lines)[0])
	}
}
