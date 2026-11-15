package plugin

import (
	"context"
	"strings"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/model"
	"lingzhu/internal/rpc"
)

const contextTimeout = 5 * time.Second

type contextEvent struct {
	SessionID string           `json:"sessionId"`
	Agent     string           `json:"agent"`
	Model     string           `json:"model"`
	Messages  []contextMessage `json:"messages"`
	Options   map[string]any   `json:"options,omitempty"`
}

type contextMessage struct {
	Role       string            `json:"role"`
	Content    string            `json:"content"`
	Name       string            `json:"name,omitempty"`
	ToolCallID string            `json:"toolCallId,omitempty"`
	ToolCalls  []contextToolCall `json:"toolCalls,omitempty"`
}

type contextToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type contextResult struct {
	Messages []contextMessage `json:"messages"`
	Options  map[string]any   `json:"options"`
}

// ApplyContext runs session.context hooks. Plugins may replace messages or
// options; hooks run in name order and each sees the previous result.
func (m *Manager) ApplyContext(ctx context.Context, event agent.ContextEvent) (agent.ContextEvent, error) {
	if m == nil {
		return event, nil
	}
	m.mu.RLock()
	clients := append([]*Client(nil), m.hooks...)
	m.mu.RUnlock()
	if len(clients) == 0 {
		return event, nil
	}

	current := contextEvent{
		SessionID: event.SessionID,
		Agent:     event.Agent,
		Model:     event.Model,
		Messages:  toContextMessages(event.Messages),
		Options:   cloneOptions(event.Options),
	}
	for _, client := range clients {
		callCtx, cancel := context.WithTimeout(ctx, contextTimeout)
		var result contextResult
		err := client.request(callCtx, "hooks/session.context", current, &result)
		cancel()
		if err != nil {
			if rpc.IsMethodNotFound(err) {
				continue
			}
			m.logf("plugin: session.context %s: %v", client.name, err)
			continue
		}
		if len(result.Messages) > 0 {
			current.Messages = result.Messages
		}
		if result.Options != nil {
			current.Options = result.Options
		}
	}

	updated := event
	updated.Messages = fromContextMessages(current.Messages)
	if current.Options != nil {
		updated.Options = current.Options
	}
	return updated, nil
}

func toContextMessages(messages []model.Message) []contextMessage {
	out := make([]contextMessage, 0, len(messages))
	for _, message := range messages {
		item := contextMessage{
			Role:       string(message.Role),
			Content:    message.Content,
			Name:       message.Name,
			ToolCallID: message.ToolCallID,
		}
		for _, call := range message.ToolCalls {
			item.ToolCalls = append(item.ToolCalls, contextToolCall{
				ID:        call.ID,
				Name:      call.Name,
				Arguments: call.Arguments,
			})
		}
		out = append(out, item)
	}
	return out
}

func fromContextMessages(messages []contextMessage) []model.Message {
	out := make([]model.Message, 0, len(messages))
	for _, item := range messages {
		role := model.Role(strings.TrimSpace(item.Role))
		switch role {
		case model.RoleSystem, model.RoleUser, model.RoleAssistant, model.RoleTool:
		default:
			continue
		}
		message := model.Message{
			Role:       role,
			Content:    item.Content,
			Name:       item.Name,
			ToolCallID: item.ToolCallID,
		}
		for _, call := range item.ToolCalls {
			message.ToolCalls = append(message.ToolCalls, model.ToolCall{
				ID:        call.ID,
				Name:      call.Name,
				Arguments: call.Arguments,
			})
		}
		out = append(out, message)
	}
	return out
}

func cloneOptions(options map[string]any) map[string]any {
	if options == nil {
		return nil
	}
	out := make(map[string]any, len(options))
	for key, value := range options {
		out[key] = value
	}
	return out
}
