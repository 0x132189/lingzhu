package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/mcp"
)

func TestMCPStatusEmpty(t *testing.T) {
	srv, _ := newSessionServer(t, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"servers":[]`) {
		t.Fatalf("unexpected empty MCP status: %d %s", rec.Code, rec.Body.String())
	}
}

func TestMCPStatusReportsFailedServer(t *testing.T) {
	manager := mcp.Start(context.Background(), mcp.Config{Servers: map[string]mcp.Server{
		"bad": {Type: "local", Command: []string{"lingzhu-command-that-does-not-exist"}},
	}}, t.TempDir(), t.Logf)
	defer manager.Close()
	srv, _ := newSessionServer(t, func(s *Server) { s.SetMCP(manager) })
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d %s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Servers []struct {
			Name   string `json:"name"`
			Status string `json:"status"`
			Error  string `json:"error"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Servers) != 1 || payload.Servers[0].Name != "bad" ||
		payload.Servers[0].Status != mcp.StatusFailed || payload.Servers[0].Error == "" {
		t.Fatalf("unexpected failed status: %+v", payload.Servers)
	}
}

func TestMCPResourceRead(t *testing.T) {
	srv, _ := newSessionServer(t, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/demo/resource", strings.NewReader(`{"uri":"mem://x"}`)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("missing manager should 501, got %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/demo/tool", strings.NewReader(`{"name":"read"}`)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("missing manager should 501 for tools, got %d %s", rec.Code, rec.Body.String())
	}

	srv, _ = newSessionServer(t, func(s *Server) { s.SetMCP(&mcp.Manager{}) })
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/demo/resource", strings.NewReader(`{"uri":" "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty uri should 400, got %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/demo/resource", strings.NewReader(`{"uri":"mem://x"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown server should 404, got %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/demo/tool", strings.NewReader(`{"name":" "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty tool name should 400, got %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/demo/tool", strings.NewReader(`{"name":"read","arguments":[1]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-object arguments should 400, got %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp/demo/tool", strings.NewReader(`{"name":"read","arguments":{}}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown server should 404, got %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	var payload struct {
		Servers []struct {
			Name string `json:"name"`
		} `json:"servers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Servers) != 0 {
		t.Fatalf("zero-value manager should list no servers: %+v", payload.Servers)
	}
}
