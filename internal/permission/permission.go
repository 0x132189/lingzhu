// Package permission evaluates action-level permissions before a tool runs.
package permission

import (
	"context"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

type Effect string

const (
	Allow Effect = "allow"
	Ask   Effect = "ask"
	Deny  Effect = "deny"
)

type Decision string

const (
	Once   Decision = "once"
	Always Decision = "always"
	Reject Decision = "reject"
)

// Rule is a permission rule: action and resource support * and ?, and the last
// matching rule wins.
type Rule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   Effect `json:"effect"`
}

type Rules []Rule

// Default matches opencode's primary-agent policy: allow ordinary tools while
// still asking before reading environment files that often hold secrets.
func Default() Rules {
	return Rules{
		{Action: "*", Resource: "*", Effect: Allow},
		{Action: "read", Resource: "*.env", Effect: Ask},
		{Action: "read", Resource: "*.env.*", Effect: Ask},
		{Action: "read", Resource: "*.env.example", Effect: Allow},
	}
}

// Evaluate returns the effective effect for a single resource.
func (r Rules) Evaluate(action, resource string) Effect {
	effect := Ask
	for _, rule := range r {
		if match(action, rule.Action) && match(resource, rule.Resource) {
			effect = rule.Effect
		}
	}
	return effect
}

// EvaluateAll merges resources: any deny wins, otherwise any ask wins.
func (r Rules) EvaluateAll(action string, resources []string) Effect {
	if len(resources) == 0 {
		return r.Evaluate(action, "*")
	}
	effect := Allow
	for _, resource := range resources {
		switch r.Evaluate(action, resource) {
		case Deny:
			return Deny
		case Ask:
			effect = Ask
		}
	}
	return effect
}

type Request struct {
	ID        string   `json:"id"`
	SessionID string   `json:"sessionId"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
	Message   string   `json:"message,omitempty"`
}

type DeniedError struct {
	Request Request
	Reason  string
}

func (e *DeniedError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("permission denied: %s (%s)", e.Request.Action, e.Reason)
	}
	return fmt.Sprintf("permission denied: %s %s", e.Request.Action, strings.Join(e.Request.Resources, ", "))
}

type ErrNotFound struct{ ID string }

func (e ErrNotFound) Error() string { return "permission request not found: " + e.ID }

type answer struct {
	decision Decision
	message  string
}

// Manager holds rules and pending requests. Check blocks until Reply or ctx ends.
type Manager struct {
	mu      sync.Mutex
	rules   Rules
	pending map[string]pendingRequest
	seq     atomic.Int64
	// saver persists the full rule set after an "always" answer. It runs
	// outside mu but under saveMu so file writes stay ordered.
	saver  func(Rules) error
	saveMu sync.Mutex
}

type pendingRequest struct {
	request Request
	reply   chan answer
}

func New(rules Rules) *Manager {
	if len(rules) == 0 {
		rules = Default()
	}
	return &Manager{rules: rules, pending: map[string]pendingRequest{}}
}

// SetSaver registers how "always" answers are persisted. The callback receives
// the complete, already updated rule set; nil keeps changes in memory only.
func (m *Manager) SetSaver(save func(Rules) error) {
	m.mu.Lock()
	m.saver = save
	m.mu.Unlock()
}

func (m *Manager) Rules() Rules {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append(Rules(nil), m.rules...)
}

// Check evaluates the rules; on ask it announces the request and waits for a reply.
func (m *Manager) Check(ctx context.Context, req Request, announce func(Request)) error {
	effect := m.rules.EvaluateAll(req.Action, req.Resources)
	switch effect {
	case Allow:
		return nil
	case Deny:
		return &DeniedError{Request: req}
	}

	if req.ID == "" {
		req.ID = fmt.Sprintf("per_%d", m.seq.Add(1))
	}
	ch := make(chan answer, 1)
	m.mu.Lock()
	m.pending[req.ID] = pendingRequest{request: req, reply: ch}
	m.mu.Unlock()
	defer m.release(req.ID)

	if announce != nil {
		announce(req)
	}

	select {
	case a := <-ch:
		if a.decision == Reject {
			return &DeniedError{Request: req, Reason: a.message}
		}
		if a.decision == Always {
			m.save(req)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (m *Manager) Reply(id string, decision Decision, message string) error {
	m.mu.Lock()
	item, ok := m.pending[id]
	m.mu.Unlock()
	if !ok {
		return ErrNotFound{ID: id}
	}
	select {
	case item.reply <- answer{decision: decision, message: message}:
		return nil
	default:
		return errors.New("permission request already answered: " + id)
	}
}

func (m *Manager) Pending(sessionID string) []Request {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Request, 0, len(m.pending))
	for _, item := range m.pending {
		if sessionID != "" && item.request.SessionID != sessionID {
			continue
		}
		out = append(out, item.request)
	}
	return out
}

func (m *Manager) save(req Request) {
	// Serialize persistence so two quick "always" answers cannot write stale
	// snapshots over each other.
	m.saveMu.Lock()
	defer m.saveMu.Unlock()

	m.mu.Lock()
	for _, resource := range req.Resources {
		m.rules = append(m.rules, Rule{Action: req.Action, Resource: resource, Effect: Allow})
	}
	snapshot := append(Rules(nil), m.rules...)
	saver := m.saver
	m.mu.Unlock()

	if saver == nil {
		return
	}
	if err := saver(snapshot); err != nil {
		// The answer already unblocked the tool; a write failure must not turn
		// an approval into a rejection.
		log.Printf("permission: save rules: %v", err)
	}
}

func (m *Manager) release(id string) {
	m.mu.Lock()
	delete(m.pending, id)
	m.mu.Unlock()
}

// match is a case-insensitive wildcard match: * matches any run, ? one character.
func match(input, pattern string) bool {
	normalized := strings.ReplaceAll(input, "\\", "/")
	pattern = strings.ReplaceAll(pattern, "\\", "/")

	var b strings.Builder
	b.WriteString("(?is)^")
	for _, r := range pattern {
		switch r {
		case '*':
			b.WriteString(".*")
		case '?':
			b.WriteString(".")
		default:
			b.WriteString(regexp.QuoteMeta(string(r)))
		}
	}
	b.WriteString("$")

	re, err := regexp.Compile(b.String())
	if err != nil {
		return false
	}
	return re.MatchString(normalized)
}
