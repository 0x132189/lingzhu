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

// Anthropic adapts the Messages API. Tools, images, PDFs and text attachments
// are translated to the block format the API expects.
type Anthropic struct {
	BaseURL string
	APIKey  string
	// TokenSource, when set, supplies a fresh bearer token per request.
	TokenSource func(ctx context.Context) (string, error)
	Client      *http.Client
	// Idle bounds the wait for the next stream chunk. Zero uses the default.
	Idle time.Duration
}

// NewAnthropic builds a Messages adapter. A base URL without /v1 keeps what the
// user configured; the caller is expected to point it at the API root.
func NewAnthropic(baseURL, apiKey string) *Anthropic {
	return &Anthropic{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Client:  NewClient(DefaultTimeouts()),
	}
}

func (a *Anthropic) Stream(ctx context.Context, req model.Request) (Stream, error) {
	body, err := anthropicBody(req)
	if err != nil {
		return nil, err
	}
	if options := normalizeOptions(req.Options); len(options) > 0 {
		for key, value := range options {
			body[key] = value
		}
	}
	if _, ok := body["max_tokens"]; !ok {
		body["max_tokens"] = 8192
	}
	payload, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.BaseURL+"/messages", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "text/event-stream")
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	for key, value := range req.Headers {
		httpReq.Header.Set(key, value)
	}
	if a.TokenSource != nil {
		token, err := a.TokenSource(ctx)
		if err != nil {
			return nil, err
		}
		httpReq.Header.Set("Authorization", "Bearer "+token)
	} else if a.APIKey != "" {
		httpReq.Header.Set("x-api-key", a.APIKey)
	}

	resp, err := a.Client.Do(httpReq)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		data, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, classifyErrorAfter(resp.StatusCode, resp.Status, string(data), ParseRetryAfter(resp.Header.Get("Retry-After")))
	}
	watched := WatchIdle(resp.Body, a.idleTimeout())
	return &anthropicStream{body: watched, reader: bufio.NewReaderSize(watched, 64*1024)}, nil
}

// idleTimeout resolves the configured idle limit.
func (a *Anthropic) idleTimeout() time.Duration {
	if a.Idle > 0 {
		return a.Idle
	}
	return DefaultIdleTimeout
}

func anthropicBody(req model.Request) (map[string]any, error) {
	body := map[string]any{"model": req.Model, "stream": true}
	var system []string
	var messages []map[string]any
	pending := map[string]any{}
	flush := func() {
		if len(pending) > 0 {
			messages = append(messages, pending)
			pending = map[string]any{}
		}
	}
	add := func(role string, block map[string]any) {
		content, _ := pending["content"].([]map[string]any)
		if current, ok := pending["role"].(string); ok && current != role {
			flush()
			content = nil
		}
		pending["role"] = role
		pending["content"] = append(content, block)
	}
	for _, message := range req.Messages {
		switch message.Role {
		case "system":
			if text := strings.TrimSpace(message.Content); text != "" {
				system = append(system, text)
			}
		case "tool":
			add("user", map[string]any{
				"type":        "tool_result",
				"tool_use_id": message.ToolCallID,
				"content":     message.Content,
			})
		case "assistant":
			// Thinking blocks come first, in the order they were produced. A
			// block with a signature is replayed as proof; a redacted one is
			// replayed encrypted. Without them Anthropic rejects a request that
			// continues a thinking turn.
			for _, block := range anthropicThinkingBlocks(message.Parts) {
				add("assistant", block)
			}
			if message.Content != "" {
				add("assistant", map[string]any{"type": "text", "text": message.Content})
			}
			for _, call := range message.ToolCalls {
				var input any = map[string]any{}
				if strings.TrimSpace(call.Arguments) != "" {
					_ = json.Unmarshal([]byte(call.Arguments), &input)
					if input == nil {
						input = map[string]any{}
					}
				}
				add("assistant", map[string]any{
					"type":  "tool_use",
					"id":    call.ID,
					"name":  call.Name,
					"input": input,
				})
			}
		default:
			for _, block := range anthropicAttachmentBlocks(message.Attachments) {
				add("user", block)
			}
			if message.Content != "" {
				add("user", map[string]any{"type": "text", "text": message.Content})
			}
		}
	}
	flush()
	if len(system) > 0 {
		text := strings.Join(system, "\n\n")
		if req.CacheTTL > 0 {
			body["system"] = []map[string]any{{
				"type":          "text",
				"text":          text,
				"cache_control": anthropicCacheControl(req.CacheTTL),
			}}
		} else {
			body["system"] = text
		}
	}
	if req.CacheTTL > 0 && len(messages) > 0 {
		// Mark the tail block so the next request can reuse the whole
		// conversation prefix, matching the reference cache policy.
		last := messages[len(messages)-1]
		if blocks, ok := last["content"].([]map[string]any); ok && len(blocks) > 0 {
			blocks[len(blocks)-1]["cache_control"] = anthropicCacheControl(req.CacheTTL)
		}
	}
	body["messages"] = messages
	var tools []map[string]any
	for _, item := range req.Tools {
		schema := item.Schema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		tools = append(tools, map[string]any{
			"name":         item.Name,
			"description":  item.Description,
			"input_schema": schema,
		})
	}
	if len(tools) > 0 {
		if req.CacheTTL > 0 {
			tools[len(tools)-1]["cache_control"] = anthropicCacheControl(req.CacheTTL)
		}
		body["tools"] = tools
	}
	switch req.ToolChoice {
	case "":
	case "none":
		body["tool_choice"] = map[string]any{"type": "none"}
	default:
		body["tool_choice"] = map[string]any{"type": "auto"}
	}
	return body, nil
}

// anthropicCacheControl keeps the default five-minute cache for normal TTLs
// and asks for the one-hour window when the catalog advertises it.
func anthropicCacheControl(ttl time.Duration) map[string]any {
	control := map[string]any{"type": "ephemeral"}
	if ttl >= 30*time.Minute {
		control["ttl"] = "1h"
	}
	return control
}

// anthropicThinkingBlocks replays the reasoning parts of one assistant step as
// thinking blocks. Anthropic requires every block to carry its signature, and
// rejects a request that drops one, so a part without proof is skipped rather
// than sent as plain text. A proof issued by another API is skipped too: the
// server would reject a blob it did not sign.
func anthropicThinkingBlocks(parts []model.MessagePart) []map[string]any {
	var blocks []map[string]any
	for _, part := range parts {
		if part.Type != "reasoning" {
			continue
		}
		if part.Source != "" && part.Source != ReasoningSourceAnthropic {
			continue
		}
		switch {
		case part.Redacted != "":
			blocks = append(blocks, map[string]any{
				"type": "redacted_thinking",
				"data": part.Redacted,
			})
		case part.Signature != "":
			blocks = append(blocks, map[string]any{
				"type":      "thinking",
				"thinking":  part.Text,
				"signature": part.Signature,
			})
		}
	}
	return blocks
}

func anthropicAttachmentBlocks(attachments []model.Attachment) []map[string]any {
	blocks := make([]map[string]any, 0, len(attachments))
	for _, attachment := range attachments {
		mime := strings.ToLower(strings.TrimSpace(attachment.Mime))
		source := map[string]any{"type": "base64", "media_type": mime, "data": attachment.Data}
		switch {
		case strings.HasPrefix(mime, "image/"):
			blocks = append(blocks, map[string]any{"type": "image", "source": source})
		case mime == "application/pdf":
			blocks = append(blocks, map[string]any{"type": "document", "source": source})
		case model.IsTextMime(mime):
			decoded, err := base64.StdEncoding.DecodeString(attachment.Data)
			if err != nil {
				continue
			}
			name := attachment.Name
			if name == "" {
				name = "attachment"
			}
			blocks = append(blocks, map[string]any{
				"type": "text",
				"text": fmt.Sprintf("[attachment %s]\n%s\n[/attachment]", name, string(decoded)),
			})
		}
	}
	return blocks
}

type anthropicStream struct {
	body   io.ReadCloser
	reader *bufio.Reader
	usage  *Usage
}

func (s *anthropicStream) addUsage(input, output, cacheRead, cacheWrite int) {
	if s.usage == nil {
		s.usage = &Usage{}
	}
	if input > 0 {
		s.usage.Input = input
	}
	if output > 0 {
		s.usage.Output = output
	}
	if cacheRead > 0 {
		s.usage.CacheRead = cacheRead
	}
	if cacheWrite > 0 {
		s.usage.CacheWrite = cacheWrite
	}
}

type anthropicEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage *struct {
			InputTokens          int `json:"input_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
			CacheCreationTokens  int `json:"cache_creation_input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type     string `json:"type"`
		ID       string `json:"id"`
		Name     string `json:"name"`
		Thinking string `json:"thinking"`
		Data     string `json:"data"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		Content     string `json:"content"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

func (s *anthropicStream) Recv() (Chunk, error) {
	var chunk Chunk
	for {
		line, err := s.reader.ReadString('\n')
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "data:") {
			payload := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
			event, parseErr := parseAnthropicEvent(payload)
			if parseErr != nil {
				return Chunk{}, parseErr
			}
			switch event.Type {
			case "error":
				return Chunk{}, classifyError(0, "stream error", event.Error.Message)
			case "message_start":
				if event.Message != nil && event.Message.Usage != nil {
					s.addUsage(
						event.Message.Usage.InputTokens,
						0,
						event.Message.Usage.CacheReadInputTokens,
						event.Message.Usage.CacheCreationTokens,
					)
				}
			case "content_block_start":
				if event.ContentBlock != nil {
					switch event.ContentBlock.Type {
					case "tool_use":
						chunk.ToolCalls = append(chunk.ToolCalls, ToolCallDelta{
							Index: event.Index,
							ID:    event.ContentBlock.ID,
							Name:  event.ContentBlock.Name,
						})
					case "redacted_thinking":
						// Encrypted reasoning arrives whole in the start block.
						chunk.ReasoningRedacted = event.ContentBlock.Data
						chunk.ReasoningSource = ReasoningSourceAnthropic
					}
				}
			case "content_block_delta":
				if event.Delta != nil {
					switch event.Delta.Type {
					case "text_delta":
						chunk.Text = event.Delta.Text
					case "thinking_delta":
						chunk.Reasoning = event.Delta.Thinking
					case "signature_delta":
						// The signature closes the thinking block it belongs to.
						chunk.ReasoningSignature = event.Delta.Signature
						chunk.ReasoningSource = ReasoningSourceAnthropic
					case "compaction_delta":
						chunk.Compaction = event.Delta.Content
					case "input_json_delta":
						chunk.ToolCalls = append(chunk.ToolCalls, ToolCallDelta{
							Index:     event.Index,
							Arguments: event.Delta.PartialJSON,
						})
					}
				}
			case "message_delta":
				if event.Delta != nil {
					chunk.FinishReason = anthropicFinish(event.Delta.StopReason)
				}
				if event.Usage != nil {
					s.addUsage(0, event.Usage.OutputTokens, 0, 0)
				}
			case "message_stop":
				if chunkProduced(chunk) {
					return chunk, nil
				}
				return Chunk{}, io.EOF
			}
			if chunkProduced(chunk) {
				if s.usage != nil {
					usage := *s.usage
					chunk.Usage = &usage
				}
				return chunk, nil
			}
		}
		if err != nil {
			if err == io.EOF && chunkProduced(chunk) {
				return chunk, nil
			}
			return chunk, err
		}
	}
}

func parseAnthropicEvent(payload string) (anthropicEvent, error) {
	var event anthropicEvent
	if err := json.Unmarshal([]byte(payload), &event); err != nil {
		return anthropicEvent{}, fmt.Errorf("decode anthropic event: %w", err)
	}
	return event, nil
}

func anthropicFinish(reason string) string {
	switch reason {
	case "tool_use":
		return "tool_calls"
	case "max_tokens":
		return "length"
	case "end_turn", "stop_sequence":
		return "stop"
	default:
		return reason
	}
}

func (s *anthropicStream) Close() error { return s.body.Close() }

// chunkProduced reports whether one decoded event carries anything the caller
// must see. A signature or redacted block counts on its own, because it is the
// only proof a replayed thinking block came from this provider.
func chunkProduced(chunk Chunk) bool {
	return chunk.Text != "" ||
		chunk.Reasoning != "" ||
		chunk.ReasoningSignature != "" ||
		chunk.ReasoningRedacted != "" ||
		chunk.Compaction != "" ||
		len(chunk.ToolCalls) > 0 ||
		chunk.FinishReason != "" ||
		chunk.Usage != nil
}
