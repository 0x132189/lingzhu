package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"lingzhu/internal/rpc"
	"os"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/tool"
)

// TestHelperMCPProcess is the fake stdio server used by the client tests. It is
// a test binary re-exec that speaks newline-delimited JSON-RPC.
func TestHelperMCPProcess(t *testing.T) {
	if os.Getenv("LINGZHU_MCP_HELPER") != "1" {
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
	notify := func(method string) {
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
		_, _ = writer.Write(data)
		_ = writer.WriteByte('\n')
		_ = writer.Flush()
	}
	notifyParams := func(method string, params map[string]any) {
		data, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
		_, _ = writer.Write(data)
		_ = writer.WriteByte('\n')
		_ = writer.Flush()
	}
	dynamic := false

	for scanner.Scan() {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		id := message.ID
		switch message.Method {
		case "initialize":
			reply(id, map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities": map[string]any{
					"tools":     map[string]any{},
					"resources": map[string]any{},
					"extensions": map[string]any{
						"io.modelcontextprotocol/events": map[string]any{},
					},
				},
				"serverInfo":   map[string]any{"name": "fake", "version": "1.0.0"},
				"instructions": "Use echo for text.",
			}, nil)
		case "notifications/initialized":
			// Notification, no response.
		case "tools/list":
			var params struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(message.Params, &params)
			if params.Cursor == "" {
				reply(id, map[string]any{
					"tools": []map[string]any{
						{
							"name":        "echo",
							"description": "Echo text",
							"inputSchema": map[string]any{
								"type":       "object",
								"properties": map[string]any{"text": map[string]any{"type": "string"}},
								"required":   []string{"text"},
							},
						},
						{
							"name":        "notify",
							"description": "Announce a catalog change",
							"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
						},
					},
					"nextCursor": "page-2",
				}, nil)
			} else {
				tools := []map[string]any{{
					"name":        "add",
					"description": "Add two numbers",
					"inputSchema": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"a": map[string]any{"type": "number"},
							"b": map[string]any{"type": "number"},
						},
					},
				}}
				if dynamic {
					tools = append(tools, map[string]any{
						"name":        "late",
						"description": "A tool added after startup",
						"inputSchema": map[string]any{"type": "object", "properties": map[string]any{}},
					})
				}
				reply(id, map[string]any{"tools": tools}, nil)
			}
		case "prompts/list":
			reply(id, map[string]any{
				"prompts": []map[string]any{{
					"name":        "review",
					"description": "Review code",
					"arguments":   []map[string]any{{"name": "path", "required": true}},
				}},
			}, nil)
		case "prompts/get":
			var params struct {
				Name      string            `json:"name"`
				Arguments map[string]string `json:"arguments"`
			}
			_ = json.Unmarshal(message.Params, &params)
			reply(id, map[string]any{
				"messages": []map[string]any{{
					"role":    "user",
					"content": map[string]any{"type": "text", "text": "Review " + params.Arguments["path"]},
				}},
			}, nil)
		case "tools/call":
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(message.Params, &params)
			switch params.Name {
			case "echo":
				text, _ := params.Arguments["text"].(string)
				reply(id, map[string]any{
					"content": []map[string]any{{"type": "text", "text": text}},
				}, nil)
			case "add":
				a, _ := params.Arguments["a"].(float64)
				b, _ := params.Arguments["b"].(float64)
				reply(id, map[string]any{
					"content":           []map[string]any{{"type": "text", "text": fmt.Sprintf("%v", a+b)}},
					"structuredContent": map[string]any{"sum": a + b},
				}, nil)
			case "notify":
				dynamic = true
				notify("notifications/tools/list_changed")
				reply(id, map[string]any{
					"content": []map[string]any{{"type": "text", "text": "notified"}},
				}, nil)
			case "crash":
				os.Exit(1)
			default:
				reply(id, nil, &rpc.Error{Code: -32602, Message: "unknown tool"})
			}
		case "resources/list":
			var params struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(message.Params, &params)
			if params.Cursor == "" {
				reply(id, map[string]any{
					"resources": []map[string]any{{
						"uri":         "memo://hello",
						"name":        "Hello",
						"description": "A greeting",
						"mimeType":    "text/plain",
					}},
					"nextCursor": "resources-2",
				}, nil)
			} else {
				reply(id, map[string]any{
					"resources": []map[string]any{{
						"uri":      "memo://second",
						"name":     "Second",
						"mimeType": "text/plain",
					}},
				}, nil)
			}
		case "resources/templates/list":
			reply(id, map[string]any{
				"resourceTemplates": []map[string]any{{
					"uriTemplate": "memo://{id}",
					"name":        "Memo",
				}},
			}, nil)
		case "resources/read":
			var params struct {
				URI string `json:"uri"`
			}
			_ = json.Unmarshal(message.Params, &params)
			if params.URI == "memo://binary" {
				reply(id, map[string]any{
					"contents": []map[string]any{{
						"uri":      params.URI,
						"mimeType": "application/octet-stream",
						"blob":     "AAECAw==",
					}},
				}, nil)
				continue
			}
			reply(id, map[string]any{
				"contents": []map[string]any{{
					"uri":      params.URI,
					"mimeType": "text/plain",
					"text":     "hello resource",
				}},
			}, nil)
		case "events/list":
			var params struct {
				Cursor string `json:"cursor"`
			}
			_ = json.Unmarshal(message.Params, &params)
			if params.Cursor == "" {
				reply(id, map[string]any{
					"events": []map[string]any{{
						"name":        "job.finished",
						"description": "A job ended",
						"delivery":    []string{"push"},
						"inputSchema": map[string]any{"type": "object"},
					}},
					"nextCursor": "events-2",
				}, nil)
				continue
			}
			reply(id, map[string]any{
				"events": []map[string]any{{"name": "job.failed", "delivery": []string{"push"}}},
			}, nil)
		case "events/stream":
			meta := map[string]any{"io.modelcontextprotocol/subscriptionId": json.RawMessage(id)}
			notifyParams("notifications/events/active", map[string]any{
				"cursor":    "c0",
				"truncated": true,
				"_meta":     meta,
			})
			notifyParams("notifications/events/event", map[string]any{
				"eventId":   "e1",
				"name":      "job.finished",
				"timestamp": "2026-11-10T00:00:00Z",
				"data":      map[string]any{"ok": true},
				"cursor":    "c1",
				"_meta":     meta,
			})
			notifyParams("notifications/events/heartbeat", map[string]any{
				"cursor": "c2",
				"_meta":  meta,
			})
			reply(id, map[string]any{}, nil)
		case "ping":
			reply(id, map[string]any{}, nil)
		default:
			if len(id) > 0 {
				reply(id, nil, &rpc.Error{Code: -32601, Message: "method not supported"})
			}
		}
	}
	os.Exit(0)
}

func helperServer(t *testing.T) Server {
	t.Helper()
	return Server{
		Type:        "local",
		Command:     []string{os.Args[0], "-test.run=TestHelperMCPProcess"},
		Environment: map[string]string{"LINGZHU_MCP_HELPER": "1"},
		Timeout:     Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
	}
}

func call(name, input string) tool.Call {
	return tool.Call{Name: name, Input: json.RawMessage(input)}
}

func TestManagerDiscoversToolsAndResources(t *testing.T) {
	manager := Start(context.Background(), Config{Servers: map[string]Server{"demo": helperServer(t)}}, t.TempDir(), t.Logf)
	defer manager.Close()

	names := map[string]bool{}
	for _, item := range manager.Tools() {
		names[item.Name()] = true
	}
	if len(names) != 3 || !names["demo_echo"] || !names["demo_add"] || !names["demo_notify"] {
		t.Fatalf("unexpected discovered tools: %v", names)
	}
	resources, ok := manager.Resources("demo")
	if !ok || len(resources) != 3 {
		t.Fatalf("unexpected resources: ok=%v items=%v", ok, resources)
	}
	if servers := manager.Servers(); len(servers) != 1 || servers[0] != "demo" {
		t.Fatalf("unexpected servers: %v", servers)
	}
	status := manager.Status()
	if len(status) != 1 || status[0].Title != "fake" {
		t.Fatalf("server title was not captured: %+v", status)
	}
	instructions := manager.Instructions()
	if len(instructions) != 1 || instructions[0].Server != "demo" || instructions[0].Instructions != "Use echo for text." {
		t.Fatalf("server instructions were not captured: %+v", instructions)
	}
}

func TestManagerListsAndStreamsEvents(t *testing.T) {
	manager := Start(context.Background(), Config{Servers: map[string]Server{"demo": helperServer(t)}}, t.TempDir(), t.Logf)
	defer manager.Close()

	if !manager.EventsSupported("demo") {
		t.Fatal("events capability was not detected")
	}
	events, err := manager.EventTypes(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[0].Name != "job.finished" || events[1].Name != "job.failed" {
		t.Fatalf("unexpected event catalog: %+v", events)
	}
	if len(events[0].Delivery) != 1 || events[0].Delivery[0] != "push" {
		t.Fatalf("delivery was not kept: %+v", events[0])
	}

	cursor := "c0"
	stream, stop, err := manager.StreamEvents(context.Background(), "demo", "job.finished", map[string]any{"job": "j1"}, &cursor, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	var messages []EventMessage
	deadline := time.After(3 * time.Second)
	for {
		select {
		case message, ok := <-stream:
			if !ok {
				if len(messages) != 3 {
					t.Fatalf("unexpected event messages: %+v", messages)
				}
				if messages[0].Type != "active" || !messages[0].Truncated {
					t.Fatalf("unexpected active message: %+v", messages[0])
				}
				if messages[1].Type != "event" || messages[1].EventID != "e1" || messages[1].Name != "job.finished" {
					t.Fatalf("unexpected event message: %+v", messages[1])
				}
				if messages[1].Cursor == nil || *messages[1].Cursor != "c1" {
					t.Fatalf("event cursor missing: %+v", messages[1])
				}
				if messages[2].Type != "heartbeat" {
					t.Fatalf("unexpected heartbeat: %+v", messages[2])
				}
				return
			}
			messages = append(messages, message)
		case <-deadline:
			t.Fatalf("event stream did not finish: %+v", messages)
		}
	}
}

func TestEventToolsListCatalog(t *testing.T) {
	manager := Start(context.Background(), Config{Servers: map[string]Server{"demo": helperServer(t)}}, t.TempDir(), t.Logf)
	defer manager.Close()

	var toolItem tool.Tool
	for _, item := range EventTools(manager) {
		if item.Name() == "lingzhu_list_mcp_events" {
			toolItem = item
		}
	}
	if toolItem == nil {
		t.Fatal("event tool not registered")
	}
	out, err := toolItem.Run(context.Background(), tool.Call{Name: toolItem.Name(), Input: json.RawMessage(`{"server":"demo"}`)})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "job.finished") || !strings.Contains(out, "job.failed") {
		t.Fatalf("unexpected event list: %q", out)
	}
	if _, err := toolItem.Run(context.Background(), tool.Call{Name: toolItem.Name(), Input: json.RawMessage(`{"server":"missing"}`)}); err == nil ||
		!strings.Contains(err.Error(), "not connected") {
		t.Fatalf("want missing-server error, got %v", err)
	}
}

func TestManagerCallsTools(t *testing.T) {
	manager := Start(context.Background(), Config{Servers: map[string]Server{"demo": helperServer(t)}}, t.TempDir(), t.Logf)
	defer manager.Close()

	for _, item := range manager.Tools() {
		switch item.Name() {
		case "demo_echo":
			out, err := item.Run(context.Background(), call("demo_echo", `{"text":"hi"}`))
			if err != nil {
				t.Fatal(err)
			}
			if out != "hi" {
				t.Fatalf("unexpected echo output: %q", out)
			}
		case "demo_add":
			out, err := item.Run(context.Background(), call("demo_add", `{"a":1,"b":2}`))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, `"sum": 3`) {
				t.Fatalf("unexpected add output: %q", out)
			}
		}
	}
}

func TestManagerBackgroundStart(t *testing.T) {
	manager := StartBackground(context.Background(), Config{Servers: map[string]Server{"demo": helperServer(t)}}, t.TempDir(), t.Logf, nil)
	defer manager.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, item := range manager.Tools() {
			if item.Name() == "demo_echo" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("background manager did not register tools: %+v", manager.Status())
}

func TestManagerAwaitStartup(t *testing.T) {
	manager := Start(context.Background(), Config{Servers: map[string]Server{"demo": helperServer(t)}}, t.TempDir(), t.Logf)
	defer manager.Close()

	done := make(chan struct{})
	go func() {
		manager.AwaitStartup(context.Background(), time.Second)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("AwaitStartup did not return after startup settled")
	}
}

func TestManagerRefreshesToolsOnNotification(t *testing.T) {
	manager := Start(context.Background(), Config{Servers: map[string]Server{"demo": helperServer(t)}}, t.TempDir(), t.Logf)
	defer manager.Close()

	var notify tool.Tool
	for _, item := range manager.Tools() {
		if item.Name() == "demo_notify" {
			notify = item
		}
	}
	if notify == nil {
		t.Fatal("notify tool not discovered")
	}
	if _, err := notify.Run(context.Background(), call("demo_notify", `{}`)); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, item := range manager.Tools() {
			if item.Name() == "demo_late" {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("refreshed tool was not registered: %v", manager.Tools())
}

func TestManagerReportsFailedServer(t *testing.T) {
	manager := Start(context.Background(), Config{Servers: map[string]Server{
		"bad": {Type: "local", Command: []string{"lingzhu-command-that-does-not-exist"}},
	}}, t.TempDir(), t.Logf)
	defer manager.Close()

	status := manager.Status()
	if len(status) != 1 || status[0].Name != "bad" || status[0].Status != StatusFailed || status[0].Error == "" {
		t.Fatalf("unexpected failed status: %+v", status)
	}
}

func TestManagerReportsDisabledServer(t *testing.T) {
	manager := Start(context.Background(), Config{Servers: map[string]Server{
		"off": {Type: "local", Disabled: true},
	}}, t.TempDir(), t.Logf)
	defer manager.Close()
	status := manager.Status()
	if len(status) != 1 || status[0].Status != StatusDisabled {
		t.Fatalf("unexpected disabled status: %+v", status)
	}
}

func TestManagerReconnectsServer(t *testing.T) {
	manager := Start(context.Background(), Config{Servers: map[string]Server{"demo": helperServer(t)}}, t.TempDir(), t.Logf)
	defer manager.Close()

	status, err := manager.Reconnect(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != StatusConnected || len(status.Tools) == 0 {
		t.Fatalf("unexpected reconnect status: %+v", status)
	}
	if names := manager.Servers(); len(names) != 1 || names[0] != "demo" {
		t.Fatalf("unexpected connected servers: %v", names)
	}
}

func TestManagerDisconnectAndReconnect(t *testing.T) {
	manager := Start(context.Background(), Config{Servers: map[string]Server{"demo": helperServer(t)}}, t.TempDir(), t.Logf)
	defer manager.Close()

	status, err := manager.Disconnect("demo")
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != StatusDisabled || len(status.Tools) != 0 {
		t.Fatalf("unexpected disconnect status: %+v", status)
	}
	if names := manager.Servers(); len(names) != 0 {
		t.Fatalf("disconnected server still connected: %v", names)
	}
	status, err = manager.Reconnect(context.Background(), "demo")
	if err != nil {
		t.Fatal(err)
	}
	if status.Status != StatusConnected || len(status.Tools) == 0 {
		t.Fatalf("reconnect after disconnect failed: %+v", status)
	}
}

func TestResourceTools(t *testing.T) {
	manager := Start(context.Background(), Config{Servers: map[string]Server{"demo": helperServer(t)}}, t.TempDir(), t.Logf)
	defer manager.Close()

	for _, item := range ResourceTools(manager) {
		switch item.Name() {
		case "lingzhu_list_mcp_resources":
			out, err := item.Run(context.Background(), call("lingzhu_list_mcp_resources", `{"server":"demo"}`))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "memo://hello") || !strings.Contains(out, "template Memo (memo://{id})") {
				t.Fatalf("unexpected resource listing: %q", out)
			}
		case "lingzhu_read_mcp_resource":
			out, err := item.Run(context.Background(), call("lingzhu_read_mcp_resource", `{"server":"demo","uri":"memo://hello"}`))
			if err != nil {
				t.Fatal(err)
			}
			if out != "hello resource" {
				t.Fatalf("unexpected resource content: %q", out)
			}
			out, err = item.Run(context.Background(), call("lingzhu_read_mcp_resource", `{"server":"demo","uri":"memo://binary"}`))
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(out, "binary resource") {
				t.Fatalf("unexpected binary resource output: %q", out)
			}
		}
	}
}

func TestConnectRejectsMissingCommand(t *testing.T) {
	if _, err := Connect(context.Background(), "bad", Server{Type: "local"}, t.TempDir()); err == nil {
		t.Fatal("want missing command error")
	}
}

func TestRootsResultUsesWorkspace(t *testing.T) {
	root := t.TempDir()
	client := &Client{workdir: root}
	result := client.rootsResult()
	roots, ok := result["roots"].([]any)
	if !ok || len(roots) != 1 {
		t.Fatalf("unexpected roots: %+v", result)
	}
	item, _ := roots[0].(map[string]string)
	if !strings.HasPrefix(item["uri"], "file://") || item["name"] == "" {
		t.Fatalf("unexpected root entry: %+v", item)
	}
}

func TestToolName(t *testing.T) {
	if got := ToolName("my.server", "my tool"); got != "my_server_my_tool" {
		t.Fatalf("unexpected tool name: %q", got)
	}
}

func TestManagerDiscoversAndExpandsPrompts(t *testing.T) {
	manager := Start(context.Background(), Config{Servers: map[string]Server{"demo": helperServer(t)}}, t.TempDir(), t.Logf)
	defer manager.Close()

	prompts := manager.Prompts()
	if len(prompts) != 1 || prompts[0].Name != "review" || prompts[0].Server != "demo" {
		t.Fatalf("unexpected prompts: %+v", prompts)
	}
	if got := PromptCommandName("my-server", "Review Code"); got != "my-server:Review_Code" {
		t.Fatalf("unexpected command name: %q", got)
	}
	text, ok, err := manager.ExpandPrompt(context.Background(), "demo:review", "src/auth.ts")
	if err != nil || !ok {
		t.Fatalf("expand failed: ok=%v err=%v", ok, err)
	}
	if text != "Review src/auth.ts" {
		t.Fatalf("unexpected prompt text: %q", text)
	}
	text, ok, err = manager.ExpandPrompt(context.Background(), "demo:review", `"src/auth file.ts"`)
	if err != nil || !ok {
		t.Fatalf("quoted expand failed: ok=%v err=%v", ok, err)
	}
	if text != "Review src/auth file.ts" {
		t.Fatalf("quoted argument was split: %q", text)
	}
	text, ok, err = manager.ExpandPrompt(context.Background(), "demo:review", `C:\src\auth.ts`)
	if err != nil || !ok || text != `Review C:\src\auth.ts` {
		t.Fatalf("windows path argument was changed: ok=%v err=%v text=%q", ok, err, text)
	}
	if _, ok, err := manager.ExpandPrompt(context.Background(), "demo:missing", ""); ok || err != nil {
		t.Fatalf("unknown prompt should not match: ok=%v err=%v", ok, err)
	}
}
