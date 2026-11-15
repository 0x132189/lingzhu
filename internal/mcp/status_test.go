package mcp

import (
	"testing"

	"lingzhu/internal/tool"
)

func TestStatusGroupsToolsAndResources(t *testing.T) {
	manager := &Manager{
		clients: map[string]*Client{"alpha": {}, "beta": {}},
		resources: map[string][]Resource{
			"alpha": {{Name: "notes", URI: "mem://notes", Description: "notes"}},
		},
		tools: []tool.Tool{
			&mcpTool{server: "alpha", rawName: "read", name: "alpha_read"},
			&mcpTool{server: "beta", rawName: "write", name: "beta_write"},
		},
	}
	status := manager.Status()
	if len(status) != 2 || status[0].Name != "alpha" || status[1].Name != "beta" {
		t.Fatalf("unexpected servers: %+v", status)
	}
	if len(status[0].Tools) != 1 || status[0].Tools[0].Name != "read" || status[0].Tools[0].Effective != "alpha_read" {
		t.Fatalf("unexpected alpha tools: %+v", status[0].Tools)
	}
	if len(status[0].Resources) != 1 || status[0].Resources[0].URI != "mem://notes" {
		t.Fatalf("unexpected alpha resources: %+v", status[0].Resources)
	}
	if len(status[1].Tools) != 1 || len(status[1].Resources) != 0 {
		t.Fatalf("unexpected beta status: %+v", status[1])
	}

	var nilManager *Manager
	if got := nilManager.Status(); got != nil {
		t.Fatalf("nil manager should return nil, got %+v", got)
	}
}
