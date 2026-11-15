package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"lingzhu/internal/tool"
)

// Tool exposes web search to the model.
func Tool(manager *Manager) tool.Tool {
	return &searchTool{manager: manager}
}

type searchTool struct {
	manager *Manager
}

func (t *searchTool) Name() string { return "websearch" }

func (t *searchTool) Description() string {
	return fmt.Sprintf("Search the web using the user's selected search integration. Use this for current information beyond the knowledge cutoff.\n\nThe current year is %d. Use this year when searching for recent information or current events.", time.Now().Year())
}

func (t *searchTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Websearch query"}
		},
		"required": ["query"]
	}`)
}

func (t *searchTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("websearch: %w", err)
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return "", fmt.Errorf("websearch: query is empty")
	}
	if err := call.Allow(ctx, "websearch", []string{query}, ""); err != nil {
		return "", err
	}
	call.SetMetadata("provider", t.manager.provider)
	results, err := t.manager.Search(ctx, query)
	if err != nil {
		return "", err
	}
	if len(results) == 0 {
		return "No search results found. Please try a different query.", nil
	}
	var b strings.Builder
	for _, item := range results {
		title := item.Title
		if title == "" {
			title = item.URL
		}
		fmt.Fprintf(&b, "## [%s](%s)", title, item.URL)
		if item.Published != "" {
			b.WriteString("\nPublished: " + item.Published)
		}
		if item.Content != "" {
			b.WriteString("\n\n" + item.Content)
		}
		b.WriteString("\n\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}
