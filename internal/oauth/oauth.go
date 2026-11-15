// Package oauth stores and refreshes OAuth tokens for configured remote
// endpoints. Tokens live in one user-only file; client secrets stay in the
// project or user configuration.
package oauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	tokenSkew   = 60 * time.Second
	httpTimeout = 20 * time.Second
)

// Config is the oauth section keyed by endpoint name.
type Config struct {
	Clients map[string]Client `json:"clients"`
}

// Client is one OAuth client definition.
type Client struct {
	AuthorizationURL string            `json:"authorization_url,omitempty"`
	TokenURL         string            `json:"token_url"`
	ClientID         string            `json:"client_id"`
	ClientSecret     string            `json:"client_secret,omitempty"`
	Scopes           []string          `json:"scopes,omitempty"`
	Grant            string            `json:"grant,omitempty"`
	RedirectURI      string            `json:"redirect_uri,omitempty"`
	Audience         string            `json:"audience,omitempty"`
	AuthMethod       string            `json:"auth_method,omitempty"`
	ExtraParams      map[string]string `json:"extra_params,omitempty"`
}

// UnmarshalJSON accepts a map keyed by endpoint name.
func (c *Config) UnmarshalJSON(data []byte) error {
	var clients map[string]Client
	if err := json.Unmarshal(data, &clients); err != nil {
		return fmt.Errorf("oauth must be an object keyed by endpoint name")
	}
	c.Clients = clients
	return nil
}

func (c Client) grant() string {
	switch strings.ToLower(strings.TrimSpace(c.Grant)) {
	case "client_credentials", "authorization_code":
		return strings.ToLower(strings.TrimSpace(c.Grant))
	}
	if strings.TrimSpace(c.AuthorizationURL) != "" {
		return "authorization_code"
	}
	return "client_credentials"
}

// Token is one stored credential. Values are never returned by status APIs.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	TokenType    string    `json:"token_type,omitempty"`
	Scope        string    `json:"scope,omitempty"`
	ExpiresAt    time.Time `json:"expires_at,omitempty"`
}

func (t Token) valid(now time.Time) bool {
	if strings.TrimSpace(t.AccessToken) == "" {
		return false
	}
	if t.ExpiresAt.IsZero() {
		return true
	}
	return now.Add(tokenSkew).Before(t.ExpiresAt)
}

// Status is the public view of one endpoint.
type Status struct {
	Name             string    `json:"name"`
	Grant            string    `json:"grant"`
	TokenURL         string    `json:"token_url"`
	AuthorizationURL string    `json:"authorization_url,omitempty"`
	ClientID         string    `json:"client_id"`
	RedirectURI      string    `json:"redirect_uri,omitempty"`
	Scopes           []string  `json:"scopes"`
	Authorized       bool      `json:"authorized"`
	ExpiresAt        time.Time `json:"expiresAt,omitempty"`
	Expired          bool      `json:"expired"`
	HasRefreshToken  bool      `json:"hasRefreshToken"`
}

// Manager owns the token file and configured clients.
type Manager struct {
	file   string
	cfg    Config
	client *http.Client

	mu      sync.Mutex
	tokens  map[string]Token
	pending map[string]pendingAuth
}

type pendingAuth struct {
	name     string
	verifier string
	created  time.Time
}

// New loads previously stored tokens.
func New(dataDir string, cfg Config) (*Manager, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		file:    filepath.Join(dataDir, "oauth.json"),
		cfg:     cfg,
		client:  &http.Client{Timeout: httpTimeout},
		tokens:  map[string]Token{},
		pending: map[string]pendingAuth{},
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(data) == 0 {
		return nil
	}
	return json.Unmarshal(data, &m.tokens)
}

func (m *Manager) saveLocked() error {
	data, err := json.MarshalIndent(m.tokens, "", "  ")
	if err != nil {
		return err
	}
	temp := m.file + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, m.file)
}

// List returns every configured client with its status.
func (m *Manager) List() []Status {
	if m == nil {
		return []Status{}
	}
	names := make([]string, 0, len(m.cfg.Clients))
	for name := range m.cfg.Clients {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Status, 0, len(names))
	for _, name := range names {
		out = append(out, m.statusLocked(name))
	}
	return out
}

// Status returns one endpoint's status.
func (m *Manager) Status(name string) (Status, bool) {
	if m == nil {
		return Status{}, false
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.cfg.Clients[name]; !ok {
		return Status{}, false
	}
	return m.statusLocked(name), true
}

func (m *Manager) statusLocked(name string) Status {
	client := m.cfg.Clients[name]
	token := m.tokens[name]
	now := time.Now()
	return Status{
		Name:             name,
		Grant:            client.grant(),
		TokenURL:         client.TokenURL,
		AuthorizationURL: client.AuthorizationURL,
		ClientID:         client.ClientID,
		RedirectURI:      client.RedirectURI,
		Scopes:           append([]string(nil), client.Scopes...),
		Authorized:       strings.TrimSpace(token.AccessToken) != "",
		ExpiresAt:        token.ExpiresAt,
		Expired:          !token.ExpiresAt.IsZero() && !now.Before(token.ExpiresAt),
		HasRefreshToken:  strings.TrimSpace(token.RefreshToken) != "",
	}
}

// AccessToken returns a valid token, refreshing or fetching as needed.
func (m *Manager) AccessToken(ctx context.Context, name string) (string, error) {
	token, err := m.Token(ctx, name)
	if err != nil {
		return "", err
	}
	return token.AccessToken, nil
}

// Token is AccessToken with the full stored token.
func (m *Manager) Token(ctx context.Context, name string) (Token, error) {
	if m == nil {
		return Token{}, errors.New("oauth is not configured")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	client, ok := m.cfg.Clients[name]
	if !ok {
		return Token{}, fmt.Errorf("oauth endpoint %q is not configured", name)
	}
	token := m.tokens[name]
	if token.valid(time.Now()) {
		return token, nil
	}
	if strings.TrimSpace(token.RefreshToken) != "" && client.grant() == "authorization_code" {
		refreshed, err := m.exchange(ctx, name, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {token.RefreshToken},
		})
		if err == nil {
			return refreshed, nil
		}
	}
	if client.grant() == "client_credentials" {
		fetched, err := m.exchange(ctx, name, url.Values{"grant_type": {"client_credentials"}})
		if err != nil {
			return Token{}, err
		}
		return fetched, nil
	}
	return Token{}, fmt.Errorf("oauth endpoint %q needs authorization", name)
}

// Refresh forces a refresh or a new client-credentials grant.
func (m *Manager) Refresh(ctx context.Context, name string) (Token, error) {
	if m == nil {
		return Token{}, errors.New("oauth is not configured")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	client, ok := m.cfg.Clients[name]
	if !ok {
		return Token{}, fmt.Errorf("oauth endpoint %q is not configured", name)
	}
	token := m.tokens[name]
	if strings.TrimSpace(token.RefreshToken) != "" && client.grant() == "authorization_code" {
		return m.exchange(ctx, name, url.Values{
			"grant_type":    {"refresh_token"},
			"refresh_token": {token.RefreshToken},
		})
	}
	if client.grant() == "client_credentials" {
		return m.exchange(ctx, name, url.Values{"grant_type": {"client_credentials"}})
	}
	return Token{}, fmt.Errorf("oauth endpoint %q has no refresh token", name)
}

// Exchange completes the authorization-code flow.
func (m *Manager) Exchange(ctx context.Context, name, code, verifier string) (Token, error) {
	if m == nil {
		return Token{}, errors.New("oauth is not configured")
	}
	if strings.TrimSpace(code) == "" {
		return Token{}, errors.New("authorization code is empty")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	values := url.Values{
		"grant_type": {"authorization_code"},
		"code":       {code},
	}
	if verifier != "" {
		values.Set("code_verifier", verifier)
	}
	return m.exchangeLocked(ctx, name, values)
}

// AuthorizeURL builds the consent URL with PKCE state.
func (m *Manager) AuthorizeURL(name, state, verifier string) (string, error) {
	if m == nil {
		return "", errors.New("oauth is not configured")
	}
	client, ok := m.cfg.Clients[name]
	if !ok {
		return "", fmt.Errorf("oauth endpoint %q is not configured", name)
	}
	if strings.TrimSpace(client.AuthorizationURL) == "" {
		return "", fmt.Errorf("oauth endpoint %q has no authorization url", name)
	}
	parsed, err := url.Parse(client.AuthorizationURL)
	if err != nil {
		return "", err
	}
	query := parsed.Query()
	query.Set("response_type", "code")
	query.Set("client_id", client.ClientID)
	if client.RedirectURI != "" {
		query.Set("redirect_uri", client.RedirectURI)
	}
	if len(client.Scopes) > 0 {
		query.Set("scope", strings.Join(client.Scopes, " "))
	}
	if state != "" {
		query.Set("state", state)
	}
	if verifier != "" {
		sum := sha256.Sum256([]byte(verifier))
		query.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
		query.Set("code_challenge_method", "S256")
	}
	if client.Audience != "" {
		query.Set("audience", client.Audience)
	}
	for key, value := range client.ExtraParams {
		query.Set(key, value)
	}
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// StartAuthorization creates state and PKCE material, remembers them, and
// returns the consent URL.
func (m *Manager) StartAuthorization(name string) (string, string, error) {
	state, err := RandomState()
	if err != nil {
		return "", "", err
	}
	verifier, err := RandomVerifier()
	if err != nil {
		return "", "", err
	}
	authURL, err := m.AuthorizeURL(name, state, verifier)
	if err != nil {
		return "", "", err
	}
	m.mu.Lock()
	cutoff := time.Now().Add(-10 * time.Minute)
	for key, pending := range m.pending {
		if pending.created.Before(cutoff) {
			delete(m.pending, key)
		}
	}
	m.pending[state] = pendingAuth{name: name, verifier: verifier, created: time.Now()}
	m.mu.Unlock()
	return authURL, state, nil
}

// ExchangeWithState completes a flow started by StartAuthorization.
func (m *Manager) ExchangeWithState(ctx context.Context, name, code, state string) (Token, error) {
	if m == nil {
		return Token{}, errors.New("oauth is not configured")
	}
	m.mu.Lock()
	pending, ok := m.pending[state]
	delete(m.pending, state)
	m.mu.Unlock()
	if !ok || pending.name != name {
		return Token{}, errors.New("authorization state is invalid or expired")
	}
	return m.Exchange(ctx, name, code, pending.verifier)
}

// Revoke deletes the stored token for one endpoint.
func (m *Manager) Revoke(name string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.cfg.Clients[name]; !ok {
		return fmt.Errorf("oauth endpoint %q is not configured", name)
	}
	delete(m.tokens, name)
	return m.saveLocked()
}

func (m *Manager) exchange(ctx context.Context, name string, values url.Values) (Token, error) {
	return m.exchangeLocked(ctx, name, values)
}

func (m *Manager) exchangeLocked(ctx context.Context, name string, values url.Values) (Token, error) {
	client := m.cfg.Clients[name]
	if strings.TrimSpace(client.TokenURL) == "" {
		return Token{}, fmt.Errorf("oauth endpoint %q has no token url", name)
	}
	if len(client.Scopes) > 0 && values.Get("grant_type") == "client_credentials" {
		values.Set("scope", strings.Join(client.Scopes, " "))
	}
	if client.Audience != "" {
		values.Set("audience", client.Audience)
	}
	switch strings.ToLower(strings.TrimSpace(client.AuthMethod)) {
	case "post", "client_secret_post":
		values.Set("client_id", client.ClientID)
		values.Set("client_secret", client.ClientSecret)
	default:
		values.Set("client_id", client.ClientID)
	}
	for key, value := range client.ExtraParams {
		if values.Get(key) == "" {
			values.Set(key, value)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, client.TokenURL, strings.NewReader(values.Encode()))
	if err != nil {
		return Token{}, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	if strings.ToLower(strings.TrimSpace(client.AuthMethod)) != "post" &&
		strings.ToLower(strings.TrimSpace(client.AuthMethod)) != "client_secret_post" &&
		client.ClientSecret != "" {
		req.SetBasicAuth(client.ClientID, client.ClientSecret)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return Token{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return Token{}, fmt.Errorf("oauth token endpoint: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload struct {
		AccessToken  string    `json:"access_token"`
		RefreshToken string    `json:"refresh_token"`
		TokenType    string    `json:"token_type"`
		Scope        string    `json:"scope"`
		ExpiresIn    float64   `json:"expires_in"`
		ExpiresAt    time.Time `json:"expires_at"`
		Error        string    `json:"error"`
		Description  string    `json:"error_description"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return Token{}, err
	}
	if payload.Error != "" {
		if payload.Description != "" {
			return Token{}, fmt.Errorf("oauth: %s: %s", payload.Error, payload.Description)
		}
		return Token{}, fmt.Errorf("oauth: %s", payload.Error)
	}
	if strings.TrimSpace(payload.AccessToken) == "" {
		return Token{}, errors.New("oauth token endpoint returned no access_token")
	}
	now := time.Now()
	token := Token{
		AccessToken:  payload.AccessToken,
		RefreshToken: payload.RefreshToken,
		TokenType:    payload.TokenType,
		Scope:        payload.Scope,
		ExpiresAt:    payload.ExpiresAt,
	}
	if token.RefreshToken == "" {
		token.RefreshToken = m.tokens[name].RefreshToken
	}
	if token.ExpiresAt.IsZero() && payload.ExpiresIn > 0 {
		token.ExpiresAt = now.Add(time.Duration(payload.ExpiresIn) * time.Second)
	}
	m.tokens[name] = token
	if err := m.saveLocked(); err != nil {
		return Token{}, err
	}
	return token, nil
}

// RandomVerifier returns a PKCE code verifier.
func RandomVerifier() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// RandomState returns an opaque OAuth state value.
func RandomState() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}
