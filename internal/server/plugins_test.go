package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"lingzhu/internal/plugin"
)

func TestPluginsStatusEmpty(t *testing.T) {
	srv, _ := newSessionServer(t, nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/plugins", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"plugins":[]`) {
		t.Fatalf("unexpected empty plugin status: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/plugins/demo/tool", strings.NewReader(`{"name":"read"}`)))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("missing manager should 501, got %d %s", rec.Code, rec.Body.String())
	}

	srv, _ = newSessionServer(t, func(s *Server) {
		s.SetPlugins(plugin.Start(context.Background(), plugin.Config{}, "", nil))
	})
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/plugins/demo/tool", strings.NewReader(`{"name":" "}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("empty tool name should 400, got %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/plugins/demo/tool", strings.NewReader(`{"name":"read","arguments":[1]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("non-object arguments should 400, got %d %s", rec.Code, rec.Body.String())
	}
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/plugins/demo/tool", strings.NewReader(`{"name":"read"}`)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown plugin tool should 404, got %d %s", rec.Code, rec.Body.String())
	}
}
