package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHandlerRequiresTheToken proves the API is closed by default: a local
// process without the token cannot drive a session.
func TestHandlerRequiresTheToken(t *testing.T) {
	srv, _ := newSessionServer(t, func(s *Server) { s.SetAuthToken("secret") })

	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without a token, got %d", rec.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set("X-Lingzhu-Token", "secret")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200 with the token, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestHandlerKeepsHealthAndSharePublic proves the probe works before a client
// has a token, and a share link stays reachable by whoever holds it.
func TestHandlerKeepsHealthAndSharePublic(t *testing.T) {
	srv, _ := newSessionServer(t, func(s *Server) { s.SetAuthToken("secret") })

	for _, target := range []string{"/healthz", "/share/missing-token"} {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code == http.StatusUnauthorized {
			t.Fatalf("%s must not require a token, got %d", target, rec.Code)
		}
	}
}

// TestHandlerRefusesCrossSiteRequests proves a browser page on another origin
// cannot drive the API even if it somehow learned the token.
func TestHandlerRefusesCrossSiteRequests(t *testing.T) {
	srv, _ := newSessionServer(t, func(s *Server) { s.SetAuthToken("secret") })

	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/interrupt", nil)
	req.Header.Set("X-Lingzhu-Token", "secret")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a cross-site request must be refused, got %d", rec.Code)
	}
}

// TestHandlerCapsTheRequestBody proves an oversized prompt is rejected instead
// of being buffered whole.
func TestHandlerCapsTheRequestBody(t *testing.T) {
	srv, _ := newSessionServer(t, nil)

	huge := strings.Repeat("x", maxRequestBody+1024)
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/prompt", strings.NewReader(`{"text":"`+huge+`"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code == http.StatusOK {
		t.Fatal("an oversized body must not be accepted")
	}
}
