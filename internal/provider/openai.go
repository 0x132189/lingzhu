package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"lingzhu/internal/model"
)

// OpenAI adapts any OpenAI-compatible chat/completions endpoint.
type OpenAI struct {
	BaseURL string
	APIKey  string
	Client  *http.Client
}

func NewOpenAI(baseURL, apiKey string) *OpenAI {
	return &OpenAI{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Client:  &http.Client{Timeout: 0},
	}
}

func (o *OpenAI) Stream(ctx context.Context, req model.Request) (Stream, error) {
	payload, err := json.Marshal(toChatRequest(req))
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	if o.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)
	}

	resp, err := o.Client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("provider %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return &openAIStream{body: resp.Body, reader: bufio.NewReaderSize(resp.Body, 64*1024)}, nil
}

type openAIStream struct {
	body   io.ReadCloser
	reader *bufio.Reader
}

func (s *openAIStream) Recv() (Chunk, error) {
	for {
		line, err := s.reader.ReadString('\n')
		if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			if payload == "[DONE]" {
				return Chunk{}, io.EOF
			}
			chunk, parseErr := parseChunk(payload)
			if parseErr != nil {
				return Chunk{}, parseErr
			}
			if chunk.Text != "" || len(chunk.ToolCalls) > 0 {
				return chunk, nil
			}
		}
		if err != nil {
			return Chunk{}, err
		}
	}
}

func (s *openAIStream) Close() error { return s.body.Close() }

type chatRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools,omitempty"`
	Stream   bool          `json:"stream"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string           `json:"type"`
	Function chatToolFunction `json:"function"`
}

type chatToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

func toChatRequest(req model.Request) chatRequest {
	out := chatRequest{Model: req.Model, Stream: true, Messages: make([]chatMessage, 0, len(req.Messages))}
	for _, m := range req.Messages {
		msg := chatMessage{Role: string(m.Role), Content: m.Content, ToolCallID: m.ToolCallID}
		for _, call := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, chatToolCall{
				ID:       call.ID,
				Type:     "function",
				Function: chatFunction{Name: call.Name, Arguments: call.Arguments},
			})
		}
		out.Messages = append(out.Messages, msg)
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, chatTool{
			Type: "function",
			Function: chatToolFunction{
				Name:        t.Name,
				Description: t.Description,
				Parameters:  t.Schema,
			},
		})
	}
	return out
}

type streamResponse struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func parseChunk(payload string) (Chunk, error) {
	var resp streamResponse
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return Chunk{}, fmt.Errorf("decode stream chunk: %w", err)
	}
	if resp.Error != nil {
		return Chunk{}, fmt.Errorf("provider error: %s", resp.Error.Message)
	}
	var chunk Chunk
	for _, choice := range resp.Choices {
		chunk.Text += choice.Delta.Content
		for _, call := range choice.Delta.ToolCalls {
			chunk.ToolCalls = append(chunk.ToolCalls, ToolCallDelta{
				Index:     call.Index,
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
	}
	return chunk, nil
}
