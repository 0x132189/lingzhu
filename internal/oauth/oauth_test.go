package oauth

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestClientCredentialsPersistRefreshAndRevoke(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		user, pass, ok := r.BasicAuth()
		if !ok || user != "client" || pass != "secret" {
			http.Error(w, "bad auth", http.StatusUnauthorized)
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("grant_type") != "client_credentials" || r.PostForm.Get("scope") != "read" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "token-" + strings.Repeat("x", int(requests.Load())),
			"token_type":    "Bearer",
			"expires_in":    3600,
			"refresh_token": "refresh-1",
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	manager, err := New(dir, Config{Clients: map[string]Client{
		"work": {
			TokenURL:     server.URL,
			ClientID:     "client",
			ClientSecret: "secret",
			Scopes:       []string{"read"},
			Grant:        "client_credentials",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	first, err := manager.AccessToken(ctx, "work")
	if err != nil {
		t.Fatal(err)
	}
	if first != "token-x" || requests.Load() != 1 {
		t.Fatalf("unexpected token: %q requests=%d", first, requests.Load())
	}
	if again, err := manager.AccessToken(ctx, "work"); err != nil || again != first || requests.Load() != 1 {
		t.Fatalf("valid token should be reused: %q %v requests=%d", again, err, requests.Load())
	}

	status, ok := manager.Status("work")
	if !ok || !status.Authorized || status.HasRefreshToken == false || len(status.Scopes) != 1 {
		t.Fatalf("unexpected status: %+v", status)
	}

	reopened, err := New(dir, managerConfig(server.URL))
	if err != nil {
		t.Fatal(err)
	}
	if token, err := reopened.AccessToken(ctx, "work"); err != nil || token != first {
		t.Fatalf("token did not persist: %q %v", token, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("reopen should not fetch again, requests=%d", requests.Load())
	}

	if _, err := manager.Refresh(ctx, "work"); err != nil || requests.Load() != 2 {
		t.Fatalf("refresh failed: %v requests=%d", err, requests.Load())
	}
	if err := manager.Revoke("work"); err != nil {
		t.Fatal(err)
	}
	if status, _ := manager.Status("work"); status.Authorized {
		t.Fatalf("revoke should clear the token: %+v", status)
	}
	if _, err := manager.AccessToken(ctx, "work"); err != nil || requests.Load() != 3 {
		t.Fatalf("revoked token should be fetched again: %v requests=%d", err, requests.Load())
	}
}

func managerConfig(tokenURL string) Config {
	return Config{Clients: map[string]Client{
		"work": {
			TokenURL:     tokenURL,
			ClientID:     "client",
			ClientSecret: "secret",
			Scopes:       []string{"read"},
			Grant:        "client_credentials",
		},
	}}
}

func TestAuthorizationCodePKCE(t *testing.T) {
	var challenge, verifier string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") != "code-1" {
			http.Error(w, "bad grant", http.StatusBadRequest)
			return
		}
		verifier = r.PostForm.Get("code_verifier")
		sum := sha256.Sum256([]byte(verifier))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
			http.Error(w, "pkce mismatch", http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"access_token":  "access-1",
			"refresh_token": "refresh-1",
			"expires_in":    3600,
		})
	}))
	defer server.Close()

	manager, err := New(t.TempDir(), Config{Clients: map[string]Client{
		"work": {
			AuthorizationURL: "https://auth.example/authorize",
			TokenURL:         server.URL,
			ClientID:         "client",
			ClientSecret:     "secret",
			RedirectURI:      "http://127.0.0.1/callback",
			Scopes:           []string{"read", "write"},
			Grant:            "authorization_code",
			AuthMethod:       "post",
		},
	}})
	if err != nil {
		t.Fatal(err)
	}
	authURL, state, err := manager.StartAuthorization("work")
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(authURL)
	if err != nil {
		t.Fatal(err)
	}
	query := parsed.Query()
	challenge = query.Get("code_challenge")
	if query.Get("code_challenge_method") != "S256" || query.Get("state") != state || query.Get("scope") != "read write" {
		t.Fatalf("unexpected authorization url: %s", authURL)
	}

	if _, err := manager.ExchangeWithState(context.Background(), "work", "code-1", "wrong"); err == nil {
		t.Fatal("wrong state should fail")
	}
	if _, err := manager.ExchangeWithState(context.Background(), "work", "code-1", state); err != nil {
		t.Fatal(err)
	}
	if verifier == "" {
		t.Fatal("token endpoint did not receive a verifier")
	}
	if token, err := manager.AccessToken(context.Background(), "work"); err != nil || token != "access-1" {
		t.Fatalf("unexpected token: %q %v", token, err)
	}
}

func TestConfigAcceptsMap(t *testing.T) {
	var cfg Config
	body := `{"work":{"token_url":"https://example.com/token","client_id":"id","grant":"client_credentials"}}`
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Clients["work"].ClientID != "id" || cfg.Clients["work"].grant() != "client_credentials" {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestEnsureClientOptions(t *testing.T) {
	var base string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mcp/.well-known/oauth-protected-resource":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authorization_servers": []string{base},
				"scopes_supported":      []string{"read"},
			})
		case r.URL.Path == "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 base,
				"authorization_endpoint": base + "/authorize",
				"token_endpoint":         base + "/token",
				"scopes_supported":       []string{"read", "write"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base = server.URL

	manager, err := New(t.TempDir(), Config{})
	if err != nil {
		t.Fatal(err)
	}
	client, err := manager.EnsureClient(context.Background(), "demo", base+"/mcp", "http://127.0.0.1:8787/oauth/callback", "", Options{
		ClientID:     "configured-id",
		ClientSecret: "configured-secret",
		Scope:        "read write",
		RedirectURI:  "http://127.0.0.1:19876/oauth/callback",
	})
	if err != nil {
		t.Fatal(err)
	}
	if client.ClientID != "configured-id" || client.ClientSecret != "configured-secret" {
		t.Fatalf("configured credentials were not used: %+v", client)
	}
	if client.RedirectURI != "http://127.0.0.1:19876/oauth/callback" {
		t.Fatalf("redirect override was not applied: %+v", client)
	}
	if strings.Join(client.Scopes, " ") != "read write" {
		t.Fatalf("scope override was not applied: %+v", client)
	}
}

func TestEnsureClientDiscoversAndRegisters(t *testing.T) {
	var base string
	var registerBody map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/mcp/.well-known/oauth-protected-resource":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"authorization_servers": []string{base},
				"scopes_supported":      []string{"read"},
			})
		case r.URL.Path == "/.well-known/oauth-authorization-server":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"issuer":                 base,
				"authorization_endpoint": base + "/authorize",
				"token_endpoint":         base + "/token",
				"registration_endpoint":  base + "/register",
				"scopes_supported":       []string{"read", "write"},
			})
		case r.URL.Path == "/register" && r.Method == http.MethodPost:
			_ = json.NewDecoder(r.Body).Decode(&registerBody)
			_ = json.NewEncoder(w).Encode(map[string]any{"client_id": "dynamic-id"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	base = server.URL

	dir := t.TempDir()
	manager, err := New(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	callback := "http://127.0.0.1:19876/oauth/callback"
	client, err := manager.EnsureClient(context.Background(), "demo", base+"/mcp", callback, "")
	if err != nil {
		t.Fatal(err)
	}
	if client.ClientID != "dynamic-id" || client.AuthorizationURL != base+"/authorize" || client.TokenURL != base+"/token" {
		t.Fatalf("unexpected dynamic client: %+v", client)
	}
	if client.RedirectURI != callback || client.AuthMethod != "none" {
		t.Fatalf("dynamic client did not keep the loopback redirect: %+v", client)
	}
	redirects, _ := registerBody["redirect_uris"].([]any)
	if len(redirects) != 1 || redirects[0] != callback {
		t.Fatalf("registration did not send the redirect: %#v", registerBody)
	}
	if tokenMethod := registerBody["token_endpoint_auth_method"]; tokenMethod != "none" {
		t.Fatalf("registration should be a public client: %#v", registerBody)
	}

	reopened, err := New(dir, Config{})
	if err != nil {
		t.Fatal(err)
	}
	persisted, ok := reopened.Client("demo")
	if !ok || persisted.ClientID != "dynamic-id" || persisted.RedirectURI != callback {
		t.Fatalf("dynamic client did not persist: %+v", persisted)
	}
}
