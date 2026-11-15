// Package subagent runs child sessions for delegated tasks. Foreground calls
// return the child's final message; background calls return immediately and
// deliver a synthetic completion notice to the parent session.
package subagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lingzhu/internal/agents"
	"lingzhu/internal/event"
	"lingzhu/internal/permission"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
)

const (
	generalAgent       = "general"
	subagentIDPrefix   = "sub_"
	subagentTitleLabel = "[subagent] "
)

// Runner is the part of the agent the subagent tool needs.
type Runner interface {
	Run(ctx context.Context, sessionID, prompt string, selection provider.Selection) (<-chan event.Event, error)
	// RunUnattended starts a turn whose permission asks are denied instead of
	// waiting for a client that is not watching.
	RunUnattended(ctx context.Context, sessionID, prompt string, selection provider.Selection) (<-chan event.Event, error)
	Notify(sessionID string, ev event.Event) error
	Resolve(selection provider.Selection) (provider.Target, error)
}

// Manager creates and continues child sessions.
type Manager struct {
	runner Runner
	store  *session.Store
	logf   func(string, ...any)
	agents *agents.Registry

	depth int

	mu       sync.Mutex
	parents  map[string]string
	started  map[string]time.Time
	notified map[string]bool
	seq      atomic.Int64
}

// Child is one child session created by a parent session.
type Child struct {
	ID      string    `json:"id"`
	Title   string    `json:"title,omitempty"`
	Agent   string    `json:"agent,omitempty"`
	Started time.Time `json:"started"`
}

// New creates a subagent manager.
func New(runner Runner, store *session.Store, logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	return &Manager{
		runner:   runner,
		store:    store,
		logf:     logf,
		depth:    1,
		parents:  map[string]string{},
		started:  map[string]time.Time{},
		notified: map[string]bool{},
	}
}

// SetDepth sets how many nested subagent levels are allowed. Values below one
// keep the default of one.
func (m *Manager) SetDepth(depth int) {
	if depth > 0 {
		m.depth = depth
	}
}

// depthOf counts the parent chain stored on the session, so the limit survives
// a process restart.
func (m *Manager) depthOf(sessionID string) int {
	depth := 0
	current := strings.TrimSpace(sessionID)
	if strings.HasPrefix(current, subagentIDPrefix) {
		depth = 1
	}
	if m.store == nil {
		return depth
	}
	for current != "" && depth < 64 {
		info, err := m.store.Info(current)
		if err != nil || info.Parent == "" {
			break
		}
		current = info.Parent
		depth++
	}
	return depth
}

// Children lists the child sessions created for one parent session.
func (m *Manager) Children(parentID string) []Child {
	m.mu.Lock()
	type entry struct {
		id      string
		started time.Time
	}
	entries := make([]entry, 0, len(m.parents))
	for id, owner := range m.parents {
		if owner != parentID {
			continue
		}
		entries = append(entries, entry{id: id, started: m.started[id]})
	}
	m.mu.Unlock()

	children := make([]Child, 0, len(entries))
	for _, item := range entries {
		child := Child{ID: item.id, Started: item.started}
		if m.store != nil {
			if info, err := m.store.Info(item.id); err == nil {
				child.Title = info.Title
			}
			if meta, err := m.store.Meta(item.id); err == nil {
				child.Agent = meta.Agent
			}
		}
		children = append(children, child)
	}
	sort.Slice(children, func(i, j int) bool { return children[i].Started.Before(children[j].Started) })
	return children
}

// SetAgents attaches the profile registry used to validate the agent argument.
func (m *Manager) SetAgents(registry *agents.Registry) {
	m.agents = registry
}

// Tool returns the model-facing subagent tool.
func (m *Manager) Tool() *Tool { return &Tool{manager: m} }

// Tool runs one child session.
type Tool struct {
	manager *Manager
}

func (t *Tool) Name() string { return "subagent" }

func (t *Tool) Description() string { return t.description(nil) }

// DescriptionFor hides the agents the calling profile may not spawn, so the
// model is never offered a choice that would only fail.
func (t *Tool) DescriptionFor(rules permission.Rules) string { return t.description(rules) }

// description renders the tool description. rules are the calling agent's
// permission rules; agents it may not spawn are left out, so the model is never
// offered a choice that would only fail.
func (t *Tool) description(rules permission.Rules) string {
	description := "Spawns an agent in a child session to work on the specified task.\n\n" +
		"The output includes a sessionID you can pass back later to continue that specific conversation with the subagent.\n" +
		"New child sessions start with fresh context, so include all relevant context and instructions when you don't pass a sessionID.\n" +
		"Foreground (default) runs the subagent to completion and returns its final response.\n" +
		"Background mode (background=true) launches it asynchronously and returns immediately; you are notified when it finishes.\n" +
		"Use background only for independent work that can run while you continue elsewhere."
	if choices := t.manager.choices(rules); choices != "" {
		description += "\n\nAvailable agents:\n" + choices
	}
	return description
}

// choices renders the subagent profiles the caller may use for the tool
// description, sorted by id.
func (m *Manager) choices(rules permission.Rules) string {
	if m.agents == nil {
		return "- general: General-purpose agent for multi-step tasks."
	}
	var lines []string
	for _, profile := range m.agents.Subagents() {
		if rules.Evaluate("subagent", profile.ID) == permission.Deny {
			continue
		}
		description := profile.Description
		if strings.TrimSpace(description) == "" {
			description = "This subagent should only be called when explicitly requested."
		}
		lines = append(lines, fmt.Sprintf("- %s: %s", profile.ID, description))
	}
	return strings.Join(lines, "\n")
}

// agentIDs lists valid subagent choices for error messages.
func (m *Manager) agentIDs() string {
	if m.agents == nil {
		return generalAgent
	}
	var ids []string
	for _, profile := range m.agents.Subagents() {
		ids = append(ids, profile.ID)
	}
	if len(ids) == 0 {
		return generalAgent
	}
	return strings.Join(ids, ", ")
}

// profile validates an agent choice for the subagent tool.
func (m *Manager) profile(id string) (agents.Profile, bool) {
	if m.agents == nil {
		if id == generalAgent {
			return agents.Profile{ID: generalAgent, Name: "General", Mode: agents.ModeSubagent}, true
		}
		return agents.Profile{}, false
	}
	profile, ok := m.agents.Get(id)
	if !ok || profile.Mode == agents.ModePrimary || profile.Hidden {
		return agents.Profile{}, false
	}
	return profile, true
}

func (t *Tool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"description": {"type": "string", "description": "A short 3-5 word label for the task, displayed to the user"},
			"prompt": {"type": "string", "description": "The task for the subagent to perform"},
			"agent": {"type": "string", "description": "The type of specialized agent to use for this task. If the user asks for a subagent by a name that is not one of the available subagents, they most likely mean a model: pick a suitable agent and pass the name through the model parameter instead."},
			"model": {"type": "string", "description": "NEVER set this unless the user explicitly asks for a particular model or variant. It must be a chat model; image and decision models can't run subagents. The value is written as \"providerID/modelID\", or \"providerID/modelID#variant\" to include a variant. Do not guess the ID: look the model up with the models tool, filtering to your own provider first."},
			"background": {"type": "boolean", "description": "Run the subagent in the background and return immediately. You will be notified when it completes. DO NOT sleep, poll, or proactively check on its progress."},
			"sessionID": {"type": "string", "description": "Send a follow-up to an existing subagent, running or finished. Omit to start a new one."}
		},
		"required": ["agent", "description", "prompt"]
	}`)
}

func (t *Tool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		Description string `json:"description"`
		Prompt      string `json:"prompt"`
		Agent       string `json:"agent"`
		Model       string `json:"model"`
		Background  bool   `json:"background"`
		SessionID   string `json:"sessionID"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("subagent: %w", err)
	}
	prompt := strings.TrimSpace(args.Prompt)
	if prompt == "" {
		return "", fmt.Errorf("subagent: prompt is empty")
	}
	description := strings.TrimSpace(args.Description)
	if description == "" {
		description = firstLine(prompt)
	}
	if t.manager.depthOf(call.SessionID) >= t.manager.depth {
		return "", fmt.Errorf("subagent: depth limit reached (%d); increase subagent_depth to allow nested subagents", t.manager.depth)
	}
	agentID := strings.TrimSpace(args.Agent)
	explicitAgent := agentID != ""
	if agentID == "" {
		agentID = generalAgent
	}
	if _, ok := t.manager.profile(agentID); !ok {
		return "", fmt.Errorf("subagent: unknown agent %q (available: %s)", agentID, t.manager.agentIDs())
	}
	if err := call.Allow(ctx, "subagent", []string{agentID}, ""); err != nil {
		return "", err
	}

	selection, childID, err := t.prepare(call.SessionID, args.SessionID, agentID, explicitAgent, args.Model)
	if err != nil {
		return "", err
	}
	created := strings.TrimSpace(args.SessionID) == ""
	call.SetMetadata("sessionID", childID)
	call.SetMetadata("background", args.Background)

	runCtx := ctx
	run := t.manager.runner.Run
	if args.Background {
		// The child outlives this tool call, but still stops on agent shutdown
		// because Agent.Run links every run to its base context. Nobody is
		// guaranteed to watch the child session, so a permission ask is denied
		// instead of blocking forever.
		runCtx = context.Background()
		run = t.manager.runner.RunUnattended
		if t.manager.store != nil {
			childSeq := int64(0)
			if events, err := t.manager.store.Events(childID); err == nil && len(events) > 0 {
				childSeq = events[len(events)-1].Seq
			}
			if _, err := t.manager.store.Append(call.SessionID, event.SubagentStartedEvent(childID, description, agentID, childSeq)); err != nil {
				return "", fmt.Errorf("subagent: %w", err)
			}
		}
	}
	stream, err := run(runCtx, childID, prompt, selection)
	if err != nil {
		if args.Background {
			t.deliver(call.SessionID, childID, description, agentID, output{reason: "error", errText: err.Error()})
		}
		return "", fmt.Errorf("subagent: %w", err)
	}
	if created && t.manager.store != nil {
		// The session row now has a user message, so the durable title update
		// applies. A title failure must not fail the child run.
		if err := t.manager.store.SetTitle(childID, subagentTitleLabel+firstLine(description)); err != nil {
			t.manager.logf("subagent: title %s: %v", childID, err)
		}
	}
	if args.Background {
		go t.finish(call.SessionID, childID, description, agentID, stream)
		return fmt.Sprintf("The subagent is working in the background (sessionID: %s).\nYou will be notified automatically when it finishes.\nTo send it follow-ups or change its instructions while it runs, call subagent again with this sessionID.", childID), nil
	}
	out := collect(stream)
	suffix := fmt.Sprintf("\n\n(subagent session: %s)", childID)
	if out.reason != "done" || out.errText != "" {
		message := out.errText
		if message == "" {
			message = "ended: " + out.reason
		}
		if out.text != "" {
			return out.text + suffix, fmt.Errorf("subagent: %s", message)
		}
		return "", fmt.Errorf("subagent: %s", message)
	}
	if strings.TrimSpace(out.text) == "" {
		out.text = "Subagent completed without a text response."
	}
	return out.text + suffix, nil
}

// prepare resolves the agent, model and child session.
func (t *Tool) prepare(parentID, requestedID, agentID string, explicitAgent bool, modelOverride string) (provider.Selection, string, error) {
	selection := provider.Selection{}
	if t.manager.store != nil {
		if meta, err := t.manager.store.Meta(parentID); err == nil {
			selection = provider.Selection{Provider: meta.Provider, Model: meta.Model}
		}
	}
	profile, _ := t.manager.profile(agentID)
	childID := strings.TrimSpace(requestedID)
	// switched reports that a continuation moves the child to a different agent.
	switched := false
	if childID != "" && !explicitAgent && t.manager.store != nil {
		if meta, err := t.manager.store.Meta(childID); err == nil && meta.Agent != "" {
			if existing, ok := t.manager.profile(meta.Agent); ok {
				agentID = meta.Agent
				profile = existing
			}
		}
	}
	if modelOverride != "" {
		parsed, err := parseModel(modelOverride)
		if err != nil {
			return provider.Selection{}, "", err
		}
		selection = parsed
	} else if childID == "" && profile.Model != "" {
		providerID, modelID, err := agents.ValidateModel(profile.Model)
		if err != nil {
			return provider.Selection{}, "", fmt.Errorf("subagent: agent %s: %w", profile.ID, err)
		}
		selection = provider.Selection{Provider: providerID, Model: modelID}
	}

	if childID != "" {
		if !strings.HasPrefix(childID, subagentIDPrefix) {
			return provider.Selection{}, "", fmt.Errorf("subagent: %q is not a subagent session", childID)
		}
		if t.manager.store != nil {
			if _, err := t.manager.store.Meta(childID); err != nil {
				return provider.Selection{}, "", fmt.Errorf("subagent: session %q not found", childID)
			}
		}
		t.manager.mu.Lock()
		owner := t.manager.parents[childID]
		t.manager.mu.Unlock()
		if owner == "" && t.manager.store != nil {
			if info, err := t.manager.store.Info(childID); err == nil {
				owner = info.Parent
				if owner != "" {
					t.manager.mu.Lock()
					t.manager.parents[childID] = owner
					t.manager.mu.Unlock()
				}
			}
		}
		t.manager.mu.Lock()
		if owner == parentID {
			if _, ok := t.manager.started[childID]; !ok {
				t.manager.started[childID] = time.Now().UTC()
			}
		}
		t.manager.mu.Unlock()
		if owner != "" && owner != parentID {
			return provider.Selection{}, "", fmt.Errorf("subagent: session %q does not belong to this session", childID)
		}
		// Continuing with a different agent switches the child, mirroring create
		// semantics: an explicit model wins over the agent's configured model,
		// which wins over the model the child already had.
		previousAgent := ""
		if t.manager.store != nil {
			if meta, err := t.manager.store.Meta(childID); err == nil {
				previousAgent = meta.Agent
			}
		}
		switched = previousAgent != "" && previousAgent != agentID
		if modelOverride == "" {
			if switched && profile.Model != "" {
				providerID, modelID, err := agents.ValidateModel(profile.Model)
				if err != nil {
					return provider.Selection{}, "", fmt.Errorf("subagent: agent %s: %w", profile.ID, err)
				}
				selection = provider.Selection{Provider: providerID, Model: modelID}
			} else if t.manager.store != nil {
				if meta, err := t.manager.store.Meta(childID); err == nil {
					selection = provider.Selection{Provider: meta.Provider, Model: meta.Model}
				}
			}
		}
	} else {
		childID = fmt.Sprintf("%s%d_%d", subagentIDPrefix, time.Now().UnixNano(), t.manager.seq.Add(1))
	}

	target, err := t.manager.runner.Resolve(selection)
	if err != nil {
		return provider.Selection{}, "", fmt.Errorf("subagent: %w", err)
	}
	if target.Model == "" {
		return provider.Selection{}, "", fmt.Errorf("subagent: model is not configured")
	}
	selection = provider.Selection{Provider: target.ProviderID, Model: target.Model}

	if strings.TrimSpace(requestedID) == "" {
		if t.manager.store != nil {
			if err := t.manager.store.SetModel(childID, selection.Provider, selection.Model); err != nil {
				return provider.Selection{}, "", fmt.Errorf("subagent: %w", err)
			}
			if err := t.manager.store.SetAgent(childID, agentID); err != nil {
				return provider.Selection{}, "", fmt.Errorf("subagent: %w", err)
			}
			if err := t.manager.store.SetParent(childID, parentID); err != nil {
				return provider.Selection{}, "", fmt.Errorf("subagent: %w", err)
			}
			// A child runs where its parent runs, so its tools and terminals are
			// the same workspace the parent is working in.
			if meta, err := t.manager.store.Meta(parentID); err == nil && meta.Directory != "" {
				if err := t.manager.store.SetDirectory(childID, meta.Directory); err != nil {
					return provider.Selection{}, "", fmt.Errorf("subagent: %w", err)
				}
			}
		}
		t.manager.mu.Lock()
		t.manager.parents[childID] = parentID
		t.manager.started[childID] = time.Now().UTC()
		t.manager.mu.Unlock()
	} else if explicitAgent && t.manager.store != nil {
		if err := t.manager.store.SetAgent(childID, agentID); err != nil {
			return provider.Selection{}, "", fmt.Errorf("subagent: %w", err)
		}
		// A switched agent takes its own model with it, so the child never runs
		// on a model the new profile does not configure.
		if switched {
			if err := t.manager.store.SetModel(childID, selection.Provider, selection.Model); err != nil {
				return provider.Selection{}, "", fmt.Errorf("subagent: %w", err)
			}
		}
	}
	return selection, childID, nil
}

// finish waits for a background child and notifies the parent.
func (t *Tool) finish(parentID, childID, description, agentID string, stream <-chan event.Event) {
	t.deliver(parentID, childID, description, agentID, collect(stream))
}

// deliver posts one completion exactly once per parent/child pair. A recovery
// watcher and the live finish goroutine may race, so the mark is claimed first.
func (t *Tool) deliver(parentID, childID, description, agentID string, out output) {
	key := parentID + ":" + childID
	t.manager.mu.Lock()
	if t.manager.notified[key] {
		t.manager.mu.Unlock()
		return
	}
	t.manager.notified[key] = true
	t.manager.mu.Unlock()

	status := completionStatus(out)
	notice := completionNotice(childID, description, status, out.text, out.errText)
	notification := event.SubagentCompletedEvent(childID, description, status, notice, out.errText)
	notification.Data["source"] = "subagent"
	notification.Data["agent"] = agentID
	if err := t.manager.runner.Notify(parentID, notification); err != nil {
		t.manager.mu.Lock()
		delete(t.manager.notified, key)
		t.manager.mu.Unlock()
		t.manager.logf("subagent: notify %s: %v", parentID, err)
	}
}

func completionStatus(out output) string {
	switch {
	case out.reason == "interrupted" || out.reason == "cancelled":
		return "cancelled"
	case out.reason != "done" || out.errText != "":
		return "error"
	}
	return "completed"
}

type backgroundStart struct {
	description string
	agentID     string
	childSeq    int64
}

// Recover re-arms completion delivery for background children whose parent
// never received a subagent.completed event. The child run itself is resumed by
// the agent's durable turn claim; this watcher waits for its terminal event.
func (m *Manager) Recover(ctx context.Context) {
	if m.store == nil {
		return
	}
	sessions, err := m.store.List()
	if err != nil {
		m.logf("subagent: recover list: %v", err)
		return
	}
	for _, info := range sessions {
		events, err := m.store.Events(info.ID)
		if err != nil {
			continue
		}
		pending := map[string]backgroundStart{}
		for _, ev := range events {
			switch ev.Type {
			case event.SubagentStarted:
				id := stringValue(ev, "id")
				if id == "" {
					continue
				}
				pending[id] = backgroundStart{
					description: stringValue(ev, "description"),
					agentID:     stringValue(ev, "agent"),
					childSeq:    intValue(ev, "childSeq"),
				}
			case event.SubagentCompleted:
				delete(pending, stringValue(ev, "id"))
			}
		}
		for childID, start := range pending {
			go m.recoverOne(ctx, info.ID, childID, start)
		}
	}
}

func (m *Manager) recoverOne(ctx context.Context, parentID, childID string, start backgroundStart) {
	timeout := time.NewTimer(30 * time.Minute)
	defer timeout.Stop()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		if out, done := m.childOutput(childID, start.childSeq); done {
			tool := &Tool{manager: m}
			tool.deliver(parentID, childID, start.description, start.agentID, out)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-timeout.C:
			tool := &Tool{manager: m}
			tool.deliver(parentID, childID, start.description, start.agentID, output{
				reason:  "error",
				errText: "background subagent did not finish before the recovery timeout",
			})
			return
		case <-ticker.C:
		}
	}
}

// childOutput reads the terminal state of one background run. Events at or
// before childSeq belong to an earlier follow-up and are ignored.
func (m *Manager) childOutput(childID string, childSeq int64) (output, bool) {
	events, err := m.store.Events(childID)
	if err != nil {
		return output{reason: "error", errText: err.Error()}, true
	}
	out := output{reason: "done"}
	terminal := false
	for _, ev := range events {
		if ev.Seq <= childSeq {
			continue
		}
		switch ev.Type {
		case event.AssistantMessage:
			if text := stringValue(ev, "text"); strings.TrimSpace(text) != "" {
				out.text = text
			}
		case event.TurnFinished:
			terminal = true
			out.reason = "done"
		case event.TurnCancelled:
			terminal = true
			out.reason = "interrupted"
		case event.Error:
			if message := stringValue(ev, "message"); message != "" {
				out.errText = message
			}
		}
	}
	return out, terminal
}

func stringValue(ev event.Event, key string) string {
	value, _ := ev.Data[key].(string)
	return value
}

func intValue(ev event.Event, key string) int64 {
	switch value := ev.Data[key].(type) {
	case int64:
		return value
	case int:
		return int64(value)
	case float64:
		return int64(value)
	}
	return 0
}

// completionNotice matches the reference harness's parent-visible envelope.
func completionNotice(childID, description, status, text, errText string) string {
	body := strings.TrimSpace(text)
	switch status {
	case "completed":
		if body == "" {
			body = "Subagent completed without a text response."
		}
	case "cancelled":
		body = "Subagent cancelled"
	default:
		body = strings.TrimSpace(errText)
		if body == "" {
			body = "Subagent failed"
		}
	}
	return fmt.Sprintf("<subagent sessionID=%q state=%q description=%q>\n%s\n</subagent>", childID, status, description, body)
}

type output struct {
	text    string
	reason  string
	errText string
}

func collect(stream <-chan event.Event) output {
	result := output{reason: "done"}
	for ev := range stream {
		switch ev.Type {
		case event.AssistantMessage:
			if value, ok := ev.Data["text"].(string); ok && strings.TrimSpace(value) != "" {
				result.text = value
			}
		case event.RunEnd:
			if value, ok := ev.Data["reason"].(string); ok && value != "" {
				result.reason = value
			}
		case event.Error:
			if value, ok := ev.Data["message"].(string); ok {
				result.errText = value
			}
		}
	}
	return result
}

func parseModel(value string) (provider.Selection, error) {
	value = agents.NormalizeModelRef(value)
	providerID, model, ok := strings.Cut(strings.TrimSpace(value), "/")
	if !ok || strings.TrimSpace(providerID) == "" || strings.TrimSpace(model) == "" {
		return provider.Selection{}, fmt.Errorf("subagent: model must be provider/model")
	}
	return provider.Selection{Provider: strings.TrimSpace(providerID), Model: strings.TrimSpace(model)}, nil
}

func firstLine(text string) string {
	text = strings.TrimSpace(text)
	if index := strings.IndexByte(text, '\n'); index >= 0 {
		text = text[:index]
	}
	runes := []rune(text)
	if len(runes) > 60 {
		return string(runes[:60])
	}
	return text
}
