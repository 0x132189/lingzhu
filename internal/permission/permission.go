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

	"lingzhu/internal/policy"
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

// Default matches the primary-agent policy: allow ordinary tools while
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

// WhollyDenies reports whether an action is removed for every resource: the
// last action-wide rule on "*" denies it, and no later rule opens a narrower
// resource again. A narrowed allow keeps the action available so Plan can
// write inside its own directory.
func (r Rules) WhollyDenies(action string) bool {
	denied := false
	for _, rule := range r {
		if !match(action, rule.Action) {
			continue
		}
		if rule.Effect == Allow {
			denied = false
			continue
		}
		if rule.Resource == "*" {
			denied = rule.Effect == Deny
		}
	}
	return denied
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

// DeniesAll reports whether every resource is denied. It is the hard-deny check
// used before an "always" approval is consulted, so a saved allow can never
// lift a rule that was set on purpose.
func (r Rules) DeniesAll(action string, resources []string) bool {
	if len(resources) == 0 {
		return r.Evaluate(action, "*") == Deny
	}
	for _, resource := range resources {
		if r.Evaluate(action, resource) != Deny {
			return false
		}
	}
	return true
}

type Request struct {
	ID        string   `json:"id"`
	SessionID string   `json:"sessionId"`
	Action    string   `json:"action"`
	Resources []string `json:"resources"`
	Message   string   `json:"message,omitempty"`
	// Unattended marks a request from a turn nobody is watching, such as a
	// scheduled wake-up. An ask becomes a deny instead of blocking forever.
	Unattended bool `json:"unattended,omitempty"`
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

// DeclinedError is a user rejection, distinct from a configured deny. The
// reference harness interrupts the current step on a decline, while a deny is
// an ordinary tool failure.
type DeclinedError struct {
	Request Request
	Reason  string
}

func (e *DeclinedError) Error() string {
	if e.Reason != "" {
		return fmt.Sprintf("permission declined by user: %s (%s)", e.Request.Action, e.Reason)
	}
	return fmt.Sprintf("permission declined by user: %s %s", e.Request.Action, strings.Join(e.Request.Resources, ", "))
}

// CorrectedError is a rejection that carries instructions. The tool call fails
// with this text as its output and the turn keeps running, so a user who wants
// a different approach can say so in one step instead of stopping the turn and
// writing a new prompt.
type CorrectedError struct {
	Request  Request
	Feedback string
}

func (e *CorrectedError) Error() string {
	if strings.TrimSpace(e.Feedback) == "" {
		return "permission declined by user"
	}
	return "permission declined by user: " + e.Feedback
}

type ErrNotFound struct{ ID string }

func (e ErrNotFound) Error() string { return "permission request not found: " + e.ID }

type answer struct {
	decision Decision
	message  string
}

// Evaluator extends permission rules with decisions from an external source,
// such as plugins. An empty effect defers to the configured rules; the message
// explains a deny or ask.
type Evaluator func(ctx context.Context, action string, resources []string) (Effect, string)

// Manager holds rules and pending requests. Check blocks until Reply or ctx ends.
type Manager struct {
	mu      sync.Mutex
	rules   Rules
	pending map[string]pendingRequest
	seq     atomic.Int64
	// sessionRules are agent-specific overlays applied after the base rules.
	sessionRules map[string]Rules
	// saved holds the allows learned from "always" answers. They are kept apart
	// from the configured rules so an approval can never lift a configured deny.
	saved Rules
	// saver persists the saved rules after an "always" answer. It runs outside
	// mu but under saveMu so file writes stay ordered.
	saver     func(Rules) error
	saveMu    sync.Mutex
	evaluator Evaluator
	policies  policy.Rules
}

type pendingRequest struct {
	request Request
	reply   chan answer
}

func New(rules Rules) *Manager {
	if len(rules) == 0 {
		rules = Default()
	}
	return &Manager{
		rules:        rules,
		pending:      map[string]pendingRequest{},
		sessionRules: map[string]Rules{},
	}
}

// SetSessionRules installs an agent-specific overlay for one session. Its rules
// are appended after the base rules, so later matches win and a base deny still
// requires an explicit agent allow to be overridden.
func (m *Manager) SetSessionRules(sessionID string, rules Rules) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return
	}
	m.mu.Lock()
	if m.sessionRules == nil {
		m.sessionRules = map[string]Rules{}
	}
	if len(rules) == 0 {
		delete(m.sessionRules, sessionID)
	} else {
		m.sessionRules[sessionID] = append(Rules(nil), rules...)
	}
	m.mu.Unlock()
}

// ClearSessionRules removes the agent overlay when a run ends.
func (m *Manager) ClearSessionRules(sessionID string) {
	m.mu.Lock()
	delete(m.sessionRules, sessionID)
	m.mu.Unlock()
}

// SetSaver registers how "always" answers are persisted. The callback receives
// the complete, already updated rule set; nil keeps changes in memory only.
func (m *Manager) SetSaver(save func(Rules) error) {
	m.mu.Lock()
	m.saver = save
	m.mu.Unlock()
}

// SetEvaluator registers an external permission source. It is consulted after
// the configured rules; a configured deny is final, while an evaluator deny or
// ask takes precedence over a rule allow.
func (m *Manager) SetEvaluator(eval Evaluator) {
	m.mu.Lock()
	m.evaluator = eval
	m.mu.Unlock()
}

// SetPolicies installs hard configuration policies. They are checked before
// permission rules and can never be lifted by an "always allow" answer.
func (m *Manager) SetPolicies(rules policy.Rules) {
	m.mu.Lock()
	m.policies = append(policy.Rules(nil), rules...)
	m.mu.Unlock()
}

// SetSaved installs the allows learned from earlier "always" answers. They are
// kept apart from the configured rules so an approval can never lift a deny.
func (m *Manager) SetSaved(rules Rules) {
	m.mu.Lock()
	m.saved = append(Rules(nil), rules...)
	m.mu.Unlock()
}

// Rules returns the configured rules followed by the learned allow rules.
func (m *Manager) Rules() Rules {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(Rules, 0, len(m.rules)+len(m.saved))
	out = append(out, m.rules...)
	out = append(out, m.saved...)
	return out
}

// Check evaluates the rules; on ask it announces the request and waits for a reply.
//
// The order matters and matches the reference harness:
//  1. hard configuration policies, which no approval can lift;
//  2. a deny from the configured rules (global + agent + session), which an
//     "always" answer saved earlier can never override;
//  3. saved allows, which only ever turn an ask into an allow;
//  4. an external evaluator, then the ask path.
func (m *Manager) Check(ctx context.Context, req Request, announce func(Request)) error {
	m.mu.Lock()
	configured := m.rules
	if overlay := m.sessionRules[req.SessionID]; len(overlay) > 0 {
		merged := make(Rules, 0, len(configured)+len(overlay))
		merged = append(merged, configured...)
		merged = append(merged, overlay...)
		configured = merged
	}
	saved := m.saved
	evaluator := m.evaluator
	policies := m.policies
	m.mu.Unlock()

	resources := req.Resources
	if len(resources) == 0 {
		resources = []string{"*"}
	}
	for _, resource := range resources {
		if policies.Denied("tool.use", req.Action+":"+resource) {
			return &DeniedError{Request: req, Reason: "blocked by configuration policy"}
		}
	}

	// A configured deny is final. Saved allows are deliberately not consulted
	// here, so approving once cannot re-open a tool that was turned off.
	if configured.DeniesAll(req.Action, req.Resources) {
		return &DeniedError{Request: req}
	}
	effect := configured.EvaluateAll(req.Action, req.Resources)
	if effect == Ask && len(saved) > 0 {
		switch saved.EvaluateAll(req.Action, req.Resources) {
		case Allow:
			effect = Allow
		}
	}
	if evaluator != nil {
		evaluated, reason := evaluator(ctx, req.Action, req.Resources)
		switch evaluated {
		case Deny:
			reason = strings.TrimSpace(reason)
			if reason == "" {
				reason = "denied by plugin"
			} else {
				reason = "denied by plugin: " + reason
			}
			return &DeniedError{Request: req, Reason: reason}
		case Ask:
			if strings.TrimSpace(reason) != "" {
				req.Message = reason
			}
			effect = Ask
		case Allow:
			if effect == Ask {
				effect = Allow
			}
		}
	}
	switch effect {
	case Allow:
		return nil
	case Deny:
		return &DeniedError{Request: req}
	}

	// Nobody is watching a scheduled or background turn, so an ask can never be
	// answered. Denying is the only outcome that keeps the turn moving.
	if req.Unattended {
		return &DeniedError{Request: req, Reason: "permission needs approval, and nobody is present to give it"}
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
			// A rejection with feedback keeps the turn alive: the tool fails
			// with the text and the model can act on it. A bare rejection
			// stops the turn, which is what "deny" has always meant.
			if strings.TrimSpace(a.message) != "" {
				return &CorrectedError{Request: req, Feedback: strings.TrimSpace(a.message)}
			}
			return &DeclinedError{Request: req, Reason: a.message}
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
		m.saved = append(m.saved, Rule{Action: req.Action, Resource: resource, Effect: Allow})
	}
	snapshot := append(Rules(nil), m.saved...)
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
