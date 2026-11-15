package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"lingzhu/internal/tool"
)

// Tools returns the artifact_* tools backed by manager.
func Tools(manager *Manager) []tool.Tool {
	return []tool.Tool{
		&createTool{manager: manager},
		&readTool{manager: manager},
		&updateTool{manager: manager},
		&deleteTool{manager: manager},
	}
}

type createTool struct {
	manager *Manager
}

func (t *createTool) Name() string { return "lingzhu_artifact_create" }

func (t *createTool) Description() string {
	return "Publish a new artifact: a self-contained bundle of text files served from the local core. Use it for HTML pages, reports or diagrams the user should open in a browser."
}

func (t *createTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"title": {"type": "string", "description": "Human-readable artifact title."},
			"files": {"type": "object", "description": "UTF-8 file map keyed by relative path, for example index.html and assets/app.js."},
			"entry": {"type": "string", "description": "Entry file path relative to the artifact root. Defaults to index.html, index.md, or the sole file."}
		},
		"required": ["title", "files"]
	}`)
}

func (t *createTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		Title string            `json:"title"`
		Files map[string]string `json:"files"`
		Entry string            `json:"entry"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_artifact_create: %w", err)
	}
	if err := call.Allow(ctx, "lingzhu_artifact_create", []string{strings.TrimSpace(args.Title)}, ""); err != nil {
		return "", err
	}
	info, err := t.manager.Create(args.Title, args.Files, args.Entry, call.SessionID)
	if err != nil {
		return "", fmt.Errorf("lingzhu_artifact_create: %w", err)
	}
	return fmt.Sprintf("Published %s (%s): %s", info.Title, info.ID, info.URL), nil
}

type readTool struct {
	manager *Manager
}

func (t *readTool) Name() string { return "lingzhu_artifact_read" }

func (t *readTool) Description() string {
	return "Read an artifact's metadata and file contents so you can inspect or update it."
}

func (t *readTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Artifact ID (art_...) to read."}
		},
		"required": ["id"]
	}`)
}

func (t *readTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_artifact_read: %w", err)
	}
	if err := call.Allow(ctx, "lingzhu_artifact_read", []string{strings.TrimSpace(args.ID)}, ""); err != nil {
		return "", err
	}
	content, err := t.manager.Get(args.ID)
	if err != nil {
		return "", fmt.Errorf("lingzhu_artifact_read: %w", err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s)\nentry: %s\nurl: %s\nfiles: %d\n", content.Title, content.ID, content.Entry, content.URL, len(content.Files))
	names := make([]string, 0, len(content.Files))
	for name := range content.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", name, content.Files[name])
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

type updateTool struct {
	manager *Manager
}

func (t *updateTool) Name() string { return "lingzhu_artifact_update" }

func (t *updateTool) Description() string {
	return "Replace an existing artifact's files at the same ID and URL."
}

func (t *updateTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Artifact ID (art_...) to update."},
			"title": {"type": "string", "description": "Updated title, or omit to keep the current title."},
			"files": {"type": "object", "description": "Replacement UTF-8 file map keyed by relative path."},
			"entry": {"type": "string", "description": "Updated entry file path."}
		},
		"required": ["id", "files"]
	}`)
}

func (t *updateTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		ID    string            `json:"id"`
		Title string            `json:"title"`
		Files map[string]string `json:"files"`
		Entry string            `json:"entry"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_artifact_update: %w", err)
	}
	if err := call.Allow(ctx, "lingzhu_artifact_update", []string{strings.TrimSpace(args.ID)}, ""); err != nil {
		return "", err
	}
	info, err := t.manager.Update(args.ID, args.Title, args.Files, args.Entry, call.SessionID)
	if err != nil {
		return "", fmt.Errorf("lingzhu_artifact_update: %w", err)
	}
	return fmt.Sprintf("Updated %s (%s): %s", info.Title, info.ID, info.URL), nil
}

type deleteTool struct {
	manager *Manager
}

func (t *deleteTool) Name() string { return "lingzhu_artifact_delete" }

func (t *deleteTool) Description() string {
	return "Delete an artifact and remove its stored copy."
}

func (t *deleteTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Artifact ID (art_...) to delete."}
		},
		"required": ["id"]
	}`)
}

func (t *deleteTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_artifact_delete: %w", err)
	}
	if err := call.Allow(ctx, "lingzhu_artifact_delete", []string{strings.TrimSpace(args.ID)}, ""); err != nil {
		return "", err
	}
	if err := t.manager.Delete(args.ID); err != nil {
		return "", fmt.Errorf("lingzhu_artifact_delete: %w", err)
	}
	return fmt.Sprintf("Deleted %s", args.ID), nil
}
