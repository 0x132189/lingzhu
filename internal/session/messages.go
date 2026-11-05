package session

import (
	"lingzhu/internal/event"
	"lingzhu/internal/model"
)

// Messages rebuilds the conversation from the event log for the next request.
func (s *Store) Messages(id string) ([]model.Message, error) {
	events, err := s.Events(id)
	if err != nil {
		return nil, err
	}
	return BuildMessages(events), nil
}

func BuildMessages(events []event.Event) []model.Message {
	return build(events, nil)
}

// build reconstructs messages and reports, after each event, how many messages
// exist. Compaction events replace everything before them with one checkpoint.
func build(events []event.Event, observe func(messages int, ev event.Event)) []model.Message {
	messages := make([]model.Message, 0, len(events))
	for _, ev := range events {
		switch ev.Type {
		case event.Compaction:
			messages = []model.Message{{Role: model.RoleUser, Content: str(ev, "checkpoint")}}
		case event.UserMessage:
			messages = append(messages, model.Message{Role: model.RoleUser, Content: text(ev)})
		case event.AssistantMessage:
			messages = append(messages, model.Message{Role: model.RoleAssistant, Content: text(ev)})
		case event.ToolCall:
			if len(messages) == 0 || messages[len(messages)-1].Role != model.RoleAssistant {
				messages = append(messages, model.Message{Role: model.RoleAssistant})
			}
			last := &messages[len(messages)-1]
			last.ToolCalls = append(last.ToolCalls, model.ToolCall{
				ID:        str(ev, "id"),
				Name:      str(ev, "name"),
				Arguments: str(ev, "arguments"),
			})
		case event.ToolResult:
			messages = append(messages, model.Message{
				Role:       model.RoleTool,
				Content:    str(ev, "output"),
				ToolCallID: str(ev, "id"),
				Name:       str(ev, "name"),
				Diff:       str(ev, "diff"),
			})
		}
		if observe != nil {
			observe(len(messages), ev)
		}
	}
	return messages
}
