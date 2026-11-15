package plugin

import (
	"context"
	"testing"

	"lingzhu/internal/tool"
)

func TestStatusGroupsToolsAndCommands(t *testing.T) {
	manager := &Manager{
		clients: map[string]*Client{
			"alpha": {},
			"beta":  {},
		},
		tools: []tool.Tool{
			&pluginTool{plugin: "alpha", rawName: "read", name: "alpha_read"},
			&pluginTool{plugin: "beta", rawName: "write", name: "beta_write"},
		},
		commands: []Command{
			{Name: "review", Plugin: "alpha"},
			{Name: "deploy", Plugin: "beta"},
		},
	}
	status := manager.Status()
	if len(status) != 2 || status[0].Name != "alpha" || status[1].Name != "beta" {
		t.Fatalf("unexpected plugins: %+v", status)
	}
	if len(status[0].Tools) != 1 || status[0].Tools[0].Name != "read" || status[0].Tools[0].Effective != "alpha_read" {
		t.Fatalf("unexpected alpha tools: %+v", status[0].Tools)
	}
	if len(status[0].Commands) != 1 || status[0].Commands[0] != "review" {
		t.Fatalf("unexpected alpha commands: %+v", status[0].Commands)
	}
	if len(status[1].Tools) != 1 || len(status[1].Commands) != 1 {
		t.Fatalf("unexpected beta status: %+v", status[1])
	}

	var nilManager *Manager
	if got := nilManager.Status(); got != nil {
		t.Fatalf("nil manager should return nil, got %+v", got)
	}
}

func TestCallToolRejectsUnknown(t *testing.T) {
	manager := &Manager{clients: map[string]*Client{}, tools: []tool.Tool{}}
	if _, err := manager.CallTool(context.Background(), "alpha", "read", map[string]any{}); err == nil {
		t.Fatal("unknown tool should return an error")
	}

	var nilManager *Manager
	if _, err := nilManager.CallTool(context.Background(), "alpha", "read", map[string]any{}); err == nil {
		t.Fatal("nil manager should return an error")
	}
}
