package session

import (
	"encoding/json"
	"strings"

	"lingzhu/internal/event"
)

// firstUserTitle reads only the first user message. It backs sessions migrated
// without a stored title; normal sessions get their title when the message is
// appended.
func (s *Store) firstUserTitle(id string) string {
	var data string
	err := s.db.QueryRow(
		`SELECT data FROM events WHERE session_id = ? AND type = ? ORDER BY seq LIMIT 1`,
		id, string(event.UserMessage),
	).Scan(&data)
	if err != nil {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal([]byte(data), &payload) != nil {
		return ""
	}
	value, _ := payload["text"].(string)
	return titleFromText(value)
}

func titleFromEvents(events []event.Event) string {
	for _, ev := range events {
		if ev.Type == event.UserMessage {
			if title := titleFromText(text(ev)); title != "" {
				return title
			}
		}
	}
	return ""
}

// titleFromText collapses whitespace and keeps titles short in the sidebar.
func titleFromText(value string) string {
	text := strings.Join(strings.Fields(value), " ")
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > 60 {
		return string(runes[:60]) + "…"
	}
	return text
}
