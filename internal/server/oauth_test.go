package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"lingzhu/internal/oauth"
)

func TestOAuthEndpoints(t *testing.T) {
	var requests atomic.Int32
	tokenServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token": "access-1",
			"token_type":   "Bearer",
			"expires_in":   3600,
		})
	}))
	defer tokenServer.Close()

	manager, err := oauth.New(t.TempDir(), oauth.Config{Clients: map[string]oauth.Client{
		"work": {
			TokenURL:     tokenServer.URL,
			ClientID:     "client",
			ClientSecret: "secret",
			Grant:        "client_credentials",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	server := newTestServer(t)
	server.SetOAuth(manager)

	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"work"`) || strings.Contains(rec.Body.String(), "access-1") {
		t.Fatalf("unexpected oauth list: %d %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/oauth/work/refresh", strings.NewReader(`{}`)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"refreshed":true`) {
		t.Fatalf("unexpected refresh: %d %s", rec.Code, rec.Body.String())
	}
	if requests.Load() != 1 {
		t.Fatalf("unexpected token requests: %d", requests.Load())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/work", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"authorized":true`) {
		t.Fatalf("unexpected status: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "access-1") {
		t.Fatal("status leaked the access token")
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/oauth/work", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("unexpected revoke status: %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/work", nil))
	if !strings.Contains(rec.Body.String(), `"authorized":false`) {
		t.Fatalf("token not revoked: %s", rec.Body.String())
	}

	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/oauth/missing", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unknown endpoint should 404: %d", rec.Code)
	}
}
