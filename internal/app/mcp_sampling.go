package app

import (
	"context"
	"encoding/json"
	"strings"

	"lingzhu/internal/agent"
	"lingzhu/internal/mcp"
	"lingzhu/internal/model"
)

// samplingHandler lets an MCP server use the default model for a transient
// generation request. The server's configuration must opt in with sampling.
func samplingHandler(runner *agent.Agent) mcp.SamplingHandler {
	return func(ctx context.Context, _ string, request mcp.SamplingRequest) (mcp.SamplingResult, error) {
		messages := make([]model.Message, 0, len(request.Messages))
		for _, item := range request.Messages {
			text := samplingText(item.Content)
			if text == "" {
				continue
			}
			role := model.RoleUser
			if item.Role == "assistant" {
				role = model.RoleAssistant
			}
			messages = append(messages, model.Message{Role: role, Content: text})
		}
		text, modelID, err := runner.Sample(ctx, request.SystemPrompt, messages)
		if err != nil {
			return mcp.SamplingResult{}, err
		}
		return mcp.SamplingResult{
			Role:    "assistant",
			Content: map[string]any{"type": "text", "text": text},
			Model:   modelID,
		}, nil
	}
}

func samplingText(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var part struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &part); err == nil && part.Type == "text" {
		return part.Text
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		texts := make([]string, 0, len(parts))
		for _, item := range parts {
			if item.Type == "text" && item.Text != "" {
				texts = append(texts, item.Text)
			}
		}
		return strings.Join(texts, "\n")
	}
	return ""
}
