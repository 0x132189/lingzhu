// Package websearch queries a configured search provider and returns compact
// results for the model. Brave, Exa, and a generic JSON endpoint are supported.
package websearch

import (
	"bytes"
	"context"
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
	defaultMaxResults = 5
	maxResultsLimit   = 10
	maxResponseBytes  = 1 << 20
	maxContentChars   = 2000
	requestTimeout    = 15 * time.Second
)

// Config is the websearch section.
type Config struct {
	Provider   string `json:"provider,omitempty"`
	Endpoint   string `json:"endpoint,omitempty"`
	APIKey     string `json:"api_key,omitempty"`
	APIKeyEnv  string `json:"api_key_env,omitempty"`
	MaxResults int    `json:"max_results,omitempty"`
}

// Result is one search hit.
type Result struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Content   string `json:"content,omitempty"`
	Published string `json:"published,omitempty"`
}

// Status is the public configuration view.
type Status struct {
	Provider   string `json:"provider"`
	Configured bool   `json:"configured"`
	Endpoint   string `json:"endpoint"`
	MaxResults int    `json:"maxResults"`
}

// Manager performs searches with one provider.
type Manager struct {
	provider   string
	endpoint   string
	apiKey     string
	maxResults int
	client     *http.Client
}

// New resolves the provider configuration. A missing key or endpoint leaves the
// manager unconfigured instead of failing core startup.
func New(cfg Config) *Manager {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = "brave"
	}
	endpoint := strings.TrimSpace(cfg.Endpoint)
	if endpoint == "" {
		switch provider {
		case "exa":
			endpoint = "https://api.exa.ai/search"
		default:
			endpoint = "https://api.search.brave.com/res/v1/web/search"
		}
	}
	key := strings.TrimSpace(cfg.APIKey)
	if key == "" && cfg.APIKeyEnv != "" {
		key = os.Getenv(cfg.APIKeyEnv)
	}
	max := cfg.MaxResults
	if max <= 0 {
		max = defaultMaxResults
	}
	if max > maxResultsLimit {
		max = maxResultsLimit
	}
	return &Manager{
		provider:   provider,
		endpoint:   endpoint,
		apiKey:     key,
		maxResults: max,
		client:     &http.Client{Timeout: requestTimeout},
	}
}

// Configured reports whether the provider has enough settings to run.
func (m *Manager) Configured() bool {
	if m == nil {
		return false
	}
	if strings.TrimSpace(m.endpoint) == "" {
		return false
	}
	switch m.provider {
	case "generic":
		return true
	case "brave", "exa":
		return strings.TrimSpace(m.apiKey) != ""
	}
	return false
}

// Status returns the provider settings without the key.
func (m *Manager) Status() Status {
	if m == nil {
		return Status{}
	}
	return Status{
		Provider:   m.provider,
		Configured: m.Configured(),
		Endpoint:   m.endpoint,
		MaxResults: m.maxResults,
	}
}

// Search runs the provider query.
func (m *Manager) Search(ctx context.Context, query string) ([]Result, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, fmt.Errorf("websearch: query is empty")
	}
	if !m.Configured() {
		return nil, fmt.Errorf("websearch is not configured")
	}
	switch m.provider {
	case "exa":
		return m.searchExa(ctx, query)
	case "generic":
		return m.searchGeneric(ctx, query)
	default:
		return m.searchBrave(ctx, query)
	}
}

func (m *Manager) searchBrave(ctx context.Context, query string) ([]Result, error) {
	parsed, err := url.Parse(m.endpoint)
	if err != nil {
		return nil, err
	}
	values := parsed.Query()
	values.Set("q", query)
	values.Set("count", fmt.Sprintf("%d", m.maxResults))
	parsed.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-Subscription-Token", m.apiKey)
	return m.do(req, func(body []byte) ([]Result, error) {
		var payload struct {
			Web struct {
				Results []struct {
					Title       string `json:"title"`
					URL         string `json:"url"`
					Description string `json:"description"`
					Age         string `json:"age"`
				} `json:"results"`
			} `json:"web"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		out := make([]Result, 0, len(payload.Web.Results))
		for _, item := range payload.Web.Results {
			out = append(out, Result{Title: item.Title, URL: item.URL, Content: item.Description, Published: item.Age})
		}
		return out, nil
	})
}

func (m *Manager) searchExa(ctx context.Context, query string) ([]Result, error) {
	payload, err := json.Marshal(map[string]any{
		"query":      query,
		"numResults": m.maxResults,
		"contents": map[string]any{
			"text": map[string]any{"maxCharacters": maxContentChars},
		},
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-api-key", m.apiKey)
	return m.do(req, func(body []byte) ([]Result, error) {
		var payload struct {
			Results []struct {
				Title         string `json:"title"`
				URL           string `json:"url"`
				Text          string `json:"text"`
				PublishedDate string `json:"publishedDate"`
			} `json:"results"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		out := make([]Result, 0, len(payload.Results))
		for _, item := range payload.Results {
			out = append(out, Result{Title: item.Title, URL: item.URL, Content: item.Text, Published: item.PublishedDate})
		}
		return out, nil
	})
}

func (m *Manager) searchGeneric(ctx context.Context, query string) ([]Result, error) {
	parsed, err := url.Parse(m.endpoint)
	if err != nil {
		return nil, err
	}
	values := parsed.Query()
	values.Set("q", query)
	values.Set("count", fmt.Sprintf("%d", m.maxResults))
	parsed.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	if m.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.apiKey)
	}
	return m.do(req, func(body []byte) ([]Result, error) {
		var payload struct {
			Results []Result `json:"results"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			var list []Result
			if err := json.Unmarshal(body, &list); err != nil {
				return nil, err
			}
			payload.Results = list
		}
		return payload.Results, nil
	})
}

func (m *Manager) do(req *http.Request, parse func([]byte) ([]Result, error)) ([]Result, error) {
	resp, err := m.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail := strings.TrimSpace(string(body))
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return nil, fmt.Errorf("websearch: HTTP %d: %s", resp.StatusCode, detail)
	}
	results, err := parse(body)
	if err != nil {
		return nil, fmt.Errorf("websearch: invalid response: %w", err)
	}
	if len(results) > m.maxResults {
		results = results[:m.maxResults]
	}
	for i := range results {
		results[i].Content = clip(results[i].Content)
	}
	return results, nil
}

func clip(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= maxContentChars {
		return value
	}
	return value[:maxContentChars] + "..."
}
