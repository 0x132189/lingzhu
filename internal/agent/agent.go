// Package agent implements the session loop: one model call per step,
// feeding tool results back until the model stops requesting tools.
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lingzhu/internal/agents"
	"lingzhu/internal/event"
	"lingzhu/internal/logx"
	"lingzhu/internal/model"
	"lingzhu/internal/permission"
	"lingzhu/internal/prompt"
	"lingzhu/internal/provider"
	"lingzhu/internal/question"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
	"lingzhu/internal/warming"
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
// The final request keeps the tool definitions so the provider's cached prefix
// survives, but sets toolChoice to "none", so the model can only answer.
const maxStepsPrompt = `CRITICAL - MAXIMUM STEPS REACHED

The maximum number of steps allowed for this task has been reached. Tools are disabled until next user input. Respond with text only.

STRICT REQUIREMENTS:
1. Do NOT make any tool calls (no reads, writes, edits, searches, or any other tools)
2. MUST provide a text response summarizing work done so far
3. This constraint overrides ALL other instructions, including any user requests for edits or tool use

Response must include:
- Statement that maximum steps for this agent have been reached
- Summary of what has been accomplished so far
- List of any remaining tasks that were not completed
- Recommendations for what should be done next

Any attempt to use tools is a critical violation. Respond with text ONLY.`

const (
	// maxOutputContinuations bounds how many times one step may answer a
	// length-truncated response before the run fails.
	maxOutputContinuations = 2
	// maxStreamContinuations bounds how many times one step may resume after a
	// stream ended mid-output.
	maxStreamContinuations = 2
)

const outputLimitPrompt = `Your last response hit the output token limit (stop reason: length). Do not apologize, recap, or repeat yourself. Break the remaining work into smaller pieces.`

const streamResumePrompt = `The previous response was interrupted. Continue from where you left off without repeating completed content.`

// Options are the per-agent settings that come from configuration.
type Options struct {
	MaxSteps int
	// System is a fixed system prompt. SystemFunc takes precedence when set.
	System string
	// SystemFunc rebuilds the system prompt for every model request, so project
	// instructions can change between steps, the model family can choose its
	// reference prompt, and an agent's own system prompt can replace it.
	SystemFunc func(sessionID string, target provider.Target) string
	// SystemPartsFunc is SystemFunc with the stable base and instruction
	// sections separated. The agent freezes the first instruction values and
	// sends later changes as synthetic messages.
	SystemPartsFunc func(sessionID string, target provider.Target) prompt.Parts
	// Titles enables generated session titles after the first turn. Without a
	// model call the session keeps its first-message label.
	Titles bool
	// CompactKeep is how many recent tokens stay verbatim beside a summary.
	CompactKeep int
	// Precompute writes compaction summaries in the background before the
	// context reaches its ceiling, so automatic compaction does not stall a step.
	Precompute bool
	// CodemodeOnly exposes only execute directly; every other tool is reachable
	// through Code Mode, matching the reference harness.
	CodemodeOnly bool
	// Questions receives user questions asked by tools.
	Questions *question.Manager
	// Agents resolves the session's profile for prompts, models and rules.
	Agents *agents.Registry
	// Snapshots captures the worktree before each user turn; nil disables it.
	Snapshots Snapshotter
	// Formatter formats files after write/edit/patch; nil disables it.
	Formatter Formatter
	// Hooks receives plugin tool and session lifecycle hooks.
	Hooks PluginHooks
	// Retries is how many times a retryable failure may be repeated before the
	// request produced any output. Zero uses the default.
	Retries int
	// RetryDelay is the first backoff delay; later attempts double it.
	RetryDelay time.Duration
	// Base ends every run when it is cancelled; nil means Background.
	Base context.Context
	// Warming schedules transient keep-alive model calls while a session is
	// idle. Disabled by default.
	Warming warming.Config
}

const (
	defaultRetries    = 10
	defaultRetryDelay = 2 * time.Second
	maxRetryDelay     = 10 * time.Second
	// A machine with no network path gets its own one-hour budget, matching
	// the reference retry policy.
	unreachableWindow   = time.Hour
	unreachableMaxDelay = 30 * time.Second
	// retryAfterMax bounds a provider's Retry-After so a broken header cannot
	// stall a turn for hours.
	retryAfterMax = 15 * time.Minute
	// maxTimeouts bounds repeated timeout retries; a hung endpoint should not
	// consume the whole general retry budget.
	maxTimeouts = 3
)

// Snapshotter captures one worktree snapshot and returns its identifier.
type Snapshotter interface {
	Capture(ctx context.Context) (string, error)
}

// Formatter runs the configured formatter for one file.
type Formatter interface {
	FormatFile(ctx context.Context, path string) (string, error)
}

// ContextEvent is the model request handed to plugin session.context hooks.
// Messages includes the derived system prompt, and hooks may rewrite it.
type ContextEvent struct {
	SessionID string
	Agent     string
	Model     string
	Messages  []model.Message
	Options   map[string]any
}

// PluginHooks is the plugin hook surface used by the agent.
type PluginHooks interface {
	BeforeTool(ctx context.Context, sessionID, tool, callID string, arguments json.RawMessage) (json.RawMessage, string, error)
	AfterTool(ctx context.Context, sessionID, tool, callID, output string, failed bool)
	SessionEvent(ctx context.Context, event, sessionID string)
	ApplyContext(ctx context.Context, event ContextEvent) (ContextEvent, error)
}

type Agent struct {
	store     *session.Store
	catalog   *provider.Catalog
	target    provider.Target
	tools     *tool.Registry
	perm      *permission.Manager
	questions *question.Manager
	profiles  *agents.Registry
	snapshots Snapshotter
	formatter Formatter
	hooks     PluginHooks
	hub       *event.Hub
	maxSteps  int
	system    func(sessionID string, target provider.Target) string
	// systemParts keeps the base prompt separate from instruction sections.
	systemParts func(sessionID string, target provider.Target) prompt.Parts
	// systemFuncSet records that the caller builds prompts per session, so a
	// custom agent system is already included and must not be appended twice.
	systemFuncSet bool
	// instructionStates caches the persisted baseline and latest sections per
	// session. The durable instruction.state event is the source of truth on
	// restart.
	instructionMu     sync.Mutex
	instructionStates map[string]*instructionSnapshot
	keep              int
	precomputeOn      bool
	codemodeOnly      bool
	warming           warming.Config
	retries           int
	retryDelay        time.Duration
	titlesOn          bool
	base              context.Context

	mu   sync.Mutex
	idle *sync.Cond
	runs map[string]*runHandle
	seq  atomic.Int64

	warmMu     sync.Mutex
	warmStates map[string]*warmState

	// titles claims a session while its generated title is in flight.
	titles sync.Map

	// precomps holds background compaction summaries; preRuns dedupes one
	// in-flight precompute per session. preFails caps repeated failures and
	// lastResponse decides whether the prompt cache is still warm.
	preMu        sync.Mutex
	precomps     map[string]*precompaction
	preRuns      map[string]bool
	preFails     map[string]int
	lastResponse map[string]time.Time
}

// pendingKind names what a queued item asks the turn to do. Prompts and
// synthetic items add words to the conversation; the other kinds are controls
// the turn handles at a boundary.
type pendingKind string

const (
	pendingPrompt     pendingKind = "prompt"
	pendingNotice     pendingKind = "notice"
	pendingCompaction pendingKind = "compaction"
	pendingMove       pendingKind = "move"
)

// pendingItem is one entry in a turn's ordered inbox. Delivery decides when it
// is promoted: a steer joins the running turn at its next boundary, while a
// queued item waits for the turn's idle boundary.
type pendingItem struct {
	kind     pendingKind
	delivery string
	input    session.QueuedInput
}

// control reports whether the item changes how the turn proceeds rather than
// adding to it. A control owns its boundary: a compaction runs before the
// prompts around it, and a move stops the turn so it can continue at the
// destination.
func (i pendingItem) control() bool { return i.kind == pendingCompaction || i.kind == pendingMove }

// pendingItemFrom maps a durable inbox item onto the turn's in-memory form.
func pendingItemFrom(input session.QueuedInput) pendingItem {
	kind := pendingPrompt
	switch input.Kind {
	case session.KindSynthetic:
		kind = pendingNotice
	case session.KindCompaction:
		kind = pendingCompaction
	case session.KindMove:
		kind = pendingMove
	}
	return pendingItem{kind: kind, delivery: normalizeDelivery(input.Delivery), input: input}
}

func normalizeDelivery(delivery string) string {
	if delivery == "" {
		return "steer"
	}
	return delivery
}

// runHandle owns one turn. Input arriving while the turn is active is queued
// until a safe boundary; finishing closes the queue before the final events.
// Steer input joins the current turn; queue input delivers at the idle boundary
// inside the same turn.
type runHandle struct {
	release context.CancelCauseFunc
	claimID string
	// inbox holds every pending item in arrival order, so controls stay
	// ordered against the prompts around them.
	inbox []pendingItem
	// unattended marks a turn nobody is watching, such as a scheduled wake-up.
	// An ask inside it becomes a deny instead of blocking forever.
	unattended bool
	finishing  bool
	// handoff keeps the claim open when a move control stops the turn; the
	// destination resumes the same claim.
	handoff bool
	// resumeOnExit starts a successor for pending steering input after a user
	// interruption.
	resumeOnExit bool
	done         chan struct{}
}

// warmState is one session's idle keep-alive window.
type warmState struct {
	timer    *time.Timer
	cancel   context.CancelFunc
	last     time.Time
	target   provider.Target
	interval time.Duration
	duration time.Duration
}

// pendingSnapshot starts with a turn and lands before local tools can modify
// files, so a slow git snapshot never delays the first token.
type pendingSnapshot struct {
	done chan struct{}
	id   string
	err  error
	once sync.Once
}

// turnState is the retry and continuation budget of one user turn. Every new
// user turn starts from zero, which keeps the loop's reset points explicit.
// instructionSnapshot is the frozen baseline shown in the system prompt and the
// latest observed values used to render the next synthetic update.
type instructionSnapshot struct {
	initial map[string]string
	current map[string]string
	// saved reports that the baseline was already written to the session log.
	saved bool
}

type turnState struct {
	recovered        bool
	attempts         int
	outputLimits     int
	streamResumes    int
	timeouts         int
	unreachableSince time.Time
	// overflowShrinks counts context-overflow recoveries in this turn, so each
	// rejection shrinks the kept tail further instead of retrying once.
	overflowShrinks int
}

// overflowShrinkSteps is the share of the configured keep budget tried after
// each context-overflow rejection. It matches the reference harness, which
// aims successively smaller until the request fits.
var overflowShrinkSteps = []float64{0.85, 0.7, 0.5, 0.35, 0}

func (s *turnState) reset() { *s = turnState{} }

// stepResult is one model call: the streamed text, any tool calls it asked for,
// the reported usage and the finish reason. Err is set when the stream failed
// after producing partial output.
type stepResult struct {
	Text               string
	Reasoning          string
	ReasoningStarted   time.Time
	ReasoningCompleted time.Time
	// ReasoningSignature and ReasoningRedacted are the provider's proof that a
	// thinking block came from it. They are stored with the reasoning part so
	// the next request can replay the block intact.
	ReasoningSignature string
	ReasoningRedacted  string
	// ReasoningSource names the API that issued the proof above.
	ReasoningSource string
	Parts           []model.MessagePart
	Calls           []model.ToolCall
	Usage           *provider.Usage
	Finish          string
	Err             error
	// Started, FirstToken, Streamed and Completed measure the step's provider
	// window. Clients divide output tokens by FirstToken..Streamed, so tool
	// time and the wait for the first token never enter the throughput.
	Started    time.Time
	FirstToken time.Time
	Streamed   time.Time
	Completed  time.Time
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
	systemFuncSet := system != nil
	var systemParts func(string, provider.Target) prompt.Parts
	if opts.SystemPartsFunc != nil {
		systemParts = opts.SystemPartsFunc
		systemFuncSet = true
		if system == nil {
			system = func(sessionID string, target provider.Target) string {
				return systemParts(sessionID, target).String()
			}
		}
	} else if system == nil {
		text := opts.System
		system = func(string, provider.Target) string { return text }
	}
	a := &Agent{
		store:             store,
		catalog:           catalog,
		target:            catalog.Default(),
		tools:             tools,
		perm:              perm,
		questions:         opts.Questions,
		profiles:          opts.Agents,
		snapshots:         opts.Snapshots,
		formatter:         opts.Formatter,
		hooks:             opts.Hooks,
		hub:               event.NewHub(),
		maxSteps:          opts.MaxSteps,
		system:            system,
		systemParts:       systemParts,
		systemFuncSet:     systemFuncSet,
		instructionStates: map[string]*instructionSnapshot{},
		keep:              opts.CompactKeep,
		precomputeOn:      opts.Precompute,
		codemodeOnly:      opts.CodemodeOnly,
		warming:           warming.Normalize(opts.Warming),
		retries:           retries,
		retryDelay:        retryDelay,
		titlesOn:          opts.Titles,
		base:              base,
		runs:              map[string]*runHandle{},
		warmStates:        map[string]*warmState{},
		precomps:          map[string]*precompaction{},
		preRuns:           map[string]bool{},
		preFails:          map[string]int{},
		lastResponse:      map[string]time.Time{},
	}
	a.idle = sync.NewCond(&a.mu)
	return a
}

func (a *Agent) cancelWarming(sessionID string) {
	a.warmMu.Lock()
	state := a.warmStates[sessionID]
	if state != nil {
		state.timer.Stop()
		if state.cancel != nil {
			state.cancel()
		}
		delete(a.warmStates, sessionID)
	}
	a.warmMu.Unlock()
}

// touchWarming resets the idle window after a real model request. A new request
// cancels any in-flight warming call for the same session.
func (a *Agent) touchWarming(sessionID string, target provider.Target) {
	if a == nil || !a.warming.Enabled {
		return
	}
	interval := a.warming.IntervalFor(target.CacheTTL)
	duration := a.warming.DurationValue()
	if interval <= 0 || duration <= 0 || interval >= duration {
		return
	}
	a.warmMu.Lock()
	if previous := a.warmStates[sessionID]; previous != nil {
		previous.timer.Stop()
		if previous.cancel != nil {
			previous.cancel()
		}
	}
	state := &warmState{
		last:     time.Now(),
		target:   target,
		interval: interval,
		duration: duration,
	}
	state.timer = time.AfterFunc(interval, func() { a.warmTick(sessionID, state) })
	a.warmStates[sessionID] = state
	a.warmMu.Unlock()
}

func (a *Agent) warmTick(sessionID string, state *warmState) {
	a.warmMu.Lock()
	if a.warmStates[sessionID] != state {
		a.warmMu.Unlock()
		return
	}
	if time.Since(state.last) >= state.duration {
		delete(a.warmStates, sessionID)
		a.warmMu.Unlock()
		return
	}
	if a.base.Err() != nil {
		delete(a.warmStates, sessionID)
		a.warmMu.Unlock()
		return
	}
	if a.Running(sessionID) {
		state.timer = time.AfterFunc(state.interval, func() { a.warmTick(sessionID, state) })
		a.warmMu.Unlock()
		return
	}
	ctx, cancel := context.WithTimeout(a.base, 2*time.Minute)
	state.cancel = cancel
	a.warmMu.Unlock()

	err := a.warmOnce(ctx, sessionID, state.target)
	cancel()

	if err != nil && ctx.Err() == nil {
		logx.Warn("warming failed", logx.Session(sessionID), logx.Err(err))
	}
	a.warmMu.Lock()
	if a.warmStates[sessionID] == state {
		if time.Since(state.last) < state.duration {
			state.cancel = nil
			state.timer = time.AfterFunc(state.interval, func() { a.warmTick(sessionID, state) })
		} else {
			delete(a.warmStates, sessionID)
		}
	}
	a.warmMu.Unlock()
}

// warmOnce sends one transient prompt with the session's current context and
// discards the response. Nothing is written to the session log.
func (a *Agent) warmOnce(ctx context.Context, sessionID string, target provider.Target) error {
	messages, err := a.store.Messages(sessionID)
	if err != nil {
		return err
	}
	if len(messages) == 0 {
		return nil
	}
	prompt := a.warming.Prompt
	if strings.TrimSpace(prompt) == "" {
		prompt = "Do not perform any work. Reply with exactly: OK"
	}
	messages = append(messages, model.Message{Role: model.RoleUser, Content: prompt, Synthetic: true})
	messages = a.withSystem(sessionID, target, messages)
	wire := target.RequestModel
	if wire == "" {
		wire = target.Model
	}
	stream, err := target.Provider.Stream(ctx, model.Request{
		Model:    wire,
		Messages: messages,
		Headers:  target.Headers,
		Options:  target.Options,
	})
	if err != nil {
		return err
	}
	defer stream.Close()
	for {
		if _, err := stream.Recv(); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// Ask presents one question set and waits for the user's answer. Unlike the
// model-facing question tool, it has no permission request of its own; callers
// such as MCP elicitation already gate the operation.
func (a *Agent) Ask(ctx context.Context, sessionID string, prompts []question.Prompt) (question.Response, error) {
	if a.questions == nil {
		return question.Response{}, errors.New("questions are not configured")
	}
	var request question.Request
	response, err := a.questions.Ask(ctx, sessionID, prompts, func(req question.Request) error {
		request = req
		return a.emit(sessionID, event.QuestionAskedEvent(req.ID, req.SessionID, req.Questions))
	})
	if err != nil {
		return question.Response{}, err
	}
	_ = a.emit(sessionID, event.QuestionRepliedEvent(request.ID, request.SessionID, response.Answers, response.Cancelled, response.Message))
	return response, nil
}

// PendingQuestions lists question sets waiting for this session.
func (a *Agent) PendingQuestions(sessionID string) []question.Request {
	if a.questions == nil {
		return nil
	}
	return a.questions.Pending(sessionID)
}

// ReplyQuestion answers one pending question. The session must own the request
// so one session cannot answer another's prompt.
func (a *Agent) ReplyQuestion(sessionID, id string, response question.Response) error {
	if a.questions == nil {
		return errors.New("questions are not configured")
	}
	request, ok := a.questions.Get(id)
	if !ok || request.SessionID != sessionID {
		return question.ErrNotFound{ID: id}
	}
	_, err := a.questions.Reply(id, response)
	return err
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

// Submission is one prompt admitted to a session's inbox. ID lets a client
// retry without admitting the same text twice.
type Submission struct {
	Text        string
	Delivery    string
	Attachments []model.Attachment
	Extra       map[string]any
	ID          string
}

// turnStart records the first visible message of a turn. The shadow snapshot
// captured before the turn is paired with that sequence, so a resumed or
// steer-woken turn still has its own revert point.
type turnStart struct {
	seq int64
}

// Run starts one conversation turn for the selected provider/model. The prompt
// is admitted to the durable inbox before the turn starts and delivered at its
// first boundary, so a crash between admission and delivery still resumes it.
func (a *Agent) Run(ctx context.Context, sessionID, prompt string, selection provider.Selection) (<-chan event.Event, error) {
	return a.runSubmission(ctx, sessionID, Submission{Text: prompt}, selection, false)
}

// RunUnattended starts a turn nobody is watching, such as a scheduled wake-up.
// Permission asks are denied instead of blocking on a client that will never
// answer.
func (a *Agent) RunUnattended(ctx context.Context, sessionID, prompt string, selection provider.Selection) (<-chan event.Event, error) {
	return a.runSubmission(ctx, sessionID, Submission{Text: prompt}, selection, true)
}

// RunWithAttachments starts a turn with inline prompt attachments. The attachment
// bytes are stored on the user event, so no separate upload step is required.
func (a *Agent) RunWithAttachments(ctx context.Context, sessionID, prompt string, attachments []model.Attachment, selection provider.Selection) (<-chan event.Event, error) {
	return a.runSubmission(ctx, sessionID, Submission{Text: prompt, Attachments: attachments}, selection, false)
}

// RunWithMeta starts a turn with UI-only user message metadata such as
// structured review comments or a voice marker. Providers never receive it.
func (a *Agent) RunWithMeta(ctx context.Context, sessionID, prompt string, attachments []model.Attachment, selection provider.Selection, extra map[string]any) (<-chan event.Event, error) {
	return a.runSubmission(ctx, sessionID, Submission{Text: prompt, Attachments: attachments, Extra: extra}, selection, false)
}

func (a *Agent) runSubmission(ctx context.Context, sessionID string, in Submission, selection provider.Selection, unattended bool) (<-chan event.Event, error) {
	stream, _, err := a.submit(ctx, sessionID, in, selection, unattended)
	if err != nil {
		return nil, err
	}
	if stream == nil {
		return closedEvents(), nil
	}
	return stream, nil
}

// Submit admits one prompt and reports whether it joined a live turn. A nil
// stream with joined=true means the text was queued into the running turn.
func (a *Agent) Submit(ctx context.Context, sessionID string, in Submission, selection provider.Selection) (<-chan event.Event, bool, error) {
	return a.submit(ctx, sessionID, in, selection, false)
}

func (a *Agent) submit(ctx context.Context, sessionID string, in Submission, selection provider.Selection, unattended bool) (<-chan event.Event, bool, error) {
	text := strings.TrimSpace(in.Text)
	if text == "" {
		return nil, false, errors.New("prompt is empty")
	}
	delivery := in.Delivery
	if delivery == "" {
		delivery = "steer"
	}
	if delivery != "steer" && delivery != "queue" {
		return nil, false, fmt.Errorf("delivery must be steer or queue")
	}
	item := session.QueuedInput{
		ID:       in.ID,
		Kind:     session.KindPrompt,
		Text:     text,
		Delivery: delivery,
		Data:     submissionData(in),
	}
	if item.ID == "" {
		item.ID = a.nextID("inp")
	}
	if in.ID != "" {
		found, pending, err := a.store.InputState(sessionID, item.ID)
		if err != nil {
			return nil, false, err
		}
		if found {
			if !pending {
				// An earlier retry already delivered it.
				return nil, false, nil
			}
			return a.submitAdmitted(ctx, sessionID, item, selection, unattended)
		}
	}
	if a.hooks != nil {
		if exists, hashErr := a.store.HasEvents(sessionID); hashErr == nil && !exists {
			a.hooks.SessionEvent(ctx, "session.created", sessionID)
		}
	}
	if _, err := a.persistInbox(sessionID, item); err != nil {
		return nil, false, err
	}
	return a.submitAdmitted(ctx, sessionID, item, selection, unattended)
}

func submissionData(in Submission) map[string]any {
	if len(in.Attachments) == 0 && len(in.Extra) == 0 {
		return nil
	}
	data := make(map[string]any, len(in.Extra)+1)
	if len(in.Attachments) > 0 {
		data["attachments"] = in.Attachments
	}
	for key, value := range in.Extra {
		if value != nil {
			data[key] = value
		}
	}
	return data
}

// submitAdmitted schedules an already durable inbox item: it joins a live turn
// or starts one. Queue delivery parks only when the caller asks it not to wake.
func (a *Agent) submitAdmitted(ctx context.Context, sessionID string, item session.QueuedInput, selection provider.Selection, unattended bool) (<-chan event.Event, bool, error) {
	for {
		a.mu.Lock()
		handle := a.runs[sessionID]
		if handle == nil {
			a.mu.Unlock()
			stream, err := a.start(ctx, sessionID, selection, "", false, unattended)
			if errors.Is(err, ErrBusy) {
				continue
			}
			return stream, false, err
		}
		if !handle.finishing {
			if !inboxHas(handle.inbox, item.ID) {
				handle.inbox = append(handle.inbox, pendingItemFrom(item))
			}
			a.mu.Unlock()
			return nil, true, nil
		}
		done := handle.done
		a.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return nil, false, ctx.Err()
		}
	}
}

// persistInbox writes one admission event and publishes it, so every client's
// pending projection sees the input immediately instead of polling for it.
func (a *Agent) persistInbox(sessionID string, item session.QueuedInput) (event.Event, error) {
	ev, err := a.store.QueueItem(sessionID, item)
	if err != nil {
		return ev, err
	}
	a.Publish(sessionID, ev)
	return ev, nil
}

func inboxHas(items []pendingItem, inputID string) bool {
	if inputID == "" {
		return false
	}
	for _, item := range items {
		if item.input.ID == inputID {
			return true
		}
	}
	return false
}

// closedEvents is an already-closed stream for a prompt that joined a live turn.
func closedEvents() <-chan event.Event {
	ch := make(chan event.Event)
	close(ch)
	return ch
}

// Resume continues a turn whose claim survived a process exit. claimID is the
// open claim from the session log; empty means only pending input survived.
// A claim that has already been resumed MaxResumeAttempts times is abandoned,
// so a turn that crashes at the same point cannot loop on every start.
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
	if claimID != "" {
		if recovery.Attempts >= session.MaxResumeAttempts {
			// Release the claim so the session is usable again, and record why
			// the turn stopped. The notice is durable, so the transcript shows
			// the reason instead of an unexplained stop.
			if err := a.store.CancelTurn(sessionID, claimID); err != nil {
				logx.Error("abandon turn failed", logx.Session(sessionID), logx.Err(err))
			}
			notice := fmt.Sprintf("Turn %s was abandoned after %d failed resume attempts.", claimID, recovery.Attempts)
			_ = a.emit(sessionID, event.SyntheticEvent(notice))
			return nil, fmt.Errorf("agent: turn %s exceeded %d resume attempts", claimID, session.MaxResumeAttempts)
		}
		// The attempt is durable before the turn runs, so a crash during the
		// resume still counts against the budget.
		if _, err := a.store.Append(sessionID, event.TurnResumedEvent(claimID)); err != nil {
			return nil, err
		}
	}
	return a.start(ctx, sessionID, selection, claimID, true, false)
}

func (a *Agent) start(ctx context.Context, sessionID string, selection provider.Selection, claimID string, resume bool, unattended bool) (<-chan event.Event, error) {
	a.cancelWarming(sessionID)
	profile := a.profile(sessionID)
	if selection.Provider == "" && selection.Model == "" && profile.Model != "" {
		providerID, modelID, err := agents.ValidateModel(profile.Model)
		if err != nil {
			logx.Warn("agent model is invalid", logx.F("agent", profile.ID), logx.Err(err))
		} else {
			selection = provider.Selection{Provider: providerID, Model: modelID}
		}
	}
	target, err := a.catalog.Resolve(selection)
	if err != nil {
		return nil, err
	}
	if target.Model == "" {
		return nil, errors.New("model is not configured")
	}
	if len(profile.Options) > 0 {
		target.Options = mergeOptions(target.Options, profile.Options)
	}
	if len(profile.Headers) > 0 {
		target.Headers = mergeHeaders(target.Headers, profile.Headers)
	}
	// The snapshot starts with the turn and runs beside the model request; it
	// only has to be ready before local tools can modify files. This keeps a
	// slow git snapshot from delaying the first token.
	var snap *pendingSnapshot
	turn := &turnStart{}
	for {
		a.mu.Lock()
		handle := a.runs[sessionID]
		if handle == nil {
			if ctx.Err() != nil {
				a.mu.Unlock()
				return nil, ctx.Err()
			}
			runCtx, release := a.runContext(ctx)
			if a.snapshots != nil {
				snap = &pendingSnapshot{done: make(chan struct{})}
				go func() {
					snap.id, snap.err = a.snapshots.Capture(runCtx)
					close(snap.done)
				}()
			}
			// Subscribe before the first durable write so the caller sees the
			// turn's events and the claim cannot outlive a lost prompt.
			sub := a.hub.Subscribe(sessionID)
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
			pending, err := a.store.Recovery(sessionID)
			if err != nil {
				a.mu.Unlock()
				sub.Close()
				release(context.Canceled)
				return nil, err
			}
			handle = &runHandle{release: release, claimID: claim, inbox: initialInbox(pending.Inputs), unattended: unattended, done: make(chan struct{})}
			a.runs[sessionID] = handle
			a.mu.Unlock()

			if a.perm != nil {
				rules := append(permission.Rules{}, profile.Permissions...)
				if extra, err := a.store.Permissions(sessionID); err == nil {
					rules = append(rules, extra...)
				}
				a.perm.SetSessionRules(sessionID, rules)
			}
			steps := a.maxSteps
			if profile.Steps > 0 {
				steps = profile.Steps
			}

			go func() {
				defer func() {
					cause := context.Cause(runCtx)
					switch {
					case errors.Is(cause, errShutdown):
						// Keep the claim and pending input for the next start.
					case handle.handoff:
						// The move handoff continues its claim at the destination.
					case errors.Is(cause, errInterrupt):
						// Interrupt released the claim without deleting input.
					default:
						if err := a.store.FinishTurn(sessionID, claim); err != nil {
							logx.Error("finish turn failed", logx.Session(sessionID), logx.Err(err))
						}
					}
					// Permissions and plugin hooks settle before the session is
					// released, so a successor never inherits a stale rule set
					// or sees an idle event after it already started.
					if a.perm != nil {
						a.perm.ClearSessionRules(sessionID)
					}
					if a.hooks != nil {
						a.hooks.SessionEvent(context.Background(), "session.idle", sessionID)
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
					if errors.Is(cause, errInterrupt) && handle.resumeOnExit && !handle.handoff {
						go a.resumePending(sessionID)
					}
				}()
				a.loop(runCtx, sessionID, target, profile.ID, steps, snap, turn, unattended)
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

// WaitIdle blocks until one session has no active turn, or ctx ends first.
// A queued move uses it to apply at the next boundary instead of rejecting a
// running session.
func (a *Agent) WaitIdle(ctx context.Context, sessionID string) error {
	for {
		a.mu.Lock()
		handle := a.runs[sessionID]
		if handle == nil {
			a.mu.Unlock()
			return nil
		}
		done := handle.done
		a.mu.Unlock()
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Interrupt stops the running turn and releases its claim without deleting any
// pending input. With resume, pending steering input and control items continue
// in a successor turn; queued prompts stay parked until the next wake.
func (a *Agent) Interrupt(sessionID string, resume bool) bool {
	a.mu.Lock()
	handle := a.runs[sessionID]
	if handle == nil {
		a.mu.Unlock()
		if resume {
			go a.resumePending(sessionID)
		}
		return false
	}
	// A finishing turn still owns a possible wrap-up model call; cancelling it
	// is safe because the turn has already stopped accepting new input.
	handle.finishing = true
	handle.resumeOnExit = resume
	// The inbox items are already durable; the successor reloads them from the
	// store. Clearing here only stops this turn from delivering more.
	handle.inbox = nil
	claim := handle.claimID
	cancel := handle.release
	a.mu.Unlock()
	// Release the claim without cancelling input, so a crash in the window does
	// not bring the stopped turn back and a successor can pick the input up.
	if err := a.store.ReleaseTurn(sessionID, claim); err != nil {
		logx.Error("release turn failed", logx.Session(sessionID), logx.Err(err))
	}
	cancel(errInterrupt)
	return true
}

// Wake starts a turn for pending steering input when the session is idle. A
// parked item changed to steer is a request for it to run, and nothing else
// would start the turn that now owns it.
func (a *Agent) Wake(sessionID string) {
	if a.base.Err() != nil {
		return
	}
	go a.resumePending(sessionID)
}

// resumePending delivers steering input and control items an interruption or a
// delivery change left behind. It waits out a finishing turn first: settling
// tools and permissions must finish before a replacement reads the same events.
// Queue-only pending input stays parked until something wakes the session.
func (a *Agent) resumePending(sessionID string) {
	for {
		a.mu.Lock()
		handle := a.runs[sessionID]
		if handle == nil {
			a.mu.Unlock()
			break
		}
		if !handle.finishing {
			a.mu.Unlock()
			return
		}
		done := handle.done
		a.mu.Unlock()
		select {
		case <-done:
		case <-a.base.Done():
			return
		}
	}
	if a.base.Err() != nil {
		return
	}
	recovery, err := a.store.Recovery(sessionID)
	if err != nil {
		logx.Error("read steering input failed", logx.Session(sessionID), logx.Err(err))
		return
	}
	steering := false
	for _, input := range recovery.Inputs {
		if input.Delivery != "queue" {
			steering = true
			break
		}
	}
	if !steering {
		return
	}
	meta, err := a.store.Meta(sessionID)
	if err != nil {
		logx.Error("read session selection failed", logx.Session(sessionID), logx.Err(err))
		return
	}
	stream, err := a.start(context.Background(), sessionID, provider.Selection{
		Provider: meta.Provider,
		Model:    meta.Model,
	}, "", true, false)
	if err != nil {
		if !errors.Is(err, ErrBusy) {
			logx.Warn("resume steering input failed", logx.Session(sessionID), logx.Err(err))
		}
		return
	}
	for range stream {
	}
}

// Running reports whether a session has an active turn, including one that is
// emitting its final events.
func (a *Agent) Running(sessionID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.runs[sessionID] != nil
}

// Enqueue persists steering input for a session. The turn delivers it at the
// next boundary; an idle session starts a turn for it.
func (a *Agent) Enqueue(sessionID, prompt string) (string, error) {
	return a.EnqueueDelivery(sessionID, prompt, "steer")
}

// EnqueueDelivery admits input with its delivery preference. Steering joins the
// running turn at its next boundary; queue delivery waits for the idle boundary
// inside that turn, or for its own turn when the session is idle.
func (a *Agent) EnqueueDelivery(sessionID, prompt, delivery string) (string, error) {
	text := strings.TrimSpace(prompt)
	if text == "" {
		return "", errors.New("prompt is empty")
	}
	delivery = normalizeDelivery(delivery)
	if delivery != "steer" && delivery != "queue" {
		return "", fmt.Errorf("delivery must be steer or queue")
	}
	item := session.QueuedInput{
		ID:       a.nextID("inp"),
		Kind:     session.KindPrompt,
		Text:     text,
		Delivery: delivery,
	}
	if _, err := a.persistInbox(sessionID, item); err != nil {
		return "", err
	}
	stream, _, err := a.submitAdmitted(context.Background(), sessionID, item, provider.Selection{}, false)
	if err != nil {
		return item.ID, err
	}
	if stream != nil {
		go func() {
			for range stream {
			}
		}()
	}
	return item.ID, nil
}

// SetInputDelivery moves a pending item between steer and queue delivery. It
// reports whether the item was still held by an active turn.
func (a *Agent) SetInputDelivery(sessionID, inputID, delivery string) bool {
	if delivery != "steer" && delivery != "queue" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	handle := a.runs[sessionID]
	if handle == nil {
		return false
	}
	for index := range handle.inbox {
		if handle.inbox[index].input.ID != inputID {
			continue
		}
		handle.inbox[index].delivery = delivery
		handle.inbox[index].input.Delivery = delivery
		return true
	}
	return false
}

// removeInput deletes one item and reports whether it was present.
func removeInput(items *[]pendingItem, inputID string) (pendingItem, bool) {
	for index, item := range *items {
		if item.input.ID != inputID {
			continue
		}
		*items = append((*items)[:index], (*items)[index+1:]...)
		return item, true
	}
	return pendingItem{}, false
}

// CancelInput removes one pending input from the active turn. It reports
// whether the item was still queued in memory; store-level cancellation is the
// caller's responsibility and covers items not yet loaded.
func (a *Agent) CancelInput(sessionID, inputID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	handle := a.runs[sessionID]
	if handle == nil {
		return false
	}
	if _, ok := removeInput(&handle.inbox, inputID); ok {
		return true
	}
	return false
}

// Notify admits a synthetic event for delivery at a boundary. It stays outside
// model history until delivered, so a queued wake-up that is cancelled never
// becomes a stale instruction. An idle session is woken; a running turn reads
// the event before it finishes.
//
// A scheduled wake-up starts an unattended turn: nobody is guaranteed to be
// watching it, so permission asks are denied instead of blocking forever.
func (a *Agent) Notify(sessionID string, ev event.Event) error {
	if a.base.Err() != nil {
		return nil
	}
	text, _ := ev.Data["text"].(string)
	if strings.TrimSpace(text) == "" {
		return errors.New("notification text is empty")
	}
	data := make(map[string]any, len(ev.Data))
	for key, value := range ev.Data {
		data[key] = value
	}
	item := session.QueuedInput{
		ID:       a.nextID("inp"),
		Kind:     session.KindSynthetic,
		Text:     text,
		Delivery: "steer",
		Data:     map[string]any{"event": string(ev.Type), "data": data},
	}
	if _, err := a.persistInbox(sessionID, item); err != nil {
		return err
	}
	meta, _ := a.store.Meta(sessionID)
	stream, _, err := a.submitAdmitted(context.Background(), sessionID, item, provider.Selection{
		Provider: meta.Provider,
		Model:    meta.Model,
	}, ev.Type == event.ScheduleFired)
	if err != nil {
		return err
	}
	if stream != nil {
		go func() {
			for range stream {
			}
		}()
	}
	return nil
}

// boundary is what one promotion pass found in the inbox. prompts are the items
// to add to the conversation now; control names a control item that owns this
// boundary and was already consumed, so the caller handles it before spending a
// model call on the prompts around it.
type boundary struct {
	prompts []pendingItem
	control pendingKind
	// controlItem is the consumed control, so the caller can name its inbox id
	// in the event that settles it.
	controlItem pendingItem
	// notice reports that a synthetic item was included in prompts.
	notice bool
}

// takeBoundary promotes steer-delivery items at a safe step boundary. A control
// owns its boundary: a steered compaction runs before earlier steers so their
// text stays verbatim after the checkpoint, while a move only stops delivery
// that follows it. Queue-delivery items are promoted one at a time only at the
// entry or idle boundary, never between tool steps.
func (a *Agent) takeBoundary(sessionID string, allowQueued bool) boundary {
	a.mu.Lock()
	defer a.mu.Unlock()
	handle := a.runs[sessionID]
	if handle == nil || len(handle.inbox) == 0 {
		return boundary{}
	}
	steers := make([]pendingItem, 0, len(handle.inbox))
	for _, item := range handle.inbox {
		if item.delivery != "queue" {
			steers = append(steers, item)
		}
	}
	if len(steers) > 0 {
		controlAt := -1
		for index, item := range steers {
			if item.control() {
				controlAt = index
				break
			}
		}
		if controlAt >= 0 && steers[controlAt].kind == pendingCompaction && controlAt > 0 {
			// The checkpoint must not summarize steers admitted before it. A
			// move earlier in the queue is never crossed.
			consumed := steers[controlAt]
			handle.inbox = removeItems(handle.inbox, []pendingItem{consumed})
			return boundary{control: consumed.kind, controlItem: consumed}
		}
		if controlAt == 0 {
			consumed := steers[0]
			handle.inbox = removeItems(handle.inbox, []pendingItem{consumed})
			return boundary{control: consumed.kind, controlItem: consumed}
		}
		limit := len(steers)
		if controlAt > 0 {
			limit = controlAt
		}
		promoted := append([]pendingItem(nil), steers[:limit]...)
		handle.inbox = removeItems(handle.inbox, promoted)
		result := boundary{prompts: promoted}
		for _, item := range promoted {
			if item.kind == pendingNotice {
				result.notice = true
			}
		}
		return result
	}
	if !allowQueued {
		return boundary{}
	}
	// The entry or idle boundary may take exactly one queued item. A queued
	// control is handled before any queued prompt behind it.
	for _, item := range handle.inbox {
		if item.delivery != "queue" {
			continue
		}
		handle.inbox = removeItems(handle.inbox, []pendingItem{item})
		if item.control() {
			return boundary{control: item.kind, controlItem: item}
		}
		result := boundary{prompts: []pendingItem{item}}
		if item.kind == pendingNotice {
			result.notice = true
		}
		return result
	}
	return boundary{}
}

// hasPromotable reports whether the next boundary would promote anything
// without consuming it, so a control-only drain can finish cleanly.
func (a *Agent) hasPromotable(sessionID string, allowQueued bool) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	handle := a.runs[sessionID]
	if handle == nil {
		return false
	}
	for _, item := range handle.inbox {
		if item.delivery != "queue" || allowQueued {
			return true
		}
	}
	return false
}

// removeItems drops the promoted items by inbox id and keeps the arrival order
// of everything that remains.
func removeItems(items []pendingItem, drop []pendingItem) []pendingItem {
	if len(drop) == 0 {
		return items
	}
	ids := make(map[string]bool, len(drop))
	for _, item := range drop {
		if item.input.ID != "" {
			ids[item.input.ID] = true
		}
	}
	if len(ids) == 0 {
		return items
	}
	out := items[:0]
	for _, item := range items {
		if ids[item.input.ID] {
			continue
		}
		out = append(out, item)
	}
	return out
}

// initialInbox rebuilds the ordered inbox a turn starts with. Every kind shares
// one list, so a compaction or move keeps its place relative to prompts.
func initialInbox(inputs []session.QueuedInput) []pendingItem {
	inbox := make([]pendingItem, 0, len(inputs))
	for _, input := range inputs {
		inbox = append(inbox, pendingItemFrom(input))
	}
	return inbox
}

// deliver records promoted prompts and synthetic items before the next model
// request. The id on the message marks the durable inbox item as delivered, so
// replay never delivers it twice.
func (a *Agent) deliver(sessionID string, items []pendingItem, agentID string, target provider.Target, turn *turnStart) bool {
	for _, item := range items {
		ev := deliveryEvent(item, agentID, target)
		seq, err := a.emitSeq(sessionID, ev)
		if err != nil {
			a.fail(sessionID, err)
			return false
		}
		if turn != nil && turn.seq == 0 {
			turn.seq = seq
		}
	}
	return true
}

// deliveryEvent builds the visible event an inbox item becomes.
func deliveryEvent(item pendingItem, agentID string, target provider.Target) event.Event {
	if item.kind == pendingNotice {
		evType := event.Synthetic
		if name, _ := item.input.Data["event"].(string); name != "" {
			evType = event.Type(name)
		}
		data := map[string]any{}
		if payload, ok := item.input.Data["data"].(map[string]any); ok {
			for key, value := range payload {
				data[key] = value
			}
		}
		ev := event.New(evType, data)
		if evType == event.Synthetic {
			ev.Data["text"] = item.input.Text
		}
		ev.Data["inputId"] = item.input.ID
		ev.Data["delivery"] = item.delivery
		return ev
	}
	user := event.User(item.input.Text)
	if attachments := attachmentsFrom(item.input.Data["attachments"]); len(attachments) > 0 {
		user = event.UserWithAttachments(item.input.Text, attachments)
	}
	if item.input.ID != "" {
		user.Data["inputId"] = item.input.ID
	}
	for key, value := range item.input.Data {
		if key == "attachments" || value == nil {
			continue
		}
		user.Data[key] = value
	}
	user.Data["delivery"] = item.delivery
	user.Data["agent"] = agentID
	user.Data["provider"] = target.ProviderID
	user.Data["model"] = target.Model
	return user
}

// attachmentsFrom accepts either the in-memory attachment slice or the generic
// JSON shape a restarted core decodes the inbox payload into.
func attachmentsFrom(value any) []model.Attachment {
	if value == nil {
		return nil
	}
	if attachments, ok := value.([]model.Attachment); ok {
		return attachments
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	var attachments []model.Attachment
	if json.Unmarshal(raw, &attachments) != nil {
		return nil
	}
	return attachments
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

// Publish sends an already persisted event to live subscribers.
func (a *Agent) Publish(sessionID string, ev event.Event) {
	a.hub.Publish(sessionID, ev)
}

// Resolve turns a selection into a provider target.
func (a *Agent) Resolve(selection provider.Selection) (provider.Target, error) {
	return a.catalog.Resolve(selection)
}

// DefaultTarget is the configured default provider/model.
func (a *Agent) DefaultTarget() provider.Target { return a.catalog.Default() }

// Sample runs one transient model call for an MCP server's sampling request.
// Nothing is written to a session or the event log.
func (a *Agent) Sample(ctx context.Context, system string, messages []model.Message) (string, string, error) {
	target := a.catalog.Default()
	wire := target.RequestModel
	if wire == "" {
		wire = target.Model
	}
	if strings.TrimSpace(system) != "" {
		messages = append([]model.Message{{Role: model.RoleSystem, Content: strings.TrimSpace(system)}}, messages...)
	}
	stream, err := target.Provider.Stream(ctx, model.Request{
		Model:    wire,
		Messages: messages,
		Headers:  target.Headers,
		Options:  target.Options,
	})
	if err != nil {
		return "", target.Model, err
	}
	defer stream.Close()
	var b strings.Builder
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", target.Model, err
		}
		b.WriteString(chunk.Text)
	}
	return b.String(), target.Model, nil
}

// Providers returns the sanitized provider catalog for clients.
func (a *Agent) Providers() []provider.ProviderInfo { return a.catalog.Providers() }

// Compact summarizes the conversation on demand, even when it is under budget.
// While a turn is running the request joins the ordered inbox as a control item;
// an idle session compacts immediately.
func (a *Agent) Compact(ctx context.Context, sessionID string, selection provider.Selection) error {
	return a.CompactDelivery(ctx, sessionID, selection, "steer")
}

// CompactDelivery admits a manual compaction with its delivery mode. A steered
// compaction owns the next boundary; a queued one waits for the idle boundary.
func (a *Agent) CompactDelivery(ctx context.Context, sessionID string, selection provider.Selection, delivery string) error {
	target, err := a.catalog.Resolve(selection)
	if err != nil {
		return err
	}
	if target.Model == "" {
		return errors.New("model is not configured")
	}
	delivery = normalizeDelivery(delivery)
	if delivery != "steer" && delivery != "queue" {
		return fmt.Errorf("delivery must be steer or queue")
	}
	item := session.QueuedInput{ID: a.nextID("cmp"), Kind: session.KindCompaction, Delivery: delivery}
	a.mu.Lock()
	if handle := a.runs[sessionID]; handle != nil && !handle.finishing {
		if _, err := a.persistInbox(sessionID, item); err != nil {
			a.mu.Unlock()
			return err
		}
		// The request joins the ordered inbox, so it lands after the prompts
		// that arrived before it and before those that arrived after it.
		handle.inbox = append(handle.inbox, pendingItemFrom(item))
		a.mu.Unlock()
		return nil
	}
	a.mu.Unlock()
	if a.Running(sessionID) {
		return ErrBusy
	}
	ctx, release := a.runContext(ctx)
	defer release(nil)
	current, err := a.store.Context(sessionID)
	if err != nil {
		return err
	}
	return a.compact(ctx, sessionID, target, current, a.keep, "manual", item.ID)
}

// QueueMove admits a move control into the running turn. The turn stops at its
// next safe boundary and keeps the claim, so the move can continue at the
// destination. An idle session returns false and the caller applies it now.
func (a *Agent) QueueMove(sessionID, target, directory, delivery string) (string, bool, error) {
	delivery = normalizeDelivery(delivery)
	if delivery != "steer" && delivery != "queue" {
		return "", false, fmt.Errorf("delivery must be steer or queue")
	}
	item := session.QueuedInput{
		ID:       a.nextID("mov"),
		Kind:     session.KindMove,
		Delivery: delivery,
		Data:     map[string]any{"target": target, "directory": directory},
	}
	a.mu.Lock()
	handle := a.runs[sessionID]
	if handle == nil || handle.finishing {
		a.mu.Unlock()
		return "", false, nil
	}
	if _, err := a.persistInbox(sessionID, item); err != nil {
		a.mu.Unlock()
		return "", false, err
	}
	handle.inbox = append(handle.inbox, pendingItemFrom(item))
	a.mu.Unlock()
	return item.ID, true, nil
}

func (a *Agent) markHandoff(sessionID string) {
	a.mu.Lock()
	if handle := a.runs[sessionID]; handle != nil {
		handle.handoff = true
	}
	a.mu.Unlock()
}

func (a *Agent) loop(ctx context.Context, sessionID string, target provider.Target, agentID string, maxSteps int, snap *pendingSnapshot, turn *turnStart, unattended bool) {
	// Name the conversation while the first step runs, as the reference does:
	// the title model must not delay the turn and must survive its cancellation.
	if a.titlesOn {
		titleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
		go func() {
			defer cancel()
			a.finishTitle(titleCtx, sessionID, target)
		}()
	}
	var state turnState
	idle := false
	stepped := false
	for step := 0; ; {
		if ctx.Err() != nil {
			_ = a.emit(sessionID, event.End("interrupted"))
			return
		}
		// The entry and idle boundaries may take one queued item; between tool
		// steps only steers join the turn.
		pending := a.takeBoundary(sessionID, !stepped || idle)
		delivered := len(pending.prompts) > 0
		if delivered {
			if !a.deliver(sessionID, pending.prompts, agentID, target, turn) {
				return
			}
			step = 0
			state.reset()
			idle = false
		}
		// A control item owns this boundary. It is consumed here so the loop
		// re-examines the inbox before spending a model call, which keeps the
		// prompts behind it verbatim instead of folding them into the summary.
		if pending.control != "" {
			if pending.control == pendingCompaction {
				current, err := a.store.Context(sessionID)
				if err != nil {
					a.fail(sessionID, err)
					return
				}
				inputID := pending.controlItem.input.ID
				if err := a.compact(ctx, sessionID, target, current, a.keep, "manual", inputID); err != nil && !strings.Contains(err.Error(), "nothing to compact") {
					a.fail(sessionID, err)
					return
				}
				state.recovered = true
			}
			if pending.control == pendingMove {
				// Stop here and keep the claim: the server applies the move and
				// the destination resumes the same turn from the event log.
				a.recordSnapshot(ctx, sessionID, turn.seq, snap)
				a.markHandoff(sessionID)
				_ = a.emit(sessionID, event.End("done"))
				return
			}
			// A turn started only to run a control must not spend a model call
			// on unchanged history once the control is done.
			if !stepped && !a.hasPromotable(sessionID, true) {
				a.recordSnapshot(ctx, sessionID, turn.seq, snap)
				a.finishTitle(ctx, sessionID, target)
				if a.continueGoal(ctx, sessionID, target) {
					step = 0
					state.reset()
					idle = false
					continue
				}
				_ = a.emit(sessionID, event.End("done"))
				return
			}
			continue
		}
		if idle {
			a.recordSnapshot(ctx, sessionID, turn.seq, snap)
			a.finishTitle(ctx, sessionID, target)
			// An active goal judges the turn and may continue it. The
			// continuation is delivered as a synthetic message, so the next
			// loop iteration runs a fresh step budget.
			if a.continueGoal(ctx, sessionID, target) {
				step = 0
				state.reset()
				idle = false
				continue
			}
			_ = a.emit(sessionID, event.End("done"))
			return
		}
		if maxSteps > 0 && step >= maxSteps {
			a.recordSnapshot(ctx, sessionID, turn.seq, snap)
			a.wrapUp(ctx, sessionID, target, agentID, maxSteps)
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
		if a.precomputeOn && !target.NativeCompaction && !a.due(current.Tokens, target.Window) {
			a.precompute(sessionID, target, current)
		}
		if a.due(current.Tokens, target.Window) {
			if !target.NativeCompaction && a.usePrecomputed(sessionID, target, current) {
				// A precomputed summary replaced the history already.
			} else if err := a.compact(ctx, sessionID, target, current, a.keep, "auto", ""); err != nil {
				a.fail(sessionID, err)
				return
			}
			if current, err = a.store.Context(sessionID); err != nil {
				a.fail(sessionID, err)
				return
			}
			state.recovered = true
		}

		messages := current.Messages
		if reminder := a.goalReminder(sessionID, messages); reminder != "" {
			// A compacted history may no longer show the objective. Add it for
			// this request only, so the cached prefix stays warm.
			messages = append(append([]model.Message{}, messages...), model.Message{
				Role:    model.RoleUser,
				Content: reminder,
			})
		}
		batch := newToolBatch(a, ctx, sessionID, target, unattended)
		result := a.stream(ctx, sessionID, messages, target, a.definitions(sessionID), "", current.Tokens, batch.start)
		a.touchWarming(sessionID, target)
		if result.Usage != nil {
			if recordErr := a.emit(sessionID, event.UsageEvent(result.Usage.Input, result.Usage.Output, result.Usage.CacheRead, result.Usage.CacheWrite)); recordErr != nil {
				a.fail(sessionID, recordErr)
				return
			}
		}
		if result.Err != nil {
			if ctx.Err() != nil {
				if recordErr := a.recordAssistant(sessionID, result, target, agentID); recordErr != nil {
					a.fail(sessionID, recordErr)
					return
				}
				batch.abort()
				_ = a.emit(sessionID, event.End("interrupted"))
				return
			}
			// The provider rejected the prompt as too long: summarize harder and
			// retry the same step. Anything already received means the prompt
			// itself fit, so recovery no longer applies. Each rejection shrinks
			// the verbatim tail further, so a low estimate cannot strand the
			// turn after a single attempt.
			if provider.IsContextOverflow(result.Err) && state.overflowShrinks < len(overflowShrinkSteps) && strings.TrimSpace(result.Text) == "" && len(result.Calls) == 0 {
				step := overflowShrinkSteps[state.overflowShrinks]
				state.overflowShrinks++
				batch.abort()
				keep := int(float64(a.keep) * step)
				if compactErr := a.compact(ctx, sessionID, target, current, keep, "overflow", ""); compactErr == nil {
					state.recovered = true
					continue
				}
				// A summary that cannot be written yet must not consume the
				// ladder, so the same step is tried by the next rejection.
				state.overflowShrinks--
			}
			// Retry only failures that produced no output, so a repeated request
			// cannot duplicate text in the transcript. An unreachable host is
			// retried for up to one hour even after the ordinary budget ends.
			timeout := provider.IsTimeout(result.Err)
			if timeout {
				state.timeouts++
			}
			unreachable := provider.IsUnreachable(result.Err)
			withinUnreachable := unreachable &&
				(state.unreachableSince.IsZero() || time.Since(state.unreachableSince) < unreachableWindow)
			retryBudget := state.attempts < a.retries || withinUnreachable
			retryable := provider.IsRetryable(result.Err) || unreachable
			if retryBudget && (!timeout || state.timeouts <= maxTimeouts) && strings.TrimSpace(result.Text) == "" && len(result.Calls) == 0 && retryable {
				state.attempts++
				delay := a.retryDelayFor(result.Err, &state)
				batch.abort()
				_ = a.emit(sessionID, event.RetryScheduled(state.attempts, time.Now().Add(delay), result.Err.Error()))
				if !sleep(ctx, delay) {
					_ = a.emit(sessionID, event.End("interrupted"))
					return
				}
				continue
			}
			if strings.TrimSpace(result.Text) != "" {
				if recordErr := a.recordAssistant(sessionID, result, target, agentID); recordErr != nil {
					a.fail(sessionID, recordErr)
					return
				}
				batch.abort()
				// Keep the work already streamed and ask for the remainder
				// instead of dropping a partially delivered answer.
				if state.streamResumes < maxStreamContinuations && provider.IsRetryable(result.Err) {
					state.streamResumes++
					if emitErr := a.emit(sessionID, event.SyntheticEvent(streamResumePrompt)); emitErr != nil {
						a.fail(sessionID, emitErr)
						return
					}
					continue
				}
			}
			batch.abort()
			a.fail(sessionID, result.Err)
			return
		}
		state.recovered = false
		state.attempts = 0
		a.markResponse(sessionID)
		if err := a.recordAssistant(sessionID, result, target, agentID); err != nil {
			a.fail(sessionID, err)
			return
		}
		// A response that ends with neither a finish reason nor any output is a
		// truncated stream, not an empty answer. Failing here keeps a silently
		// cut turn from looking like a deliberate stop.
		if result.Finish == "" && strings.TrimSpace(result.Text) == "" && len(result.Calls) == 0 {
			batch.abort()
			a.fail(sessionID, provider.ErrIncompleteStream)
			return
		}
		if result.Finish == "length" {
			batch.abort()
			if state.outputLimits >= maxOutputContinuations {
				a.fail(sessionID, errors.New("response still truncated after two output limit continuations"))
				return
			}
			state.outputLimits++
			if emitErr := a.emit(sessionID, event.SyntheticEvent(outputLimitPrompt)); emitErr != nil {
				a.fail(sessionID, emitErr)
				return
			}
			idle = false
			stepped = true
			continue
		}
		if len(result.Calls) == 0 {
			batch.abort()
			// The turn would go idle. The loop head delivers any pending input
			// first and finalizes only when nothing is left, so a queued prompt
			// continues the same turn instead of starting a new one.
			idle = true
			stepped = true
			continue
		}
		a.recordSnapshot(ctx, sessionID, turn.seq, snap)
		if !batch.announceAndFinish() {
			return
		}
		if batch.declined.Load() {
			// A user rejection is a hard stop, matching the reference step
			// interrupt. Tool results are already persisted for replay.
			_ = a.emit(sessionID, event.End("interrupted"))
			return
		}
		step++
		stepped = true
		state.outputLimits = 0
		state.streamResumes = 0
	}
}

// recordSnapshot waits for the turn's snapshot and records it once. It is
// called before tools run and before the turn ends, never before the first
// model request, so capture overlaps the request.
func (a *Agent) recordSnapshot(ctx context.Context, sessionID string, turnSeq int64, snap *pendingSnapshot) {
	if snap == nil || turnSeq == 0 {
		return
	}
	snap.once.Do(func() {
		select {
		case <-snap.done:
		case <-ctx.Done():
			return
		}
		if snap.err != nil {
			logx.Warn("capture snapshot failed", logx.Session(sessionID), logx.Err(snap.err))
			return
		}
		if snap.id == "" {
			return
		}
		if err := a.emit(sessionID, event.TurnSnapshotEvent(turnSeq, snap.id)); err != nil {
			logx.Warn("record snapshot failed", logx.Session(sessionID), logx.Err(err))
		}
	})
}

// finishTitle names an untitled session once its first turn produced a reply.
// It never blocks the turn's final events for longer than one model call and
// releases the per-session claim afterwards.
func (a *Agent) finishTitle(ctx context.Context, sessionID string, target provider.Target) {
	if !a.titlesOn {
		return
	}
	if a.titleBusy(sessionID) {
		return
	}
	defer a.releaseTitle(sessionID)
	a.generateTitle(ctx, sessionID, target)
}

// recordAssistant persists one model step as an assistant message. A step that
// only called tools has no text but still carries its timing and token counts,
// so a turn's throughput covers every step the provider ran.
func (a *Agent) recordAssistant(sessionID string, result stepResult, target provider.Target, agentID string) error {
	if strings.TrimSpace(result.Text) == "" && strings.TrimSpace(result.Reasoning) == "" &&
		(result.Err != nil || len(result.Calls) == 0) {
		return nil
	}
	ev := event.AssistantWithReasoning(result.Text, result.Reasoning)
	ev.Data["provider"] = target.ProviderID
	ev.Data["model"] = target.Model
	if agentID != "" {
		ev.Data["agent"] = agentID
	}
	if !result.ReasoningStarted.IsZero() {
		ev.Data["reasoningCreated"] = result.ReasoningStarted.Format(time.RFC3339Nano)
	}
	if !result.ReasoningCompleted.IsZero() {
		ev.Data["reasoningCompleted"] = result.ReasoningCompleted.Format(time.RFC3339Nano)
	}
	if !result.Started.IsZero() {
		ev.Data["started"] = result.Started.UTC().Format(time.RFC3339Nano)
	}
	if !result.FirstToken.IsZero() {
		ev.Data["firstToken"] = result.FirstToken.UTC().Format(time.RFC3339Nano)
	}
	if !result.Streamed.IsZero() {
		ev.Data["streamed"] = result.Streamed.UTC().Format(time.RFC3339Nano)
	}
	if !result.Completed.IsZero() {
		ev.Data["completed"] = result.Completed.UTC().Format(time.RFC3339Nano)
	}
	if result.Usage != nil {
		ev.Data["outputTokens"] = result.Usage.Output
		// Providers here count reasoning inside their output token total, so the
		// separate field stays zero and clients can still add both like upstream.
		ev.Data["reasoningTokens"] = 0
	}
	if len(result.Parts) > 0 {
		ev.Data["parts"] = result.Parts
	}
	return a.emit(sessionID, ev)
}

// wrapUp spends one final model call with toolChoice "none" but keeps the tool
// definitions, so a turn that exhausted its step budget still ends with a
// summary while preserving the provider's cached prefix. The instruction is
// synthetic and never stored as history.
func (a *Agent) wrapUp(ctx context.Context, sessionID string, target provider.Target, agentID string, steps int) {
	if ctx.Err() != nil {
		_ = a.emit(sessionID, event.End("interrupted"))
		return
	}
	_ = a.emit(sessionID, event.Step(steps))

	current, err := a.store.Context(sessionID)
	if err != nil {
		a.fail(sessionID, err)
		return
	}
	messages := make([]model.Message, 0, len(current.Messages)+1)
	messages = append(messages, current.Messages...)
	messages = append(messages, model.Message{Role: model.RoleAssistant, Content: maxStepsPrompt})

	result := a.stream(ctx, sessionID, messages, target, a.definitions(sessionID), "none", current.Tokens, nil)
	a.touchWarming(sessionID, target)
	if result.Usage != nil {
		if recordErr := a.emit(sessionID, event.UsageEvent(result.Usage.Input, result.Usage.Output, result.Usage.CacheRead, result.Usage.CacheWrite)); recordErr != nil {
			a.fail(sessionID, recordErr)
			return
		}
	}
	if result.Err != nil {
		if recordErr := a.recordAssistant(sessionID, result, target, agentID); recordErr != nil {
			a.fail(sessionID, recordErr)
			return
		}
		if ctx.Err() != nil {
			_ = a.emit(sessionID, event.End("interrupted"))
			return
		}
		a.fail(sessionID, result.Err)
		return
	}
	if err := a.recordAssistant(sessionID, result, target, agentID); err != nil {
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

// retryDelayFor chooses the wait before the next attempt. An unreachable host
// gets its own capped schedule for up to one hour; every other retryable
// failure uses the general exponential backoff.
func (a *Agent) retryDelayFor(err error, state *turnState) time.Duration {
	fallback := a.backoff(state.attempts)
	if provider.IsUnreachable(err) {
		if state.unreachableSince.IsZero() {
			state.unreachableSince = time.Now()
		}
		if time.Since(state.unreachableSince) < unreachableWindow {
			delay := time.Duration(1<<uint(min(state.attempts-1, 16))) * a.retryDelay
			if delay > unreachableMaxDelay {
				return unreachableMaxDelay
			}
			return delay
		}
	}
	return retryDelayAfter(err, fallback)
}

// retryDelayAfter honors the provider's Retry-After when it is longer than the
// exponential fallback, capped so a broken header cannot stall a turn.
func retryDelayAfter(err error, fallback time.Duration) time.Duration {
	var status *provider.StatusError
	if errors.As(err, &status) && status.RetryAfter > fallback {
		if status.RetryAfter > retryAfterMax {
			return retryAfterMax
		}
		return status.RetryAfter
	}
	return fallback
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

// maxOutputTokens keeps the reply within the model's advertised limit and the
// room the prompt leaves in the context window, matching the reference
// harness. Zero means the catalog did not report a limit.
func maxOutputTokens(target provider.Target, inputTokens int) int {
	const cap = 256000
	limit := target.MaxOutput
	if limit > cap {
		limit = cap
	}
	if limit <= 0 || target.Window <= 0 {
		return limit
	}
	room := target.Window - inputTokens - target.Window/100
	if room < 1024 {
		room = 1024
	}
	if room < limit {
		return room
	}
	return limit
}

// mergeOptions overlays per-agent request options on the model defaults
// without mutating the catalog's shared map.
func mergeOptions(base, override map[string]any) map[string]any {
	out := make(map[string]any, len(base)+len(override))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range override {
		out[key] = value
	}
	return out
}

// mergeHeaders overlays per-agent request headers on the target defaults.
func mergeHeaders(base, override map[string]string) map[string]string {
	out := make(map[string]string, len(base)+len(override))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range override {
		out[key] = value
	}
	return out
}

// due reports whether the conversation should be compacted before the next call.
func (a *Agent) due(tokens, window int) bool {
	ceiling := session.Ceiling(window)
	return ceiling > 0 && tokens >= ceiling
}

// stream returns partial output even when the provider fails mid-response, so
// the caller can record it and decide between resuming and failing.
func (a *Agent) stream(ctx context.Context, sessionID string, messages []model.Message, target provider.Target, tools []model.ToolDef, toolChoice string, inputTokens int, onTool func(index int, call model.ToolCall)) stepResult {
	wire := target.RequestModel
	if wire == "" {
		wire = target.Model
	}
	if target.NoTools {
		tools = nil
	}
	if update := a.takeInstructionUpdate(sessionID, target); update != "" {
		// The update is durable and part of the transcript; it supersedes the
		// frozen sections in the system prompt without rewriting that prefix.
		messages = append(messages, model.Message{Role: model.RoleUser, Content: update, Synthetic: true})
	}
	messages = a.withSystem(sessionID, target, messages)
	options := target.Options
	promptTokens := inputTokens
	for _, message := range messages {
		if message.Role == model.RoleSystem {
			promptTokens += session.Estimate(message.Content)
		}
	}
	if limit := maxOutputTokens(target, promptTokens); limit > 0 {
		if _, set := options["max_tokens"]; !set {
			cloned := make(map[string]any, len(options)+1)
			for key, value := range options {
				cloned[key] = value
			}
			cloned["max_tokens"] = limit
			options = cloned
		}
	}
	if a.hooks != nil {
		profile := a.profile(sessionID)
		modelRef := target.Model
		if target.ProviderID != "" {
			modelRef = target.ProviderID + "/" + target.Model
		}
		updated, err := a.hooks.ApplyContext(ctx, ContextEvent{
			SessionID: sessionID,
			Agent:     profile.ID,
			Model:     modelRef,
			Messages:  messages,
			Options:   options,
		})
		if err != nil {
			logx.Warn("session context hook failed", logx.Session(sessionID), logx.Err(err))
		} else {
			if len(updated.Messages) > 0 {
				messages = updated.Messages
			}
			if updated.Options != nil {
				options = updated.Options
			}
		}
	}
	messages = boundImages(messages)
	req := model.Request{
		Model:      wire,
		Messages:   messages,
		Tools:      tools,
		ToolChoice: toolChoice,
		Headers:    target.Headers,
		Options:    options,
		CacheTTL:   target.CacheTTL,
		CacheKey:   sessionID,
	}
	stepStarted := time.Now()
	stream, err := target.Provider.Stream(ctx, req)
	if err != nil {
		logx.Warn("model request failed",
			logx.Session(sessionID),
			logx.F("provider", target.ProviderID),
			logx.F("model", target.Model),
			logx.Err(err),
		)
		return stepResult{Err: err}
	}
	defer stream.Close()
	requestStarted := time.Now()

	var result stepResult
	result.Started = stepStarted
	var text strings.Builder
	var reasoning strings.Builder
	var reasoningStarted, reasoningCompleted time.Time
	var parts []model.MessagePart
	appendPart := func(kind, value string) {
		now := time.Now().UTC().Format(time.RFC3339Nano)
		if len(parts) > 0 && parts[len(parts)-1].Type == kind {
			parts[len(parts)-1].Text += value
			parts[len(parts)-1].Completed = now
			return
		}
		parts = append(parts, model.MessagePart{Type: kind, Text: value, Created: now, Completed: now})
	}
	calls := map[int]*model.ToolCall{}
	started := map[int]bool{}
	lastIndex := -1
	start := func(index int) {
		if onTool == nil || started[index] {
			return
		}
		if call, ok := calls[index]; ok && call != nil {
			started[index] = true
			onTool(index, *call)
		}
	}
	startBefore := func(index int) {
		for i := 0; i < index; i++ {
			start(i)
		}
	}
	startAll := func() {
		for i := 0; i <= lastIndex; i++ {
			start(i)
		}
	}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			// A stream that dies mid-response still measured the tokens it
			// delivered, so the turn keeps its partial throughput.
			result.Text = text.String()
			result.Reasoning = reasoning.String()
			result.ReasoningStarted = reasoningStarted
			result.ReasoningCompleted = reasoningCompleted
			result.Parts = parts
			result.Calls = ordered(calls)
			result.Streamed = time.Now().UTC()
			result.Completed = result.Streamed
			result.Err = err
			return result
		}
		if result.FirstToken.IsZero() && (chunk.Text != "" || chunk.Reasoning != "" || len(chunk.ToolCalls) > 0) {
			result.FirstToken = time.Now().UTC()
		}
		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}
		if chunk.FinishReason != "" {
			result.Finish = chunk.FinishReason
			startAll()
		}
		if chunk.Reasoning != "" {
			now := time.Now().UTC()
			if reasoningStarted.IsZero() {
				reasoningStarted = now
			}
			reasoningCompleted = now
			reasoning.WriteString(chunk.Reasoning)
			appendPart("reasoning", chunk.Reasoning)
			a.hub.Publish(sessionID, event.ReasoningDeltaEvent(chunk.Reasoning))
		}
		if chunk.ReasoningSignature != "" || chunk.ReasoningRedacted != "" {
			// The proof belongs to the thinking block it closes, so it is kept
			// on the last reasoning part instead of a part of its own.
			if chunk.ReasoningSignature != "" {
				result.ReasoningSignature = chunk.ReasoningSignature
			}
			if chunk.ReasoningRedacted != "" {
				result.ReasoningRedacted = chunk.ReasoningRedacted
			}
			if chunk.ReasoningSource != "" {
				result.ReasoningSource = chunk.ReasoningSource
			}
			parts = attachReasoningProof(parts, chunk)
		}
		if chunk.Text != "" {
			text.WriteString(chunk.Text)
			appendPart("text", chunk.Text)
			a.hub.Publish(sessionID, event.Delta(chunk.Text))
		}
		for _, delta := range chunk.ToolCalls {
			if delta.Index > lastIndex {
				startBefore(delta.Index)
				lastIndex = delta.Index
			}
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
			// Start as soon as the accumulated arguments are complete JSON.
			// The provider gives no explicit per-call end marker, so this is
			// the earliest safe point for a single tool call.
			if call.Name != "" && json.Valid([]byte(call.Arguments)) {
				start(delta.Index)
			}
		}
	}
	startAll()
	result.Text = text.String()
	result.Reasoning = reasoning.String()
	result.ReasoningStarted = reasoningStarted
	result.ReasoningCompleted = reasoningCompleted
	result.Parts = parts
	result.Calls = ordered(calls)
	result.Streamed = time.Now().UTC()
	result.Completed = result.Streamed
	logx.Info("model call finished",
		logx.Session(sessionID),
		logx.F("provider", target.ProviderID),
		logx.F("model", target.Model),
		logx.F("finish", result.Finish),
		logx.F("text", len(result.Text)),
		logx.F("tools", len(result.Calls)),
		logx.Timing(requestStarted),
		logx.Err(result.Err),
	)
	return result
}

// attachReasoningProof records a provider's thinking proof on the reasoning part
// it closes. The two APIs differ: Anthropic starts a redacted block of its own,
// while Responses streams the summary text first and sends the encrypted blob
// only when the item completes, so there the blob belongs to the trailing part.
func attachReasoningProof(parts []model.MessagePart, chunk provider.Chunk) []model.MessagePart {
	if chunk.ReasoningRedacted != "" {
		if chunk.ReasoningSource == provider.ReasoningSourceResponses {
			for index := len(parts) - 1; index >= 0; index-- {
				if parts[index].Type != "reasoning" || parts[index].Signature != "" || parts[index].Redacted != "" {
					break
				}
				parts[index].Redacted = chunk.ReasoningRedacted
				parts[index].Source = chunk.ReasoningSource
				return parts
			}
		}
		return append(parts, model.MessagePart{
			Type:     "reasoning",
			Redacted: chunk.ReasoningRedacted,
			Source:   chunk.ReasoningSource,
		})
	}
	for index := len(parts) - 1; index >= 0; index-- {
		if parts[index].Type != "reasoning" || parts[index].Redacted != "" {
			continue
		}
		if chunk.ReasoningSignature != "" {
			parts[index].Signature = chunk.ReasoningSignature
			parts[index].Source = chunk.ReasoningSource
		}
		return parts
	}
	return parts
}

// profile resolves the session's agent, falling back to the default.
func (a *Agent) profile(sessionID string) agents.Profile {
	if a.profiles == nil {
		return agents.Profile{}
	}
	id := ""
	if meta, err := a.store.Meta(sessionID); err == nil {
		id = meta.Agent
	}
	return a.profiles.Resolve(id)
}

// systemText renders the system prompt with the frozen instruction baseline.
// The latest instruction values are delivered separately as synthetic updates.
func (a *Agent) systemText(sessionID string, target provider.Target) string {
	if a.systemParts == nil {
		return a.system(sessionID, target)
	}
	parts := a.systemParts(sessionID, target)
	snapshot := a.ensureInstructionSnapshot(sessionID, parts)
	return prompt.Parts{Base: parts.Base, Sections: sectionsFromMap(snapshot.initial)}.String()
}

// takeInstructionUpdate compares the current sections with the last observed
// values. The first call establishes the baseline; later changes are persisted
// and returned as a synthetic message for the model.
func (a *Agent) takeInstructionUpdate(sessionID string, target provider.Target) string {
	if a.systemParts == nil {
		return ""
	}
	parts := a.systemParts(sessionID, target)
	current := sectionsMap(parts.Sections)
	snapshot := a.ensureInstructionSnapshot(sessionID, parts)

	a.instructionMu.Lock()
	if mapsEqual(snapshot.current, current) {
		a.instructionMu.Unlock()
		return ""
	}
	update, sources := renderInstructionUpdate(snapshot.current, current)
	snapshot.current = current
	a.instructionMu.Unlock()
	a.persistInstructionState(sessionID, snapshot)
	if update == "" {
		return ""
	}
	_ = a.emit(sessionID, event.InstructionUpdatedEvent(update, sources))
	return update
}

// ensureInstructionSnapshot returns the cached state, loading or creating it
// once. A new baseline is written synchronously so a crash right after the
// first request cannot lose it.
func (a *Agent) ensureInstructionSnapshot(sessionID string, parts prompt.Parts) *instructionSnapshot {
	current := sectionsMap(parts.Sections)
	a.instructionMu.Lock()
	snapshot := a.instructionStates[sessionID]
	if snapshot != nil {
		a.instructionMu.Unlock()
		return snapshot
	}
	if loaded := a.loadInstructionState(sessionID); loaded != nil {
		a.instructionStates[sessionID] = loaded
		a.instructionMu.Unlock()
		return loaded
	}
	snapshot = &instructionSnapshot{initial: current, current: current, saved: true}
	a.instructionStates[sessionID] = snapshot
	a.instructionMu.Unlock()
	a.persistInstructionState(sessionID, snapshot)
	return snapshot
}

// loadInstructionState reads the latest durable baseline for a session.
func (a *Agent) loadInstructionState(sessionID string) *instructionSnapshot {
	if a.store == nil {
		return nil
	}
	events, err := a.store.Events(sessionID)
	if err != nil {
		return nil
	}
	for index := len(events) - 1; index >= 0; index-- {
		ev := events[index]
		if ev.Type != event.InstructionState {
			continue
		}
		initial := stringMap(ev, "initial")
		current := stringMap(ev, "current")
		if initial == nil || current == nil {
			continue
		}
		return &instructionSnapshot{initial: initial, current: current, saved: true}
	}
	return nil
}

// persistInstructionState writes the baseline and latest values to the session
// log. It is safe to call more than once; the last event wins.
func (a *Agent) persistInstructionState(sessionID string, snapshot *instructionSnapshot) {
	a.instructionMu.Lock()
	initial := cloneMap(snapshot.initial)
	current := cloneMap(snapshot.current)
	a.instructionMu.Unlock()
	if err := a.emit(sessionID, event.InstructionStateEvent(initial, current)); err != nil {
		logx.Warn("persist instruction state failed", logx.Session(sessionID), logx.Err(err))
	}
}

// advanceInstructionEpoch moves the baseline to the latest values after a
// compaction, because the checkpoint replaces the history that carried the
// earlier synthetic updates.
func (a *Agent) advanceInstructionEpoch(sessionID string) {
	a.instructionMu.Lock()
	snapshot := a.instructionStates[sessionID]
	if snapshot == nil {
		a.instructionMu.Unlock()
		return
	}
	snapshot.initial = cloneMap(snapshot.current)
	a.instructionMu.Unlock()
	a.persistInstructionState(sessionID, snapshot)
}

func sectionsMap(sections []prompt.Section) map[string]string {
	out := make(map[string]string, len(sections))
	for _, section := range sections {
		if strings.TrimSpace(section.Text) == "" {
			continue
		}
		out[section.Key] = section.Text
	}
	return out
}

// instructionSectionOrder is the shared-to-local order used by BuildParts. The
// map stored in the event loses insertion order, so rendering restores it.
var instructionSectionOrder = []string{
	"codemode", "mcp", "references", "skills", "agents", "instructions", "date", "environment", "extra",
}

func sectionsFromMap(sections map[string]string) []prompt.Section {
	out := make([]prompt.Section, 0, len(sections))
	seen := map[string]bool{}
	for _, key := range instructionSectionOrder {
		if text, ok := sections[key]; ok {
			out = append(out, prompt.Section{Key: key, Text: text})
			seen[key] = true
		}
	}
	extra := make([]string, 0)
	for key := range sections {
		if !seen[key] {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	for _, key := range extra {
		out = append(out, prompt.Section{Key: key, Text: sections[key]})
	}
	return out
}

func mapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func cloneMap(value map[string]string) map[string]string {
	out := make(map[string]string, len(value))
	for key, text := range value {
		out[key] = text
	}
	return out
}

// stringMap reads a map stored in an event, tolerating both map[string]string
// and the map[string]any form produced by JSON decoding.
func stringMap(ev event.Event, key string) map[string]string {
	value, ok := ev.Data[key]
	if !ok {
		return nil
	}
	switch typed := value.(type) {
	case map[string]string:
		return cloneMap(typed)
	case map[string]any:
		out := make(map[string]string, len(typed))
		for name, text := range typed {
			if text, ok := text.(string); ok {
				out[name] = text
			}
		}
		return out
	}
	return nil
}

// renderInstructionUpdate renders the model-facing update and returns the keys
// of the sections that changed, in prompt order.
func renderInstructionUpdate(previous, current map[string]string) (string, []string) {
	keys := make([]string, 0, len(previous)+len(current))
	seen := map[string]bool{}
	for key := range previous {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	for key := range current {
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	var changed, removed, sources []string
	for _, key := range keys {
		before, had := previous[key]
		after, has := current[key]
		switch {
		case !has && had:
			removed = append(removed, instructionLabel(key))
			sources = append(sources, key)
		case has && before != after:
			changed = append(changed, key)
			sources = append(sources, key)
		}
	}
	var b strings.Builder
	b.WriteString("The harness instructions changed. The following sections supersede earlier instructions.")
	for _, key := range changed {
		b.WriteString("\n\n<section name=\"")
		b.WriteString(instructionLabel(key))
		b.WriteString("\">\n")
		b.WriteString(strings.TrimSpace(current[key]))
		b.WriteString("\n</section>")
	}
	if len(removed) > 0 {
		b.WriteString("\n\nThe following instruction sections are no longer available: ")
		b.WriteString(strings.Join(removed, ", "))
		b.WriteString(".")
	}
	return b.String(), orderInstructionKeys(sources)
}

// orderInstructionKeys puts changed sections in the order they appear in the
// system prompt, leaving unknown sections last in alphabetical order.
func orderInstructionKeys(keys []string) []string {
	rank := map[string]int{}
	for index, key := range instructionSectionOrder {
		rank[key] = index
	}
	out := append([]string(nil), keys...)
	sort.SliceStable(out, func(i, j int) bool {
		left, leftKnown := rank[out[i]]
		right, rightKnown := rank[out[j]]
		if leftKnown != rightKnown {
			return leftKnown
		}
		if leftKnown {
			return left < right
		}
		return out[i] < out[j]
	})
	return out
}

func instructionLabel(key string) string {
	switch key {
	case "codemode":
		return "Code Mode"
	case "mcp":
		return "MCP instructions"
	case "references":
		return "Project references"
	case "skills":
		return "Skills"
	case "agents":
		return "AGENTS.md"
	case "instructions":
		return "Configured instructions"
	case "date":
		return "Today's date"
	case "environment":
		return "Environment"
	case "extra":
		return "Additional instructions"
	}
	return key
}

// definitions returns the tool definitions this session's agent may use. A
// markdown or config agent can switch individual tools off with tools.<name>.
func (a *Agent) definitions(sessionID string) []model.ToolDef {
	defs := a.tools.Definitions()
	if a.codemodeOnly {
		filtered := make([]model.ToolDef, 0, 1)
		for _, def := range defs {
			if def.Name == "execute" {
				filtered = append(filtered, def)
				break
			}
		}
		defs = filtered
	}
	profile := a.profile(sessionID)
	rules := append(permission.Rules{}, profile.Permissions...)
	if extra, err := a.store.Permissions(sessionID); err == nil {
		rules = append(rules, extra...)
	}
	out := make([]model.ToolDef, 0, len(defs))
	for _, def := range defs {
		if !profile.AllowsTool(def.Name) {
			continue
		}
		action := def.Name
		if action == "write" || action == "patch" {
			action = "edit"
		}
		// Session rules can narrow an agent further. Only a whole-action deny
		// removes the tool, so a narrowed allow (Plan's own directory) keeps it.
		if rules.WhollyDenies(action) {
			continue
		}
		// A tool may offer fewer options than it supports, such as a subagent
		// list without the agents this profile may not spawn.
		if t, ok := a.tools.Get(def.Name); ok {
			def.Description = tool.Describe(t, rules)
		}
		out = append(out, def)
	}
	return out
}

// withSystem prepends the derived system prompt without touching stored history.
// Plan mode is delivered as a tail system-reminder, matching the reference: it
// survives compaction and keeps the plan rules close to the current turn.
func (a *Agent) withSystem(sessionID string, target provider.Target, messages []model.Message) []model.Message {
	system := a.systemText(sessionID, target)
	profile := a.profile(sessionID)
	plan := profile.ID == "plan"
	enter, leave := agents.PlanEnterReminder(), agents.PlanLeaveReminder()
	last := lastPlanReminder(messages)
	reminder := ""
	switch {
	case plan && last != enter:
		reminder = enter
	case !plan && last == enter:
		reminder = leave
	}
	if profile.System != "" && !plan {
		if !a.systemFuncSet {
			// A fixed prompt cannot include per-agent text, so append it here.
			if system == "" {
				system = profile.System
			} else {
				system = strings.TrimRight(system, "\n") + "\n\n# Agent\n\n" + profile.System
			}
		}
	}
	if system == "" && reminder == "" {
		return messages
	}
	if reminder != "" {
		// Persist the transition once; later steps read it back from history.
		_ = a.emit(sessionID, event.SyntheticEvent(reminder))
	}
	out := make([]model.Message, 0, len(messages)+2)
	if system != "" {
		out = append(out, model.Message{Role: model.RoleSystem, Content: system})
	}
	out = append(out, messages...)
	if reminder != "" {
		out = insertBeforeLastUser(out, reminder)
	}
	return out
}

// lastPlanReminder returns the newest Plan reminder in the transcript. Reminders
// are exact strings, so a compaction checkpoint naturally clears the state.
func lastPlanReminder(messages []model.Message) string {
	enter, leave := agents.PlanEnterReminder(), agents.PlanLeaveReminder()
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != model.RoleUser {
			continue
		}
		switch messages[index].Content {
		case enter:
			return enter
		case leave:
			return leave
		}
	}
	return ""
}

// insertBeforeLastUser lands a reminder immediately before the prompt, keeping
// the stable prefix warm like the reference harness's context hook.
func insertBeforeLastUser(messages []model.Message, text string) []model.Message {
	at := len(messages)
	if at > 0 && messages[at-1].Role == model.RoleUser {
		at--
	}
	out := make([]model.Message, 0, len(messages)+1)
	out = append(out, messages[:at]...)
	out = append(out, model.Message{Role: model.RoleUser, Content: text})
	out = append(out, messages[at:]...)
	return out
}

// toolResult is one finished tool call waiting to be recorded. Results are
// recorded in completion order, so a slow tool never blocks the others.
type toolResult struct {
	call         model.ToolCall
	output       string
	diff         string
	failed       bool
	cancelled    bool
	attachments  []model.Attachment
	metadata     map[string]any
	instructions []tool.Instruction
}

// toolRun is one tool call started while the model response is still streamed.
type toolRun struct {
	index     int
	call      model.ToolCall
	result    toolResult
	done      chan struct{}
	completed time.Time
	liveSent  bool
	emitted   bool
}

// toolBatch starts tools as their calls complete and publishes each tool.call
// immediately, matching the reference event order. Results are held in memory
// until the assistant message is recorded, so replay always sees a complete
// assistant tool-call message before its tool results. Live result events are
// published as soon as a tool finishes.
type toolBatch struct {
	agent     *Agent
	ctx       context.Context
	cancel    context.CancelFunc
	sessionID string
	target    provider.Target
	profile   agents.Profile
	// unattended marks a turn nobody is watching, so an ask becomes a deny
	// instead of blocking until the turn is cancelled.
	unattended bool
	// rules are the calling agent's permission rules plus the session overlay,
	// used to filter what tools offer the model without prompting.
	rules permission.Rules

	mu      sync.Mutex
	runs    []*toolRun
	started map[int]bool
	aborted bool

	declined atomic.Bool
}

func newToolBatch(a *Agent, ctx context.Context, sessionID string, target provider.Target, unattended bool) *toolBatch {
	batchCtx, cancel := context.WithCancel(ctx)
	profile := a.profile(sessionID)
	rules := append(permission.Rules{}, profile.Permissions...)
	if extra, err := a.store.Permissions(sessionID); err == nil {
		rules = append(rules, extra...)
	}
	return &toolBatch{
		agent: a, ctx: batchCtx, cancel: cancel, sessionID: sessionID, target: target,
		profile: profile, started: map[int]bool{}, unattended: unattended, rules: rules,
	}
}

// start prepares one completed tool call and runs it immediately. It is called
// from the provider stream as call arguments finish. The tool.call event is
// written before execution so the UI can show the row while the stream is
// still arriving.
func (b *toolBatch) start(index int, call model.ToolCall) {
	b.mu.Lock()
	if b.started[index] || b.aborted {
		b.mu.Unlock()
		return
	}
	b.started[index] = true
	if call.ID == "" {
		call.ID = "call_" + call.Name
	}
	b.mu.Unlock()

	reason := ""
	if !b.profile.AllowsTool(call.Name) {
		reason = "tool " + call.Name + " is disabled for this agent"
	}
	if b.agent.hooks != nil {
		arguments, hookReason, err := b.agent.hooks.BeforeTool(b.ctx, b.sessionID, call.Name, call.ID, json.RawMessage(call.Arguments))
		if err != nil {
			logx.Warn("before tool hook failed", logx.F("tool", call.Name), logx.Err(err))
		}
		if len(arguments) > 0 && string(arguments) != "null" {
			call.Arguments = string(arguments)
		}
		if hookReason != "" {
			reason = hookReason
		}
	}
	if err := b.agent.emit(b.sessionID, event.ToolCallEvent(call.ID, call.Name, call.Arguments)); err != nil {
		b.agent.fail(b.sessionID, err)
		return
	}
	run := &toolRun{index: index, call: call, done: make(chan struct{})}
	b.mu.Lock()
	b.runs = append(b.runs, run)
	b.mu.Unlock()
	go b.execute(run, reason)
}

func (b *toolBatch) execute(run *toolRun, reason string) {
	defer close(run.done)
	if reason != "" {
		output := "blocked by plugin: " + reason
		logx.Info("tool blocked", logx.Session(b.sessionID), logx.F("tool", run.call.Name), logx.F("reason", reason))
		if b.agent.hooks != nil {
			b.agent.hooks.AfterTool(b.ctx, b.sessionID, run.call.Name, run.call.ID, output, true)
		}
		b.finishRun(run, toolResult{call: run.call, output: output, failed: true})
		return
	}
	started := time.Now()
	var diff string
	var attachments []model.Attachment
	var instructions []tool.Instruction
	metadata := map[string]any{}
	output, err := b.agent.tools.Run(b.ctx, tool.Call{
		SessionID:  b.sessionID,
		ID:         run.call.ID,
		Name:       run.call.Name,
		Input:      []byte(run.call.Arguments),
		Perm:       b.agent.perm,
		CanAttach:  b.target.Attachment,
		Metadata:   metadata,
		Unattended: b.unattended,
		OnProgress: func(update map[string]any) {
			b.publish(event.ToolProgressEvent(run.call.ID, update))
		},
		ToolFilter: func(name string) bool {
			return b.profile.AllowsTool(name)
		},
		Rules: b.rules,
		OnAsk: func(req permission.Request) {
			b.publish(event.PermissionAskedEvent(req.ID, req.SessionID, req.Action, req.Resources, req.Message))
		},
		OnDiff: func(value string) {
			diff = value
		},
		OnAttachment: func(attachment model.Attachment) {
			attachments = append(attachments, attachment)
		},
		OnInstruction: func(instruction tool.Instruction) {
			instructions = append(instructions, instruction)
		},
		Emit: b.emit,
		OnFormat: func(ctx context.Context, path string) (string, error) {
			if b.agent.formatter == nil {
				return "", nil
			}
			return b.agent.formatter.FormatFile(ctx, path)
		},
	})
	cancelled := b.ctx.Err() != nil
	if err != nil {
		var declined *permission.DeclinedError
		var corrected *permission.CorrectedError
		// A denied call is a rejection, not a cancellation, even though the
		// bare rejection also cancels the rest of the batch.
		if errors.As(err, &declined) {
			cancelled = false
		}
		switch {
		case errors.As(err, &declined):
			b.declined.Store(true)
			// A bare rejection ends the turn, as the reference does. Stop
			// parallel tools instead of letting them keep modifying the
			// workspace.
			b.cancel()
			output = strings.TrimSpace(output + "\n" + err.Error())
		case errors.As(err, &corrected):
			// A rejection with feedback fails only this call. The model reads
			// the feedback as the tool result and continues, so one message
			// redirects the turn instead of ending it.
			output = strings.TrimSpace(output + "\nThe user declined this call and said: " + corrected.Feedback)
		default:
			output = strings.TrimSpace(output + "\n" + err.Error())
		}
	}
	failed := err != nil || cancelled
	// Every tool result is bounded here, so a result from an MCP server, a
	// plugin or a subagent is capped like a built-in tool's own output. The
	// marker names the saved file, which keeps the full text recoverable.
	output = tool.Limit(output)
	if b.agent.hooks != nil {
		b.agent.hooks.AfterTool(b.ctx, b.sessionID, run.call.Name, run.call.ID, output, failed)
	}
	// One record per tool call is the unit a production log is read by: which
	// tool ran, whether it failed, and how long it took.
	logx.Info("tool finished",
		logx.Session(b.sessionID),
		logx.F("tool", run.call.Name),
		logx.F("call", run.call.ID),
		logx.F("failed", failed),
		logx.F("bytes", len(output)),
		logx.Timing(started),
		logx.Err(err),
	)
	b.finishRun(run, toolResult{call: run.call, output: output, diff: diff, failed: failed, cancelled: cancelled, attachments: attachments, metadata: metadata, instructions: instructions})
}

// finishRun stores a result and publishes it to live subscribers immediately.
// The durable event is emitted later, after the assistant message exists.
func (b *toolBatch) finishRun(run *toolRun, result toolResult) {
	b.mu.Lock()
	run.result = result
	run.completed = time.Now()
	b.mu.Unlock()
	b.publishResult(run)
}

// resultEvent renders one tool result. A cancellation is still failed for the
// model, but carries the flag clients use to avoid an error card.
func resultEvent(result toolResult) event.Event {
	if result.cancelled {
		return event.ToolResultCancelled(result.call.ID, result.call.Name, result.output, result.attachments, result.metadata)
	}
	return event.ToolResultWithAttachments(result.call.ID, result.call.Name, result.output, result.diff, result.failed, result.attachments, result.metadata)
}

// publishResult sends one result to live subscribers once.
func (b *toolBatch) publishResult(run *toolRun) {
	b.mu.Lock()
	if run.liveSent {
		b.mu.Unlock()
		return
	}
	run.liveSent = true
	result := run.result
	b.mu.Unlock()
	b.agent.hub.Publish(b.sessionID, resultEvent(result))
}

// emitResult persists one result once, in completion order.
func (b *toolBatch) emitResult(run *toolRun) bool {
	b.mu.Lock()
	if run.emitted {
		b.mu.Unlock()
		return true
	}
	run.emitted = true
	result := run.result
	b.mu.Unlock()
	if err := b.agent.emit(b.sessionID, resultEvent(result)); err != nil {
		b.agent.fail(b.sessionID, err)
		return false
	}
	return true
}

// emit publishes a tool-owned event. After abort, late tool output is dropped.
func (b *toolBatch) emit(ev event.Event) error {
	b.mu.Lock()
	aborted := b.aborted
	b.mu.Unlock()
	if aborted {
		return nil
	}
	return b.agent.emit(b.sessionID, ev)
}

func (b *toolBatch) publish(ev event.Event) {
	b.mu.Lock()
	aborted := b.aborted
	b.mu.Unlock()
	if aborted {
		return
	}
	b.agent.hub.Publish(b.sessionID, ev)
}

// abort stops running tools and settles every started call with a failed
// result, so a turn that ends mid-stream never leaves a dangling tool row.
func (b *toolBatch) abort() {
	b.mu.Lock()
	if b.aborted {
		b.mu.Unlock()
		return
	}
	b.aborted = true
	runs := append([]*toolRun(nil), b.runs...)
	for _, run := range runs {
		if run.emitted {
			continue
		}
		if strings.TrimSpace(run.result.output) == "" {
			run.result = toolResult{call: run.call, output: "aborted", failed: true}
		}
		run.result.failed = true
		run.result.cancelled = true
		run.completed = time.Now()
	}
	b.mu.Unlock()
	b.cancel()
	for _, run := range runs {
		b.publishResult(run)
		if !b.emitResult(run) {
			return
		}
	}
}

func (b *toolBatch) announceAndFinish() bool {
	defer b.cancel()
	return b.finish()
}

// abortGrace bounds how long a cancelled turn waits for a tool that ignores
// cancellation before the call is settled as aborted.
const abortGrace = 5 * time.Second

// finish waits for every tool, then persists results in completion order.
//
// A tool runs for as long as it needs while the turn is live: a question waits
// for the user's answer and a shell command carries its own timeout. Only a
// cancelled turn bounds the wait, so a tool that ignores cancellation cannot
// hold the session open forever; that call is settled as aborted.
func (b *toolBatch) finish() bool {
	b.mu.Lock()
	runs := append([]*toolRun(nil), b.runs...)
	b.mu.Unlock()
	for _, run := range runs {
		select {
		case <-run.done:
			continue
		case <-b.ctx.Done():
		}
		timer := time.NewTimer(abortGrace)
		select {
		case <-run.done:
			if !timer.Stop() {
				<-timer.C
			}
		case <-timer.C:
		}
	}
	sort.SliceStable(runs, func(i, j int) bool {
		if runs[i].completed.IsZero() || runs[j].completed.IsZero() {
			return runs[i].index < runs[j].index
		}
		if runs[i].completed.Equal(runs[j].completed) {
			return runs[i].index < runs[j].index
		}
		return runs[i].completed.Before(runs[j].completed)
	})
	var instructions []tool.Instruction
	for _, run := range runs {
		b.mu.Lock()
		if run.completed.IsZero() {
			run.result = toolResult{call: run.call, output: "aborted", failed: true, cancelled: true}
			run.completed = time.Now()
		}
		result := run.result
		b.mu.Unlock()
		if !b.emitResult(run) {
			return false
		}
		instructions = append(instructions, result.instructions...)
	}
	for _, instruction := range instructions {
		if err := b.agent.emit(b.sessionID, event.SyntheticInstruction(instruction.Text, instruction.Description, instruction.Paths)); err != nil {
			b.agent.fail(b.sessionID, err)
			return false
		}
	}
	return true
}

// emit persists durable events and publishes every event to live subscribers.
func (a *Agent) emit(sessionID string, ev event.Event) error {
	_, err := a.emitSeq(sessionID, ev)
	return err
}

// emitSeq is emit plus the durable sequence number, used to key snapshots to
// their user turn.
func (a *Agent) emitSeq(sessionID string, ev event.Event) (int64, error) {
	if ev.Durable() {
		stored, err := a.store.AppendEvent(sessionID, ev)
		if err != nil {
			return 0, err
		}
		a.hub.Publish(sessionID, stored)
		return stored.Seq, nil
	}
	a.hub.Publish(sessionID, ev)
	return 0, nil
}

// fail records why a turn stopped and closes it. The reason is durable, so a
// client that reconnects or restarts still shows the failure instead of a
// transcript that ends mid-reply; the live error and run.end events follow so
// connected subscribers react immediately.
func (a *Agent) fail(sessionID string, err error) {
	message := err.Error()
	if appendErr := a.emit(sessionID, event.RunFailedEvent(message)); appendErr != nil {
		logx.Error("record failure event failed", logx.Session(sessionID), logx.Err(appendErr))
	}
	a.hub.Publish(sessionID, event.ErrorEvent(message))
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
