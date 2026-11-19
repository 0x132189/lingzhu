package mcp

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type fakeAttachedTransport struct {
	label     string
	onMessage func([]byte)
}

func (f *fakeAttachedTransport) Start(onMessage func([]byte), _ func()) error {
	f.onMessage = onMessage
	return nil
}

func (f *fakeAttachedTransport) Close() error { return nil }

func (f *fakeAttachedTransport) Send(_ context.Context, data []byte) error {
	var message struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
	}
	if err := json.Unmarshal(data, &message); err != nil {
		return err
	}
	reply := func(result any) {
		if f.onMessage == nil {
			return
		}
		frame, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
		f.onMessage(frame)
	}
	switch message.Method {
	case "initialize":
		reply(map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}, "resources": map[string]any{}},
			"serverInfo":      map[string]any{"name": f.label},
		})
	case "notifications/initialized":
	case "tools/list":
		reply(map[string]any{"tools": []map[string]any{{
			"name":        "echo",
			"description": "Echo from " + f.label,
			"inputSchema": map[string]any{"type": "object"},
		}}})
	case "tools/call":
		reply(map[string]any{"content": []map[string]any{{"type": "text", "text": f.label}}})
	case "resources/list", "resources/templates/list", "prompts/list":
		reply(map[string]any{})
	}
	return nil
}

func attachedToolOutput(t *testing.T, manager *Manager) string {
	t.Helper()
	for _, item := range manager.Tools() {
		if item.Name() == "host_echo" {
			out, err := item.Run(context.Background(), call("host_echo", `{}`))
			if err != nil {
				t.Fatal(err)
			}
			return out
		}
	}
	return ""
}

func TestAttachStacksTransports(t *testing.T) {
	manager := Start(context.Background(), Config{}, t.TempDir(), t.Logf)
	defer manager.Close()

	ctx := context.Background()
	first := &fakeAttachedTransport{label: "first"}
	detachFirst, err := manager.Attach(ctx, "host", first)
	if err != nil {
		t.Fatal(err)
	}
	if names := manager.Servers(); len(names) != 0 {
		t.Fatalf("attached server must stay out of the configured list: %v", names)
	}
	if out := attachedToolOutput(t, manager); out != "first" {
		t.Fatalf("unexpected first tool output: %q", out)
	}

	second := &fakeAttachedTransport{label: "second"}
	detachSecond, err := manager.Attach(ctx, "host", second)
	if err != nil {
		t.Fatal(err)
	}
	if out := attachedToolOutput(t, manager); out != "second" {
		t.Fatalf("top attached transport should win: %q", out)
	}
	detachSecond()
	if out := attachedToolOutput(t, manager); out != "first" {
		t.Fatalf("previous transport should be restored: %q", out)
	}
	detachFirst()
	if out := attachedToolOutput(t, manager); out != "" {
		t.Fatalf("tool should be removed after detach: %q", out)
	}
}

func TestAttachRejectsEmptyName(t *testing.T) {
	manager := Start(context.Background(), Config{}, t.TempDir(), t.Logf)
	defer manager.Close()
	if _, err := manager.Attach(context.Background(), " ", &fakeAttachedTransport{}); err == nil ||
		!strings.Contains(err.Error(), "name") {
		t.Fatalf("want empty name error, got %v", err)
	}
}
