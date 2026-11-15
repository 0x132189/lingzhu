package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"lingzhu/internal/rpc"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func rpcReply(id json.RawMessage, result any) []byte {
	data, _ := json.Marshal(result)
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"result":%s}`, string(id), string(data)))
}

func rpcErrorReply(id json.RawMessage, code int, message string) []byte {
	data, _ := json.Marshal(message)
	return []byte(fmt.Sprintf(`{"jsonrpc":"2.0","id":%s,"error":{"code":%d,"message":%s}}`, string(id), code, string(data)))
}

func fakeResult(t *testing.T, message rpc.Message) (any, []byte) {
	t.Helper()
	switch message.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "remote-fake", "version": "1.0.0"},
		}, nil
	case "notifications/initialized":
		return nil, nil
	case "tools/list":
		return map[string]any{"tools": []map[string]any{
			{
				"name":        "echo",
				"description": "Echo text",
				"inputSchema": map[string]any{
					"type":       "object",
					"properties": map[string]any{"text": map[string]any{"type": "string"}},
				},
			},
			{
				"name":        "slow",
				"description": "Answer over a stream",
				"inputSchema": map[string]any{"type": "object"},
			},
		}}, nil
	case "tools/call":
		var params struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		_ = json.Unmarshal(message.Params, &params)
		switch params.Name {
		case "echo":
			text, _ := params.Arguments["text"].(string)
			return map[string]any{"content": []map[string]any{{"type": "text", "text": text}}}, nil
		case "slow":
			return map[string]any{"content": []map[string]any{{"type": "text", "text": "streamed"}}}, nil
		}
		return nil, rpcErrorReply(message.ID, -32602, "unknown tool")
	}
	return nil, rpcErrorReply(message.ID, -32601, "method not supported")
}

type remoteState struct {
	mu       sync.Mutex
	requests []string
	sessions []string
	protocol []string
	auth     []string
}

func (s *remoteState) record(message rpc.Message, header http.Header) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, message.Method)
	s.sessions = append(s.sessions, header.Get("Mcp-Session-Id"))
	s.protocol = append(s.protocol, header.Get("MCP-Protocol-Version"))
	s.auth = append(s.auth, header.Get("Authorization"))
}

func (s *remoteState) snapshot() ([]string, []string, []string, []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.requests...),
		append([]string(nil), s.sessions...),
		append([]string(nil), s.protocol...),
		append([]string(nil), s.auth...)
}

func TestRemoteStreamableHTTP(t *testing.T) {
	t.Setenv("MCP_TEST_TOKEN", "secret-token")
	state := &remoteState{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var message rpc.Message
		if err := json.Unmarshal(body, &message); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		state.record(message, r.Header)

		if message.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-42")
		}
		result, failure := fakeResult(t, message)
		if message.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if failure != nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(failure)
			return
		}
		if message.Method == "tools/call" && strings.Contains(string(message.Params), `"slow"`) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "event: message\ndata: {\"jsonrpc\":\"2.0\",\"method\":\"notifications/progress\",\"params\":{}}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", rpcReply(message.ID, result))
			if flusher != nil {
				flusher.Flush()
			}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(rpcReply(message.ID, result))
	}))
	defer server.Close()

	manager := Start(context.Background(), Config{Servers: map[string]Server{
		"remote": {
			Type:    "remote",
			URL:     server.URL,
			Headers: map[string]string{"Authorization": "Bearer {env:MCP_TEST_TOKEN}"},
			Timeout: Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
		},
	}}, t.TempDir(), t.Logf)
	defer manager.Close()

	if got := manager.Servers(); len(got) != 1 || got[0] != "remote" {
		t.Fatalf("unexpected servers: %v", got)
	}
	tools := map[string]bool{}
	for _, item := range manager.Tools() {
		tools[item.Name()] = true
	}
	if !tools["remote_echo"] || !tools["remote_slow"] {
		t.Fatalf("unexpected tools: %v", tools)
	}

	for _, item := range manager.Tools() {
		switch item.Name() {
		case "remote_echo":
			out, err := item.Run(context.Background(), call("remote_echo", `{"text":"hi"}`))
			if err != nil {
				t.Fatal(err)
			}
			if out != "hi" {
				t.Fatalf("unexpected echo output: %q", out)
			}
		case "remote_slow":
			out, err := item.Run(context.Background(), call("remote_slow", `{}`))
			if err != nil {
				t.Fatal(err)
			}
			if out != "streamed" {
				t.Fatalf("unexpected streamed output: %q", out)
			}
		}
	}

	methods, sessions, protocol, auth := state.snapshot()
	for i, method := range methods {
		if method == "initialize" {
			continue
		}
		if sessions[i] != "sess-42" {
			t.Fatalf("session id missing on %s: %q", method, sessions[i])
		}
		if protocol[i] != "2024-11-05" {
			t.Fatalf("protocol version missing on %s: %q", method, protocol[i])
		}
		if auth[i] != "Bearer secret-token" {
			t.Fatalf("header expansion failed on %s: %q", method, auth[i])
		}
	}
}

func TestRemoteLegacySSE(t *testing.T) {
	messages := make(chan []byte, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/sse":
			if !strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
				http.Error(w, "bad accept", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "event: endpoint\ndata: /message?session=abc\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			for {
				select {
				case <-r.Context().Done():
					return
				case payload := <-messages:
					_, _ = fmt.Fprintf(w, "event: message\ndata: %s\n\n", payload)
					if flusher != nil {
						flusher.Flush()
					}
				}
			}
		case "/message":
			if r.URL.Query().Get("session") != "abc" {
				http.Error(w, "bad session", http.StatusBadRequest)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var message rpc.Message
			if err := json.Unmarshal(body, &message); err != nil {
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(message.ID) > 0 {
				result, failure := fakeResult(t, message)
				reply := failure
				if reply == nil {
					reply = rpcReply(message.ID, result)
				}
				messages <- reply
			}
			w.WriteHeader(http.StatusAccepted)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	manager := Start(context.Background(), Config{Servers: map[string]Server{
		"sse": {
			Type:    "sse",
			URL:     server.URL + "/sse",
			Timeout: Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
		},
	}}, t.TempDir(), t.Logf)
	defer manager.Close()

	if got := manager.Servers(); len(got) != 1 || got[0] != "sse" {
		t.Fatalf("unexpected servers: %v", got)
	}
	found := false
	for _, item := range manager.Tools() {
		if item.Name() != "sse_echo" {
			continue
		}
		found = true
		out, err := item.Run(context.Background(), call("sse_echo", `{"text":"legacy"}`))
		if err != nil {
			t.Fatal(err)
		}
		if out != "legacy" {
			t.Fatalf("unexpected echo output: %q", out)
		}
	}
	if !found {
		t.Fatal("sse_echo tool not discovered")
	}
}

func TestHTTPTransportRejectsBadURL(t *testing.T) {
	if _, err := Connect(context.Background(), "bad", Server{Type: "remote", URL: "ftp://example.com/mcp"}, t.TempDir()); err == nil {
		t.Fatal("want invalid url error")
	}
}

func TestExpandEnv(t *testing.T) {
	t.Setenv("MCP_TEST_VALUE", "value")
	if got := expandEnv("https://{env:MCP_TEST_VALUE}/${MCP_TEST_VALUE}"); got != "https://value/value" {
		t.Fatalf("unexpected expansion: %q", got)
	}
}

func TestDecodeMessagesBatch(t *testing.T) {
	messages, err := decodeMessages([]byte(`[{"jsonrpc":"2.0","id":1,"result":{}},{"jsonrpc":"2.0","id":2,"result":{}}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 {
		t.Fatalf("unexpected batch size: %d", len(messages))
	}
}

func TestStreamableHTTPGetStream(t *testing.T) {
	streamOpened := make(chan string, 1)
	replyReceived := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && strings.Contains(r.Header.Get("Accept"), "text/event-stream") {
			if r.Header.Get("Mcp-Session-Id") != "sess-stream" {
				http.Error(w, "missing session", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			flusher, _ := w.(http.Flusher)
			_, _ = fmt.Fprint(w, "data: {\"jsonrpc\":\"2.0\",\"id\":777,\"method\":\"ping\"}\n\n")
			if flusher != nil {
				flusher.Flush()
			}
			streamOpened <- r.Header.Get("MCP-Protocol-Version")
			<-r.Context().Done()
			return
		}

		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), `"id":777`) {
			replyReceived <- string(body)
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var message rpc.Message
		if err := json.Unmarshal(body, &message); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if message.Method == "initialize" {
			w.Header().Set("Mcp-Session-Id", "sess-stream")
		}
		result, failure := fakeResult(t, message)
		if message.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if failure != nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(failure)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(rpcReply(message.ID, result))
	}))
	defer server.Close()

	manager := Start(context.Background(), Config{Servers: map[string]Server{
		"stream": {
			Type:    "remote",
			URL:     server.URL,
			Timeout: Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
		},
	}}, t.TempDir(), t.Logf)
	defer manager.Close()

	select {
	case version := <-streamOpened:
		if version != "2024-11-05" {
			t.Fatalf("unexpected protocol version on stream: %q", version)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("GET stream was not opened")
	}

	select {
	case reply := <-replyReceived:
		if !strings.Contains(reply, `"result"`) {
			t.Fatalf("unexpected reply: %s", reply)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("client did not answer the server ping")
	}
}

func TestRemoteOAuthToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("Authorization") != "Bearer oauth-token" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var message rpc.Message
		if err := json.Unmarshal(body, &message); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		result, failure := fakeResult(t, message)
		if message.Method == "notifications/initialized" {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		if failure != nil {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(failure)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(rpcReply(message.ID, result))
	}))
	defer server.Close()

	manager := StartWithTokens(context.Background(), Config{Servers: map[string]Server{
		"remote": {
			Type:    "remote",
			URL:     server.URL,
			OAuth:   OAuthRef{Name: "work"},
			Timeout: Timeout{Startup: 5000, Catalog: 5000, Execution: 5000},
		},
	}}, t.TempDir(), t.Logf, func(_ context.Context, endpoint string) (string, error) {
		if endpoint != "work" {
			t.Errorf("unexpected oauth endpoint %q", endpoint)
		}
		return "oauth-token", nil
	})
	defer manager.Close()
	if got := manager.Servers(); len(got) != 1 || got[0] != "remote" {
		t.Fatalf("oauth server did not connect: %v", got)
	}
	found := false
	for _, item := range manager.Tools() {
		if item.Name() == "remote_echo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("oauth server tools missing: %v", manager.Tools())
	}

	failing := StartWithTokens(context.Background(), Config{Servers: map[string]Server{
		"remote": {
			Type:    "remote",
			URL:     server.URL,
			OAuth:   OAuthRef{Name: "work"},
			Timeout: Timeout{Startup: 2000, Catalog: 2000, Execution: 2000},
		},
	}}, t.TempDir(), t.Logf, func(context.Context, string) (string, error) {
		return "", errors.New("no token")
	})
	defer failing.Close()
	if got := failing.Servers(); len(got) != 0 {
		t.Fatalf("token failure should skip the server: %v", got)
	}
}
