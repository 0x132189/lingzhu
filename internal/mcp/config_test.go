package mcp

import (
	"encoding/json"
	"testing"
)

func TestConfigAcceptsFlatAndServersForms(t *testing.T) {
	flat := []byte(`{"demo":{"type":"local","command":["echo","hi"],"enabled":false}}`)
	var c Config
	if err := json.Unmarshal(flat, &c); err != nil {
		t.Fatal(err)
	}
	if len(c.Servers) != 1 || c.Servers["demo"].On() {
		t.Fatalf("unexpected flat config: %+v", c)
	}
	if got := c.Servers["demo"].Kind(); got != "local" {
		t.Fatalf("unexpected kind: %q", got)
	}

	wrapped := []byte(`{"servers":{"remote":{"type":"remote","url":"https://example.com/mcp"},"off":{"type":"local","command":["x"],"disabled":true}}}`)
	var w Config
	if err := json.Unmarshal(wrapped, &w); err != nil {
		t.Fatal(err)
	}
	if len(w.Servers) != 2 {
		t.Fatalf("unexpected wrapped config: %+v", w)
	}
	if w.Servers["remote"].Kind() != "remote" || w.Servers["off"].On() {
		t.Fatalf("unexpected server flags: %+v", w.Servers)
	}
}

func TestTimeoutDefaults(t *testing.T) {
	c := Server{}
	timeouts := c.timeouts()
	if timeouts.Startup <= 0 || timeouts.Catalog <= 0 || timeouts.Execution <= 0 {
		t.Fatalf("timeouts not defaulted: %+v", timeouts)
	}
	c.Timeout = Timeout{Startup: 123}
	if got := c.timeouts().Startup; got != 123 {
		t.Fatalf("configured startup timeout ignored: %d", got)
	}
}

func TestOAuthRefForms(t *testing.T) {
	cases := []struct {
		body string
		want string
	}{
		{`{"remote":{"type":"remote","url":"https://example.com/mcp","oauth":"work"}}`, "work"},
		{`{"remote":{"type":"remote","url":"https://example.com/mcp","oauth":{"name":"work"}}}`, "work"},
		{`{"remote":{"type":"remote","url":"https://example.com/mcp","oauth":false}}`, ""},
		{`{"remote":{"type":"remote","url":"https://example.com/mcp"}}`, ""},
	}
	for _, tc := range cases {
		var cfg Config
		if err := json.Unmarshal([]byte(tc.body), &cfg); err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		if got := cfg.Servers["remote"].OAuth.Name; got != tc.want {
			t.Fatalf("%s: want %q, got %q", tc.body, tc.want, got)
		}
	}
	var invalid Config
	if err := json.Unmarshal([]byte(`{"remote":{"type":"remote","url":"x","oauth":123}}`), &invalid); err == nil {
		t.Fatal("numeric oauth should be rejected")
	}
}

func TestProtocolAndPoolConfig(t *testing.T) {
	var cfg Config
	body := `{"servers":{"a":{"type":"remote","url":"https://example.com/mcp","protocol":"2026-07-28","pool":true},"b":{"type":"remote","url":"https://example.com/mcp","pool":true}}}`
	if err := json.Unmarshal([]byte(body), &cfg); err != nil {
		t.Fatal(err)
	}
	a := cfg.Servers["a"]
	if a.Protocol != "2026-07-28" || !a.Pool || a.ProtocolMode() != "2026-07-28" || !a.Pooled() {
		t.Fatalf("unexpected modern server: %+v", a)
	}
	b := cfg.Servers["b"]
	if b.ProtocolMode() != "auto" || !b.Pooled() {
		t.Fatalf("pool without protocol should be auto: %+v", b)
	}
}
