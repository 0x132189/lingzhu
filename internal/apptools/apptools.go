// Package apptools exposes model-facing tools for managing lingzhu itself:
// session titles, git worktrees, and the model catalog.
package apptools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
	"lingzhu/internal/worktree"
)

// Tools returns the lingzhu namespace tools backed by existing services.
func Tools(store *session.Store, worktrees *worktree.Manager, catalog *provider.Catalog) []tool.Tool {
	return []tool.Tool{
		&renameTool{store: store},
		&worktreeCreateTool{manager: worktrees},
		&worktreeListTool{manager: worktrees},
		&worktreeRemoveTool{manager: worktrees},
		&modelsTool{catalog: catalog},
		&selfUpdateTool{},
	}
}

type renameTool struct {
	store *session.Store
}

func (t *renameTool) Name() string { return "lingzhu_session_rename" }

func (t *renameTool) Description() string {
	return "Rename a session, or omit sessionID to rename the current session. Use a short, specific title that summarizes the work being done."
}

func (t *renameTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"sessionID": {"type": "string", "description": "Omit to rename the current session."},
			"title": {"type": "string", "description": "New session title."}
		},
		"required": ["title"]
	}`)
}

func (t *renameTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		SessionID string `json:"sessionID"`
		Title     string `json:"title"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_session_rename: %w", err)
	}
	sessionID := strings.TrimSpace(args.SessionID)
	if sessionID == "" {
		sessionID = call.SessionID
	}
	title := strings.TrimSpace(args.Title)
	if title == "" {
		return "", fmt.Errorf("lingzhu_session_rename: title is empty")
	}
	if err := call.Allow(ctx, "lingzhu_session_rename", []string{sessionID}, ""); err != nil {
		return "", err
	}
	if t.store == nil {
		return "", fmt.Errorf("lingzhu_session_rename: session store is not available")
	}
	if err := t.store.SetTitle(sessionID, title); err != nil {
		return "", fmt.Errorf("lingzhu_session_rename: %w", err)
	}
	return fmt.Sprintf("Renamed session %s to %s.", sessionID, title), nil
}

type worktreeCreateTool struct {
	manager *worktree.Manager
}

func (t *worktreeCreateTool) Name() string { return "lingzhu_worktree_create" }

func (t *worktreeCreateTool) Description() string {
	return "Create a named worktree. Returns its directory."
}

func (t *worktreeCreateTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Worktree name."}
		},
		"required": ["name"]
	}`)
}

func (t *worktreeCreateTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_worktree_create: %w", err)
	}
	if err := call.Allow(ctx, "lingzhu_worktree_create", []string{args.Name}, ""); err != nil {
		return "", err
	}
	if t.manager == nil {
		return "", fmt.Errorf("lingzhu_worktree_create: worktrees require a git repository")
	}
	entry, err := t.manager.Create(ctx, args.Name, "", "")
	if err != nil {
		return "", fmt.Errorf("lingzhu_worktree_create: %w", err)
	}
	return fmt.Sprintf("Created worktree in %s.", entry.Path), nil
}

type worktreeListTool struct {
	manager *worktree.Manager
}

func (t *worktreeListTool) Name() string { return "lingzhu_worktree_list" }

func (t *worktreeListTool) Description() string {
	return "List this repository's git worktrees, including the main checkout."
}

func (t *worktreeListTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *worktreeListTool) Run(ctx context.Context, call tool.Call) (string, error) {
	if err := call.Allow(ctx, "lingzhu_worktree_list", []string{"*"}, ""); err != nil {
		return "", err
	}
	if t.manager == nil {
		return "", fmt.Errorf("lingzhu_worktree_list: worktrees require a git repository")
	}
	entries, err := t.manager.List(ctx)
	if err != nil {
		return "", fmt.Errorf("lingzhu_worktree_list: %w", err)
	}
	if len(entries) == 0 {
		return "No worktrees found.", nil
	}
	var b strings.Builder
	for _, entry := range entries {
		b.WriteString(entry.Path)
		if entry.Branch != "" {
			b.WriteString(" (" + entry.Branch + ")")
		}
		if entry.Main {
			b.WriteString(" [main]")
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

type worktreeRemoveTool struct {
	manager *worktree.Manager
}

func (t *worktreeRemoveTool) Name() string { return "lingzhu_worktree_remove" }

func (t *worktreeRemoveTool) Description() string {
	return "Remove a git worktree by absolute directory. Set force only when the user confirmed discarding modified or untracked files."
}

func (t *worktreeRemoveTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"directory": {"type": "string", "description": "Absolute path of the worktree to remove."},
			"force": {"type": "boolean", "description": "Discard modified and untracked files."}
		},
		"required": ["directory"]
	}`)
}

func (t *worktreeRemoveTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		Directory string `json:"directory"`
		Force     bool   `json:"force"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_worktree_remove: %w", err)
	}
	if err := call.Allow(ctx, "lingzhu_worktree_remove", []string{args.Directory}, ""); err != nil {
		return "", err
	}
	if t.manager == nil {
		return "", fmt.Errorf("lingzhu_worktree_remove: worktrees require a git repository")
	}
	if err := t.manager.Remove(ctx, args.Directory, args.Force); err != nil {
		return "", fmt.Errorf("lingzhu_worktree_remove: %w", err)
	}
	return fmt.Sprintf("Removed worktree: %s", args.Directory), nil
}

type modelsTool struct {
	catalog *provider.Catalog
}

func (t *modelsTool) Name() string { return "lingzhu_models" }

func (t *modelsTool) Description() string {
	return "Search the models available to use. Use this to turn a model name the user mentions into an exact reference for a tool that takes a model. Check your own provider first."
}

func (t *modelsTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Text to search for in model names and IDs."},
			"provider": {"type": "string", "description": "Provider id or name to filter by. Try your own provider first."},
			"type": {"type": "string", "enum": ["chat", "image"], "description": "Which kind of model to list. Defaults to chat."},
			"all": {"type": "boolean", "description": "Include older versions of each model family. By default only the newest version is listed."},
			"limit": {"type": "integer", "description": "Maximum models to return. Defaults to 20, maximum 100."},
			"offset": {"type": "integer", "description": "Models to skip for paging."}
		}
	}`)
}

func (t *modelsTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		Query    string `json:"query"`
		Provider string `json:"provider"`
		Type     string `json:"type"`
		All      bool   `json:"all"`
		Limit    int    `json:"limit"`
		Offset   int    `json:"offset"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_models: %w", err)
	}
	if args.Limit <= 0 {
		args.Limit = 20
	}
	if args.Limit > 100 {
		args.Limit = 100
	}
	if args.Offset < 0 {
		args.Offset = 0
	}
	modelType := strings.ToLower(strings.TrimSpace(args.Type))
	if modelType == "" {
		modelType = "chat"
	}
	if modelType != "chat" && modelType != "image" {
		return "", fmt.Errorf("lingzhu_models: type must be chat or image")
	}
	if err := call.Allow(ctx, "lingzhu_models", []string{strings.TrimSpace(args.Query)}, ""); err != nil {
		return "", err
	}
	if t.catalog == nil {
		return "", fmt.Errorf("lingzhu_models: model catalog is not available")
	}
	terms := strings.Fields(strings.ToLower(strings.TrimSpace(args.Query)))
	providerFilter := strings.ToLower(strings.TrimSpace(args.Provider))

	type row struct {
		provider provider.ProviderInfo
		model    provider.ModelInfo
	}
	var rows []row
	for _, item := range t.catalog.Providers() {
		if providerFilter != "" &&
			strings.ToLower(item.ID) != providerFilter &&
			strings.ToLower(item.Name) != providerFilter {
			continue
		}
		for _, model := range item.Models {
			kind := strings.ToLower(model.Type)
			if kind == "" {
				kind = "chat"
			}
			if kind != modelType {
				continue
			}
			text := strings.ToLower(item.ID + "/" + model.ID + " " + model.Name)
			matched := true
			for _, term := range terms {
				if !strings.Contains(text, term) {
					matched = false
					break
				}
			}
			if !matched {
				continue
			}
			rows = append(rows, row{provider: item, model: model})
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if rows[i].provider.ID != rows[j].provider.ID {
			return rows[i].provider.ID < rows[j].provider.ID
		}
		if rows[i].model.Released != rows[j].model.Released {
			return rows[i].model.Released > rows[j].model.Released
		}
		return rows[i].model.ID < rows[j].model.ID
	})
	if !args.All {
		// Only the newest release of a family stays listed; models without a
		// family are always their own entry.
		seen := map[string]bool{}
		kept := rows[:0]
		for _, item := range rows {
			if item.model.Family != "" {
				key := item.provider.ID + "\x00" + item.model.Family
				if seen[key] {
					continue
				}
				seen[key] = true
			}
			kept = append(kept, item)
		}
		rows = kept
	}
	total := len(rows)
	if args.Offset >= total {
		return fmt.Sprintf("No %s models found (total %d).", modelType, total), nil
	}
	end := min(args.Offset+args.Limit, total)
	var b strings.Builder
	fmt.Fprintf(&b, "%s models:\n", strings.ToUpper(modelType[:1])+modelType[1:])
	lastProvider := ""
	for _, item := range rows[args.Offset:end] {
		if item.provider.ID != lastProvider {
			if lastProvider != "" {
				b.WriteString("\n")
			}
			fmt.Fprintf(&b, "%s (%s):\n", item.provider.Name, item.provider.ID)
			lastProvider = item.provider.ID
		}
		fmt.Fprintf(&b, "  - %s/%s (%s)", item.provider.ID, item.model.ID, item.model.Name)
		if item.model.ContextWindow > 0 {
			fmt.Fprintf(&b, " window=%d", item.model.ContextWindow)
		}
		if item.model.Family != "" {
			fmt.Fprintf(&b, " family=%s", item.model.Family)
		}
		if item.model.Released > 0 {
			fmt.Fprintf(&b, " released=%s", time.UnixMilli(item.model.Released).UTC().Format("2006-01-02"))
		}
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("Showing %d-%d of %d.", args.Offset+1, end, total))
	return b.String(), nil
}
