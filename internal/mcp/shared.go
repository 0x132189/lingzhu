package mcp

import (
	"context"
	"sync"
)

// sharedEligible reports whether one remote server may be shared between
// worktrees. Only the modern revision is pooled, matching the reference
// implementation; legacy clients keep one connection per location. Sampling
// and elicitation stay private because their handlers belong to a location.
func sharedEligible(server Server) bool {
	return server.Pooled() &&
		server.ProtocolMode() == "2026-07-28" &&
		remoteKind(server.Kind()) &&
		!server.Sampling &&
		!server.Elicitation
}

func sharedKey(name string, server Server) string {
	return name + "\x00" + poolKey(server)
}

type sharedEntry struct {
	client *Client
	refs   int
	ready  chan struct{}
	err    error
}

type sharedPool struct {
	mu      sync.Mutex
	entries map[string]*sharedEntry
}

var processPool = &sharedPool{entries: map[string]*sharedEntry{}}

// acquire joins an existing pooled connection or opens one. The release
// function detaches this caller; the last caller closes the client.
func (p *sharedPool) acquire(ctx context.Context, key string, open func() (*Client, error)) (*Client, func(), error) {
	p.mu.Lock()
	if entry, ok := p.entries[key]; ok {
		entry.refs++
		p.mu.Unlock()
		<-entry.ready
		if entry.err != nil {
			p.release(key, entry)
			return nil, nil, entry.err
		}
		return entry.client, func() { p.release(key, entry) }, nil
	}
	entry := &sharedEntry{refs: 1, ready: make(chan struct{})}
	p.entries[key] = entry
	p.mu.Unlock()

	client, err := open()
	if client != nil {
		client.shared = true
	}
	p.mu.Lock()
	entry.client = client
	entry.err = err
	if err != nil {
		delete(p.entries, key)
	}
	close(entry.ready)
	p.mu.Unlock()
	if err != nil {
		return nil, nil, err
	}
	// The listener only unregisters the slot; closing is driven by refcount.
	client.AddHandlers(nil, func() { p.drop(key, entry) })
	return client, func() { p.release(key, entry) }, nil
}

func (p *sharedPool) release(key string, entry *sharedEntry) {
	p.mu.Lock()
	current, ok := p.entries[key]
	if !ok || current != entry {
		p.mu.Unlock()
		return
	}
	entry.refs--
	if entry.refs > 0 {
		p.mu.Unlock()
		return
	}
	delete(p.entries, key)
	client := entry.client
	p.mu.Unlock()
	if client != nil {
		_ = client.Close()
	}
}

func (p *sharedPool) drop(key string, entry *sharedEntry) {
	p.mu.Lock()
	if current, ok := p.entries[key]; ok && current == entry {
		delete(p.entries, key)
	}
	p.mu.Unlock()
}
