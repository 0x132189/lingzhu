package plugin

import (
	"context"
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"

	"lingzhu/internal/permission"
	"lingzhu/internal/tool"
)

const (
	maxPluginTools    = 64
	maxPluginCommands = 64
	permissionTimeout = 2 * time.Second
)

// Command is one slash command exposed to clients and expanded before a prompt
// reaches the model.
type Command struct {
	Name        string `json:"name"`
	Description string `json:"description"`
	Template    string `json:"template"`
	Plugin      string `json:"plugin"`
}

// Manager owns every connected plugin, the tools they contribute, and the
// commands they expose.
type Manager struct {
	clients    map[string]*Client
	failed     map[string]string
	permission []*Client
	hooks      []*Client
	tools      []tool.Tool
	commands   []Command
	panels     map[string][]PanelInfo
	byName     map[string]Command
	logf       func(string, ...any)
	mu         sync.RWMutex
}

// Start connects enabled plugins, discovers their tools and commands, and
// returns a manager. Connection failures are logged and skipped.
func Start(ctx context.Context, cfg Config, workdir string, logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m := &Manager{
		clients: map[string]*Client{},
		failed:  map[string]string{},
		panels:  map[string][]PanelInfo{},
		byName:  map[string]Command{},
		logf:    logf,
	}
	names := make([]string, 0, len(cfg.Plugins))
	for name := range cfg.Plugins {
		names = append(names, name)
	}
	sort.Strings(names)

	type started struct {
		name     string
		err      error
		client   *Client
		tools    []ToolInfo
		commands []CommandInfo
		panels   []PanelInfo
	}
	results := make(chan started, len(names))
	var wg sync.WaitGroup
	for _, name := range names {
		server := cfg.Plugins[name]
		if !server.on() {
			continue
		}
		wg.Add(1)
		go func(name string, server Server) {
			defer wg.Done()
			client, err := connect(ctx, name, server, workdir)
			if err != nil {
				logf("%v", err)
				results <- started{name: name, err: err}
				return
			}
			item := started{name: name, client: client}
			catalogCtx, cancel := context.WithTimeout(ctx, millis(client.timeout.Catalog))
			defer cancel()
			item.tools, err = client.Tools(catalogCtx)
			if err != nil {
				logf("plugin: list tools for %q: %v", name, err)
			}
			item.commands, err = client.Commands(catalogCtx)
			if err != nil {
				logf("plugin: list commands for %q: %v", name, err)
			}
			if client.Capabilities().Panels {
				item.panels, err = client.Panels(catalogCtx)
				if err != nil {
					logf("plugin: list panels for %q: %v", name, err)
				}
			}
			results <- item
		}(name, server)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	for item := range results {
		if item.client == nil {
			if item.err != nil {
				m.failed[item.name] = item.err.Error()
			}
			continue
		}
		m.clients[item.name] = item.client
		m.panels[item.name] = item.panels
		if item.client.Capabilities().Permissions {
			m.permission = append(m.permission, item.client)
		}
		if item.client.Capabilities().Hooks {
			m.hooks = append(m.hooks, item.client)
		}
		if len(item.tools) > maxPluginTools {
			logf("plugin: %q exposes %d tools, using the first %d", item.name, len(item.tools), maxPluginTools)
			item.tools = item.tools[:maxPluginTools]
		}
		for _, info := range item.tools {
			if strings.TrimSpace(info.Name) == "" {
				continue
			}
			m.tools = append(m.tools, &pluginTool{
				client:      item.client,
				plugin:      item.name,
				rawName:     info.Name,
				name:        ToolName(item.name, info.Name),
				description: info.Description,
				schema:      info.InputSchema,
			})
		}
		if len(item.commands) > maxPluginCommands {
			logf("plugin: %q exposes %d commands, using the first %d", item.name, len(item.commands), maxPluginCommands)
			item.commands = item.commands[:maxPluginCommands]
		}
		for _, info := range item.commands {
			name := strings.TrimSpace(info.Name)
			if !validCommandName(name) || strings.TrimSpace(info.Template) == "" {
				continue
			}
			if _, exists := m.byName[name]; exists {
				logf("plugin: command %q from %q ignored: already defined", name, item.name)
				continue
			}
			command := Command{
				Name:        name,
				Description: strings.TrimSpace(info.Description),
				Template:    info.Template,
				Plugin:      item.name,
			}
			m.byName[name] = command
			m.commands = append(m.commands, command)
		}
	}
	sort.Slice(m.tools, func(i, j int) bool { return m.tools[i].Name() < m.tools[j].Name() })
	sort.Slice(m.commands, func(i, j int) bool { return m.commands[i].Name < m.commands[j].Name })

	// Flattened tool names can collide across plugins. Keep the first one.
	seen := map[string]bool{}
	unique := m.tools[:0]
	for _, item := range m.tools {
		if seen[item.Name()] {
			logf("plugin: duplicate tool name %q skipped", item.Name())
			continue
		}
		seen[item.Name()] = true
		unique = append(unique, item)
	}
	m.tools = unique

	if len(m.clients) > 0 {
		logf("plugins: loaded %d (%d tools, %d commands)", len(m.clients), len(m.tools), len(m.commands))
	}
	sort.Slice(m.hooks, func(i, j int) bool { return m.hooks[i].name < m.hooks[j].name })
	return m
}

// Tools returns the plugin tools in stable name order.
func (m *Manager) Tools() []tool.Tool {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]tool.Tool(nil), m.tools...)
}

// Commands returns slash commands in stable name order.
func (m *Manager) Commands() []Command {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Command(nil), m.commands...)
}

// Expand replaces a matched slash command with its template. The bool reports
// whether the text was recognized; unmatched text is returned unchanged.
func (m *Manager) Expand(text string) (string, bool) {
	if m == nil || !strings.HasPrefix(text, "/") {
		return text, false
	}
	body := strings.TrimPrefix(text, "/")
	index := strings.IndexFunc(body, unicode.IsSpace)
	name := body
	args := ""
	if index >= 0 {
		name = body[:index]
		args = strings.TrimSpace(body[index:])
	}
	m.mu.RLock()
	command, ok := m.byName[name]
	m.mu.RUnlock()
	if !ok {
		return text, false
	}
	return renderCommand(command.Template, args), true
}

func renderCommand(template, args string) string {
	out := strings.ReplaceAll(template, "$ARGUMENTS", args)
	out = strings.ReplaceAll(out, "{{args}}", args)
	if args != "" && !strings.Contains(template, "$ARGUMENTS") && !strings.Contains(template, "{{args}}") {
		out = strings.TrimRight(out, "\n") + "\n\n" + args
	}
	return out
}

// BeforeTool runs plugin tool.execute.before hooks. Arguments can be replaced;
// the first blocked decision wins and stops the tool.
func (m *Manager) BeforeTool(ctx context.Context, sessionID, tool, callID string, arguments json.RawMessage) (json.RawMessage, string, error) {
	if m == nil {
		return arguments, "", nil
	}
	m.mu.RLock()
	clients := append([]*Client(nil), m.hooks...)
	m.mu.RUnlock()
	updated := arguments
	for _, client := range clients {
		decision, err := client.BeforeTool(ctx, ToolHookEvent{
			SessionID: sessionID,
			Tool:      tool,
			CallID:    callID,
			Arguments: updated,
		})
		if err != nil {
			m.logf("plugin: before hook %s: %v", client.name, err)
			continue
		}
		if len(decision.Arguments) > 0 && string(decision.Arguments) != "null" {
			updated = decision.Arguments
		}
		if decision.Blocked {
			return updated, decision.Reason, nil
		}
	}
	return updated, "", nil
}

// AfterTool notifies plugin tool.execute.after hooks.
func (m *Manager) AfterTool(ctx context.Context, sessionID, tool, callID, output string, failed bool) {
	if m == nil {
		return
	}
	m.mu.RLock()
	clients := append([]*Client(nil), m.hooks...)
	m.mu.RUnlock()
	event := ToolHookEvent{SessionID: sessionID, Tool: tool, CallID: callID, Output: output, Failed: failed}
	for _, client := range clients {
		if err := client.Fire("hooks/tool.execute.after", event); err != nil {
			m.logf("plugin: after hook %s: %v", client.name, err)
		}
	}
}

// SessionEvent notifies session.created and session.idle hooks.
func (m *Manager) SessionEvent(ctx context.Context, name, sessionID string) {
	if m == nil {
		return
	}
	m.mu.RLock()
	clients := append([]*Client(nil), m.hooks...)
	m.mu.RUnlock()
	payload := map[string]string{"sessionId": sessionID}
	for _, client := range clients {
		if err := client.Fire("hooks/"+name, payload); err != nil {
			m.logf("plugin: session hook %s: %v", name, err)
		}
	}
}

// Evaluate merges permission effects from plugins. A plugin may deny or ask;
// an allow only helps when the configured rules would otherwise ask. Missing
// methods, errors, and timeouts defer to the configured rules.
func (m *Manager) Evaluate(ctx context.Context, action string, resources []string) (permission.Effect, string) {
	if m == nil {
		return "", ""
	}
	m.mu.RLock()
	clients := append([]*Client(nil), m.permission...)
	m.mu.RUnlock()
	if len(clients) == 0 {
		return "", ""
	}

	ctx, cancel := context.WithTimeout(ctx, permissionTimeout)
	defer cancel()
	type answer struct {
		effect  string
		message string
	}
	results := make(chan answer, len(clients))
	for _, client := range clients {
		go func(client *Client) {
			effect, message := client.Evaluate(ctx, action, resources)
			results <- answer{effect: effect, message: message}
		}(client)
	}

	effect := permission.Effect("")
	message := ""
	for range clients {
		item := <-results
		switch permission.Effect(item.effect) {
		case permission.Deny:
			return permission.Deny, item.message
		case permission.Ask:
			effect = permission.Ask
			if item.message != "" {
				message = item.message
			}
		case permission.Allow:
			if effect == "" {
				effect = permission.Allow
			}
		}
	}
	return effect, message
}

// Close stops every plugin process.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.mu.Lock()
	clients := m.clients
	m.clients = map[string]*Client{}
	m.permission = nil
	m.mu.Unlock()
	for _, client := range clients {
		_ = client.Close()
	}
}

var toolNameSanitizer = regexp.MustCompile(`[^a-zA-Z0-9_]`)

// ToolName flattens a plugin and tool pair into one registry name.
func ToolName(pluginName, name string) string {
	cleanPlugin := toolNameSanitizer.ReplaceAllString(strings.ReplaceAll(pluginName, ".", "_"), "_")
	return cleanPlugin + "_" + toolNameSanitizer.ReplaceAllString(name, "_")
}

func validCommandName(name string) bool {
	if name == "" || name != strings.TrimSpace(name) {
		return false
	}
	return !strings.ContainsAny(name, "/\\ \t\r\n\x00")
}
