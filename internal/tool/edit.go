package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// Edit replaces one exact occurrence of old_string in a text file.
type Edit struct {
	Dir string
}

func (e *Edit) Name() string { return "edit" }

func (e *Edit) Description() string {
	return "Replace one exact occurrence of old_string in a text file. Fails when old_string is missing or appears more than once, so include enough surrounding context to make it unique."
}

func (e *Edit) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string"},
			"old_string": {"type": "string", "description": "Exact text to replace, unique in the file"},
			"new_string": {"type": "string", "description": "Replacement text"}
		},
		"required": ["path", "old_string", "new_string"]
	}`)
}

func (e *Edit) Run(ctx context.Context, call Call) (string, error) {
	var args struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	if args.OldString == "" {
		return "", fmt.Errorf("edit: old_string is empty")
	}
	path, err := call.Resolve(ctx, "edit", e.Dir, args.Path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	content := string(data)
	switch count := strings.Count(content, args.OldString); {
	case count == 0:
		return "", fmt.Errorf("edit: old_string not found in %s", path)
	case count > 1:
		return "", fmt.Errorf("edit: old_string appears %d times in %s; include more context to make it unique", count, path)
	}
	updated := strings.Replace(content, args.OldString, args.NewString, 1)
	if err := os.WriteFile(path, []byte(updated), info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	call.SetDiff(lineDiff(e.Dir, path, content, updated))
	return fmt.Sprintf("edited %s (%d -> %d bytes)", path, len(content), len(updated)), nil
}
