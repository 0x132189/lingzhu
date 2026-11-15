package server

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/plugin"
)

// TestHelperCommandPluginProcess is the fake plugin used by the command test.
func TestHelperCommandPluginProcess(t *testing.T) {
	if os.Getenv("LINGZHU_CMD_PLUGIN") != "1" {
		return
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	writer := bufio.NewWriter(os.Stdout)
	reply := func(id json.RawMessage, result any) {
		message := map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
		data, _ := json.Marshal(message)
		_, _ = writer.Write(append(data, '\n'))
		_ = writer.Flush()
	}
	for scanner.Scan() {
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			continue
		}
		switch message.Method {
		case "initialize":
			reply(message.ID, map[string]any{
				"name":         "command-plugin",
				"version":      "1.0.0",
				"capabilities": map[string]any{"tools": true, "commands": true},
			})
		case "tools/list":
			reply(message.ID, map[string]any{"tools": []any{}})
		case "commands/list":
			reply(message.ID, map[string]any{"commands": []map[string]any{{
				"name":        "review",
				"description": "Review code",
				"template":    "Review this code: $ARGUMENTS",
			}}})
		case "shutdown":
			reply(message.ID, map[string]any{})
			_ = writer.Flush()
			os.Exit(0)
		default:
			if len(message.ID) > 0 {
				failure := map[string]any{"jsonrpc": "2.0", "id": message.ID, "error": map[string]any{"code": -32601, "message": "unsupported"}}
				data, _ := json.Marshal(failure)
				_, _ = writer.Write(append(data, '\n'))
				_ = writer.Flush()
			}
		}
	}
	_ = writer.Flush()
	os.Exit(0)
}

func TestCommandEndpoint(t *testing.T) {
	manager := plugin.Start(context.Background(), plugin.Config{Plugins: map[string]plugin.Server{
		"cmd": {
			Command:     []string{os.Args[0], "-test.run=TestHelperCommandPluginProcess"},
			Environment: map[string]string{"LINGZHU_CMD_PLUGIN": "1"},
			Timeout:     plugin.Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
		},
	}}, t.TempDir(), t.Logf)
	t.Cleanup(manager.Close)

	server := newTestServer(t)
	server.SetPlugins(manager)
	if _, err := server.store.Append("s1", event.User("before")); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/command", strings.NewReader(`{"name":"review","arguments":"this change"}`)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected command status: %d %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		messages, err := server.store.Messages("s1")
		if err != nil {
			t.Fatal(err)
		}
		foundUser := false
		foundAssistant := false
		for _, message := range messages {
			if message.Role == "user" && message.Content == "Review this code: this change" {
				foundUser = true
			}
			if message.Role == "assistant" && strings.Contains(message.Content, "hi") {
				foundAssistant = true
			}
		}
		if foundUser && foundAssistant {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("command prompt missing: %+v", messages)
		}
		time.Sleep(20 * time.Millisecond)
	}

	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"name":"missing"}`, http.StatusNotFound},
		{`{"name":""}`, http.StatusBadRequest},
		{`{"name":"review","arguments":"x","agent":"nope"}`, http.StatusBadRequest},
	} {
		rec = httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/command", strings.NewReader(tc.body)))
		if rec.Code != tc.want {
			t.Fatalf("%s: want %d, got %d %s", tc.body, tc.want, rec.Code, rec.Body.String())
		}
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/missing/command", strings.NewReader(`{"name":"review"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing session should 404: %d", rec.Code)
	}
}
