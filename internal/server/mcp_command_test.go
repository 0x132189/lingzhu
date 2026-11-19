package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"lingzhu/internal/mcp"
)

// TestMCPPromptRunsAsCommand verifies that a prompt listed by /commands can
// also be executed through the session command endpoint.
func TestMCPPromptRunsAsCommand(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.Error(w, "no stream", http.StatusMethodNotAllowed)
			return
		}
		var message struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(message.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch message.Method {
		case "initialize":
			result = map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}, "prompts": map[string]any{}},
				"serverInfo":      map[string]any{"name": "test", "version": "1"},
			}
		case "tools/list":
			result = map[string]any{"tools": []any{}}
		case "prompts/list":
			result = map[string]any{"prompts": []map[string]any{{
				"name":        "review",
				"description": "Review code",
				"arguments":   []map[string]any{{"name": "path"}},
			}}}
		case "prompts/get":
			result = map[string]any{"messages": []map[string]any{{
				"role":    "user",
				"content": map[string]any{"type": "text", "text": "Review src/auth.ts"},
			}}}
		case "resources/list":
			result = map[string]any{"resources": []any{}}
		case "resources/templates/list":
			result = map[string]any{"resourceTemplates": []any{}}
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      message.ID,
				"error":   map[string]any{"code": -32601, "message": "method not supported"},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": message.ID, "result": result})
	}))
	defer upstream.Close()

	manager := mcp.StartWithTokens(context.Background(), mcp.Config{Servers: map[string]mcp.Server{
		"demo": {Type: "remote", URL: upstream.URL, Timeout: mcp.Timeout{Startup: 5000, Catalog: 5000, Execution: 5000}},
	}}, t.TempDir(), t.Logf, nil)
	defer manager.Close()

	srv, store := newSessionServer(t, func(s *Server) { s.SetMCP(manager) })
	seedSession(t, store, "s1")
	body := `{"name":"demo:review","arguments":"src/auth.ts"}`
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/sessions/s1/command", strings.NewReader(body)))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected command status: %d %s", rec.Code, rec.Body.String())
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		messages, err := store.Messages("s1")
		if err == nil {
			for _, message := range messages {
				if strings.Contains(message.Content, "Review src/auth.ts") {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("MCP prompt text was not written to the session")
}
