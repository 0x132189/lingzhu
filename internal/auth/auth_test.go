package auth

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTokenIsStableAndStored(t *testing.T) {
	dir := t.TempDir()
	first, err := Token(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(first) < 32 {
		t.Fatalf("token is too short: %q", first)
	}
	stored, err := os.ReadFile(filepath.Join(dir, tokenFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(stored)) != first {
		t.Fatalf("stored token does not match: %q", stored)
	}
	second, err := Token(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second != first {
		t.Fatalf("a second read must return the same token: %q != %q", second, first)
	}
}

func TestTokenEnvWinsAndIsStored(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(TokenEnv, "pinned-token")
	token, err := Token(dir)
	if err != nil {
		t.Fatal(err)
	}
	if token != "pinned-token" {
		t.Fatalf("env token should win, got %q", token)
	}
	// A later process without the environment must still reach the same core.
	t.Setenv(TokenEnv, "")
	again, err := Token(dir)
	if err != nil {
		t.Fatal(err)
	}
	if again != "pinned-token" {
		t.Fatalf("the env token should have been persisted, got %q", again)
	}
}

func TestMiddlewareRejectsMissingToken(t *testing.T) {
	handler := Middleware("secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401 without a token, got %d", rec.Code)
	}
}

func TestMiddlewareAcceptsHeaderAndBearer(t *testing.T) {
	handler := Middleware("secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, header := range []struct{ key, value string }{
		{Header, "secret"},
		{"Authorization", "Bearer secret"},
	} {
		req := httptest.NewRequest(http.MethodPost, "/sessions/s1/prompt", nil)
		req.Header.Set(header.key, header.value)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s: want 204, got %d", header.key, rec.Code)
		}
	}
}

func TestMiddlewareRejectsWrongToken(t *testing.T) {
	handler := Middleware("secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	req.Header.Set(Header, "other")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("want 401, got %d", rec.Code)
	}
}

func TestMiddlewareAllowsPublicPaths(t *testing.T) {
	handler := Middleware("secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, target := range []string{"/healthz", "/share/abc123"} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusNoContent {
			t.Fatalf("%s should be public, got %d", target, rec.Code)
		}
	}
}

func TestMiddlewareRejectsCrossSiteEvenWithToken(t *testing.T) {
	handler := Middleware("secret", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/sessions/s1/interrupt", nil)
	req.Header.Set(Header, "secret")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("a cross-site request must be refused, got %d", rec.Code)
	}
}

func TestMiddlewareDisabledWithoutToken(t *testing.T) {
	handler := Middleware("", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("an empty token should disable the check, got %d", rec.Code)
	}
}
