// Package agent implements the session loop: one model call per step,
// feeding tool results back until the model stops requesting tools.
package agent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/permission"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
)

// ErrBusy is returned when a session already has a running turn.
var ErrBusy = errors.New("session is already running")

// ErrNotRunning is returned when input was meant for an active turn that has
// already started closing.
var ErrNotRunning = errors.New("session is not running")

var (
	errShutdown  = errors.New("shutdown")
	errInterrupt = errors.New("interrupted")
)

// maxStepsPrompt is sent as an assistant message once the step budget is spent.
// The final request carries no tool definitions, so the model can only answer.
const maxStepsPrompt = `CRITICAL - MAXIMUM STEPS REACHED

The maximum number of steps allowed for this task has been reached. Tools are disabled until the next user input. Respond with text only.

Requirements:
1. Do not call any tool.
2. State that the maximum step budget was reached.
3. Summarize what was accomplished.
4. List any remaining work and the recommended next step.`

const (
	// maxOutputContinuations bounds how many times one step may answer a
	// length-truncated response before the run fails.
	maxOutputContinuations = 2
	// maxStreamContinuations bounds how many times one step may resume after a
	// stream ended mid-output.
	maxStreamContinuations = 2
)

const outputLimitPrompt = `Your last response hit the output token limit. Continue from exactly where it stopped. Do not apologize, recap, or repeat completed content. Break the remaining work into smaller pieces.`

const streamResumePrompt = `The previous response was interrupted mid-stream. Continue from where it left off without repeating completed content.`

// Options are the per-agent settings that come from configuration.
type Options struct {
	MaxSteps int
	// System is a fixed system prompt. SystemFunc takes precedence when set.
	System string
	// SystemFunc rebuilds the system prompt for every model request, so project
	// instructions can change between steps.
	SystemFunc func() string
	// CompactKeep is how many recent tokens stay verbatim beside a summary.
	CompactKeep int
	// Retries is how many times a retryable failure may be repeated before the
	// request produced any output. Zero uses the default.
	Retries int
	// RetryDelay is the first backoff delay; later attempts double it.
	RetryDelay time.Duration
	// Base ends every run when it is cancelled; nil means Background.
	Base context.Context
}

const (
	defaultRetries    = 2
	defaultRetryDelay = 500 * time.Millisecond
	maxRetryDelay     = 8 * time.Second
	// maxQueuedInput bounds the in-memory steering queue of one turn.
	maxQueuedInput = 16
)

type Agent struct {
	store      *session.Store
	catalog    *provider.Catalog
	target     provider.Target
	tools      *tool.Registry
	perm       *permission.Manager
	hub        *event.Hub
	maxSteps   int
	system     func() string
	keep       int
	retries    int
	retryDelay time.Duration
	base       context.Context

	mu   sync.Mutex
	idle *sync.Cond
	runs map[string]*runHandle
	seq  atomic.Int64
}

// runHandle owns one turn. Input arriving while the turn is active is queued
// until a safe boundary; finishing closes the queue before the final events.
type runHandle struct {
	release   context.CancelCauseFunc
	claimID   string
	inbox     []session.QueuedInput
	finishing bool
	done      chan struct{}
}

func New(store *session.Store, catalog *provider.Catalog, tools *tool.Registry, perm *permission.Manager, opts Options) *Agent {
	base := opts.Base
	if base == nil {
		base = context.Background()
	}
	retries := opts.Retries
	if retries <= 0 {
		retries = defaultRetries
	}
	retryDelay := opts.RetryDelay
	if retryDelay <= 0 {
		retryDelay = defaultRetryDelay
	}
	system := opts.SystemFunc
	if system == nil {
		text := opts.System
		system = func() string { return text }
	}
	a := &Agent{
		store:      store,
		catalog:    catalog,
		target:     catalog.Default(),
		tools:      tools,
		perm:       perm,
		hub:        event.NewHub(),
		maxSteps:   opts.MaxSteps,
		system:     system,
		keep:       opts.CompactKeep,
		retries:    retries,
		retryDelay: retryDelay,
		base:       base,
		runs:       map[string]*runHandle{},
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
	return a.start(ctx, sessionID, prompt, selection, "", nil, false)
}

// Resume continues a turn whose claim survived a process exit. claimID is the
// open claim from the session log; empty means only queued input survived.
func (a *Agent) Resume(ctx context.Context, sessionID, claimID string, selection provider.Selection) (<-chan event.Event, error) {
	if err := a.store.SettleTools(sessionID); err != nil {
		return nil, err
	}
	recovery, err := a.store.Recovery(sessionID)
	if err != nil {
		return nil, err
	}
	if claimID == "" {
		claimID = recovery.ClaimID
	}
	return a.start(ctx, sessionID, "", selection, claimID, recovery.Inputs, true)
}

func (a *Agent) start(ctx context.Context, sessionID, prompt string, selection provider.Selection, claimID string, initial []session.QueuedInput, resume bool) (<-chan event.Event, error) {
	target, err := a.catalog.Resolve(selection)
	if err != nil {
		return nil, err
	}
	if target.Model == "" {
		return nil, errors.New("model is not configured")
	}
	for {
		a.mu.Lock()
		handle := a.runs[sessionID]
		if handle == nil {
			if ctx.Err() != nil {
				a.mu.Unlock()
				return nil, ctx.Err()
			}
			runCtx, release := a.runContext(ctx)
			// Subscribe before the first durable write so the caller sees the
			// user message and the turn claim cannot outlive a lost prompt.
			sub := a.hub.Subscribe(sessionID)
			if !resume {
				if err := a.emit(sessionID, event.User(prompt)); err != nil {
					a.mu.Unlock()
					sub.Close()
					release(context.Canceled)
					return nil, err
				}
			}
			claim := claimID
			if claim == "" {
				claim = a.nextID("turn")
				if _, err := a.store.Append(sessionID, event.TurnStartedEvent(claim)); err != nil {
					a.mu.Unlock()
					sub.Close()
					release(context.Canceled)
					return nil, err
				}
			}
			handle = &runHandle{release: release, claimID: claim, inbox: initial, done: make(chan struct{})}
			a.runs[sessionID] = handle
			a.mu.Unlock()

			go func() {
				defer func() {
					cause := context.Cause(runCtx)
					switch {
					case errors.Is(cause, errShutdown):
						// Keep the claim and pending input for the next start.
					case cause != nil:
						if err := a.store.CancelTurn(sessionID, claim); err != nil {
							log.Printf("agent: cancel turn %s: %v", sessionID, err)
						}
					default:
						if err := a.store.FinishTurn(sessionID, claim); err != nil {
							log.Printf("agent: finish turn %s: %v", sessionID, err)
						}
					}
					a.mu.Lock()
					if a.runs[sessionID] == handle {
						delete(a.runs, sessionID)
					}
					close(handle.done)
					a.idle.Broadcast()
					a.mu.Unlock()
					release(context.Canceled)
					sub.Close()
				}()
				a.loop(runCtx, sessionID, target)
			}()
			return sub.Events(), nil
		}
		if !handle.finishing {
			a.mu.Unlock()
			return nil, fmt.Errorf("%w: %s", ErrBusy, sessionID)
		}
		// A turn that already started closing rejects new work, but a new turn
		// must not race its cleanup.
		done := handle.done
		a.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// nextID makes ids unique within a process and across quick restarts.
func (a *Agent) nextID(prefix string) string {
	return fmt.Sprintf("%s_%d_%d", prefix, time.Now().UnixNano(), a.seq.Add(1))
}

// runContext derives a cancellable context that also stops when the agent's
// base context ends, so shutdown interrupts every running turn while keeping
// its claim for the next start.
func (a *Agent) runContext(parent context.Context) (context.Context, context.CancelCauseFunc) {
	ctx, cancel := context.WithCancelCause(parent)
	stop := context.AfterFunc(a.base, func() { cancel(errShutdown) })
	return ctx, func(err error) {
		stop()
		if err == nil {
			err = context.Canceled
		}
		cancel(err)
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
// running. Input queued for the interrupted turn is discarded.
func (a *Agent) Interrupt(sessionID string) bool {
	a.mu.Lock()
	handle := a.runs[sessionID]
	if handle == nil {
		a.mu.Unlock()
		return false
	}
	// A finishing turn still owns a possible wrap-up model call; cancelling it
	// is safe because the turn has already stopped accepting new input.
	handle.finishing = true
	handle.inbox = nil
	claim := handle.claimID
	cancel := handle.release
	a.mu.Unlock()
	// Close the durable claim before the process can lose it, so a crash in the
	// interruption window does not resume a turn the user stopped.
	if err := a.store.CancelTurn(sessionID, claim); err != nil {
		log.Printf("agent: cancel turn %s: %v", sessionID, err)
	}
	cancel(errInterrupt)
	return true
}

// Running reports whether a session has an active turn, including one that is
// emitting its final events.
func (a *Agent) Running(sessionID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs[sessionID] != nil
}

// Enqueue persists input for the active turn. The turn delivers it at the next
// boundary between its model calls. ErrNotRunning means the caller should start
// a normal Run instead.
func (a *Agent) Enqueue(sessionID, prompt string) (string, error) {
	if strings.TrimSpace(prompt) == "" {
		return "", errors.New("prompt is empty")
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	handle := a.runs[sessionID]
	if handle == nil || handle.finishing {
		return "", ErrNotRunning
	}
	if len(handle.inbox) >= maxQueuedInput {
		return "", errors.New("too many queued prompts")
	}
	inputID := a.nextID("inp")
	if _, err := a.store.QueueInput(sessionID, inputID, prompt); err != nil {
		return "", err
	}
	handle.inbox = append(handle.inbox, session.QueuedInput{ID: inputID, Text: prompt})
	return inputID, nil
}

// takeQueued removes pending input without changing the turn state.
func (a *Agent) takeQueued(sessionID string) []session.QueuedInput {
	a.mu.Lock()
	defer a.mu.Unlock()
	handle := a.runs[sessionID]
	if handle == nil || len(handle.inbox) == 0 {
		return nil
	}
	queued := handle.inbox
	handle.inbox = nil
	return queued
}

// takeOrFinish atomically takes queued input or closes the turn to new input.
// Closing under the same lock as Enqueue ensures no prompt can slip between the
// final eligibility check and the end of the turn.
func (a *Agent) takeOrFinish(sessionID string) ([]session.QueuedInput, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	handle := a.runs[sessionID]
	if handle == nil {
		return nil, true
	}
	if len(handle.inbox) > 0 {
		queued := handle.inbox
		handle.inbox = nil
		return queued, false
	}
	handle.finishing = true
	return nil, true
}

// deliver records queued user messages before the next model request. The id
// on the message marks the durable queue item as delivered.
func (a *Agent) deliver(sessionID string, inputs []session.QueuedInput) bool {
	for _, input := range inputs {
		if err := a.emit(sessionID, event.UserInput(input.Text, input.ID)); err != nil {
			a.fail(sessionID, err)
			return false
		}
	}
	return true
}

// Subscribe delivers live events of a session to a caller that is not the
// prompt request, such as an event log that follows a replay.
func (a *Agent) Subscribe(sessionID string) *event.Subscription {
	return a.hub.Subscribe(sessionID)
}

// SubscribeAll delivers every live event with its session ID.
func (a *Agent) SubscribeAll() *event.GlobalSubscription {
	return a.hub.SubscribeAll()
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
	defer release(nil)
	current, err := a.store.Context(sessionID)
	if err != nil {
		return err
	}
	return a.compact(ctx, sessionID, target, current, a.keep)
}

func (a *Agent) loop(ctx context.Context, sessionID string, target provider.Target) {
	recovered := false
	attempts := 0
	requestNotice := ""
	outputLimits := 0
	streamResumes := 0
	for step := 0; ; {
		if ctx.Err() != nil {
			_ = a.emit(sessionID, event.End("interrupted"))
			return
		}
		// Input queued during the previous model call or its tools becomes a
		// new user turn and resets the step budget.
		if queued := a.takeQueued(sessionID); len(queued) > 0 {
			if !a.deliver(sessionID, queued) {
				return
			}
			step = 0
			recovered = false
			attempts = 0
			requestNotice = ""
			outputLimits = 0
			streamResumes = 0
		}
		if step >= a.maxSteps {
			queued, done := a.takeOrFinish(sessionID)
			if !done {
				if !a.deliver(sessionID, queued) {
					return
				}
				step = 0
				recovered = false
				attempts = 0
				requestNotice = ""
				outputLimits = 0
				streamResumes = 0
				continue
			}
			a.wrapUp(ctx, sessionID, target)
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

		messages := current.Messages
		if requestNotice != "" {
			// Synthetic only for this request; persistence would surface it as a
			// user turn in history.
			messages = append(append([]model.Message{}, messages...), model.Message{
				Role:    model.RoleUser,
				Content: requestNotice,
			})
		}
		text, calls, usage, finish, err := a.stream(ctx, sessionID, messages, target, a.tools.Definitions())
		if usage != nil {
			if recordErr := a.emit(sessionID, event.UsageEvent(usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite)); recordErr != nil {
				a.fail(sessionID, recordErr)
				return
			}
		}
		if err != nil {
			if ctx.Err() != nil {
				if recordErr := a.recordAssistant(sessionID, text); recordErr != nil {
					a.fail(sessionID, recordErr)
					return
				}
				_ = a.emit(sessionID, event.End("interrupted"))
				return
			}
			// The provider rejected the prompt as too long: summarize harder and
			// retry the same step once. Anything already received means the
			// prompt itself fit, so recovery no longer applies.
			if provider.IsContextOverflow(err) && !recovered && strings.TrimSpace(text) == "" && len(calls) == 0 {
				recovered = true
				if compactErr := a.compact(ctx, sessionID, target, current, a.keep/2); compactErr == nil {
					continue
				}
				if compactErr := a.compact(ctx, sessionID, target, current, 0); compactErr == nil {
					continue
				}
			}
			// Retry only failures that produced no output, so a repeated request
			// cannot duplicate text in the transcript.
			if attempts < a.retries && strings.TrimSpace(text) == "" && len(calls) == 0 && provider.IsRetryable(err) {
				attempts++
				if !sleep(ctx, a.backoff(attempts)) {
					_ = a.emit(sessionID, event.End("interrupted"))
					return
				}
				continue
			}
			if strings.TrimSpace(text) != "" {
				if recordErr := a.recordAssistant(sessionID, text); recordErr != nil {
					a.fail(sessionID, recordErr)
					return
				}
				// Keep the work already streamed and ask for the remainder
				// instead of dropping a partially delivered answer.
				if streamResumes < maxStreamContinuations && provider.IsRetryable(err) {
					streamResumes++
					requestNotice = streamResumePrompt
					continue
				}
			}
			a.fail(sessionID, err)
			return
		}
		recovered = false
		attempts = 0
		requestNotice = ""

		if err := a.recordAssistant(sessionID, text); err != nil {
			a.fail(sessionID, err)
			return
		}
		if finish == "length" {
			if outputLimits >= maxOutputContinuations {
				a.fail(sessionID, errors.New("response still truncated after two output limit continuations"))
				return
			}
			outputLimits++
			requestNotice = outputLimitPrompt
			continue
		}
		if len(calls) == 0 {
			queued, done := a.takeOrFinish(sessionID)
			if done {
				_ = a.emit(sessionID, event.End("done"))
				return
			}
			if !a.deliver(sessionID, queued) {
				return
			}
			step = 0
			recovered = false
			attempts = 0
			requestNotice = ""
			outputLimits = 0
			streamResumes = 0
			continue
		}
		if !a.runTools(ctx, sessionID, calls) {
			return
		}
		step++
		outputLimits = 0
		streamResumes = 0
	}
}

// recordAssistant persists non-empty assistant text as one message.
func (a *Agent) recordAssistant(sessionID, text string) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	return a.emit(sessionID, event.Assistant(text))
}

// wrapUp spends one final model call without tool definitions, so a turn that
// exhausted its step budget still ends with a summary instead of stopping
// silently. The instruction is synthetic and never stored as history.
func (a *Agent) wrapUp(ctx context.Context, sessionID string, target provider.Target) {
	if ctx.Err() != nil {
		_ = a.emit(sessionID, event.End("interrupted"))
		return
	}
	_ = a.emit(sessionID, event.Step(a.maxSteps))

	current, err := a.store.Context(sessionID)
	if err != nil {
		a.fail(sessionID, err)
		return
	}
	messages := make([]model.Message, 0, len(current.Messages)+1)
	messages = append(messages, current.Messages...)
	messages = append(messages, model.Message{Role: model.RoleAssistant, Content: maxStepsPrompt})

	text, _, usage, _, err := a.stream(ctx, sessionID, messages, target, nil)
	if usage != nil {
		if recordErr := a.emit(sessionID, event.UsageEvent(usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite)); recordErr != nil {
			a.fail(sessionID, recordErr)
			return
		}
	}
	if err != nil {
		if recordErr := a.recordAssistant(sessionID, text); recordErr != nil {
			a.fail(sessionID, recordErr)
			return
		}
		if ctx.Err() != nil {
			_ = a.emit(sessionID, event.End("interrupted"))
			return
		}
		a.fail(sessionID, err)
		return
	}
	if err := a.recordAssistant(sessionID, text); err != nil {
		a.fail(sessionID, err)
		return
	}
	_ = a.emit(sessionID, event.End("max_steps"))
}

// backoff doubles the first delay for each attempt and caps it.
func (a *Agent) backoff(attempt int) time.Duration {
	delay := a.retryDelay << (attempt - 1)
	if delay <= 0 || delay > maxRetryDelay {
		return maxRetryDelay
	}
	return delay
}

// sleep waits for d or until ctx ends, reporting whether the wait completed.
func sleep(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// due reports whether the conversation should be compacted before the next call.
func (a *Agent) due(tokens, window int) bool {
	ceiling := session.Ceiling(window)
	return ceiling > 0 && tokens >= ceiling
}

// stream returns partial output even when the provider fails mid-response, so
// the caller can record it and decide between resuming and failing.
func (a *Agent) stream(ctx context.Context, sessionID string, messages []model.Message, target provider.Target, tools []model.ToolDef) (string, []model.ToolCall, *provider.Usage, string, error) {
	req := model.Request{Model: target.Model, Messages: a.withSystem(messages), Tools: tools}
	stream, err := target.Provider.Stream(ctx, req)
	if err != nil {
		return "", nil, nil, "", err
	}
	defer stream.Close()

	var text strings.Builder
	var usage *provider.Usage
	finish := ""
	calls := map[int]*model.ToolCall{}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return text.String(), ordered(calls), usage, finish, err
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.FinishReason != "" {
			finish = chunk.FinishReason
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
	return text.String(), ordered(calls), usage, finish, nil
}

// withSystem prepends the derived system prompt without touching stored history.
func (a *Agent) withSystem(messages []model.Message) []model.Message {
	system := a.system()
	if system == "" {
		return messages
	}
	out := make([]model.Message, 0, len(messages)+1)
	out = append(out, model.Message{Role: model.RoleSystem, Content: system})
	return append(out, messages...)
}

// toolResult is one finished tool call waiting to be recorded. Results are
// recorded in completion order, so a slow tool never blocks the others.
type toolResult struct {
	call   model.ToolCall
	output string
	diff   string
	failed bool
}

// runTools records every call first, then executes them concurrently. All
// calls are announced before any result so the rebuilt transcript keeps one
// assistant message for the whole batch.
func (a *Agent) runTools(ctx context.Context, sessionID string, calls []model.ToolCall) bool {
	for _, call := range calls {
		if call.ID == "" {
			call.ID = "call_" + call.Name
		}
		if err := a.emit(sessionID, event.ToolCallEvent(call.ID, call.Name, call.Arguments)); err != nil {
			a.fail(sessionID, err)
			return false
		}
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	results := make(chan toolResult, len(calls))
	var wg sync.WaitGroup
	for _, call := range calls {
		wg.Add(1)
		go func(call model.ToolCall) {
			defer wg.Done()
			var diff string
			output, err := a.tools.Run(ctx, tool.Call{
				SessionID: sessionID,
				Name:      call.Name,
				Input:     []byte(call.Arguments),
				Perm:      a.perm,
				OnAsk: func(req permission.Request) {
					a.hub.Publish(sessionID, event.PermissionAskedEvent(req.ID, req.SessionID, req.Action, req.Resources, req.Message))
				},
				OnDiff: func(value string) {
					diff = value
				},
			})
			failed := err != nil
			if err != nil {
				output = strings.TrimSpace(output + "\n" + err.Error())
			}
			results <- toolResult{call: call, output: output, diff: diff, failed: failed}
		}(call)
	}
	go func() {
		wg.Wait()
		close(results)
	}()

	var emitErr error
	for result := range results {
		if emitErr != nil {
			continue
		}
		if err := a.emit(sessionID, event.ToolResultWithDiff(result.call.ID, result.call.Name, result.output, result.diff, result.failed)); err != nil {
			// Stop the remaining tools but keep draining so their goroutines exit.
			emitErr = err
			cancel()
		}
	}
	if emitErr != nil {
		a.fail(sessionID, emitErr)
		return false
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
