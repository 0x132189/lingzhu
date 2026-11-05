package session

import "lingzhu/internal/event"

// str reads a string field from an event payload, returning empty when the
// field is missing or has another type.
func str(ev event.Event, key string) string {
	if v, ok := ev.Data[key].(string); ok {
		return v
	}
	return ""
}

// text reads the text field shared by user and assistant messages.
func text(ev event.Event) string { return str(ev, "text") }
