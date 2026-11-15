package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"lingzhu/internal/tool"
)

type pluginTool struct {
	client      *Client
	plugin      string
	rawName     string
	name        string
	description string
	schema      json.RawMessage
}

func (t *pluginTool) Name() string { return t.name }

func (t *pluginTool) Description() string {
	if t.description != "" {
		return t.description
	}
	return fmt.Sprintf("Plugin tool %q from %q.", t.rawName, t.plugin)
}

func (t *pluginTool) Schema() json.RawMessage {
	if len(t.schema) == 0 || string(t.schema) == "null" {
		return json.RawMessage(`{"type":"object","properties":{}}`)
	}
	return t.schema
}

func (t *pluginTool) Run(ctx context.Context, call tool.Call) (string, error) {
	if err := call.Allow(ctx, t.name, []string{"*"}, ""); err != nil {
		return "", err
	}
	args := call.Input
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var probe map[string]any
	if err := json.Unmarshal(args, &probe); err != nil {
		return "", fmt.Errorf("plugin: %s: invalid arguments: %w", t.name, err)
	}
	result, err := t.client.CallTool(ctx, t.rawName, args)
	if err != nil {
		return "", fmt.Errorf("plugin: %s: %w", t.name, err)
	}
	text := result.Text()
	if result.IsError {
		if text == "" {
			text = fmt.Sprintf("plugin tool %q returned an error", t.rawName)
		}
		return "", errors.New(text)
	}
	return text, nil
}
