package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"

	"lingzhu/internal/tool"
)

// Manager owns every connected MCP server and the tools discovered from them.
type Manager struct {
	mu        sync.RWMutex
	clients   map[string]*Client
	resources map[string][]Resource
	prompts   []PromptInfo
	tools     []tool.Tool
	logf      func(string, ...any)
}

// Start connects enabled local, remote, and legacy SSE servers, discovers their
// tools and resources, and returns a manager holding the registrations.
// Connection failures are logged and skipped; one broken server never prevents
// the others from loading.
func Start(ctx context.Context, cfg Config, workdir string, logf func(string, ...any)) *Manager {
	return StartWithTokens(ctx, cfg, workdir, logf, nil)
}

// StartWithTokens is Start with an OAuth token resolver for servers that set
// "oauth" to a configured oauth endpoint name.
func StartWithTokens(ctx context.Context, cfg Config, workdir string, logf func(string, ...any), tokens TokenFunc) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m := &Manager{
		clients:   map[string]*Client{},
		resources: map[string][]Resource{},
		logf:      logf,
	}
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	type started struct {
		name      string
		client    *Client
		tools     []ToolInfo
		resources []Resource
		prompts   []PromptInfo
	}
	results := make(chan started, len(names))
	var wg sync.WaitGroup
	groups := map[string][]string{}
	pooled := map[string]*poolGroup{}
	for _, name := range names {
		server := cfg.Servers[name]
		if !server.On() {
			continue
		}
		if !supportedKind(server.Kind()) {
			logf("mcp: skipping server %q: unsupported transport %q", name, server.Kind())
			continue
		}
		if server.Pooled() && remoteKind(server.Kind()) {
			key := poolKey(server)
			if group, ok := pooled[key]; ok {
				group.names = append(group.names, name)
				groups[group.canonical] = append([]string(nil), group.names...)
				logf("mcp: %q shares the connection of %q", name, group.canonical)
				continue
			}
			pooled[key] = &poolGroup{canonical: name, names: []string{name}}
		}
		wg.Add(1)
		go func(name string, server Server) {
			defer wg.Done()
			client, err := connect(ctx, name, server, workdir, tokens)
			if err != nil {
				logf("%v", err)
				return
			}
			item := started{client: client}
			item.tools, err = client.Tools(ctx)
			if err != nil {
				logf("mcp: list tools for %q: %v", name, err)
			}
			item.resources, err = client.Resources(ctx)
			if err != nil {
				logf("mcp: list resources for %q: %v", name, err)
			}
			item.prompts, err = client.Prompts(ctx)
			if err != nil {
				logf("mcp: list prompts for %q: %v", name, err)
			}
			targets := groups[name]
			if len(targets) == 0 {
				targets = []string{name}
			}
			for _, target := range targets {
				item.name = target
				results <- item
			}
		}(name, server)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	for item := range results {
		if item.client == nil {
			continue
		}
		m.clients[item.name] = item.client
		m.resources[item.name] = item.resources
		for _, prompt := range item.prompts {
			prompt.Server = item.name
			m.prompts = append(m.prompts, prompt)
		}
		for _, info := range item.tools {
			m.tools = append(m.tools, &mcpTool{
				client:      item.client,
				server:      item.name,
				rawName:     info.Name,
				name:        ToolName(item.name, info.Name),
				description: info.Description,
				schema:      info.InputSchema,
			})
		}
	}
	sort.Slice(m.tools, func(i, j int) bool { return m.tools[i].Name() < m.tools[j].Name() })
	sort.Slice(m.prompts, func(i, j int) bool {
		if m.prompts[i].Server != m.prompts[j].Server {
			return m.prompts[i].Server < m.prompts[j].Server
		}
		return m.prompts[i].Name < m.prompts[j].Name
	})

	// Effective names are flattened, so two servers can collide after
	// sanitizing. Keep the first registration and report the rest.
	seen := map[string]bool{}
	unique := m.tools[:0]
	for _, t := range m.tools {
		if seen[t.Name()] {
			logf("mcp: duplicate tool name %q skipped", t.Name())
			continue
		}
		seen[t.Name()] = true
		unique = append(unique, t)
	}
	m.tools = unique
	return m
}

// PromptCommandName maps a server prompt to a command name, matching the
// reference harness's sanitization.
func PromptCommandName(server, prompt string) string {
	sanitize := func(value string) string {
		var b strings.Builder
		for _, r := range value {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
				b.WriteRune(r)
			default:
				b.WriteRune('_')
			}
		}
		return b.String()
	}
	return sanitize(server) + ":" + sanitize(prompt)
}

// Prompts returns every discovered server prompt in stable order.
func (m *Manager) Prompts() []PromptInfo {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]PromptInfo(nil), m.prompts...)
}

// ExpandPrompt renders "server:prompt args" to the prompt's text. The bool
// reports whether the name matched a discovered prompt.
func (m *Manager) ExpandPrompt(ctx context.Context, command, args string) (string, bool, error) {
	if m == nil {
		return "", false, nil
	}
	m.mu.RLock()
	var match *PromptInfo
	for index := range m.prompts {
		if PromptCommandName(m.prompts[index].Server, m.prompts[index].Name) == command {
			match = &m.prompts[index]
			break
		}
	}
	m.mu.RUnlock()
	if match == nil {
		return "", false, nil
	}
	client, ok := m.client(match.Server)
	if !ok {
		return "", true, fmt.Errorf("MCP server %q is not connected", match.Server)
	}
	values := map[string]string{}
	for index, argument := range match.Arguments {
		fields := strings.Fields(args)
		if index < len(fields) {
			values[argument.Name] = fields[index]
		}
	}
	text, err := client.Prompt(ctx, match.Name, values)
	return text, true, err
}

// Tools returns the discovered MCP tools in stable name order.
func (m *Manager) Tools() []tool.Tool {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]tool.Tool(nil), m.tools...)
}

// Servers returns connected server names in sorted order.
func (m *Manager) Servers() []string {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.clients))
	for name := range m.clients {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Instructions returns every connected server's own guidance in name order.
// Servers that sent none are skipped.
func (m *Manager) Instructions() []ServerInstruction {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.clients))
	for name, client := range m.clients {
		if strings.TrimSpace(client.Instructions()) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	out := make([]ServerInstruction, 0, len(names))
	for _, name := range names {
		out = append(out, ServerInstruction{Server: name, Instructions: m.clients[name].Instructions()})
	}
	return out
}

// Resources returns the catalog discovered for one server, or nil when the
// server is unknown.
func (m *Manager) Resources(server string) ([]Resource, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	items, ok := m.resources[server]
	return append([]Resource(nil), items...), ok
}

// ReadResource reads one resource from a connected server.
func (m *Manager) ReadResource(ctx context.Context, server, uri string) ([]ResourceContent, error) {
	client, ok := m.client(server)
	if !ok {
		return nil, fmt.Errorf("MCP server %q is not connected", server)
	}
	return client.ReadResource(ctx, uri)
}

// CallTool invokes one MCP tool by server and original tool name.
func (m *Manager) CallTool(ctx context.Context, server, name string, args map[string]any) (*CallResult, error) {
	client, ok := m.client(server)
	if !ok {
		return nil, fmt.Errorf("MCP server %q is not connected", server)
	}
	return client.CallTool(ctx, name, args)
}

func (m *Manager) client(server string) (*Client, bool) {
	if m == nil {
		return nil, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	client, ok := m.clients[server]
	return client, ok
}

// Close stops every server process.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	clients := m.clients
	m.clients = map[string]*Client{}
	m.mu.Unlock()
	seen := map[*Client]bool{}
	for _, client := range clients {
		if seen[client] {
			continue
		}
		seen[client] = true
		_ = client.Close()
	}
}

type poolGroup struct {
	canonical string
	names     []string
}

func supportedKind(kind string) bool {
	switch kind {
	case "local", "remote", "sse", "http", "streamable", "streamable-http":
		return true
	}
	return false
}

func remoteKind(kind string) bool {
	return kind != "local"
}

// poolKey identifies connections that may be shared. It excludes the server
// name so two entries with identical transport settings pool together.
func poolKey(server Server) string {
	payload, _ := json.Marshal(struct {
		Kind     string
		URL      string
		Headers  map[string]string
		OAuth    OAuthRef
		Protocol string
		Timeout  Timeout
	}{
		Kind:     server.Kind(),
		URL:      strings.TrimSpace(expandEnv(server.URL)),
		Headers:  expandHeaders(server.Headers),
		OAuth:    server.OAuth,
		Protocol: server.ProtocolMode(),
		Timeout:  server.timeouts(),
	})
	return string(payload)
}

var toolNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// ToolName flattens a server and tool pair into one registry name, matching the
// reference implementation's namespace_tool convention.
func ToolName(server, name string) string {
	cleanServer := strings.ReplaceAll(server, ".", "_")
	return toolNameSanitizer.ReplaceAllString(cleanServer, "_") + "_" + toolNameSanitizer.ReplaceAllString(name, "_")
}
