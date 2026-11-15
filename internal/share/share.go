// Package share stores revocable public transcripts. A share is addressed by an
// unguessable token and served as a self-contained HTML page. When a remote
// endpoint is configured, the same transcript is uploaded there first and the
// remote URL is returned; an upload failure falls back to the local copy.
package share

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"lingzhu/internal/model"
)

const (
	maxRenderedBytes = 5 * 1024 * 1024
	uploadTimeout    = 15 * time.Second
	deleteTimeout    = 5 * time.Second
)

var tokenPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Config is the share section of the configuration file.
type Config struct {
	// Mode is manual (default), auto or disabled.
	Mode string `json:"mode,omitempty"`
	// Endpoint receives uploads and DELETEs for remote shares. Empty keeps every
	// share local to this core.
	Endpoint  string `json:"endpoint,omitempty"`
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// Secret signs uploads with HMAC-SHA256 in X-Share-Signature.
	Secret    string `json:"secret,omitempty"`
	SecretEnv string `json:"secret_env,omitempty"`
	// Expiry is a duration such as 24h, 7d or 2w. Empty shares never expire.
	Expiry string `json:"expiry,omitempty"`
}

// Status is the public view of the share configuration; it never includes
// credentials.
type Status struct {
	Mode     string `json:"mode"`
	Enabled  bool   `json:"enabled"`
	Remote   bool   `json:"remote"`
	Endpoint string `json:"endpoint,omitempty"`
	Expiry   string `json:"expiry,omitempty"`
}

// Record is the persisted share metadata.
type Record struct {
	Token      string    `json:"token"`
	SessionID  string    `json:"sessionId"`
	Title      string    `json:"title"`
	Created    time.Time `json:"created"`
	Messages   int       `json:"messages"`
	Path       string    `json:"path"`
	URL        string    `json:"url,omitempty"`
	Remote     bool      `json:"remote,omitempty"`
	RemoteID   string    `json:"remoteId,omitempty"`
	ExpiresAt  time.Time `json:"expiresAt,omitempty"`
	RemoteNote string    `json:"remoteNote,omitempty"`
}

// Manager owns the share directory and the optional remote endpoint.
type Manager struct {
	dir        string
	mode       string
	endpoint   string
	apiKey     string
	secret     string
	expiry     time.Duration
	expiryText string
	client     *http.Client
	now        func() time.Time
	logf       func(string, ...any)

	mu sync.Mutex
}

// New creates the share directory under dataDir. A disabled mode still opens
// the directory so existing local links keep resolving.
func New(dataDir string, cfg Config) (*Manager, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("share needs a data directory")
	}
	mode := strings.ToLower(strings.TrimSpace(cfg.Mode))
	switch mode {
	case "manual", "auto", "disabled":
	default:
		mode = "manual"
	}
	dir := filepath.Join(dataDir, "shares")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	expiry, err := parseExpiry(cfg.Expiry)
	if err != nil {
		return nil, err
	}
	secret := firstNonEmpty(cfg.Secret, envValue(cfg.SecretEnv))
	return &Manager{
		dir:        dir,
		mode:       mode,
		endpoint:   strings.TrimRight(strings.TrimSpace(cfg.Endpoint), "/"),
		apiKey:     firstNonEmpty(cfg.APIKey, envValue(cfg.APIKeyEnv)),
		secret:     secret,
		expiry:     expiry,
		expiryText: strings.TrimSpace(cfg.Expiry),
		client:     &http.Client{},
		now:        time.Now,
		logf:       func(string, ...any) {},
	}, nil
}

// SetLogger installs a logger for remote share failures.
func (m *Manager) SetLogger(logf func(string, ...any)) {
	if m == nil {
		return
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m.mu.Lock()
	m.logf = logf
	m.mu.Unlock()
}

// Enabled reports whether new shares may be created.
func (m *Manager) Enabled() bool { return m != nil && m.mode != "disabled" }

// Auto reports whether sessions may be shared automatically.
func (m *Manager) Auto() bool { return m != nil && m.mode == "auto" }

// Status returns the sanitized configuration.
func (m *Manager) Status() Status {
	if m == nil {
		return Status{Mode: "disabled"}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return Status{
		Mode:     m.mode,
		Enabled:  m.mode != "disabled",
		Remote:   m.endpoint != "",
		Endpoint: m.endpoint,
		Expiry:   m.expiryText,
	}
}

// Create stores a rendered transcript. An earlier share for the same session is
// replaced so the link always points at the latest transcript.
func (m *Manager) Create(sessionID, title string, messages []model.Message) (Record, error) {
	if m == nil {
		return Record{}, errors.New("sharing is not available")
	}
	if !m.Enabled() {
		return Record{}, errors.New("sharing is disabled")
	}
	if strings.TrimSpace(title) == "" {
		title = "Untitled session"
	}
	token, err := newToken()
	if err != nil {
		return Record{}, err
	}
	record := Record{
		Token:     token,
		SessionID: sessionID,
		Title:     title,
		Created:   m.now().UTC(),
		Messages:  len(messages),
		Path:      "/share/" + token,
	}
	if m.expiry > 0 {
		record.ExpiresAt = record.Created.Add(m.expiry)
	}
	page := RenderHTML(record, messages)
	if len(page) > maxRenderedBytes {
		record.Messages = 0
		page = RenderHTML(record, nil)
	}
	markdown := RenderMarkdown(title, messages)

	m.mu.Lock()
	if err := m.revokeLocked(sessionID); err != nil {
		m.mu.Unlock()
		return Record{}, err
	}
	if err := m.writeLocked(record, page); err != nil {
		m.mu.Unlock()
		return Record{}, err
	}
	endpoint := m.endpoint
	m.mu.Unlock()

	if endpoint == "" {
		return record, nil
	}

	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	remoteURL, remoteID, uploadErr := m.upload(ctx, record, page, markdown)

	m.mu.Lock()
	defer m.mu.Unlock()
	if uploadErr != nil {
		record.RemoteNote = uploadErr.Error()
		m.logf("share: remote upload failed, kept local copy: %v", uploadErr)
		_ = m.writeMetaLocked(record)
		return record, nil
	}
	record.URL = remoteURL
	record.RemoteID = remoteID
	record.Remote = true
	if err := m.writeMetaLocked(record); err != nil {
		return record, err
	}
	return record, nil
}

// Get returns one share and its HTML page. Expired shares are removed.
func (m *Manager) Get(token string) (Record, []byte, error) {
	if m == nil || !tokenPattern.MatchString(token) {
		return Record{}, nil, os.ErrNotExist
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	record, err := m.readLocked(token)
	if err != nil {
		return Record{}, nil, err
	}
	if m.expired(record) {
		m.removeLocked(record.Token)
		return Record{}, nil, os.ErrNotExist
	}
	page, err := os.ReadFile(filepath.Join(m.dir, token+".html"))
	if err != nil {
		return Record{}, nil, err
	}
	return record, page, nil
}

// BySession returns the current share for a session.
func (m *Manager) BySession(sessionID string) (Record, bool, error) {
	if m == nil {
		return Record{}, false, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	records, err := m.listLocked()
	if err != nil {
		return Record{}, false, err
	}
	for _, record := range records {
		if record.SessionID == sessionID {
			return record, true, nil
		}
	}
	return Record{}, false, nil
}

// Revoke removes every share created for a session, including its remote copy.
func (m *Manager) Revoke(sessionID string) error {
	if m == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.revokeLocked(sessionID)
}

func (m *Manager) revokeLocked(sessionID string) error {
	records, err := m.listLocked()
	if err != nil {
		return err
	}
	for _, record := range records {
		if record.SessionID != sessionID {
			continue
		}
		m.removeLocked(record.Token)
		if record.Remote && record.RemoteID != "" {
			ctx, cancel := context.WithTimeout(context.Background(), deleteTimeout)
			err := m.deleteRemote(ctx, record.RemoteID)
			cancel()
			if err != nil {
				m.logf("share: remote delete %s failed: %v", record.RemoteID, err)
			}
		}
	}
	return nil
}

func (m *Manager) listLocked() ([]Record, error) {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return nil, err
	}
	var records []Record
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.dir, entry.Name()))
		if err != nil {
			continue
		}
		var record Record
		if err := json.Unmarshal(data, &record); err != nil {
			continue
		}
		if m.expired(record) {
			m.removeLocked(record.Token)
			continue
		}
		records = append(records, record)
	}
	return records, nil
}

func (m *Manager) readLocked(token string) (Record, error) {
	data, err := os.ReadFile(filepath.Join(m.dir, token+".json"))
	if err != nil {
		return Record{}, err
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		return Record{}, err
	}
	return record, nil
}

func (m *Manager) writeLocked(record Record, page []byte) error {
	meta, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(m.dir, record.Token+".json"), meta, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(m.dir, record.Token+".html"), page, 0o644); err != nil {
		_ = os.Remove(filepath.Join(m.dir, record.Token+".json"))
		return err
	}
	return nil
}

func (m *Manager) writeMetaLocked(record Record) error {
	meta, err := json.MarshalIndent(record, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.dir, record.Token+".json"), meta, 0o644)
}

func (m *Manager) removeLocked(token string) {
	for _, extension := range []string{".json", ".html"} {
		_ = os.Remove(filepath.Join(m.dir, token+extension))
	}
}

func (m *Manager) expired(record Record) bool {
	return !record.ExpiresAt.IsZero() && !m.now().Before(record.ExpiresAt)
}

type uploadPayload struct {
	Title     string `json:"title"`
	HTML      string `json:"html"`
	Markdown  string `json:"markdown"`
	ExpiresAt string `json:"expiresAt,omitempty"`
}

func (m *Manager) upload(ctx context.Context, record Record, page []byte, markdown string) (string, string, error) {
	payload := uploadPayload{Title: record.Title, HTML: string(page), Markdown: markdown}
	if !record.ExpiresAt.IsZero() {
		payload.ExpiresAt = record.ExpiresAt.Format(time.RFC3339)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.endpoint, bytes.NewReader(body))
	if err != nil {
		return "", "", err
	}
	m.authorize(req, body)
	resp, err := m.client.Do(req)
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", "", fmt.Errorf("share endpoint returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	var result struct {
		URL   string `json:"url"`
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&result); err != nil && !errors.Is(err, io.EOF) {
		return "", "", err
	}
	id := firstNonEmpty(result.ID, result.Token)
	if strings.TrimSpace(result.URL) == "" {
		return "", "", errors.New("share endpoint returned no url")
	}
	return strings.TrimSpace(result.URL), id, nil
}

func (m *Manager) deleteRemote(ctx context.Context, id string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, m.endpoint+"/"+url.PathEscape(id), nil)
	if err != nil {
		return err
	}
	m.authorize(req, nil)
	resp, err := m.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		detail, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("share endpoint returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(detail)))
	}
	return nil
}

func (m *Manager) authorize(req *http.Request, body []byte) {
	if m.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+m.apiKey)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if m.secret != "" {
		req.Header.Set("X-Share-Signature", "sha256="+signature(m.secret, body))
	}
}

func signature(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

func parseExpiry(value string) (time.Duration, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return 0, nil
	}
	scale := time.Duration(0)
	number := ""
	switch {
	case strings.HasSuffix(value, "w"):
		scale, number = 7*24*time.Hour, strings.TrimSuffix(value, "w")
	case strings.HasSuffix(value, "d"):
		scale, number = 24*time.Hour, strings.TrimSuffix(value, "d")
	}
	if scale > 0 {
		count, err := strconv.ParseFloat(number, 64)
		if err != nil || count <= 0 {
			return 0, fmt.Errorf("invalid share expiry %q", value)
		}
		return time.Duration(count * float64(scale)), nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("invalid share expiry %q", value)
	}
	return duration, nil
}

func envValue(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	return strings.TrimSpace(os.Getenv(name))
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func newToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("share token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// RenderHTML turns a session transcript into one standalone page.
func RenderHTML(record Record, messages []model.Message) []byte {
	var b strings.Builder
	b.WriteString("<!doctype html><html><head><meta charset=\"utf-8\">")
	b.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">")
	b.WriteString("<title>" + html.EscapeString(record.Title) + "</title>")
	b.WriteString("<style>")
	b.WriteString("body{max-width:780px;margin:40px auto;padding:0 20px;font:15px/1.55 system-ui,sans-serif;color:#1f2328;background:#fff}")
	b.WriteString("h1{font-size:22px;margin:0 0 4px}h2{font-size:13px;text-transform:uppercase;letter-spacing:.04em;color:#656d76;margin:28px 0 8px}")
	b.WriteString(".meta{color:#656d76;font-size:13px}.msg{border-top:1px solid #d0d7de;padding:14px 0;white-space:pre-wrap;overflow-wrap:anywhere}")
	b.WriteString(".notice{color:#656d76;font-style:italic}.tool{border:1px solid #d0d7de;border-radius:6px;padding:10px;margin-top:8px;background:#f6f8fa}")
	b.WriteString(".tool strong{display:block;font-size:13px;margin-bottom:6px}pre{margin:0;white-space:pre-wrap;overflow-wrap:anywhere;font:12px/1.5 ui-monospace,monospace}")
	b.WriteString("</style></head><body>")
	b.WriteString("<h1>" + html.EscapeString(record.Title) + "</h1>")
	b.WriteString("<div class=\"meta\">" + record.Created.Format("2006-01-02 15:04 UTC") + " &middot; " + fmt.Sprintf("%d messages", record.Messages))
	if !record.ExpiresAt.IsZero() {
		b.WriteString(" &middot; expires " + record.ExpiresAt.Format("2006-01-02 15:04 UTC"))
	}
	b.WriteString("</div>")
	for _, message := range messages {
		if strings.TrimSpace(message.Content) == "" && len(message.ToolCalls) == 0 {
			continue
		}
		switch message.Role {
		case model.RoleUser:
			b.WriteString("<h2>" + roleLabel(message) + "</h2>")
			b.WriteString("<div class=\"msg" + syntheticClass(message) + "\">" + html.EscapeString(message.Content) + "</div>")
		case model.RoleAssistant:
			b.WriteString("<h2>Assistant</h2>")
			if strings.TrimSpace(message.Content) != "" {
				b.WriteString("<div class=\"msg\">" + html.EscapeString(message.Content) + "</div>")
			}
			for _, call := range message.ToolCalls {
				b.WriteString("<div class=\"tool\"><strong>Tool call: " + html.EscapeString(call.Name) + "</strong><pre>" + html.EscapeString(call.Arguments) + "</pre></div>")
			}
		case model.RoleTool:
			name := message.Name
			if name == "" {
				name = "tool"
			}
			b.WriteString("<h2>Tool result: " + html.EscapeString(name) + "</h2>")
			b.WriteString("<div class=\"tool\">")
			if message.Diff != "" {
				b.WriteString("<pre>" + html.EscapeString(message.Diff) + "</pre>")
			} else {
				b.WriteString("<pre>" + html.EscapeString(message.Content) + "</pre>")
			}
			b.WriteString("</div>")
		}
	}
	b.WriteString("</body></html>")
	return []byte(b.String())
}

// RenderMarkdown is the downloadable transcript.
func RenderMarkdown(title string, messages []model.Message) string {
	if strings.TrimSpace(title) == "" {
		title = "Untitled session"
	}
	var b strings.Builder
	b.WriteString("# " + title + "\n\n")
	for _, message := range messages {
		switch message.Role {
		case model.RoleUser:
			b.WriteString("## " + roleLabel(message) + "\n\n" + message.Content + "\n\n")
		case model.RoleAssistant:
			if strings.TrimSpace(message.Content) != "" {
				b.WriteString("## Assistant\n\n" + message.Content + "\n\n")
			}
			for _, call := range message.ToolCalls {
				b.WriteString("### Tool call: " + call.Name + "\n\n```json\n" + call.Arguments + "\n```\n\n")
			}
		case model.RoleTool:
			name := message.Name
			if name == "" {
				name = "tool"
			}
			b.WriteString("### Tool result: " + name + "\n\n```\n" + message.Content + "\n```\n\n")
		}
	}
	return strings.TrimRight(b.String(), "\n") + "\n"
}

func roleLabel(message model.Message) string {
	if message.Synthetic {
		return "Notice"
	}
	return "User"
}

func syntheticClass(message model.Message) string {
	if message.Synthetic {
		return " notice"
	}
	return ""
}
