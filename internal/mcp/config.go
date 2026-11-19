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
	defaultStartupTimeout   = 30 * time.Second
	defaultCatalogTimeout   = 30 * time.Second
	defaultExecutionTimeout = 12 * time.Hour
)

// Config is the MCP section of the configuration file. Both the flat form
// ("mcp": {"server": {...}}) and the namespaced form
// ("mcp": {"servers": {...}}) are accepted.
type Config struct {
	Servers map[string]Server `json:"servers"`
	// Timeout supplies defaults for every server that does not set its own.
	Timeout Timeout `json:"timeout,omitempty"`
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
	Pool bool `json:"pool,omitempty"`
	// Codemode exposes this server's tools through execute. Nil means true.
	Codemode *bool `json:"codemode,omitempty"`
	// Sampling allows this server to ask the current model to generate text.
	Sampling bool `json:"sampling,omitempty"`
	// Elicitation allows this server to ask the user for structured input.
	Elicitation bool `json:"elicitation,omitempty"`
	Disabled    bool `json:"disabled"`
	// Enabled is the older flag; when present it wins over Disabled.
	Enabled *bool   `json:"enabled"`
	Timeout Timeout `json:"timeout"`
}

// OAuthRef accepts a configured oauth endpoint name, {"name": ...}, false, or
// the per-server client options used by automatic discovery.
type OAuthRef struct {
	Name         string
	Disabled     bool
	ClientID     string
	ClientSecret string
	Scope        string
	RedirectURI  string
	CallbackPort int
}

// UnmarshalJSON keeps "oauth": false meaningful while accepting a plain name
// or an options object.
func (o *OAuthRef) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		*o = OAuthRef{}
		return nil
	}
	if trimmed == "false" {
		*o = OAuthRef{Disabled: true}
		return nil
	}
	var name string
	if err := json.Unmarshal(data, &name); err == nil {
		*o = OAuthRef{Name: strings.TrimSpace(name)}
		return nil
	}
	var object struct {
		Name         string `json:"name"`
		ClientID     string `json:"clientId"`
		ClientSecret string `json:"clientSecret"`
		Scope        string `json:"scope"`
		RedirectURI  string `json:"redirectUri"`
		CallbackPort int    `json:"callbackPort"`
	}
	if err := json.Unmarshal(data, &object); err == nil {
		if object.CallbackPort < 0 || object.CallbackPort > 65535 {
			return fmt.Errorf("mcp oauth callbackPort must be between 1 and 65535")
		}
		*o = OAuthRef{
			Name:         strings.TrimSpace(object.Name),
			ClientID:     strings.TrimSpace(object.ClientID),
			ClientSecret: object.ClientSecret,
			Scope:        strings.TrimSpace(object.Scope),
			RedirectURI:  strings.TrimSpace(object.RedirectURI),
			CallbackPort: object.CallbackPort,
		}
		return nil
	}
	return fmt.Errorf("mcp oauth must be an object, a configured oauth endpoint name, or false")
}

// Configured reports whether the object sets per-server OAuth options.
func (o OAuthRef) Configured() bool {
	return o.ClientID != "" || o.ClientSecret != "" || o.Scope != "" || o.RedirectURI != "" || o.CallbackPort > 0
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
	if raw, ok := probe["timeout"]; ok {
		if err := json.Unmarshal(raw, &c.Timeout); err != nil {
			return err
		}
	}
	if raw, ok := probe["servers"]; ok {
		var servers map[string]Server
		if err := json.Unmarshal(raw, &servers); err != nil {
			return err
		}
		c.Servers = servers
		return nil
	}
	delete(probe, "timeout")
	filtered, err := json.Marshal(probe)
	if err != nil {
		return err
	}
	var servers map[string]Server
	if err := json.Unmarshal(filtered, &servers); err != nil {
		return err
	}
	c.Servers = servers
	return nil
}

func mergeTimeout(server, global Timeout) Timeout {
	if server.Startup <= 0 {
		server.Startup = global.Startup
	}
	if server.Catalog <= 0 {
		server.Catalog = global.Catalog
	}
	if server.Execution <= 0 {
		server.Execution = global.Execution
	}
	return server
}

// On reports whether this server should be connected. A missing type is local
// when a command exists and remote when a URL exists.
func (s Server) On() bool {
	if s.Enabled != nil {
		return *s.Enabled
	}
	return !s.Disabled
}

// CodemodeEnabled reports whether tools should be exposed through execute.
func (s Server) CodemodeEnabled() bool {
	return s.Codemode == nil || *s.Codemode
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
