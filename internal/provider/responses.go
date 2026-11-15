package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"lingzhu/internal/model"
)

// OpenAIResponses adapts the Responses API, which uses item-based input and a
// different streaming event vocabulary than chat completions.
type OpenAIResponses struct {
	BaseURL string
	APIKey  string
	// TokenSource, when set, supplies a fresh bearer token per request.
	TokenSource func(ctx context.Context) (string, error)
	Client      *http.Client
	// Idle bounds the wait for the next stream chunk. Zero uses the default.
	Idle time.Duration
}

func NewOpenAIResponses(baseURL, apiKey string) *OpenAIResponses {
	return &OpenAIResponses{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Client:  NewClient(DefaultTimeouts()),
	}
}

func (o *OpenAIResponses) Stream(ctx context.Context, req model.Request) (Stream, error) {
	body, err := responsesBody(req)
	if err != nil {
		return nil, err
	}
	for key, value := range normalizeOptions(req.Options) {
		switch key {
		case "max_tokens":
			body["max_output_tokens"] = value
		case "reasoning_effort", "reasoning_summary":
			reasoning, _ := body["reasoning"].(map[string]any)
			if reasoning == nil {
				reasoning = map[string]any{}
			}
			reasoning[strings.TrimPrefix(key, "reasoning_")] = value
			body["reasoning"] = reasoning
		default:
			body[key] = value
		}
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}
	if o.TokenSource != nil {
		token, err := o.TokenSource(ctx)
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+token)
	} else if o.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+o.APIKey)
	}

	resp, err := o.Client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, classifyErrorAfter(resp.StatusCode, resp.Status, string(data), ParseRetryAfter(resp.Header.Get("Retry-After")))
	}
	watched := WatchIdle(resp.Body, o.idleTimeout())
	return &responsesStream{body: watched, reader: bufio.NewReaderSize(watched, 64*1024), index: map[string]int{}}, nil
}

// idleTimeout resolves the configured idle limit.
func (o *OpenAIResponses) idleTimeout() time.Duration {
	if o.Idle > 0 {
		return o.Idle
	}
	return DefaultIdleTimeout
}

func responsesBody(req model.Request) (map[string]any, error) {
	// The agent replays the whole transcript on every call, so the server has
	// no state to continue from. Storing would keep the conversation on the
	// provider side and make an id in the transcript resolve to a different
	// item, so stateless is the only safe default.
	body := map[string]any{"model": req.Model, "stream": true, "store": false}
	if !responsesStoresReasoning(req.Options) {
		// Without stored state, encrypted reasoning is the only way the model
		// can see its own earlier chain on the next turn.
		body["include"] = []string{"reasoning.encrypted_content"}
	}
	input := make([]map[string]any, 0, len(req.Messages))
	for _, message := range req.Messages {
		if len(message.Attachments) > 0 {
			content := responsesAttachmentContent(message.Attachments)
			if message.Content != "" {
				content = append([]map[string]any{{"type": "input_text", "text": message.Content}}, content...)
			}
			input = append(input, map[string]any{"type": "message", "role": string(message.Role), "content": content})
			continue
		}
		switch message.Role {
		case "tool":
			input = append(input, map[string]any{
				"type":    "function_call_output",
				"call_id": message.ToolCallID,
				"output":  message.Content,
			})
			continue
		case "assistant":
			// A reasoning item must precede the call it led to. The API
			// rejects a function_call whose reasoning item is missing, so the
			// proof is replayed before the text and the calls.
			for _, item := range responsesReasoningItems(message.Parts) {
				input = append(input, item)
			}
			if message.Content != "" {
				input = append(input, map[string]any{
					"type":    "message",
					"role":    "assistant",
					"content": []map[string]any{{"type": "output_text", "text": message.Content}},
				})
			}
			for _, call := range message.ToolCalls {
				input = append(input, map[string]any{
					"type":      "function_call",
					"call_id":   call.ID,
					"name":      call.Name,
					"arguments": call.Arguments,
				})
			}
			continue
		}
		role := string(message.Role)
		partType := "input_text"
		if role == "assistant" {
			partType = "output_text"
		}
		if message.Content != "" {
			input = append(input, map[string]any{
				"type":    "message",
				"role":    role,
				"content": []map[string]any{{"type": partType, "text": message.Content}},
			})
		}
	}
	body["input"] = input
	var tools []map[string]any
	for _, item := range req.Tools {
		schema := item.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		tools = append(tools, map[string]any{
			"type":        "function",
			"name":        item.Name,
			"description": item.Description,
			"parameters":  schema,
		})
	}
	if len(tools) > 0 {
		body["tools"] = tools
	}
	switch req.ToolChoice {
	case "":
	case "none":
		body["tool_choice"] = "none"
	default:
		body["tool_choice"] = "auto"
	}
	return body, nil
}

// responsesReasoningItems replays the reasoning parts of one assistant step as
// reasoning input items. A part without encrypted content cannot be replayed:
// the API needs either a stored id or the encrypted blob, and an invented one
// is rejected, so a bare summary is skipped.
func responsesReasoningItems(parts []model.MessagePart) []map[string]any {
	var items []map[string]any
	for _, part := range parts {
		if part.Type != "reasoning" || part.Redacted == "" {
			continue
		}
		if part.Source != "" && part.Source != ReasoningSourceResponses {
			continue
		}
		items = append(items, map[string]any{
			"type":              "reasoning",
			"summary":           []map[string]any{},
			"encrypted_content": part.Redacted,
		})
	}
	return items
}

// responsesStoresReasoning reports whether the model configuration asks the
// provider to keep the response, which replaces the need for encrypted
// reasoning. A caller that sets store explicitly keeps that choice.
func responsesStoresReasoning(options map[string]any) bool {
	value, ok := options["store"]
	if !ok {
		return false
	}
	stored, _ := value.(bool)
	return stored
}

func responsesAttachmentContent(attachments []model.Attachment) []map[string]any {
	content := make([]map[string]any, 0, len(attachments))
	for _, attachment := range attachments {
		mime := strings.ToLower(strings.TrimSpace(attachment.Mime))
		name := attachment.Name
		if name == "" {
			name = "attachment"
		}
		switch {
		case strings.HasPrefix(mime, "image/"):
			content = append(content, map[string]any{
				"type":      "input_image",
				"image_url": "data:" + mime + ";base64," + attachment.Data,
			})
		case mime == "application/pdf":
			content = append(content, map[string]any{
				"type":      "input_file",
				"filename":  name,
				"file_data": "data:" + mime + ";base64," + attachment.Data,
			})
		case model.IsTextMime(mime):
			decoded, err := base64.StdEncoding.DecodeString(attachment.Data)
			if err != nil {
				continue
			}
			content = append(content, map[string]any{
				"type": "input_text",
				"text": fmt.Sprintf("[attachment %s]\n%s\n[/attachment]", name, string(decoded)),
			})
		}
	}
	return content
}

type responsesStream struct {
	body   io.ReadCloser
	reader *bufio.Reader
	index  map[string]int
	next   int
}

type responsesEvent struct {
	Type   string `json:"type"`
	Delta  string `json:"delta"`
	ItemID string `json:"item_id"`
	Item   *struct {
		Type             string `json:"type"`
		ID               string `json:"id"`
		CallID           string `json:"call_id"`
		Name             string `json:"name"`
		Arguments        string `json:"arguments"`
		EncryptedContent string `json:"encrypted_content"`
	} `json:"item"`
	Response *struct {
		Status string `json:"status"`
		Usage  *struct {
			InputTokens        int `json:"input_tokens"`
			OutputTokens       int `json:"output_tokens"`
			InputTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"input_tokens_details"`
		} `json:"usage"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	} `json:"response"`
	Message string `json:"message"`
}

func (s *responsesStream) Recv() (Chunk, error) {
	var chunk Chunk
	for {
		line, err := s.reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			var event responsesEvent
			if parseErr := json.Unmarshal([]byte(payload), &event); parseErr != nil {
				return Chunk{}, fmt.Errorf("decode responses event: %w", parseErr)
			}
			switch event.Type {
			case "response.output_text.delta":
				chunk.Text = event.Delta
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta":
				chunk.Reasoning = event.Delta
			case "response.output_item.added":
				if event.Item != nil && event.Item.Type == "function_call" {
					index := s.next
					s.next++
					s.index[event.Item.ID] = index
					chunk.ToolCalls = append(chunk.ToolCalls, ToolCallDelta{
						Index:     index,
						ID:        event.Item.CallID,
						Name:      event.Item.Name,
						Arguments: event.Item.Arguments,
					})
				}
			case "response.function_call_arguments.delta":
				index, ok := s.index[event.ItemID]
				if !ok {
					index = s.next
					s.next++
					s.index[event.ItemID] = index
				}
				chunk.ToolCalls = append(chunk.ToolCalls, ToolCallDelta{Index: index, Arguments: event.Delta})
			case "response.output_item.done":
				// The completed reasoning item carries the encrypted blob; the
				// added event usually has none. It closes the summary text that
				// streamed in between, so the proof joins that part.
				if event.Item != nil && event.Item.Type == "reasoning" && event.Item.EncryptedContent != "" {
					chunk.ReasoningRedacted = event.Item.EncryptedContent
					chunk.ReasoningSource = ReasoningSourceResponses
				}
			case "response.completed", "response.incomplete":
				if event.Response != nil {
					if event.Response.Usage != nil {
						cached := 0
						if event.Response.Usage.InputTokensDetails != nil {
							cached = event.Response.Usage.InputTokensDetails.CachedTokens
						}
						input := event.Response.Usage.InputTokens - cached
						if input < 0 {
							input = 0
						}
						chunk.Usage = &Usage{Input: input, Output: event.Response.Usage.OutputTokens, CacheRead: cached}
					}
					if event.Response.Status == "incomplete" {
						chunk.FinishReason = "length"
					} else {
						chunk.FinishReason = "stop"
					}
				}
			case "response.failed":
				message := "response failed"
				if event.Response != nil && event.Response.Error != nil && event.Response.Error.Message != "" {
					message = event.Response.Error.Message
				}
				return Chunk{}, classifyError(0, "stream error", message)
			case "error":
				message := event.Message
				if message == "" {
					message = "response error"
				}
				return Chunk{}, classifyError(0, "stream error", message)
			}
			if chunk.Text != "" || chunk.Reasoning != "" || len(chunk.ToolCalls) > 0 || chunk.FinishReason != "" || chunk.Usage != nil {
				return chunk, nil
			}
		}
		if err != nil {
			if err == io.EOF && (chunk.Text != "" || chunk.Reasoning != "" || len(chunk.ToolCalls) > 0 || chunk.Usage != nil) {
				return chunk, nil
			}
			return chunk, err
		}
	}
}

func (s *responsesStream) Close() error { return s.body.Close() }
