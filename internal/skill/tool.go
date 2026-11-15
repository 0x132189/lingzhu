package skill

import (
	"context"
	"encoding/json"
	"fmt"

	"lingzhu/internal/tool"
)

// Tool loads one discovered skill into the conversation.
type Tool struct {
	manager *Manager
}

// NewTool returns the skill tool backed by manager.
func NewTool(manager *Manager) *Tool {
	return &Tool{manager: manager}
}

func (t *Tool) Name() string { return "skill" }

func (t *Tool) Description() string {
	return "Load a specialized skill's instructions and resources into the current conversation when the task at hand matches its description.\n\n" +
		"The skill ID must match an available skill or a skill explicitly referenced by the user."
}

func (t *Tool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "The ID of an available skill or a skill explicitly referenced by the user"}
		},
		"required": ["id"]
	}`)
}

func (t *Tool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("skill: %w", err)
	}
	item, ok := t.manager.Get(args.ID)
	if !ok {
		return "", fmt.Errorf("unable to load skill %q: not found", args.ID)
	}
	if err := call.Allow(ctx, "skill", []string{item.ID}, ""); err != nil {
		return "", err
	}
	call.SetMetadata("name", item.ID)
	return item.Loaded(), nil
}
