// Package plugin runs out-of-process extensions that add tools, slash commands,
// and permission decisions to the agent. Plugins speak newline-delimited
// JSON-RPC over stdio and are started with the core.
package plugin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

const (
	defaultStartupTimeout   = 10 * time.Second
	defaultCatalogTimeout   = 10 * time.Second
	defaultExecutionTimeout = 120 * time.Second
)

// Config is the plugins section of the configuration file. Both a map keyed by
// plugin name and an array of entries with a name field are accepted.
type Config struct {
	Plugins map[string]Server `json:"plugins"`
}

// Server is one plugin process definition.
type Server struct {
	Name        string            `json:"name,omitempty"`
	Command     []string          `json:"command"`
	Cwd         string            `json:"cwd,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Enabled     *bool             `json:"enabled,omitempty"`
	Disabled    bool              `json:"disabled,omitempty"`
	Timeout     Timeout           `json:"timeout,omitempty"`
}

// Timeout holds per-plugin startup, catalog and execution deadlines in
// milliseconds.
type Timeout struct {
	Startup   int `json:"startup"`
	Catalog   int `json:"catalog"`
	Execution int `json:"execution"`
}

// UnmarshalJSON accepts the object and array forms used by configuration files.
func (c *Config) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		c.Plugins = nil
		return nil
	}
	if trimmed[0] == '[' {
		var list []Server
		if err := json.Unmarshal(trimmed, &list); err != nil {
			return err
		}
		plugins := make(map[string]Server, len(list))
		for _, item := range list {
			name := strings.TrimSpace(item.Name)
			if name == "" {
				return fmt.Errorf("plugins written as an array need a name")
			}
			plugins[name] = item
		}
		c.Plugins = plugins
		return nil
	}
	var plugins map[string]Server
	if err := json.Unmarshal(trimmed, &plugins); err != nil {
		return fmt.Errorf("plugins must be an object or an array of entries")
	}
	c.Plugins = plugins
	return nil
}

func (s Server) on() bool {
	if s.Enabled != nil {
		return *s.Enabled
	}
	return !s.Disabled
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
