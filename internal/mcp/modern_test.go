package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"lingzhu/internal/rpc"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type modernState struct {
	mu            sync.Mutex
	discover      int
	initialize    int
	toolsList     int
	toolsCall     int
	subscriptions int
	envelopeBad   int
}

func (s *modernState) snapshot() (int, int, int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.discover, s.initialize, s.toolsList, s.toolsCall, s.envelopeBad
}

func (s *modernState) subscriptionCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.subscriptions
}

func envelopeVersion(params json.RawMessage) string {
	var payload struct {
		Meta map[string]any `json:"_meta"`
	}
	if err := json.Unmarshal(params, &payload); err != nil {
		return ""
	}
	value, _ := payload.Meta[protocolMetaKey].(string)
	return value
}

func modernServer(t *testing.T, state *modernState, modern bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var message rpc.Message
		if err := json.Unmarshal(body, &message); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		reply := func(result any) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(rpcReply(message.ID, result))
		}
		switch message.Method {
		case "server/discover":
			state.mu.Lock()
			state.discover++
			state.mu.Unlock()
			if !modern {
				reply(map[string]any{"supportedVersions": []string{"2025-06-18"}})
				return
			}
			if version := envelopeVersion(message.Params); version != modernProtocolVersion {
				state.mu.Lock()
				state.envelopeBad++
				state.mu.Unlock()
			}
			reply(map[string]any{
				"supportedVersions": []string{modernProtocolVersion, "2025-11-25"},
				"capabilities":      map[string]any{"tools": map[string]any{}},
				"instructions":      "modern server",
			})
		case "initialize":
			state.mu.Lock()
			state.initialize++
			state.mu.Unlock()
			reply(map[string]any{
				"protocolVersion": "2024-11-05",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "legacy-server", "version": "1.0.0"},
			})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			state.mu.Lock()
			state.toolsList++
			state.mu.Unlock()
			if modern && envelopeVersion(message.Params) != modernProtocolVersion {
				state.mu.Lock()
				state.envelopeBad++
				state.mu.Unlock()
			}
			reply(map[string]any{"tools": []map[string]any{{
				"name":        "echo",
				"description": "Echo",
				"inputSchema": map[string]any{"type": "object"},
			}}})
		case "subscriptions/listen":
			state.mu.Lock()
			state.subscriptions++
			state.mu.Unlock()
			reply(map[string]any{})
		case "tools/call":
			state.mu.Lock()
			state.toolsCall++
			state.mu.Unlock()
			if modern && envelopeVersion(message.Params) != modernProtocolVersion {
				state.mu.Lock()
				state.envelopeBad++
				state.mu.Unlock()
			}
			var params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			}
			_ = json.Unmarshal(message.Params, &params)
			reply(map[string]any{"content": []map[string]any{{
				"type": "text",
				"text": fmt.Sprintf("%s:%v", params.Name, params.Arguments["text"]),
			}}})
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(rpcErrorReply(message.ID, -32601, "method not supported"))
		}
	}))
}

func TestModernProtocolNegotiation(t *testing.T) {
	state := &modernState{}
	server := modernServer(t, state, true)
	defer server.Close()

	manager := Start(context.Background(), Config{Servers: map[string]Server{
		"modern": {
			Type:     "remote",
			URL:      server.URL,
			Protocol: "2026-07-28",
			Timeout:  Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
		},
	}}, t.TempDir(), t.Logf)
	defer manager.Close()

	if got := manager.Servers(); len(got) != 1 || got[0] != "modern" {
		t.Fatalf("modern server did not connect: %v", got)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && state.subscriptionCount() == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if state.subscriptionCount() == 0 {
		t.Fatal("modern catalog subscription was not opened")
	}
	found := false
	for _, item := range manager.Tools() {
		if item.Name() == "modern_echo" {
			found = true
			out, err := item.Run(context.Background(), call("modern_echo", `{"text":"hello"}`))
			if err != nil {
				t.Fatal(err)
			}
			if out != "echo:hello" {
				t.Fatalf("unexpected tool output: %q", out)
			}
		}
	}
	if !found {
		t.Fatalf("modern tool missing: %v", manager.Tools())
	}
	discover, initialize, _, calls, bad := state.snapshot()
	if discover != 1 || initialize != 0 {
		t.Fatalf("unexpected handshake: discover=%d initialize=%d", discover, initialize)
	}
	if calls != 1 || bad != 0 {
		t.Fatalf("envelope or calls wrong: calls=%d bad=%d", calls, bad)
	}
}

func TestAutoFallsBackToLegacy(t *testing.T) {
	state := &modernState{}
	server := modernServer(t, state, false)
	defer server.Close()

	manager := Start(context.Background(), Config{Servers: map[string]Server{
		"legacy": {
			Type:     "remote",
			URL:      server.URL,
			Protocol: "auto",
			Timeout:  Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
		},
	}}, t.TempDir(), t.Logf)
	defer manager.Close()

	if got := manager.Servers(); len(got) != 1 || got[0] != "legacy" {
		t.Fatalf("legacy fallback failed: %v", got)
	}
	discover, initialize, _, _, _ := state.snapshot()
	if discover != 1 || initialize != 1 {
		t.Fatalf("unexpected fallback handshake: discover=%d initialize=%d", discover, initialize)
	}
}

func TestPinnedModernFailsWithoutOverlap(t *testing.T) {
	state := &modernState{}
	server := modernServer(t, state, false)
	defer server.Close()

	manager := Start(context.Background(), Config{Servers: map[string]Server{
		"pinned": {
			Type:     "remote",
			URL:      server.URL,
			Protocol: "2026-07-28",
			Timeout:  Timeout{Startup: 2000, Catalog: 2000, Execution: 2000},
		},
	}}, t.TempDir(), t.Logf)
	defer manager.Close()

	if got := manager.Servers(); len(got) != 0 {
		t.Fatalf("pinned negotiation should fail: %v", got)
	}
	discover, initialize, _, _, _ := state.snapshot()
	if discover != 1 || initialize != 0 {
		t.Fatalf("unexpected pinned handshake: discover=%d initialize=%d", discover, initialize)
	}
}

func TestPoolingSharesModernConnection(t *testing.T) {
	state := &modernState{}
	server := modernServer(t, state, true)
	defer server.Close()

	manager := Start(context.Background(), Config{Servers: map[string]Server{
		"alpha": {
			Type:     "remote",
			URL:      server.URL,
			Protocol: "auto",
			Pool:     true,
			Timeout:  Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
		},
		"beta": {
			Type:     "remote",
			URL:      server.URL,
			Protocol: "auto",
			Pool:     true,
			Timeout:  Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
		},
	}}, t.TempDir(), t.Logf)
	defer manager.Close()

	if got := manager.Servers(); len(got) != 2 {
		t.Fatalf("both pooled names should be present: %v", got)
	}
	discover, initialize, _, _, _ := state.snapshot()
	if discover != 1 || initialize != 0 {
		t.Fatalf("pooled connection should negotiate once: discover=%d initialize=%d", discover, initialize)
	}
	names := map[string]bool{}
	for _, item := range manager.Tools() {
		names[item.Name()] = true
	}
	if !names["alpha_echo"] || !names["beta_echo"] {
		t.Fatalf("pooled tools missing: %v", names)
	}
}

func TestPoolSharesOneConnectionAcrossManagers(t *testing.T) {
	state := &modernState{}
	server := modernServer(t, state, true)
	defer server.Close()
	config := Config{Servers: map[string]Server{
		"shared": {
			Type:     "remote",
			URL:      server.URL,
			Protocol: "2026-07-28",
			Pool:     true,
			Timeout:  Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
		},
	}}

	first := Start(context.Background(), config, t.TempDir(), t.Logf)
	if got := first.Servers(); len(got) != 1 || got[0] != "shared" {
		t.Fatalf("first manager did not connect: %v", got)
	}
	second := Start(context.Background(), config, t.TempDir(), t.Logf)

	discover, initialize, _, _, _ := state.snapshot()
	if discover != 1 || initialize != 0 {
		t.Fatalf("managers should share one handshake: discover=%d initialize=%d", discover, initialize)
	}
	for index, manager := range []*Manager{first, second} {
		found := false
		for _, item := range manager.Tools() {
			if item.Name() == "shared_echo" {
				found = true
				out, err := item.Run(context.Background(), call("shared_echo", `{"text":"ok"}`))
				if err != nil || out != "echo:ok" {
					t.Fatalf("manager %d tool failed: %q %v", index, out, err)
				}
			}
		}
		if !found {
			t.Fatalf("manager %d is missing the pooled tool", index)
		}
	}
	if client, ok := first.client("shared"); !ok {
		t.Fatal("first manager lost its client")
	} else if roots := client.rootsResult()["roots"].([]any); len(roots) != 0 {
		t.Fatalf("shared connections must not answer workspace roots: %+v", roots)
	}

	first.Close()
	if got := second.Servers(); len(got) != 1 {
		t.Fatalf("closing one manager broke the shared connection: %v", got)
	}
	second.Close()

	third := Start(context.Background(), config, t.TempDir(), t.Logf)
	defer third.Close()
	if got := third.Servers(); len(got) != 1 {
		t.Fatalf("third manager did not reconnect: %v", got)
	}
	discover, _, _, _, _ = state.snapshot()
	if discover != 2 {
		t.Fatalf("closing every manager should close the pooled connection: discover=%d", discover)
	}
}

func TestProtocolModeDefaults(t *testing.T) {
	cases := []struct {
		server Server
		want   string
	}{
		{Server{}, "legacy"},
		{Server{Pool: true}, "auto"},
		{Server{Protocol: "auto"}, "auto"},
		{Server{Protocol: "modern"}, "2026-07-28"},
		{Server{Protocol: "legacy", Pool: true}, "legacy"},
	}
	for _, tc := range cases {
		if got := tc.server.ProtocolMode(); got != tc.want {
			t.Fatalf("%+v: want %q, got %q", tc.server, tc.want, got)
		}
	}
	if (Server{Protocol: "legacy", Pool: true}).Pooled() {
		t.Fatal("legacy connections must not pool")
	}
	if !(Server{Pool: true}).Pooled() {
		t.Fatal("auto connections should pool when asked")
	}
}
