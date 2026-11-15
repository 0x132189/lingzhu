// Package imagegen calls a configurable OpenAI-compatible image endpoint and
// returns decoded image bytes. The model-facing tool saves one image per call.
package imagegen

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	defaultModel    = "gpt-image-1"
	defaultSize     = "1024x1024"
	defaultEndpoint = "https://api.openai.com/v1/images/generations"
	maxImageBytes   = 20 * 1024 * 1024
	generateTimeout = 3 * time.Minute
	downloadTimeout = 30 * time.Second
)

// Config is the image generation section.
type Config struct {
	Provider  string `json:"provider,omitempty"`
	Endpoint  string `json:"endpoint,omitempty"`
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	Model     string `json:"model,omitempty"`
	Size      string `json:"size,omitempty"`
}

// Result is one decoded image.
type Result struct {
	Data          []byte
	MediaType     string
	Extension     string
	RevisedPrompt string
}

// Status is the public configuration view.
type Status struct {
	Provider   string `json:"provider"`
	Configured bool   `json:"configured"`
	Endpoint   string `json:"endpoint"`
	Model      string `json:"model"`
	Size       string `json:"size"`
}

// Manager performs one image generation request at a time.
type Manager struct {
	provider string
	endpoint string
	apiKey   string
	model    string
	size     string
	client   *http.Client
}

// New resolves configuration and leaves the manager unconfigured when the
// endpoint or key is missing.
func New(cfg Config) *Manager {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = "openai"
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" && provider == "openai" {
		endpoint = defaultEndpoint
	}
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" && cfg.APIKeyEnv != "" {
		key = os.Getenv(cfg.APIKeyEnv)
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		model = defaultModel
	}
	size := strings.TrimSpace(cfg.Size)
	if size == "" {
		size = defaultSize
	}
	return &Manager{
		provider: provider,
		endpoint: endpoint,
		apiKey:   key,
		model:    model,
		size:     size,
		client:   &http.Client{Timeout: generateTimeout},
	}
}

// Configured reports whether generation can run.
func (m *Manager) Configured() bool {
	if m == nil || strings.TrimSpace(m.endpoint) == "" {
		return false
	}
	return m.provider == "generic" || strings.TrimSpace(m.apiKey) != ""
}

// Status returns configuration without the key.
func (m *Manager) Status() Status {
	if m == nil {
		return Status{}
	}
	return Status{
		Provider:   m.provider,
		Configured: m.Configured(),
		Endpoint:   m.endpoint,
		Model:      m.model,
		Size:       m.size,
	}
}

// Generate creates one image.
func (m *Manager) Generate(ctx context.Context, prompt, size string) (Result, error) {
	if !m.Configured() {
		return Result{}, fmt.Errorf("image generation is not configured")
	}
	prompt = strings.TrimSpace(prompt)
	if prompt == "" {
		return Result{}, fmt.Errorf("image prompt is empty")
	}
	if strings.TrimSpace(size) == "" {
		size = m.size
	}
	payload, err := json.Marshal(map[string]any{
		"model":  m.model,
		"prompt": prompt,
		"n":      1,
		"size":   size,
	})
	if err != nil {
		return Result{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(payload))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if m.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.apiKey)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+64*1024))
	if err != nil {
		return Result{}, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := strings.TrimSpace(string(body))
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return Result{}, fmt.Errorf("image endpoint: HTTP %d: %s", resp.StatusCode, detail)
	}
	var payloadOut struct {
		Data []struct {
			B64JSON       string `json:"b64_json"`
			URL           string `json:"url"`
			RevisedPrompt string `json:"revised_prompt"`
		} `json:"data"`
		Images []struct {
			B64   string `json:"b64"`
			URL   string `json:"url"`
			Media string `json:"media_type"`
		} `json:"images"`
	}
	if err := json.Unmarshal(body, &payloadOut); err != nil {
		return Result{}, fmt.Errorf("image endpoint: invalid response: %w", err)
	}
	if len(payloadOut.Data) == 0 && len(payloadOut.Images) == 0 {
		return Result{}, fmt.Errorf("image endpoint returned no image")
	}
	var data []byte
	revised := ""
	if len(payloadOut.Images) > 0 {
		item := payloadOut.Images[0]
		revised = ""
		if item.B64 != "" {
			data, err = decodeBase64(item.B64)
		} else if item.URL != "" {
			data, err = m.download(ctx, item.URL)
		}
	} else {
		item := payloadOut.Data[0]
		revised = item.RevisedPrompt
		if item.B64JSON != "" {
			data, err = decodeBase64(item.B64JSON)
		} else if item.URL != "" {
			data, err = m.download(ctx, item.URL)
		}
	}
	if err != nil {
		return Result{}, err
	}
	if len(data) == 0 {
		return Result{}, fmt.Errorf("image endpoint returned an empty image")
	}
	if len(data) > maxImageBytes {
		return Result{}, fmt.Errorf("image is larger than 20 MiB")
	}
	mediaType, extension := detect(data)
	return Result{Data: data, MediaType: mediaType, Extension: extension, RevisedPrompt: revised}, nil
}

func (m *Manager) download(ctx context.Context, raw string) ([]byte, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("invalid image url %q", raw)
	}
	ctx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("image download: HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, maxImageBytes+1))
}

func decodeBase64(value string) ([]byte, error) {
	if strings.HasPrefix(value, "data:") {
		if index := strings.Index(value, ","); index >= 0 {
			value = value[index+1:]
		}
	}
	return base64.StdEncoding.DecodeString(value)
}

func detect(data []byte) (string, string) {
	switch {
	case len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		return "image/png", ".png"
	case len(data) >= 3 && data[0] == 0xff && data[1] == 0xd8 && data[2] == 0xff:
		return "image/jpeg", ".jpg"
	case len(data) >= 6 && (bytes.Equal(data[:6], []byte("GIF87a")) || bytes.Equal(data[:6], []byte("GIF89a"))):
		return "image/gif", ".gif"
	case len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return "image/webp", ".webp"
	}
	return "image/png", ".png"
}
