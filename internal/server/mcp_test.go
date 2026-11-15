package server

import (
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
