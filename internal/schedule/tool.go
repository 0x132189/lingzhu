package schedule

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"lingzhu/internal/tool"
)

// Tools returns the lingzhu_schedule_* tools backed by manager.
func Tools(manager *Manager) []tool.Tool {
	return []tool.Tool{
		&createTool{manager: manager},
		&listTool{manager: manager},
		&pauseTool{manager: manager},
		&resumeTool{manager: manager},
		&deleteTool{manager: manager},
	}
}

func whenSchema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"type": {"type": "string", "enum": ["in", "at", "every"], "description": "Trigger kind."},
			"in": {"type": "string", "description": "Delay such as 90s, 5m, 2h or \"5 minutes\" when type is in."},
			"at": {"type": "string", "description": "RFC3339 time, 2006-01-02T15:04, or 15:04 when type is at."},
			"every": {"type": "string", "description": "Interval such as 1h or \"30 minutes\" when type is every."}
		},
		"required": ["type"]
	}`)
}

type createTool struct {
	manager *Manager
}

func (t *createTool) Name() string { return "lingzhu_schedule_create" }

func (t *createTool) Description() string {
	return "Schedule a prompt that wakes this session later: once after a delay, at a time, or every interval. Returns the schedule with its next run. Repeating schedules keep firing until paused or deleted."
}

func (t *createTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Optional short name for the schedule."},
			"prompt": {"type": "string", "description": "Prompt delivered to this session when the schedule fires."},
			"when": ` + string(whenSchema()) + `
		},
		"required": ["prompt", "when"]
	}`)
}

func (t *createTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		Name   string `json:"name"`
		Prompt string `json:"prompt"`
		When   When   `json:"when"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_schedule_create: %w", err)
	}
	if err := call.Allow(ctx, "lingzhu_schedule_create", []string{strings.TrimSpace(args.Name)}, ""); err != nil {
		return "", err
	}
	item, err := t.manager.Create(call.SessionID, CreateRequest{Name: args.Name, Prompt: args.Prompt, When: args.When})
	if err != nil {
		return "", fmt.Errorf("lingzhu_schedule_create: %w", err)
	}
	return "Scheduled: " + Summary(item), nil
}

type listTool struct {
	manager *Manager
}

func (t *listTool) Name() string { return "lingzhu_schedule_list" }

func (t *listTool) Description() string {
	return "List this session's schedules with their status and next run, and the current time."
}

func (t *listTool) Schema() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}

func (t *listTool) Run(ctx context.Context, call tool.Call) (string, error) {
	if err := call.Allow(ctx, "lingzhu_schedule_list", []string{"*"}, ""); err != nil {
		return "", err
	}
	now := t.manager.now()
	items := t.manager.List(call.SessionID)
	lines := []string{"Now: " + now.UTC().Format("2006-01-02T15:04:05Z07:00")}
	if len(items) == 0 {
		lines = append(lines, "No schedules.")
	}
	for _, item := range items {
		lines = append(lines, Summary(item))
	}
	return strings.Join(lines, "\n"), nil
}

type pauseTool struct {
	manager *Manager
}

func (t *pauseTool) Name() string { return "lingzhu_schedule_pause" }

func (t *pauseTool) Description() string {
	return "Pause a schedule. It keeps its settings and does not fire until resumed."
}

func (t *pauseTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {"id": {"type": "string", "description": "Schedule id from schedule_list."}},
		"required": ["id"]
	}`)
}

func (t *pauseTool) Run(ctx context.Context, call tool.Call) (string, error) {
	return runIDTool(ctx, call, t.manager, "lingzhu_schedule_pause", func(manager *Manager, id, sessionID string) (Item, error) {
		return manager.Pause(id, sessionID)
	}, "Paused: ")
}

type resumeTool struct {
	manager *Manager
}

func (t *resumeTool) Name() string { return "lingzhu_schedule_resume" }

func (t *resumeTool) Description() string {
	return "Resume a paused schedule. A one-shot that fell due while paused fires once now; a repeating one resumes at its next slot."
}

func (t *resumeTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {"id": {"type": "string", "description": "Schedule id from schedule_list."}},
		"required": ["id"]
	}`)
}

func (t *resumeTool) Run(ctx context.Context, call tool.Call) (string, error) {
	return runIDTool(ctx, call, t.manager, "lingzhu_schedule_resume", func(manager *Manager, id, sessionID string) (Item, error) {
		return manager.Resume(id, sessionID)
	}, "Resumed: ")
}

type deleteTool struct {
	manager *Manager
}

func (t *deleteTool) Name() string { return "lingzhu_schedule_delete" }

func (t *deleteTool) Description() string { return "Delete a schedule for good." }

func (t *deleteTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {"id": {"type": "string", "description": "Schedule id from schedule_list."}},
		"required": ["id"]
	}`)
}

func (t *deleteTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_schedule_delete: %w", err)
	}
	if err := call.Allow(ctx, "lingzhu_schedule_delete", []string{strings.TrimSpace(args.ID)}, ""); err != nil {
		return "", err
	}
	if err := t.manager.Delete(args.ID, call.SessionID); err != nil {
		return "", fmt.Errorf("lingzhu_schedule_delete: %w", err)
	}
	return "Deleted schedule " + args.ID + ".", nil
}

func runIDTool(ctx context.Context, call tool.Call, manager *Manager, name string, run func(*Manager, string, string) (Item, error), prefix string) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	if err := call.Allow(ctx, name, []string{strings.TrimSpace(args.ID)}, ""); err != nil {
		return "", err
	}
	item, err := run(manager, args.ID, call.SessionID)
	if err != nil {
		return "", fmt.Errorf("%s: %w", name, err)
	}
	return prefix + Summary(item), nil
}
