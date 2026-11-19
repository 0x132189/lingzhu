package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"lingzhu/internal/tool"
)

// Server status values exposed by the status endpoint.
const (
	StatusConnected = "connected"
	StatusFailed    = "failed"
	StatusDisabled  = "disabled"
	StatusPending   = "pending"
)

// Manager owns every configured MCP server, the connections that succeeded,
// and the tools discovered from them. Entries survive a failed connection so
// the client can show the failure and retry without restarting the core.
type Manager struct {
	mu sync.RWMutex

	states  map[string]*serverState
	clients map[string]*Client

	serverTools   map[string][]tool.Tool
	serverPrompts map[string][]PromptInfo
	resources     map[string][]Resource

	tools   []tool.Tool
	prompts []PromptInfo

	onChange func()
	logf     func(string, ...any)
	workdir  string
	tokens   TokenFunc

	// opMu serializes reconnects; refresh callbacks do not hold it.
	opMu sync.Mutex
}

type serverState struct {
	config Server
	status string
	err    string
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
		states:        map[string]*serverState{},
		clients:       map[string]*Client{},
		serverTools:   map[string][]tool.Tool{},
		serverPrompts: map[string][]PromptInfo{},
		resources:     map[string][]Resource{},
		logf:          logf,
		workdir:       workdir,
		tokens:        tokens,
	}
	names := make([]string, 0, len(cfg.Servers))
	for name := range cfg.Servers {
		names = append(names, name)
	}
	sort.Strings(names)

	// Resolve pool groups before starting any connection, so an alias added
	// after the canonical server still shares the same client.
	type group struct {
		canonical string
		names     []string
	}
	groups := map[string][]string{}
	canonical := map[string]string{}
	byKey := map[string]*group{}
	for _, name := range names {
		server := cfg.Servers[name]
		m.states[name] = &serverState{config: server, status: StatusPending}
		if !server.On() {
			m.states[name].status = StatusDisabled
			continue
		}
		if !supportedKind(server.Kind()) {
			m.states[name].status = StatusFailed
			m.states[name].err = fmt.Sprintf("unsupported transport %q", server.Kind())
			logf("mcp: skipping server %q: unsupported transport %q", name, server.Kind())
			continue
		}
		if !server.Pooled() || !remoteKind(server.Kind()) {
			continue
		}
		key := poolKey(server)
		if existing, ok := byKey[key]; ok {
			existing.names = append(existing.names, name)
			groups[existing.canonical] = append([]string(nil), existing.names...)
			canonical[name] = existing.canonical
			logf("mcp: %q shares the connection of %q", name, existing.canonical)
			continue
		}
		byKey[key] = &group{canonical: name, names: []string{name}}
		groups[name] = []string{name}
		canonical[name] = name
	}

	type started struct {
		names     []string
		client    *Client
		tools     []ToolInfo
		resources []Resource
		prompts   []PromptInfo
		err       error
	}
	results := make(chan started, len(names))
	var wg sync.WaitGroup
	for _, name := range names {
		state := m.states[name]
		if state.status != StatusPending {
			continue
		}
		if canonical[name] != "" && canonical[name] != name {
			continue
		}
		targets := groups[name]
		if len(targets) == 0 {
			targets = []string{name}
		}
		server := state.config
		wg.Add(1)
		go func(name string, targets []string, server Server) {
			defer wg.Done()
			item := started{names: targets}
			client, err := connect(ctx, name, server, workdir, tokens)
			if err != nil {
				item.err = err
				results <- item
				return
			}
			item.client = client
			item.tools, err = client.Tools(ctx)
			if err != nil {
				_ = client.Close()
				item.err = fmt.Errorf("list tools: %w", err)
				results <- item
				return
			}
			item.resources, err = client.Resources(ctx)
			if err != nil {
				logf("mcp: list resources for %q: %v", name, err)
			}
			item.prompts, err = client.Prompts(ctx)
			if err != nil {
				logf("mcp: list prompts for %q: %v", name, err)
			}
			results <- item
		}(name, targets, server)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	for item := range results {
		if item.client == nil {
			m.markFailed(item.names, item.err)
			continue
		}
		m.mu.Lock()
		for _, name := range item.names {
			m.states[name].status = StatusConnected
			m.states[name].err = ""
			m.clients[name] = item.client
			m.serverTools[name] = toolsForServer(name, item.client, item.tools)
			m.serverPrompts[name] = promptsForServer(name, item.prompts)
			m.resources[name] = item.resources
		}
		m.rebuildLocked()
		m.mu.Unlock()
		m.observeClient(item.client)
		if item.client.Closed() {
			m.handleClosed(item.client)
		}
	}
	return m
}

// SetOnChange installs the callback run after a catalog or status change. The
// workspace uses it to reload MCP tools in the tool registry.
func (m *Manager) SetOnChange(callback func()) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.onChange = callback
	m.mu.Unlock()
}

func (m *Manager) notify() {
	if m == nil {
		return
	}
	m.mu.RLock()
	callback := m.onChange
	m.mu.RUnlock()
	if callback != nil {
		callback()
	}
}

func (m *Manager) observeClient(client *Client) {
	if client == nil {
		return
	}
	client.SetHandlers(func(string) { m.refresh(client) }, func() { m.handleClosed(client) })
}

// refresh re-lists all catalogs after a server notification. A notification
// names one catalog, but re-listing all three is both simpler and safe.
func (m *Manager) refresh(client *Client) {
	go func() {
		client.refreshMu.Lock()
		defer client.refreshMu.Unlock()
		if !m.live(client) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		tools, toolsErr := client.Tools(ctx)
		resources, resourcesErr := client.Resources(ctx)
		prompts, promptsErr := client.Prompts(ctx)
		if toolsErr != nil && resourcesErr != nil && promptsErr != nil {
			m.logf("mcp: refresh catalogs: %v", toolsErr)
			return
		}
		m.mu.Lock()
		names := m.aliasesLocked(client)
		if len(names) == 0 {
			m.mu.Unlock()
			return
		}
		if toolsErr == nil {
			for _, name := range names {
				m.serverTools[name] = toolsForServer(name, client, tools)
			}
		} else {
			m.logf("mcp: refresh tools: %v", toolsErr)
		}
		if resourcesErr == nil {
			for _, name := range names {
				m.resources[name] = resources
			}
		} else {
			m.logf("mcp: refresh resources: %v", resourcesErr)
		}
		if promptsErr == nil {
			for _, name := range names {
				m.serverPrompts[name] = promptsForServer(name, prompts)
			}
		} else {
			m.logf("mcp: refresh prompts: %v", promptsErr)
		}
		m.rebuildLocked()
		m.mu.Unlock()
		m.notify()
	}()
}

func (m *Manager) live(client *Client) bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return len(m.aliasesLocked(client)) > 0
}

func (m *Manager) aliasesLocked(client *Client) []string {
	var names []string
	for name, current := range m.clients {
		if current == client {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

func (m *Manager) handleClosed(client *Client) {
	m.mu.Lock()
	names := m.aliasesLocked(client)
	if len(names) == 0 {
		m.mu.Unlock()
		return
	}
	for _, name := range names {
		delete(m.clients, name)
		delete(m.serverTools, name)
		delete(m.serverPrompts, name)
		delete(m.resources, name)
		if state := m.states[name]; state != nil {
			state.status = StatusFailed
			state.err = "connection closed"
		}
	}
	m.rebuildLocked()
	m.mu.Unlock()
	m.logf("mcp: connection closed for %s", strings.Join(names, ", "))
	m.notify()
}

// Reconnect closes the current connection (and every alias sharing it) and
// starts a new one with the configured server. It is the manual recovery path
// for a failed server; it is safe to call while the session is idle.
func (m *Manager) Reconnect(ctx context.Context, name string) (ServerStatus, error) {
	if m == nil {
		return ServerStatus{}, fmt.Errorf("MCP is not available")
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.RLock()
	state, ok := m.states[name]
	if !ok {
		m.mu.RUnlock()
		return ServerStatus{}, fmt.Errorf("MCP server %q is not configured", name)
	}
	cfg := state.config
	old := m.clients[name]
	names := m.aliasesLocked(old)
	if len(names) == 0 {
		names = []string{name}
	}
	m.mu.RUnlock()

	if !cfg.On() {
		return m.statusFor(name), fmt.Errorf("MCP server %q is disabled", name)
	}
	if !supportedKind(cfg.Kind()) {
		err := fmt.Errorf("MCP server %q uses unsupported transport %q", name, cfg.Kind())
		m.markFailed(names, err)
		return m.statusFor(name), err
	}

	if old != nil {
		_ = old.Close()
	}
	m.mu.Lock()
	for _, alias := range names {
		delete(m.clients, alias)
		delete(m.serverTools, alias)
		delete(m.serverPrompts, alias)
		delete(m.resources, alias)
		if current := m.states[alias]; current != nil {
			current.status = StatusPending
			current.err = ""
		}
	}
	m.rebuildLocked()
	m.mu.Unlock()
	m.notify()

	client, err := connect(ctx, name, cfg, m.workdir, m.tokens)
	if err != nil {
		m.markFailed(names, err)
		return m.statusFor(name), err
	}
	tools, err := client.Tools(ctx)
	if err != nil {
		_ = client.Close()
		err = fmt.Errorf("list tools: %w", err)
		m.markFailed(names, err)
		return m.statusFor(name), err
	}
	resources, err := client.Resources(ctx)
	if err != nil {
		m.logf("mcp: list resources for %q: %v", name, err)
	}
	prompts, err := client.Prompts(ctx)
	if err != nil {
		m.logf("mcp: list prompts for %q: %v", name, err)
	}

	m.mu.Lock()
	for _, alias := range names {
		if current := m.states[alias]; current != nil {
			current.status = StatusConnected
			current.err = ""
		}
		m.clients[alias] = client
		m.serverTools[alias] = toolsForServer(alias, client, tools)
		m.serverPrompts[alias] = promptsForServer(alias, prompts)
		m.resources[alias] = resources
	}
	m.rebuildLocked()
	m.mu.Unlock()
	m.observeClient(client)
	if client.Closed() {
		m.handleClosed(client)
	}
	m.notify()
	return m.statusFor(name), nil
}

func (m *Manager) markFailed(names []string, err error) {
	if m == nil || len(names) == 0 {
		return
	}
	message := "connection failed"
	if err != nil {
		message = err.Error()
	}
	m.mu.Lock()
	for _, name := range names {
		delete(m.clients, name)
		delete(m.serverTools, name)
		delete(m.serverPrompts, name)
		delete(m.resources, name)
		if state := m.states[name]; state != nil {
			state.status = StatusFailed
			state.err = message
		}
	}
	m.rebuildLocked()
	m.mu.Unlock()
	m.logf("mcp: %s: %s", strings.Join(names, ", "), message)
	m.notify()
}

func (m *Manager) rebuildLocked() {
	var tools []tool.Tool
	for _, items := range m.serverTools {
		tools = append(tools, items...)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name() < tools[j].Name() })
	seen := map[string]bool{}
	unique := tools[:0]
	for _, item := range tools {
		if seen[item.Name()] {
			continue
		}
		seen[item.Name()] = true
		unique = append(unique, item)
	}
	m.tools = unique

	var prompts []PromptInfo
	for _, items := range m.serverPrompts {
		prompts = append(prompts, items...)
	}
	sort.Slice(prompts, func(i, j int) bool {
		if prompts[i].Server != prompts[j].Server {
			return prompts[i].Server < prompts[j].Server
		}
		return prompts[i].Name < prompts[j].Name
	})
	m.prompts = prompts
}

func toolsForServer(server string, client *Client, items []ToolInfo) []tool.Tool {
	out := make([]tool.Tool, 0, len(items))
	for _, info := range items {
		out = append(out, &mcpTool{
			client:      client,
			server:      server,
			rawName:     info.Name,
			name:        ToolName(server, info.Name),
			description: info.Description,
			schema:      info.InputSchema,
		})
	}
	return out
}

func promptsForServer(server string, items []PromptInfo) []PromptInfo {
	out := make([]PromptInfo, 0, len(items))
	for _, item := range items {
		item.Server = server
		out = append(out, item)
	}
	return out
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
			item := m.prompts[index]
			match = &item
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
	fields := splitPromptArgs(args)
	for index, argument := range match.Arguments {
		if index < len(fields) {
			values[argument.Name] = fields[index]
		}
	}
	text, err := client.Prompt(ctx, match.Name, values)
	return text, true, err
}

// splitPromptArgs splits command arguments on whitespace while keeping a
// quoted argument together. Both quote styles and backslash escapes work.
func splitPromptArgs(input string) []string {
	var out []string
	var current strings.Builder
	quote := rune(0)
	escaped := false
	started := false
	flush := func() {
		if started {
			out = append(out, current.String())
			current.Reset()
			started = false
		}
	}
	for _, r := range input {
		if escaped {
			current.WriteRune(r)
			started = true
			escaped = false
			continue
		}
		if quote != 0 {
			if r == '\\' && quote == '"' {
				escaped = true
				continue
			}
			if r == quote {
				quote = 0
				started = true
				continue
			}
			current.WriteRune(r)
			started = true
			continue
		}
		switch r {
		case '"', '\'':
			quote = r
			started = true
		case ' ', '\t', '\n', '\r':
			flush()
		default:
			current.WriteRune(r)
			started = true
		}
	}
	if escaped {
		current.WriteRune('\\')
	}
	flush()
	return out
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
	m.serverTools = map[string][]tool.Tool{}
	m.serverPrompts = map[string][]PromptInfo{}
	m.resources = map[string][]Resource{}
	m.tools = nil
	m.prompts = nil
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
