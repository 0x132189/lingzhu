package tool

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWebFetchConvertsHTML(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html><html><head><style>body{color:red}</style></head><body><h1>Title</h1><script>alert(1)</script><p>Hello <a href="https://example.com">world</a>.</p></body></html>`))
	}))
	defer server.Close()

	out, err := run(t, mustRegistry(t), "webfetch", `{"url":`+quoteJSON(server.URL)+`}`, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Title") || !strings.Contains(out, "Hello world (https://example.com)") {
		t.Fatalf("unexpected markdown: %q", out)
	}
	if strings.Contains(out, "alert") || strings.Contains(out, "color:red") {
		t.Fatalf("script/style leaked into output: %q", out)
	}
}

func TestWebFetchRejectsNonHTTP(t *testing.T) {
	if _, err := run(t, mustRegistry(t), "webfetch", `{"url":"file:///etc/passwd"}`, allowAll()); err == nil {
		t.Fatal("want URL scheme error")
	}
}

func mustRegistry(t *testing.T) *Registry {
	t.Helper()
	registry, _ := newRegistry(t, nil)
	return registry
}
