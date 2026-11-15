package imagegen

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lingzhu/internal/tool"
)

// Tool exposes experimental image generation to the model.
func Tool(manager *Manager, root string) tool.Tool {
	return &generateTool{manager: manager, root: root}
}

type generateTool struct {
	manager *Manager
	root    string
}

func (t *generateTool) Name() string { return "experimental_image_generate" }

func (t *generateTool) Description() string {
	return "Generates a new image from a prompt. Only call this when the user asks you to create an image; if an image would help but the user did not ask, ask first. Each call makes one image. Save to the default path unless the user names a file."
}

func (t *generateTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"prompt": {"type": "string", "description": "What to create. Be specific about subject, style, composition and any text that should appear."},
			"path": {"type": "string", "description": "Where to save the image, relative to the working directory or absolute. The extension is replaced with the real format. Defaults to images/image-<timestamp>.<ext>."},
			"size": {"type": "string", "description": "Optional image size accepted by the endpoint, such as 1024x1024."}
		},
		"required": ["prompt"]
	}`)
}

func (t *generateTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		Prompt string `json:"prompt"`
		Path   string `json:"path"`
		Size   string `json:"size"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("experimental_image_generate: %w", err)
	}
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		return "", fmt.Errorf("experimental_image_generate: prompt is empty")
	}
	if err := call.Allow(ctx, "experimental_image_generate", []string{prompt}, ""); err != nil {
		return "", err
	}
	result, err := t.manager.Generate(ctx, prompt, args.Size)
	if err != nil {
		return "", fmt.Errorf("experimental_image_generate: %w", err)
	}

	path := strings.TrimSpace(args.Path)
	if path == "" {
		path = filepath.Join("images", fmt.Sprintf("image-%d%s", time.Now().UnixNano(), result.Extension))
	} else {
		path = strings.TrimSuffix(path, filepath.Ext(path)) + result.Extension
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(t.root, path)
	}
	path = filepath.Clean(path)
	root, err := filepath.Abs(t.root)
	if err != nil {
		return "", fmt.Errorf("experimental_image_generate: %w", err)
	}
	rel, err := filepath.Rel(root, path)
	if err == nil && (rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))) {
		if err := call.Allow(ctx, "external_directory", []string{path}, ""); err != nil {
			return "", err
		}
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("experimental_image_generate: %w", err)
	}
	if err := os.WriteFile(path, result.Data, 0o644); err != nil {
		return "", fmt.Errorf("experimental_image_generate: %w", err)
	}
	display := path
	if err == nil {
		display = filepath.ToSlash(rel)
	}
	message := fmt.Sprintf("Generated image: %s (%s, %d bytes)", display, result.MediaType, len(result.Data))
	if result.RevisedPrompt != "" {
		message += "\nRevised prompt: " + result.RevisedPrompt
	}
	return message, nil
}
