// Package mcp connects to Model Context Protocol servers over local stdio, the
// Streamable HTTP transport, or the legacy HTTP+SSE transport, and exposes their
// tools and resources through the lingzhu tool registry.
package mcp

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// Default timeouts in milliseconds. A configured value overrides each one.
const (
	defaultStartupTimeout   = 10 * time.Second
	defaultCatalogTimeout   = 15 * time.Second
	defaultExecutionTimeout = 120 * time.Second
)

// Config is the MCP section of the configuration file. Both the flat form
// ("mcp": {"server": {...}}) and the namespaced form
// ("mcp": {"servers": {...}}) are accepted.
type Config struct {
	Servers map[string]Server `json:"servers"`
}

// Server is one MCP server definition. Local servers run a stdio process;
// remote servers use Streamable HTTP ("remote") or the legacy HTTP+SSE
// transport ("sse"). URLs and headers may embed {env:NAME} or ${NAME}.
type Server struct {
	Type        string            `json:"type"`
	Command     []string          `json:"command"`
	Cwd         string            `json:"cwd"`
	Environment map[string]string `json:"environment"`
	URL         string            `json:"url"`
	Headers     map[string]string `json:"headers"`
	// OAuth references a named client from the oauth config section; the
	// bearer token is fetched and refreshed by that manager.
	OAuth OAuthRef `json:"oauth"`
	// Protocol is legacy, auto or 2026-07-28. An empty value means legacy,
	// or auto when Pool is set.
	Protocol string `json:"protocol,omitempty"`
	// Pool shares one connection between identically configured remote
	// servers that negotiate the 2026-07-28 revision.
	Pool     bool `json:"pool,omitempty"`
	Disabled bool `json:"disabled"`
	// Enabled is the older flag; when present it wins over Disabled.
	Enabled *bool   `json:"enabled"`
	Timeout Timeout `json:"timeout"`
}

// OAuthRef accepts a configured oauth endpoint name, {"name": ...}, or false.
type OAuthRef struct {
	Name string
}

// UnmarshalJSON keeps "oauth": false meaningful while accepting a plain name.
func (o *OAuthRef) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" || trimmed == "false" {
		o.Name = ""
		return nil
	}
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		o.Name = strings.TrimSpace(name)
		return nil
	}
	var object struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(data, &object); err == nil && object.Name != "" {
		o.Name = strings.TrimSpace(object.Name)
		return nil
	}
	return fmt.Errorf("mcp oauth must be a configured oauth endpoint name or false")
}

// Timeout holds per-server startup, catalog and execution deadlines in
// milliseconds, matching the reference configuration.
type Timeout struct {
	Startup   int `json:"startup"`
	Catalog   int `json:"catalog"`
	Execution int `json:"execution"`
}

// UnmarshalJSON accepts either a flat server map or an object with a servers
// key, so existing configuration can be copied unchanged.
func (c *Config) UnmarshalJSON(data []byte) error {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	if raw, ok := probe["servers"]; ok {
		var servers map[string]Server
		if err := json.Unmarshal(raw, &servers); err != nil {
			return err
		}
		c.Servers = servers
		return nil
	}
	var servers map[string]Server
	if err := json.Unmarshal(data, &servers); err != nil {
		return err
	}
	c.Servers = servers
	return nil
}

// On reports whether this server should be connected. A missing type is local
// when a command exists and remote when a URL exists.
func (s Server) On() bool {
	if s.Enabled != nil {
		return *s.Enabled
	}
	return !s.Disabled
}

// Kind returns the resolved transport kind.
func (s Server) Kind() string {
	if s.Type != "" {
		return strings.ToLower(strings.TrimSpace(s.Type))
	}
	if len(s.Command) > 0 {
		return "local"
	}
	if s.URL != "" {
		return "remote"
	}
	return ""
}

func (s Server) timeouts() Timeout {
	t := s.Timeout
	if t.Startup <= 0 {
		t.Startup = int(defaultStartupTimeout / time.Millisecond)
	}
	if t.Catalog <= 0 {
		t.Catalog = int(defaultCatalogTimeout / time.Millisecond)
	}
	if t.Execution <= 0 {
		t.Execution = int(defaultExecutionTimeout / time.Millisecond)
	}
	return t
}

func millis(value int) time.Duration {
	if value <= 0 {
		return 0
	}
	return time.Duration(value) * time.Millisecond
}

// ProtocolMode resolves the configured negotiation mode. Legacy is the
// default; pooling without an explicit protocol opts into auto.
func (s Server) ProtocolMode() string {
	switch strings.ToLower(strings.TrimSpace(s.Protocol)) {
	case "legacy":
		return "legacy"
	case "auto":
		return "auto"
	case "2026-07-28", "modern":
		return "2026-07-28"
	}
	if s.Pool {
		return "auto"
	}
	return "legacy"
}

// Pooled reports whether this server may share a connection.
func (s Server) Pooled() bool {
	return s.Pool && s.ProtocolMode() != "legacy"
}
