package goal

import (
	"encoding/json"
	"strings"
)

// ParseVerdict decodes the reviewer's reply. Models sometimes wrap the JSON in a
// code fence or add a sentence around it, so the object is extracted first.
func ParseVerdict(text string) (Verdict, bool) {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return Verdict{}, false
	}
	var verdict Verdict
	if json.Unmarshal([]byte(text[start:end+1]), &verdict) != nil {
		return Verdict{}, false
	}
	switch verdict.Verdict {
	case "done", "continue", "blocked", "paused":
		return verdict, true
	}
	return Verdict{}, false
}

// SameReason reports whether two rounds stalled on the same objection.
func SameReason(previous, next string) bool {
	normalize := func(text string) string {
		return strings.ToLower(strings.Join(strings.Fields(text), " "))
	}
	return previous != "" && normalize(previous) == normalize(next)
}

// Describe renders a one-line status for a goal.
func Describe(state State) string {
	status := "Goal active"
	switch state.Status {
	case Paused:
		status = "Goal paused"
	case Complete:
		status = "Goal achieved"
	}
	parts := []string{status + ": " + state.Objective}
	if state.Reason != "" {
		parts = append(parts, state.Reason)
	}
	return strings.Join(parts, "\n")
}

// Truncate collapses whitespace and bounds a label.
func Truncate(text string, length int) string {
	line := strings.Join(strings.Fields(text), " ")
	runes := []rune(line)
	if len(runes) <= length {
		return line
	}
	return strings.TrimSpace(string(runes[:length-1])) + "…"
}
