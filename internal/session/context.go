package session

import (
	"strings"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
)

// charsPerToken is the rough ratio used when no provider measurement exists.
const charsPerToken = 4

// reserveMin and reserveShare keep room free for the reply and the next steps.
const (
	reserveMin   = 16_000
	reserveShare = 0.15
)

// Estimate approximates the token count of text without a tokenizer. ASCII
// stays on the four-characters-per-token rule; CJK and other wide scripts are
// counted closer to one token per character so Chinese sessions compact at the
// same point as the reference instead of much later.
func Estimate(text string) int {
	if text == "" {
		return 0
	}
	ascii, wide := 0, 0
	for _, r := range text {
		if r >= 0x2E80 {
			wide++
		} else {
			ascii++
		}
	}
	return (ascii+charsPerToken-1)/charsPerToken + wide
}

// Context is the conversation rebuilt from the log plus its estimated size.
type Context struct {
	Messages []model.Message
	Tokens   int
}

// Context rebuilds the conversation and sizes it: the last measured prompt plus
// an estimate of every message added since. Without a measurement it estimates
// the whole conversation.
func (s *Store) Context(id string) (Context, error) {
	events, err := s.Events(id)
	if err != nil {
		return Context{}, err
	}
	return BuildContext(events), nil
}

func BuildContext(events []event.Event) Context {
	anchor, measured := 0, 0
	messages := build(events, func(count int, ev event.Event) {
		if ev.Type == event.Usage {
			anchor = count
			measured = num(ev, "input") + num(ev, "cacheRead") + num(ev, "cacheWrite")
		}
	})
	tokens := measured
	for _, message := range messages[min(anchor, len(messages)):] {
		tokens += messageTokens(message)
	}
	return Context{Messages: messages, Tokens: tokens}
}

func messageTokens(m model.Message) int {
	total := Estimate(m.Content) + Estimate(m.Name)
	for _, call := range m.ToolCalls {
		total += Estimate(call.Name) + Estimate(call.Arguments)
	}
	for _, attachment := range m.Attachments {
		mime := strings.ToLower(attachment.Mime)
		switch {
		case strings.HasPrefix(mime, "image/"):
			total += 1500
		case mime == "application/pdf":
			total += 2000
		}
	}
	return total
}

// Ceiling is the token count at which compaction starts: the window minus a
// reserve of 15%, or 16k for windows large enough to afford it. Zero or less
// disables compaction.
func Ceiling(window int) int {
	if window <= 0 {
		return 0
	}
	reserve := int(float64(window) * reserveShare)
	if window >= 2*reserveMin {
		reserve = max(reserve, reserveMin)
	}
	return window - reserve
}

// Checkpoint renders a compaction result: the summary plus the tail kept
// verbatim beside it. Later requests open with this as a single message.
func Checkpoint(summary, recent string) string {
	var b strings.Builder
	b.WriteString("<conversation-checkpoint>\n<summary>\n")
	b.WriteString(strings.TrimSpace(summary))
	b.WriteString("\n</summary>")
	if recent = strings.TrimSpace(recent); recent != "" {
		b.WriteString("\n<recent-context>\n")
		b.WriteString(recent)
		b.WriteString("\n</recent-context>")
	}
	b.WriteString("\n</conversation-checkpoint>")
	return b.String()
}

func num(ev event.Event, key string) int {
	switch v := ev.Data[key].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}
