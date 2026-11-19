package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"lingzhu/internal/rpc"
	"lingzhu/internal/tool"
)

// AttachedTransport is a caller-owned connection to an MCP server. Start
// delivers decoded JSON-RPC frames to onMessage and calls onClose when the
// connection ends; Send writes one frame; Close stops it.
type AttachedTransport interface {
	Start(onMessage func([]byte), onClose func()) error
	Send(ctx context.Context, message []byte) error
	Close() error
}

type attachedAdapter struct {
	inner AttachedTransport
}

func (a attachedAdapter) Send(ctx context.Context, message []byte, _ int64) error {
	return a.inner.Send(ctx, message)
}

func (a attachedAdapter) Close() error { return a.inner.Close() }

type attachedEntry struct {
	client    *Client
	tools     []ToolInfo
	resources []Resource
	prompts   []PromptInfo
	remove    func()
}

type attachedBase struct {
	present   bool
	client    *Client
	tools     []tool.Tool
	prompts   []PromptInfo
	resources []Resource
}

// Attach connects a client-hosted MCP server. The returned function removes
// this connection; attaching the same name again stacks the new connection on
// top and the previous one takes over when it is removed. Attached servers do
// not appear in status or the configured-server list.
func (m *Manager) Attach(ctx context.Context, name string, transport AttachedTransport) (func(), error) {
	if m == nil {
		return nil, errors.New("MCP is not available")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, errors.New("MCP server name is empty")
	}
	if transport == nil {
		return nil, errors.New("MCP transport is required")
	}
	m.opMu.Lock()
	defer m.opMu.Unlock()

	client, err := connectAttached(ctx, name, m.workdir, transport)
	if err != nil {
		return nil, err
	}
	tools, err := client.Tools(ctx)
	if err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("list tools: %w", err)
	}
	resources, _ := client.Resources(ctx)
	prompts, _ := client.Prompts(ctx)
	entry := &attachedEntry{client: client, tools: tools, resources: resources, prompts: prompts}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		_ = client.Close()
		return nil, errors.New("MCP is closed")
	}
	if m.attached == nil {
		m.attached = map[string][]*attachedEntry{}
	}
	if m.attachBase == nil {
		m.attachBase = map[string]attachedBase{}
	}
	if len(m.attached[name]) == 0 {
		m.attachBase[name] = m.baseLocked(name)
	}
	m.attached[name] = append(m.attached[name], entry)
	m.installAttachedLocked(name, entry)
	m.rebuildLocked()
	m.mu.Unlock()

	client.SetLogger(m.logf)
	remove := m.observeClient(client)
	m.mu.Lock()
	entry.remove = remove
	m.mu.Unlock()
	m.notify()

	var once sync.Once
	return func() {
		once.Do(func() { m.detachAttached(name, entry) })
	}, nil
}

func connectAttached(ctx context.Context, name, workdir string, transport AttachedTransport) (*Client, error) {
	client := newClient(name, Server{}, workdir)
	client.tr = attachedAdapter{inner: transport}
	if err := transport.Start(func(data []byte) {
		var message rpc.Message
		if err := json.Unmarshal(data, &message); err == nil {
			client.receive(message)
		}
	}, client.closed); err != nil {
		return nil, err
	}
	initCtx, cancel := context.WithTimeout(ctx, millis(client.timeout.Startup))
	defer cancel()
	if err := client.initialize(initCtx); err != nil {
		_ = transport.Close()
		client.closed()
		return nil, err
	}
	return client, nil
}

func (m *Manager) baseLocked(name string) attachedBase {
	base := attachedBase{
		client:    m.clients[name],
		tools:     m.serverTools[name],
		prompts:   m.serverPrompts[name],
		resources: m.resources[name],
	}
	base.present = base.client != nil || base.tools != nil || base.prompts != nil || base.resources != nil
	return base
}

func (m *Manager) installAttachedLocked(name string, entry *attachedEntry) {
	codemode := true
	if state := m.states[name]; state != nil {
		codemode = state.config.CodemodeEnabled()
	}
	m.clients[name] = entry.client
	m.serverTools[name] = toolsForServer(name, entry.client, entry.tools, codemode)
	m.serverPrompts[name] = promptsForServer(name, entry.prompts)
	m.resources[name] = entry.resources
}

func (m *Manager) detachAttached(name string, entry *attachedEntry) {
	m.opMu.Lock()
	defer m.opMu.Unlock()

	m.mu.Lock()
	stack := m.attached[name]
	index := -1
	for i := len(stack) - 1; i >= 0; i-- {
		if stack[i] == entry {
			index = i
			break
		}
	}
	if index < 0 {
		m.mu.Unlock()
		return
	}
	stack = append(stack[:index], stack[index+1:]...)
	if len(stack) > 0 {
		m.attached[name] = stack
		m.installAttachedLocked(name, stack[len(stack)-1])
	} else {
		delete(m.attached, name)
		base := m.attachBase[name]
		delete(m.attachBase, name)
		if base.present {
			m.clients[name] = base.client
			m.serverTools[name] = base.tools
			m.serverPrompts[name] = base.prompts
			m.resources[name] = base.resources
		} else {
			delete(m.clients, name)
			delete(m.serverTools, name)
			delete(m.serverPrompts, name)
			delete(m.resources, name)
		}
	}
	m.rebuildLocked()
	m.mu.Unlock()

	if entry.remove != nil {
		entry.remove()
	}
	_ = entry.client.Close()
	m.notify()
}
