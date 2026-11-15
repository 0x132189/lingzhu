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
	"unicode/utf8"

	"lingzhu/internal/model"
)

// OpenAI adapts any OpenAI-compatible chat/completions endpoint.
type OpenAI struct {
	BaseURL string
	APIKey  string
	// TokenSource, when set, supplies a fresh bearer token per request.
	TokenSource func(ctx context.Context) (string, error)
	Client      *http.Client
	// Idle bounds the wait for the next stream chunk. Zero uses the default.
	Idle time.Duration
}

func NewOpenAI(baseURL, apiKey string) *OpenAI {
	return &OpenAI{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		Client:  NewClient(DefaultTimeouts()),
	}
}

func (o *OpenAI) Stream(ctx context.Context, req model.Request) (Stream, error) {
	payload, err := json.Marshal(toChatRequest(req))
	if err != nil {
		return nil, err
	}
	if options := normalizeOptions(req.Options); len(options) > 0 {
		body := map[string]any{}
		if err := json.Unmarshal(payload, &body); err != nil {
			return nil, err
		}
		for key, value := range options {
			body[key] = value
		}
		if payload, err = json.Marshal(body); err != nil {
			return nil, err
		}
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, o.BaseURL+"/chat/completions", bytes.NewReader(payload))
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, classifyErrorAfter(resp.StatusCode, resp.Status, string(body), ParseRetryAfter(resp.Header.Get("Retry-After")))
	}
	body := WatchIdle(resp.Body, o.idleTimeout())
	return &openAIStream{body: body, reader: bufio.NewReaderSize(body, 64*1024)}, nil
}

// idleTimeout resolves the configured idle limit.
func (o *OpenAI) idleTimeout() time.Duration {
	if o.Idle > 0 {
		return o.Idle
	}
	return DefaultIdleTimeout
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
			if chunk.Text != "" || chunk.Reasoning != "" || len(chunk.ToolCalls) > 0 || chunk.FinishReason != "" || chunk.Usage != nil {
				return chunk, nil
			}
		}
		if err != nil {
			return Chunk{}, err
		}
	}
}

func (s *openAIStream) Close() error { return s.body.Close() }

// optionAliases accept the option spelling agents use in their configs and
// send the chat-completions field of the same meaning. Unknown keys pass
// through untouched for vendor-specific fields.
var optionAliases = map[string]string{
	"reasoningEffort":   "reasoning_effort",
	"reasoningSummary":  "reasoning_summary",
	"textVerbosity":     "verbosity",
	"maxOutputTokens":   "max_tokens",
	"topP":              "top_p",
	"topK":              "top_k",
	"presencePenalty":   "presence_penalty",
	"frequencyPenalty":  "frequency_penalty",
	"stopSequences":     "stop",
	"parallelToolCalls": "parallel_tool_calls",
}

func normalizeOptions(options map[string]any) map[string]any {
	if len(options) == 0 {
		return nil
	}
	out := make(map[string]any, len(options))
	for key, value := range options {
		if alias, ok := optionAliases[key]; ok {
			key = alias
		}
		out[key] = value
	}
	return out
}

type chatRequest struct {
	Model         string             `json:"model"`
	Messages      []chatMessage      `json:"messages"`
	Tools         []chatTool         `json:"tools,omitempty"`
	ToolChoice    string             `json:"tool_choice,omitempty"`
	Stream        bool               `json:"stream"`
	StreamOptions *chatStreamOptions `json:"stream_options,omitempty"`
}

type chatStreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    any            `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

type chatContentPart struct {
	Type     string        `json:"type"`
	Text     string        `json:"text,omitempty"`
	ImageURL *chatImageURL `json:"image_url,omitempty"`
	File     *chatFile     `json:"file,omitempty"`
}

type chatImageURL struct {
	URL string `json:"url"`
}

type chatFile struct {
	Filename string `json:"filename"`
	FileData string `json:"file_data"`
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
	out := chatRequest{
		Model:         req.Model,
		Stream:        true,
		StreamOptions: &chatStreamOptions{IncludeUsage: true},
		Messages:      make([]chatMessage, 0, len(req.Messages)),
	}
	for _, m := range req.Messages {
		msg := chatMessage{Role: string(m.Role), ToolCallID: m.ToolCallID}
		if m.Content != "" {
			msg.Content = m.Content
		}
		for _, call := range m.ToolCalls {
			msg.ToolCalls = append(msg.ToolCalls, chatToolCall{
				ID:       call.ID,
				Type:     "function",
				Function: chatFunction{Name: call.Name, Arguments: call.Arguments},
			})
		}
		// A prompt carrying only attachments has no text body. Emitting the
		// empty message alongside the media parts makes providers reject the
		// whole request ("content must be a string or text-part array").
		if msg.Content != nil || len(msg.ToolCalls) > 0 || msg.ToolCallID != "" {
			out.Messages = append(out.Messages, msg)
		}
		if parts := mediaParts(m.Attachments); len(parts) > 0 {
			out.Messages = append(out.Messages, chatMessage{Role: "user", Content: parts})
		}
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
	out.ToolChoice = req.ToolChoice
	return out
}

func mediaParts(attachments []model.Attachment) []chatContentPart {
	parts := make([]chatContentPart, 0, len(attachments))
	for _, attachment := range attachments {
		mime := strings.ToLower(strings.TrimSpace(attachment.Mime))
		name := attachment.Name
		if name == "" {
			if mime == "application/pdf" {
				name = "document.pdf"
			} else {
				name = "attachment"
			}
		}
		dataURL := "data:" + mime + ";base64," + attachment.Data
		switch {
		case mime == "application/pdf":
			parts = append(parts, chatContentPart{
				Type: "file",
				File: &chatFile{Filename: name, FileData: dataURL},
			})
		case strings.HasPrefix(mime, "image/"):
			parts = append(parts, chatContentPart{
				Type:     "image_url",
				ImageURL: &chatImageURL{URL: dataURL},
			})
		case model.IsTextMime(mime):
			decoded, err := base64.StdEncoding.DecodeString(attachment.Data)
			if err != nil || !utf8.Valid(decoded) || bytes.IndexByte(decoded, 0) >= 0 {
				parts = append(parts, chatContentPart{
					Type: "text",
					Text: fmt.Sprintf("[attachment %s (%s) omitted: unreadable text]", name, mime),
				})
				continue
			}
			label := name
			if attachment.Kind == "directory" {
				label = "directory " + name
			} else if attachment.Start > 0 {
				label = fmt.Sprintf("%s L%d-L%d", name, attachment.Start, attachment.End)
			}
			parts = append(parts, chatContentPart{
				Type: "text",
				Text: fmt.Sprintf("[attachment %s]\n%s\n[/attachment]", label, string(decoded)),
			})
		default:
			parts = append(parts, chatContentPart{
				Type: "text",
				Text: fmt.Sprintf("[attachment %s (%s) omitted: unsupported media type]", name, mime),
			})
		}
	}
	return parts
}

type streamResponse struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`
			ReasoningContent string `json:"reasoning_content"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage *chatUsage `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

type chatUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptCacheHit      int `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func parseChunk(payload string) (Chunk, error) {
	var resp streamResponse
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return Chunk{}, fmt.Errorf("decode stream chunk: %w", err)
	}
	if resp.Error != nil {
		return Chunk{}, classifyError(0, "stream error", resp.Error.Message)
	}
	var chunk Chunk
	for _, choice := range resp.Choices {
		chunk.Text += choice.Delta.Content
		if choice.Delta.ReasoningContent != "" {
			chunk.Reasoning += choice.Delta.ReasoningContent
		} else {
			chunk.Reasoning += choice.Delta.Reasoning
		}
		if choice.FinishReason != "" {
			chunk.FinishReason = choice.FinishReason
		}
		for _, call := range choice.Delta.ToolCalls {
			chunk.ToolCalls = append(chunk.ToolCalls, ToolCallDelta{
				Index:     call.Index,
				ID:        call.ID,
				Name:      call.Function.Name,
				Arguments: call.Function.Arguments,
			})
		}
	}
	chunk.Usage = parseUsage(resp.Usage)
	return chunk, nil
}

// parseUsage normalizes provider usage: prompt tokens include cached ones, so
// cached tokens are moved out of Input and into CacheRead.
func parseUsage(usage *chatUsage) *Usage {
	if usage == nil || usage.PromptTokens == 0 && usage.CompletionTokens == 0 {
		return nil
	}
	cached := usage.PromptCacheHit
	if usage.PromptTokensDetails != nil && usage.PromptTokensDetails.CachedTokens > cached {
		cached = usage.PromptTokensDetails.CachedTokens
	}
	if cached > usage.PromptTokens {
		cached = usage.PromptTokens
	}
	return &Usage{
		Input:     usage.PromptTokens - cached,
		Output:    usage.CompletionTokens,
		CacheRead: cached,
	}
}
