package mcp

import (
	"context"
	"encoding/json"
)

// SamplingMessage is one message supplied by a server for sampling.
type SamplingMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// SamplingRequest is a server's sampling/createMessage request.
type SamplingRequest struct {
	Messages       []SamplingMessage `json:"messages"`
	SystemPrompt   string            `json:"systemPrompt,omitempty"`
	MaxTokens      int               `json:"maxTokens,omitempty"`
	IncludeContext string            `json:"includeContext,omitempty"`
}

// SamplingResult is the model answer returned to the server.
type SamplingResult struct {
	Role    string         `json:"role"`
	Content map[string]any `json:"content"`
	Model   string         `json:"model,omitempty"`
}

// SamplingHandler runs one sampling request for a server and returns the
// assistant message. A nil handler disables sampling for the manager.
type SamplingHandler func(ctx context.Context, server string, request SamplingRequest) (SamplingResult, error)
