package share

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"lingzhu/internal/model"
)

func messages() []model.Message {
	return []model.Message{
		{Role: model.RoleUser, Content: "hello <script>alert(1)</script>"},
		{Role: model.RoleAssistant, Content: "working", ToolCalls: []model.ToolCall{
			{ID: "call_1", Name: "read", Arguments: `{"path":"a.txt"}`},
		}},
		{Role: model.RoleTool, Name: "read", Content: "file body"},
		{Role: model.RoleUser, Content: "background done", Synthetic: true},
	}
}

func newManager(t *testing.T, cfg Config) *Manager {
	t.Helper()
	manager, err := New(t.TempDir(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestCreateGetAndRevoke(t *testing.T) {
	manager := newManager(t, Config{})
	record, err := manager.Create("s1", "Share <title>", messages())
	if err != nil {
		t.Fatal(err)
	}
	if record.Token == "" || record.Path != "/share/"+record.Token || record.Messages != 4 {
		t.Fatalf("unexpected record: %+v", record)
	}

	stored, page, err := manager.Get(record.Token)
	if err != nil {
		t.Fatal(err)
	}
	if stored.SessionID != "s1" || stored.Title != "Share <title>" {
		t.Fatalf("unexpected metadata: %+v", stored)
	}
	body := string(page)
	for _, want := range []string{"Share &lt;title&gt;", "hello &lt;script&gt;", "Tool call: read", "Tool result: read", "Notice"} {
		if !strings.Contains(body, want) {
			t.Fatalf("share page missing %q", want)
		}
	}
	if strings.Contains(body, "<script>alert") {
		t.Fatal("share page contains unescaped markup")
	}

	found, ok, err := manager.BySession("s1")
	if err != nil || !ok || found.Token != record.Token {
		t.Fatalf("unexpected lookup: %+v %v %v", found, ok, err)
	}

	if err := manager.Revoke("s1"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.Get(record.Token); !os.IsNotExist(err) {
		t.Fatalf("share should be gone: %v", err)
	}
	if _, ok, _ := manager.BySession("s1"); ok {
		t.Fatal("session should no longer be shared")
	}
}

func TestCreateReplacesPreviousShare(t *testing.T) {
	manager := newManager(t, Config{})
	first, err := manager.Create("s1", "First", messages()[:1])
	if err != nil {
		t.Fatal(err)
	}
	second, err := manager.Create("s1", "Second", messages())
	if err != nil {
		t.Fatal(err)
	}
	if first.Token == second.Token {
		t.Fatal("replacement should use a new token")
	}
	if _, _, err := manager.Get(first.Token); !os.IsNotExist(err) {
		t.Fatalf("old share should be removed: %v", err)
	}
	found, _, err := manager.Get(second.Token)
	if err != nil || found.Title != "Second" {
		t.Fatalf("unexpected replacement: %+v %v", found, err)
	}
}

func TestMarkdownExport(t *testing.T) {
	body := RenderMarkdown("Demo", messages())
	for _, want := range []string{"# Demo", "## User", "hello", "## Assistant", "Tool call: read", "### Tool result: read", "Notice"} {
		if !strings.Contains(body, want) {
			t.Fatalf("markdown missing %q: %s", want, body)
		}
	}
}

func TestRejectsBadToken(t *testing.T) {
	manager := newManager(t, Config{})
	if _, _, err := manager.Get("../escape"); !os.IsNotExist(err) {
		t.Fatalf("bad token should be missing: %v", err)
	}
}

func TestRemoteUploadSignatureAndRevoke(t *testing.T) {
	const secret = "signing-secret"
	var mu sync.Mutex
	var uploaded uploadPayload
	var deleted string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/shares":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Errorf("read body: %v", err)
			}
			if r.Header.Get("Authorization") != "Bearer api-key" {
				t.Errorf("unexpected authorization: %q", r.Header.Get("Authorization"))
			}
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(body)
			want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
			if got := r.Header.Get("X-Share-Signature"); got != want {
				t.Errorf("unexpected signature: %q", got)
			}
			if err := json.Unmarshal(body, &uploaded); err != nil {
				t.Errorf("decode payload: %v", err)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"url":"https://share.example/s/remote1","id":"remote1"}`))
		case r.Method == http.MethodDelete && r.URL.Path == "/shares/remote1":
			mu.Lock()
			deleted = r.URL.Path
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	manager := newManager(t, Config{
		Endpoint: server.URL + "/shares",
		APIKey:   "api-key",
		Secret:   secret,
		Expiry:   "2h",
	})
	record, err := manager.Create("s1", "Remote", messages())
	if err != nil {
		t.Fatal(err)
	}
	if !record.Remote || record.URL != "https://share.example/s/remote1" || record.RemoteID != "remote1" {
		t.Fatalf("unexpected remote record: %+v", record)
	}
	if record.ExpiresAt.IsZero() || time.Until(record.ExpiresAt) > 2*time.Hour+time.Minute {
		t.Fatalf("unexpected expiry: %v", record.ExpiresAt)
	}
	if uploaded.Title != "Remote" || !strings.Contains(uploaded.HTML, "Tool result: read") || !strings.Contains(uploaded.Markdown, "# Remote") {
		t.Fatalf("unexpected upload payload: %+v", uploaded)
	}
	if uploaded.ExpiresAt == "" {
		t.Fatal("upload should include the expiry")
	}
	if _, page, err := manager.Get(record.Token); err != nil || !strings.Contains(string(page), "Remote") {
		t.Fatalf("local fallback page missing: %v", err)
	}

	if err := manager.Revoke("s1"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if deleted != "/shares/remote1" {
		t.Fatalf("remote delete not sent: %q", deleted)
	}
}

func TestRemoteFailureFallsBackToLocal(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()

	manager := newManager(t, Config{Endpoint: server.URL})
	record, err := manager.Create("s1", "Fallback", messages())
	if err != nil {
		t.Fatal(err)
	}
	if record.Remote || record.URL != "" || !strings.HasPrefix(record.Path, "/share/") {
		t.Fatalf("unexpected fallback record: %+v", record)
	}
	if record.RemoteNote == "" {
		t.Fatal("fallback should record the remote error")
	}
	if _, page, err := manager.Get(record.Token); err != nil || !strings.Contains(string(page), "Fallback") {
		t.Fatalf("local fallback missing: %v", err)
	}
	if manager.Status().Remote != true {
		t.Fatal("status should still report the configured endpoint")
	}
}

func TestExpiryRemovesShare(t *testing.T) {
	manager := newManager(t, Config{Expiry: "1h"})
	clock := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	manager.now = func() time.Time { return clock }
	record, err := manager.Create("s1", "Short", messages())
	if err != nil {
		t.Fatal(err)
	}
	if !record.ExpiresAt.Equal(clock.Add(time.Hour)) {
		t.Fatalf("unexpected expiry: %v", record.ExpiresAt)
	}
	if _, _, err := manager.Get(record.Token); err != nil {
		t.Fatal(err)
	}
	clock = clock.Add(2 * time.Hour)
	if _, _, err := manager.Get(record.Token); !os.IsNotExist(err) {
		t.Fatalf("expired share should be missing: %v", err)
	}
	if _, ok, _ := manager.BySession("s1"); ok {
		t.Fatal("expired share should not be listed")
	}
	if _, err := os.Stat(filepath.Join(manager.dir, record.Token+".json")); !os.IsNotExist(err) {
		t.Fatalf("expired metadata should be removed: %v", err)
	}
}

func TestDisabledMode(t *testing.T) {
	manager := newManager(t, Config{Mode: "disabled"})
	if manager.Enabled() {
		t.Fatal("disabled manager should not be enabled")
	}
	if _, err := manager.Create("s1", "x", messages()); err == nil {
		t.Fatal("disabled create should fail")
	}
	if manager.Status().Mode != "disabled" {
		t.Fatalf("unexpected status: %+v", manager.Status())
	}
	missing, err := New(t.TempDir(), Config{Mode: "disabled"})
	if err != nil || missing == nil {
		t.Fatalf("disabled manager should still open local shares: %v %v", missing, err)
	}
}

func TestAutoMode(t *testing.T) {
	manager := newManager(t, Config{Mode: "auto"})
	if !manager.Enabled() || !manager.Auto() {
		t.Fatalf("auto should enable sharing: %+v", manager.Status())
	}
}

func TestStatusHidesCredentials(t *testing.T) {
	manager := newManager(t, Config{Endpoint: "https://share.example/upload", APIKey: "secret-key", Secret: "secret-hmac", Expiry: "7d"})
	status := manager.Status()
	data, _ := json.Marshal(status)
	if strings.Contains(string(data), "secret") {
		t.Fatalf("status leaked credentials: %s", data)
	}
	if !status.Remote || status.Expiry != "7d" || status.Mode != "manual" {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestParseExpiry(t *testing.T) {
	cases := map[string]time.Duration{
		"":     0,
		"90m":  90 * time.Minute,
		"24h":  24 * time.Hour,
		"3d":   3 * 24 * time.Hour,
		"2w":   14 * 24 * time.Hour,
		"1.5d": 36 * time.Hour,
	}
	for input, want := range cases {
		got, err := parseExpiry(input)
		if err != nil || got != want {
			t.Fatalf("%q: want %v, got %v (%v)", input, want, got, err)
		}
	}
	for _, input := range []string{"0d", "abc", "-2h"} {
		if _, err := parseExpiry(input); err == nil {
			t.Fatalf("%q should be rejected", input)
		}
	}
}
