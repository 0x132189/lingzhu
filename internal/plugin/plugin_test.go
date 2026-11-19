package plugin

import (
	"bufio"
	"context"
	"encoding/json"
	"lingzhu/internal/rpc"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/agent"
	"lingzhu/internal/model"
	"lingzhu/internal/permission"
	"lingzhu/internal/tool"
)

// TestHelperPluginProcess is the fake plugin used by the tests. It is a test
// binary re-exec that speaks newline-delimited JSON-RPC.
func TestHelperPluginProcess(t *testing.T) {
	if os.Getenv("LINGZHU_PLUGIN_HELPER") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	writer := bufio.NewWriter(os.Stdout)
	defer writer.Flush()

	reply := func(id json.RawMessage, result any, rpcErr *rpc.Error) {
		message := map[string]any{"jsonrpc": "2.0", "id": id}
		if rpcErr != nil {
			message["error"] = rpcErr
		} else {
			message["result"] = result
		}
		data, _ := json.Marshal(message)
		_, _ = writer.Write(data)
		_ = writer.WriteByte('\n')
		_ = writer.Flush()
	}

	for scanner.Scan() {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		switch message.Method {
		case "initialize":
			reply(message.ID, map[string]any{
				"name":    "fake-plugin",
				"version": "1.0.0",
				"capabilities": map[string]any{
					"tools": true, "commands": true, "permissions": true, "hooks": true,
				},
			}, nil)
		case "tools/list":
			reply(message.ID, map[string]any{"tools": []map[string]any{{
				"name":        "echo",
				"description": "Echo text",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
					"required":   []string{"text"},
				},
			}}}, nil)
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(message.Params, &params)
			if params.Name != "echo" {
				reply(message.ID, nil, &rpc.Error{Code: -32602, Message: "unknown tool"})
				continue
			}
			text, _ := params.Arguments["text"].(string)
			reply(message.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "echo:" + text}},
			}, nil)
		case "commands/list":
			reply(message.ID, map[string]any{"commands": []map[string]any{{
				"name":        "greet",
				"description": "Greet someone",
				"template":    "COMMAND_MARKER $ARGUMENTS",
			}}}, nil)
		case "hooks/tool.execute.before", "hooks/tool.execute.after", "hooks/session.created", "hooks/session.idle":
			if logPath := os.Getenv("LINGZHU_PLUGIN_HOOK_LOG"); logPath != "" {
				file, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
				if err == nil {
					_, _ = file.WriteString(message.Method + "\n")
					_ = file.Close()
				}
			}
			if message.Method == "hooks/tool.execute.before" {
				var params struct {
					Tool      string         `json:"tool"`
					Arguments map[string]any `json:"arguments"`
				}
				_ = json.Unmarshal(message.Params, &params)
				decision := HookDecision{}
				if params.Tool == "shell" {
					decision.Blocked = true
					decision.Reason = "blocked by helper"
				} else if params.Tool == "write" && params.Arguments["text"] == "hi" {
					params.Arguments["text"] = "hooked"
					data, _ := json.Marshal(params.Arguments)
					decision.Arguments = data
				}
				reply(message.ID, decision, nil)
				continue
			}
			if len(message.ID) > 0 {
				reply(message.ID, map[string]any{}, nil)
			}
		case "hooks/session.context":
			var event contextEvent
			_ = json.Unmarshal(message.Params, &event)
			for i := range event.Messages {
				if event.Messages[i].Content == "context in" {
					event.Messages[i].Content = "context out"
				}
			}
			event.Options = map[string]any{"temperature": 0.2}
			reply(message.ID, event, nil)
		case "hooks/permission.evaluate", "permissions/evaluate":
			var params struct {
				Action    string   `json:"action"`
				Resources []string `json:"resources"`
			}
			_ = json.Unmarshal(message.Params, &params)
			effect := "defer"
			switch {
			case params.Action == "shell":
				effect = "deny"
			case params.Action == "read" && strings.Contains(strings.Join(params.Resources, " "), "secret"):
				effect = "ask"
			case params.Action == "glob":
				effect = "allow"
			}
			payload := map[string]any{"effect": effect}
			if effect == "deny" {
				payload["message"] = "blocked by helper policy"
			}
			reply(message.ID, payload, nil)
		default:
			if len(message.ID) > 0 {
				reply(message.ID, nil, &rpc.Error{Code: -32601, Message: "method not supported"})
			}
		}
	}
	os.Exit(0)
}

func helperPlugin(t *testing.T) Server {
	t.Helper()
	return Server{
		Command:     []string{os.Args[0], "-test.run=TestHelperPluginProcess"},
		Environment: map[string]string{"LINGZHU_PLUGIN_HELPER": "1"},
		Timeout:     Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
	}
}

func startHelper(t *testing.T) *Manager {
	t.Helper()
	manager := Start(context.Background(), Config{Plugins: map[string]Server{"demo": helperPlugin(t)}}, t.TempDir(), t.Logf)
	t.Cleanup(manager.Close)
	return manager
}

func TestPluginDiscoversToolsAndCommands(t *testing.T) {
	manager := startHelper(t)

	tools := manager.Tools()
	if len(tools) != 1 || tools[0].Name() != "demo_echo" {
		t.Fatalf("unexpected tools: %v", toolNames(tools))
	}
	commands := manager.Commands()
	if len(commands) != 1 || commands[0].Name != "greet" || commands[0].Plugin != "demo" {
		t.Fatalf("unexpected commands: %+v", commands)
	}

	expanded, ok := manager.Expand("/greet world")
	if !ok || expanded != "COMMAND_MARKER world" {
		t.Fatalf("unexpected command expansion: %q ok=%v", expanded, ok)
	}
	if expanded, ok := manager.Expand("/missing world"); ok || expanded != "/missing world" {
		t.Fatalf("unknown command should stay unchanged: %q ok=%v", expanded, ok)
	}
	if expanded, ok := manager.Expand("plain text"); ok || expanded != "plain text" {
		t.Fatalf("plain text should stay unchanged: %q ok=%v", expanded, ok)
	}
}

func TestPluginToolRuns(t *testing.T) {
	manager := startHelper(t)
	allow := permission.New(permission.Rules{{Action: "*", Resource: "*", Effect: permission.Allow}})

	for _, item := range manager.Tools() {
		if item.Name() != "demo_echo" {
			continue
		}
		out, err := item.Run(context.Background(), tool.Call{
			Name:  "demo_echo",
			Input: json.RawMessage(`{"text":"hi"}`),
			Perm:  allow,
		})
		if err != nil {
			t.Fatal(err)
		}
		if out != "echo:hi" {
			t.Fatalf("unexpected output: %q", out)
		}
	}
}

func TestPluginPermissionDecisions(t *testing.T) {
	manager := startHelper(t)
	if got, message := manager.Evaluate(context.Background(), "shell", []string{"echo hi"}); got != permission.Deny || message != "blocked by helper policy" {
		t.Fatalf("want deny with message, got %q %q", got, message)
	}
	if got, _ := manager.Evaluate(context.Background(), "read", []string{"secret.env"}); got != permission.Ask {
		t.Fatalf("want ask, got %q", got)
	}
	if got, _ := manager.Evaluate(context.Background(), "glob", []string{"**/*"}); got != permission.Allow {
		t.Fatalf("want allow, got %q", got)
	}
	if got, _ := manager.Evaluate(context.Background(), "webfetch", []string{"https://example.com"}); got != "" {
		t.Fatalf("want defer, got %q", got)
	}
}

func TestPluginToolName(t *testing.T) {
	if got := ToolName("my.plugin", "my tool"); got != "my_plugin_my_tool" {
		t.Fatalf("unexpected tool name: %q", got)
	}
	if got := ToolName("my-plugin", "run-task"); got != "my_plugin_run_task" {
		t.Fatalf("hyphens should become underscores: %q", got)
	}
}

func TestPluginConfigForms(t *testing.T) {
	var object Config
	if err := json.Unmarshal([]byte(`{"demo":{"command":["x"],"enabled":false}}`), &object); err != nil {
		t.Fatal(err)
	}
	if len(object.Plugins) != 1 || object.Plugins["demo"].on() {
		t.Fatalf("unexpected object config: %+v", object)
	}

	var list Config
	if err := json.Unmarshal([]byte(`[{"name":"demo","command":["x"]},{"name":"off","command":["y"],"disabled":true}]`), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Plugins) != 2 || list.Plugins["off"].on() {
		t.Fatalf("unexpected array config: %+v", list)
	}

	if err := json.Unmarshal([]byte(`[{"command":["x"]}]`), &list); err == nil {
		t.Fatal("want missing name error")
	}
}

func TestPluginSkipsDisabled(t *testing.T) {
	server := helperPlugin(t)
	server.Disabled = true
	manager := Start(context.Background(), Config{Plugins: map[string]Server{"demo": server}}, t.TempDir(), t.Logf)
	defer manager.Close()
	if len(manager.Tools()) != 0 {
		t.Fatalf("disabled plugin should not load: %v", toolNames(manager.Tools()))
	}
}

func toolNames(items []tool.Tool) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.Name())
	}
	return out
}

func TestPluginToolHooks(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "hooks.log")
	t.Setenv("LINGZHU_PLUGIN_HOOK_LOG", logPath)
	manager := startHelper(t)

	arguments, reason, err := manager.BeforeTool(context.Background(), "s1", "write", "call_1", json.RawMessage(`{"text":"hi"}`))
	if err != nil || reason != "" {
		t.Fatalf("unexpected before result: %q %q %v", arguments, reason, err)
	}
	var updated map[string]string
	if err := json.Unmarshal(arguments, &updated); err != nil || updated["text"] != "hooked" {
		t.Fatalf("hook did not replace arguments: %s %v", arguments, err)
	}
	if _, reason, err := manager.BeforeTool(context.Background(), "s1", "shell", "call_2", json.RawMessage(`{}`)); err != nil || reason != "blocked by helper" {
		t.Fatalf("hook did not block shell: %q %v", reason, err)
	}

	manager.AfterTool(context.Background(), "s1", "write", "call_1", "ok", false)
	manager.SessionEvent(context.Background(), "session.created", "s1")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		data, _ := os.ReadFile(logPath)
		text := string(data)
		if strings.Contains(text, "hooks/tool.execute.after") && strings.Contains(text, "hooks/session.created") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	data, _ := os.ReadFile(logPath)
	t.Fatalf("hook notifications missing: %s", data)
}

func TestPluginSessionContext(t *testing.T) {
	manager := startHelper(t)
	updated, err := manager.ApplyContext(context.Background(), agent.ContextEvent{
		SessionID: "s1",
		Agent:     "build",
		Model:     "work/fast",
		Messages: []model.Message{
			{Role: model.RoleSystem, Content: "base"},
			{Role: model.RoleUser, Content: "context in"},
		},
		Options: map[string]any{"temperature": 0.7},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Messages) != 2 || updated.Messages[1].Content != "context out" {
		t.Fatalf("context messages not rewritten: %+v", updated.Messages)
	}
	if updated.Messages[0].Content != "base" {
		t.Fatalf("system message lost: %+v", updated.Messages)
	}
	if value, ok := updated.Options["temperature"].(float64); !ok || value != 0.2 {
		t.Fatalf("options not rewritten: %+v", updated.Options)
	}
}
