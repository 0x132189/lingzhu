// Package agent implements the session loop: one model call per step,
// feeding tool results back until the model stops requesting tools.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/permission"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
)

// ErrBusy is returned when a session already has a running turn.
var ErrBusy = errors.New("session is already running")

// Options are the per-agent settings that come from configuration.
type Options struct {
	MaxSteps int
	// System is the derived system prompt sent with every request.
	System string
	// CompactKeep is how many recent tokens stay verbatim beside a summary.
	CompactKeep int
	// Base ends every run when it is cancelled; nil means Background.
	Base context.Context
}

type Agent struct {
	store    *session.Store
	catalog  *provider.Catalog
	target   provider.Target
	tools    *tool.Registry
	perm     *permission.Manager
	hub      *event.Hub
	maxSteps int
	system   string
	keep     int
	base     context.Context

	mu   sync.Mutex
	idle *sync.Cond
	runs map[string]context.CancelFunc
}

func New(store *session.Store, catalog *provider.Catalog, tools *tool.Registry, perm *permission.Manager, opts Options) *Agent {
	base := opts.Base
	if base == nil {
		base = context.Background()
	}
	a := &Agent{
		store:    store,
		catalog:  catalog,
		target:   catalog.Default(),
		tools:    tools,
		perm:     perm,
		hub:      event.NewHub(),
		maxSteps: opts.MaxSteps,
		system:   opts.System,
		keep:     opts.CompactKeep,
		base:     base,
		runs:     map[string]context.CancelFunc{},
	}
	a.idle = sync.NewCond(&a.mu)
	return a
}

// Reply answers a pending permission request.
func (a *Agent) Reply(id string, decision permission.Decision, message string) error {
	if a.perm == nil {
		return errors.New("permission is not configured")
	}
	return a.perm.Reply(id, decision, message)
}

// Pending lists pending permission requests; an empty sessionID returns all.
func (a *Agent) Pending(sessionID string) []permission.Request {
	if a.perm == nil {
		return nil
	}
	return a.perm.Pending(sessionID)
}

// Run starts one conversation turn for the selected provider/model. The turn
// is owned by the session, not by the caller: cancelling ctx stops it, and so
// does Interrupt. The returned channel delivers live events and closes when the
// turn ends.
func (a *Agent) Run(ctx context.Context, sessionID, prompt string, selection provider.Selection) (<-chan event.Event, error) {
	target, err := a.catalog.Resolve(selection)
	if err != nil {
		return nil, err
	}
	if target.Model == "" {
		return nil, errors.New("model is not configured")
	}
	a.mu.Lock()
	if _, busy := a.runs[sessionID]; busy {
		a.mu.Unlock()
		return nil, fmt.Errorf("%w: %s", ErrBusy, sessionID)
	}
	runCtx, release := a.runContext(ctx)
	a.runs[sessionID] = release
	a.mu.Unlock()

	sub := a.hub.Subscribe(sessionID)
	go func() {
		defer func() {
			a.mu.Lock()
			delete(a.runs, sessionID)
			a.idle.Broadcast()
			a.mu.Unlock()
			release()
			sub.Close()
		}()
		a.loop(runCtx, sessionID, prompt, target)
	}()
	return sub.Events(), nil
}

// runContext derives a cancellable context that also stops when the agent's
// base context ends, so shutdown interrupts every running turn.
func (a *Agent) runContext(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	stop := context.AfterFunc(a.base, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// Wait blocks until no turn is running, or ctx ends first. It is used during
// shutdown, after the base context has been cancelled.
func (a *Agent) Wait(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		a.mu.Lock()
		for len(a.runs) > 0 {
			a.idle.Wait()
		}
		a.mu.Unlock()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Interrupt cancels the running turn of a session and reports whether one was
// running.
func (a *Agent) Interrupt(sessionID string) bool {
	a.mu.Lock()
	cancel, running := a.runs[sessionID]
	a.mu.Unlock()
	if running {
		cancel()
	}
	return running
}

// Running reports whether a session has an active turn.
func (a *Agent) Running(sessionID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	_, running := a.runs[sessionID]
	return running
}

// Subscribe delivers live events of a session to a caller that is not the
// prompt request, such as an event log that follows a replay.
func (a *Agent) Subscribe(sessionID string) *event.Subscription {
	return a.hub.Subscribe(sessionID)
}

// Window is the context window of a resolved target, zero when compaction is
// disabled.
func (a *Agent) Resolve(selection provider.Selection) (provider.Target, error) {
	return a.catalog.Resolve(selection)
}

// DefaultTarget is the configured default provider/model.
func (a *Agent) DefaultTarget() provider.Target { return a.catalog.Default() }

// Compact summarizes the conversation on demand, even when it is under budget.
func (a *Agent) Compact(ctx context.Context, sessionID string, selection provider.Selection) error {
	target, err := a.catalog.Resolve(selection)
	if err != nil {
		return err
	}
	if target.Model == "" {
		return errors.New("model is not configured")
	}
	if a.Running(sessionID) {
		return ErrBusy
	}
	ctx, release := a.runContext(ctx)
	defer release()
	current, err := a.store.Context(sessionID)
	if err != nil {
		return err
	}
	return a.compact(ctx, sessionID, target, current, a.keep)
}

func (a *Agent) loop(ctx context.Context, sessionID, prompt string, target provider.Target) {
	if err := a.emit(sessionID, event.User(prompt)); err != nil {
		a.fail(sessionID, err)
		return
	}

	recovered := false
	for step := 0; step < a.maxSteps; {
		if ctx.Err() != nil {
			_ = a.emit(sessionID, event.End("interrupted"))
			return
		}
		if err := a.emit(sessionID, event.Step(step)); err != nil {
			a.fail(sessionID, err)
			return
		}
		current, err := a.store.Context(sessionID)
		if err != nil {
			a.fail(sessionID, err)
			return
		}
		if a.due(current.Tokens, target.Window) {
			if err := a.compact(ctx, sessionID, target, current, a.keep); err != nil {
				a.fail(sessionID, err)
				return
			}
			if current, err = a.store.Context(sessionID); err != nil {
				a.fail(sessionID, err)
				return
			}
			recovered = true
		}

		text, calls, usage, err := a.stream(ctx, sessionID, current.Messages, target)
		if usage != nil {
			if recordErr := a.emit(sessionID, event.UsageEvent(usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite)); recordErr != nil {
				a.fail(sessionID, recordErr)
				return
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				_ = a.emit(sessionID, event.End("interrupted"))
				return
			}
			// The provider rejected the prompt as too long: summarize harder and
			// retry the same step once.
			if provider.IsContextOverflow(err) && !recovered {
				recovered = true
				if compactErr := a.compact(ctx, sessionID, target, current, a.keep/2); compactErr == nil {
					continue
				}
				if compactErr := a.compact(ctx, sessionID, target, current, 0); compactErr == nil {
					continue
				}
			}
			a.fail(sessionID, err)
			return
		}
		recovered = false

		if strings.TrimSpace(text) != "" {
			if err := a.emit(sessionID, event.Assistant(text)); err != nil {
				a.fail(sessionID, err)
				return
			}
		}
		if len(calls) == 0 {
			_ = a.emit(sessionID, event.End("done"))
			return
		}
		if !a.runTools(ctx, sessionID, calls) {
			return
		}
		step++
	}
	_ = a.emit(sessionID, event.End("max_steps"))
}

// due reports whether the conversation should be compacted before the next call.
func (a *Agent) due(tokens, window int) bool {
	ceiling := session.Ceiling(window)
	return ceiling > 0 && tokens >= ceiling
}

func (a *Agent) stream(ctx context.Context, sessionID string, messages []model.Message, target provider.Target) (string, []model.ToolCall, *provider.Usage, error) {
	req := model.Request{Model: target.Model, Messages: a.withSystem(messages), Tools: a.tools.Definitions()}
	stream, err := target.Provider.Stream(ctx, req)
	if err != nil {
		return "", nil, nil, err
	}
	defer stream.Close()

	var text strings.Builder
	var usage *provider.Usage
	calls := map[int]*model.ToolCall{}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, usage, err
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.Text != "" {
			text.WriteString(chunk.Text)
			a.hub.Publish(sessionID, event.Delta(chunk.Text))
		}
		for _, delta := range chunk.ToolCalls {
			call, ok := calls[delta.Index]
			if !ok {
				call = &model.ToolCall{}
				calls[delta.Index] = call
			}
			if delta.ID != "" {
				call.ID = delta.ID
			}
			call.Name += delta.Name
			call.Arguments += delta.Arguments
		}
	}
	return text.String(), ordered(calls), usage, nil
}

// withSystem prepends the derived system prompt without touching stored history.
func (a *Agent) withSystem(messages []model.Message) []model.Message {
	if a.system == "" {
		return messages
	}
	out := make([]model.Message, 0, len(messages)+1)
	out = append(out, model.Message{Role: model.RoleSystem, Content: a.system})
	return append(out, messages...)
}

func (a *Agent) runTools(ctx context.Context, sessionID string, calls []model.ToolCall) bool {
	for _, call := range calls {
		if call.ID == "" {
			call.ID = "call_" + call.Name
		}
		if err := a.emit(sessionID, event.ToolCallEvent(call.ID, call.Name, call.Arguments)); err != nil {
			a.fail(sessionID, err)
			return false
		}
		output, err := a.tools.Run(ctx, tool.Call{
			SessionID: sessionID,
			Name:      call.Name,
			Input:     []byte(call.Arguments),
			Perm:      a.perm,
			OnAsk: func(req permission.Request) {
				a.hub.Publish(sessionID, event.PermissionAskedEvent(req.ID, req.SessionID, req.Action, req.Resources, req.Message))
			},
		})
		failed := err != nil
		if err != nil {
			output = strings.TrimSpace(output + "\n" + err.Error())
		}
		if err := a.emit(sessionID, event.ToolResultEvent(call.ID, call.Name, output, failed)); err != nil {
			a.fail(sessionID, err)
			return false
		}
	}
	return true
}

// emit persists durable events and publishes every event to live subscribers.
func (a *Agent) emit(sessionID string, ev event.Event) error {
	if ev.Durable() {
		seq, err := a.store.Append(sessionID, ev)
		if err != nil {
			return err
		}
		ev.Seq = seq
	}
	a.hub.Publish(sessionID, ev)
	return nil
}

func (a *Agent) fail(sessionID string, err error) {
	a.hub.Publish(sessionID, event.ErrorEvent(err.Error()))
	a.hub.Publish(sessionID, event.End("error"))
}

func ordered(calls map[int]*model.ToolCall) []model.ToolCall {
	indexes := make([]int, 0, len(calls))
	for i := range calls {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	out := make([]model.ToolCall, 0, len(indexes))
	for _, i := range indexes {
		out = append(out, *calls[i])
	}
	return out
}
