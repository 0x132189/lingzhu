package agent

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"lingzhu/internal/agents"
	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
)

// maxToolOutput caps one tool result inside a summary transcript.
const maxToolOutput = 2000

// summaryTemplate is the section layout shared by both summary prompts. It is
// copied from the reference harness so compaction output stays compatible.
const summaryTemplate = `You MUST use this format for your response (you may omit sections that aren't applicable). Do not include the <template> tags in your response.
<template>
## Objective
- [one or two brief sentences describing what the user is trying to accomplish]

## Requirements
- [constraints, preferences, requirements, and scope boundaries stated by the user, or "(none)"]

## Decisions
- [decisions already made and why, or "(none)"]

## Work State
Break the objective into smaller goals and report which are completed, which are being worked on, and which are blocked.
### Completed
- [goals that have been completed; otherwise "(none)"]

### Active
- [goals currently being worked on; otherwise "(none)"]

### Blocked
- [anything blocking progress, and why; otherwise "(none)"]

## Next Move
1. [ordered list of next actions, or "(none)"]

## Relevant Files
List the files and directories, other than the current working directory, that another agent would need to open to continue this work. Include at most 15, most important first. Do not list every file that was read or changed. Include paths outside the current working directory when relevant. If none, write "(none)".
- ` + "`[file or directory path]`" + `: [brief reason it matters]

## Important Context
- [facts the next agent cannot continue without and cannot easily find on its own; or "(none)"]
</template>`

// summaryRules applies to both the initial and the update prompt.
const summaryRules = `Rules:
- Keep each section concise. Use terse, single-line bullets, not prose paragraphs or nested lists.
- Prefer short references over detailed restatement. It is fine to leave out information the next agent can recover from the code or the files listed above.
- Preserve exact file paths, symbols, commands, error strings, URLs, and identifiers.
- Carry forward only user questions or requests that remain unanswered or require further action. Do not repeat ones that newer history has answered or resolved. Preserve exact wording when carrying one forward.
- Preserve consequential workflow state, including whether changes are uncommitted, committed, pushed, under review, or merged.
- Do not mention the summary process or that context was compacted.`

// compactionPrompt writes a fresh summary from the conversation.
var compactionPrompt = strings.Join([]string{
	"You MUST summarize the conversation above into a structured summary that will be given to another agent to resume the work.",
	"Summarize only what the user and the assistant said and did. Leave out instructions and setup the assistant was given rather than told by the user: repository conventions, instruction files such as AGENTS.md, and environment details like the session ID. The next agent receives current versions of all of these separately.",
	summaryTemplate,
	summaryRules,
	"Do not continue the task or call tools.",
	"Return only the structured summary in the requested format. Do not include a preamble, explanation, or other commentary.",
}, "\n\n")

// compactionUpdatePrompt consolidates an existing checkpoint with newer history.
var compactionUpdatePrompt = strings.Join([]string{
	"Update the existing checkpoint in the conversation above into one consolidated summary.",
	"Newer history always takes precedence over the existing checkpoint. Preserve previous information unless newer history clearly contradicts, supersedes, resolves, or makes it stale. If something is no longer relevant to continuing the work, you may remove it.",
	"Incorporate newer requirements, decisions, progress, and context. Reconcile Work State and Next Move: move completed work out of Active, remove resolved blockers and answered questions, and preserve unresolved or pending work.",
	"Return only the updated Markdown sections. Do not reproduce the <conversation-checkpoint>, <summary>, or <recent-context> wrapper tags from the previous checkpoint.",
	"Summarize only what the user and the assistant said and did. Leave out instructions and setup the assistant was given rather than told by the user: repository conventions, instruction files such as AGENTS.md, and environment details like the session ID. The next agent receives current versions of all of these separately.",
	summaryTemplate,
	summaryRules,
	"Do not continue the task or call tools.",
	"Return only the structured summary in the requested format. Do not include a preamble, explanation, or other commentary.",
}, "\n\n")

// summaryNudge retries a response that ignored the required template.
const summaryNudge = "The previous response did not fill in the required summary template. Do not call tools. Return the summary as text using the exact section headings from the template."

// hasCheckpoint reports whether the history already opens with a compaction summary.
func hasCheckpoint(messages []model.Message) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, "<conversation-checkpoint>") {
			return true
		}
	}
	return false
}

// precomputeLead starts a background summary before the context reaches the
// ceiling. Writing one takes tens of seconds, so starting at the trigger would
// stall the next step.
const precomputeLead = 60_000

const (
	// precomputeCacheLifetime is used when the model does not report a cache
	// lifetime; the background request should still hit the provider cache.
	precomputeCacheLifetime = 5 * time.Minute
	// precomputeCacheMargin stops starting a summary that would likely miss the
	// cache before it finishes.
	precomputeCacheMargin = 30 * time.Second
	// precomputeMaxFailures stops retrying background summaries until the next
	// synchronous compaction.
	precomputeMaxFailures = 2
	// precomputedTailShare is the largest verbatim tail a precomputed summary
	// may keep, as a share of the context ceiling.
	precomputedTailShare = 0.4
)

// precompaction is a summary written ahead of automatic compaction.
type precompaction struct {
	split       int
	fingerprint string
	summary     string
	usage       *provider.Usage
}

// precompute starts a background summary when the context is close to the
// ceiling. A second request for the same session is ignored while one is in
// flight or a summary is already waiting.
func (a *Agent) precompute(sessionID string, target provider.Target, current session.Context) {
	if !a.precomputeAllowed(sessionID, target, time.Now()) {
		return
	}
	target = a.compactionTarget(target)
	ceiling := session.Ceiling(target.Window)
	if ceiling <= 0 || current.Tokens < ceiling-precomputeLead {
		return
	}
	split := keepTail(current.Messages, a.keep)
	if split <= 0 {
		return
	}
	a.preMu.Lock()
	if a.precomps[sessionID] != nil || a.preRuns[sessionID] {
		a.preMu.Unlock()
		return
	}
	a.preRuns[sessionID] = true
	a.preMu.Unlock()

	history := append([]model.Message(nil), current.Messages[:split]...)
	fingerprint := messageFingerprint(history)
	go func() {
		defer func() {
			a.preMu.Lock()
			delete(a.preRuns, sessionID)
			a.preMu.Unlock()
		}()
		runCtx, cancel := context.WithTimeout(a.base, 2*time.Minute)
		defer cancel()
		summary, usage, err := a.summarize(runCtx, sessionID, target, history)
		if err != nil || strings.TrimSpace(summary) == "" {
			if err == nil || !errors.Is(err, context.Canceled) {
				a.preMu.Lock()
				a.preFails[sessionID]++
				a.preMu.Unlock()
			}
			return
		}
		a.preMu.Lock()
		a.preFails[sessionID] = 0
		a.precomps[sessionID] = &precompaction{split: split, fingerprint: fingerprint, summary: summary, usage: usage}
		a.preMu.Unlock()
	}()
}

// precomputeAllowed gates background summaries on the provider cache lifetime
// and repeated failures, matching the reference harness.
func (a *Agent) precomputeAllowed(sessionID string, target provider.Target, now time.Time) bool {
	if a.keep <= 0 || target.Window <= 0 {
		return false
	}
	a.preMu.Lock()
	failures := a.preFails[sessionID]
	last := a.lastResponse[sessionID]
	a.preMu.Unlock()
	if failures >= precomputeMaxFailures {
		return false
	}
	lifetime := target.CacheTTL
	if lifetime <= 0 {
		lifetime = precomputeCacheLifetime
	}
	if lifetime <= precomputeCacheMargin {
		return false
	}
	return !last.IsZero() && now.Sub(last) < lifetime-precomputeCacheMargin
}

// markResponse records a successful main model call so precompute can decide
// whether the prompt cache is still warm.
func (a *Agent) markResponse(sessionID string) {
	a.preMu.Lock()
	a.lastResponse[sessionID] = time.Now()
	a.preMu.Unlock()
}

// usePrecomputed applies a waiting summary when the current history still has
// the same prefix. It returns false when the caller must compact synchronously.
func (a *Agent) usePrecomputed(sessionID string, target provider.Target, current session.Context) bool {
	a.preMu.Lock()
	cached := a.precomps[sessionID]
	if cached != nil {
		delete(a.precomps, sessionID)
		delete(a.preFails, sessionID)
	}
	a.preMu.Unlock()
	if cached == nil {
		return false
	}
	split := keepTail(current.Messages, a.keep)
	if cached.split <= 0 || cached.split > split || cached.split > len(current.Messages) {
		return false
	}
	if messageFingerprint(current.Messages[:cached.split]) != cached.fingerprint {
		return false
	}
	if ceiling := session.Ceiling(target.Window); ceiling > 0 {
		tail := 0
		for _, message := range current.Messages[cached.split:] {
			tail += session.Estimate(message.Content)
		}
		if tail > int(precomputedTailShare*float64(ceiling)) {
			return false
		}
	}
	if cached.usage != nil {
		if err := a.emit(sessionID, event.UsageEvent(cached.usage.Input, cached.usage.Output, cached.usage.CacheRead, cached.usage.CacheWrite)); err != nil {
			return false
		}
	}
	recent := transcript(current.Messages[cached.split:], 0)
	if err := a.emit(sessionID, event.CompactionEventWithReason(session.Checkpoint(cached.summary, recent), cached.split, "auto")); err != nil {
		return false
	}
	a.advanceInstructionEpoch(sessionID)
	return true
}

// clearPrecomputed drops a waiting summary, for example before a manual
// compaction changes the history it was written from.
func (a *Agent) clearPrecomputed(sessionID string) {
	a.preMu.Lock()
	delete(a.precomps, sessionID)
	delete(a.preFails, sessionID)
	a.preMu.Unlock()
}

// messageFingerprint identifies the messages a summary covers. Attachments
// contribute their type and name, not their payload.
func messageFingerprint(messages []model.Message) string {
	h := sha256.New()
	for _, message := range messages {
		fmt.Fprintf(h, "%s\x00%s\x00", message.Role, message.Content)
		for _, call := range message.ToolCalls {
			fmt.Fprintf(h, "%s\x00%s\x00%s\x00", call.ID, call.Name, call.Arguments)
		}
		for _, attachment := range message.Attachments {
			fmt.Fprintf(h, "%s\x00%s\x00", attachment.Mime, attachment.Name)
		}
	}
	return fmt.Sprintf("%x", h.Sum(nil))
}

// compact summarizes everything before the kept tail and records a checkpoint.
// keep is the token budget of the tail left verbatim; zero keeps only the last
// exchange.
func (a *Agent) compact(ctx context.Context, sessionID string, target provider.Target, current session.Context, keep int, reason string) error {
	a.clearPrecomputed(sessionID)
	target = a.compactionTarget(target)
	if target.NativeCompaction {
		return a.compactNative(ctx, sessionID, target, current, keep, reason)
	}
	split := keepTail(current.Messages, keep)
	if split <= 0 {
		return errors.New("compact: nothing to compact yet")
	}
	if err := a.emit(sessionID, event.CompactionStartEvent()); err != nil {
		return err
	}

	summary, usage, err := a.summarize(ctx, sessionID, target, current.Messages[:split])
	if err != nil {
		_ = a.emit(sessionID, event.CompactionFailedEvent(err.Error()))
		return fmt.Errorf("compact: %w", err)
	}
	if strings.TrimSpace(summary) == "" {
		_ = a.emit(sessionID, event.CompactionFailedEvent("model returned an empty summary"))
		return errors.New("compact: model returned an empty summary")
	}
	if !hasRequiredSections(summary) {
		retry, retryUsage, retryErr := a.summarizeFollowUp(ctx, sessionID, target, current.Messages[:split], summary, summaryNudge)
		if retryErr == nil && hasRequiredSections(retry) {
			summary = retry
			if retryUsage != nil {
				usage = retryUsage
			}
		}
	}
	if usage != nil {
		if err := a.emit(sessionID, event.UsageEvent(usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite)); err != nil {
			return err
		}
	}

	recent := transcript(current.Messages[split:], 0)
	compaction := event.CompactionEventWithReason(session.Checkpoint(summary, recent), split, reason)
	if usage != nil {
		compaction.Data["usage"] = map[string]any{
			"input":      usage.Input,
			"output":     usage.Output,
			"cacheRead":  usage.CacheRead,
			"cacheWrite": usage.CacheWrite,
		}
	}
	if err := a.emit(sessionID, compaction); err != nil {
		return err
	}
	a.advanceInstructionEpoch(sessionID)
	return nil
}

// compactNative asks the provider to replace its own context window. The
// checkpoint text is provider-produced, so the transcript keeps it verbatim and
// later requests replay the provider's window instead of a local summary.
func (a *Agent) compactNative(ctx context.Context, sessionID string, target provider.Target, current session.Context, keep int, reason string) error {
	if err := a.emit(sessionID, event.CompactionStartEvent()); err != nil {
		return err
	}
	wire := target.RequestModel
	if wire == "" {
		wire = target.Model
	}
	trigger := target.CompactionTrigger
	if trigger <= 0 {
		trigger = session.Ceiling(target.Window)
	}
	options := target.Options
	if options == nil {
		options = map[string]any{}
	} else {
		cloned := make(map[string]any, len(options)+1)
		for key, value := range options {
			cloned[key] = value
		}
		options = cloned
	}
	options["context_management"] = map[string]any{
		"edits": []map[string]any{{
			"type":    "compact_20260112",
			"trigger": map[string]any{"type": "input_tokens", "value": trigger},
		}},
	}
	request := model.Request{
		Model:             wire,
		Messages:          a.withSystem(sessionID, target, current.Messages),
		Headers:           target.Headers,
		Options:           options,
		CacheTTL:          target.CacheTTL,
		CacheKey:          sessionID,
		NativeCompaction:  true,
		CompactionTrigger: trigger,
	}
	stream, err := target.Provider.Stream(ctx, request)
	if err != nil {
		return err
	}
	defer stream.Close()

	var checkpoint strings.Builder
	var usage *provider.Usage
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if chunk.Compaction != "" {
			checkpoint.WriteString(chunk.Compaction)
		}
	}
	if usage != nil {
		if err := a.emit(sessionID, event.UsageEvent(usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite)); err != nil {
			return err
		}
	}
	text := strings.TrimSpace(checkpoint.String())
	if text == "" {
		return errors.New("compact: provider returned no native checkpoint")
	}
	if err := a.emit(sessionID, event.CompactionEventWithReason(session.Checkpoint(text, ""), 0, reason)); err != nil {
		return err
	}
	a.advanceInstructionEpoch(sessionID)
	return nil
}

// compactionTarget applies the hidden compaction agent's model override. When
// the agent sets no model — or it cannot be resolved — the session model keeps
// writing summaries, matching the reference fallback.
func (a *Agent) compactionTarget(target provider.Target) provider.Target {
	if a.profiles == nil {
		return target
	}
	profile, ok := a.profiles.Get("compaction")
	if !ok || strings.TrimSpace(profile.Model) == "" {
		return target
	}
	providerID, modelID, err := agents.ValidateModel(profile.Model)
	if err != nil {
		return target
	}
	resolved, err := a.catalog.Resolve(provider.Selection{Provider: providerID, Model: modelID})
	if err != nil {
		return target
	}
	if len(profile.Options) > 0 {
		resolved.Options = mergeOptions(resolved.Options, profile.Options)
	}
	if len(profile.Headers) > 0 {
		resolved.Headers = mergeHeaders(resolved.Headers, profile.Headers)
	}
	return resolved
}

// summarize runs one model call that writes the structured summary. An existing
// checkpoint switches the request to the update prompt so earlier decisions are
// consolidated instead of restated.
func (a *Agent) summarize(ctx context.Context, sessionID string, target provider.Target, history []model.Message) (string, *provider.Usage, error) {
	prompt := compactionPrompt
	if hasCheckpoint(history) {
		prompt = compactionUpdatePrompt
	}
	return a.summarizeWith(ctx, sessionID, target, history, prompt)
}

// summarizeWith sends one summary request built from history and prompt.
func (a *Agent) summarizeWith(ctx context.Context, sessionID string, target provider.Target, history []model.Message, prompt string) (string, *provider.Usage, error) {
	budget := session.Ceiling(target.Window)
	overhead := session.Estimate(a.system(sessionID, target)) + session.Estimate(prompt)
	text := transcript(history, 0)
	if budget > 0 && session.Estimate(text)+overhead > budget {
		text = transcript(history, max(budget-overhead-1000, 1000))
	}
	if strings.TrimSpace(text) == "" {
		return "", nil, errors.New("history is empty")
	}

	messages := []model.Message{{Role: model.RoleUser, Content: text + "\n\n" + prompt}}
	return a.complete(ctx, sessionID, target, messages)
}

// summarizeFollowUp re-asks after a response that skipped the required sections.
func (a *Agent) summarizeFollowUp(ctx context.Context, sessionID string, target provider.Target, history []model.Message, previous, nudge string) (string, *provider.Usage, error) {
	text := transcript(history, 0)
	messages := []model.Message{
		{Role: model.RoleUser, Content: text + "\n\n" + compactionPrompt},
		{Role: model.RoleAssistant, Content: previous},
		{Role: model.RoleUser, Content: nudge},
	}
	return a.complete(ctx, sessionID, target, messages)
}

// complete streams one non-tool completion and returns its text.
func (a *Agent) complete(ctx context.Context, sessionID string, target provider.Target, messages []model.Message) (string, *provider.Usage, error) {
	wire := target.RequestModel
	if wire == "" {
		wire = target.Model
	}
	stream, err := target.Provider.Stream(ctx, model.Request{Model: wire, Messages: a.withSystem(sessionID, target, messages), Headers: target.Headers, Options: target.Options})
	if err != nil {
		return "", nil, err
	}
	defer stream.Close()

	var summary strings.Builder
	var usage *provider.Usage
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", usage, err
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		summary.WriteString(chunk.Text)
	}
	return strings.TrimSpace(summary.String()), usage, nil
}

// requiredSections are the headings every summary must carry.
var requiredSections = []string{"## Objective", "## Work State", "## Next Move"}

// hasRequiredSections reports whether a summary filled in the template.
func hasRequiredSections(summary string) bool {
	for _, section := range requiredSections {
		if !strings.Contains(summary, section) {
			return false
		}
	}
	return true
}

// keepTail returns the index where the verbatim tail starts: the earliest user
// message whose tail still fits keep tokens, else the earliest message that
// fits, else only the newest message stays out of the summary.
func keepTail(messages []model.Message, keep int) int {
	user, any, total := -1, -1, 0
	for i := len(messages) - 1; i >= 0; i-- {
		total += session.Estimate(messages[i].Content)
		if total > keep {
			continue
		}
		any = i
		if messages[i].Role == model.RoleUser {
			user = i
		}
	}
	if user > 0 {
		return user
	}
	if any > 0 {
		return any
	}
	if len(messages) > 1 {
		return len(messages) - 1
	}
	return 0
}

// transcript renders messages as plain text. It shrinks content in the same
// levels as the reference harness and only drops the oldest exchanges as a last
// resort, so compaction loses as little context as possible. Budget zero keeps
// the whole transcript.
func transcript(messages []model.Message, budget int) string {
	groups := exchanges(messages)
	if budget <= 0 {
		return renderTranscript(groups, 0)
	}
	for level := 0; level <= 3; level++ {
		text := renderTranscript(groups, level)
		if session.Estimate(text) <= budget {
			return text
		}
	}
	for drop := 1; drop < len(groups); drop++ {
		text := renderTranscript(groups[drop:], 3)
		if session.Estimate(text) <= budget {
			return fmt.Sprintf("[%d older exchanges omitted]\n\n%s", drop, text)
		}
	}
	return renderTranscript(groups[len(groups)-1:], 3)
}

func renderTranscript(groups [][]model.Message, level int) string {
	blocks := make([]string, 0, len(groups))
	for _, group := range groups {
		blocks = append(blocks, renderGroupLevel(group, level))
	}
	return strings.Join(blocks, "\n\n")
}

// exchanges groups messages into turns, each starting at a user message.
func exchanges(messages []model.Message) [][]model.Message {
	var groups [][]model.Message
	for _, message := range messages {
		if message.Role == model.RoleUser || len(groups) == 0 {
			groups = append(groups, nil)
		}
		last := len(groups) - 1
		groups[last] = append(groups[last], message)
	}
	return groups
}

// renderGroupLevel renders one exchange at a shrink level: 0 keeps everything,
// 1 clips tool outputs, 2 drops tool outputs and clips tool inputs, 3 keeps only
// clipped user text.
func renderGroupLevel(messages []model.Message, level int) string {
	lines := make([]string, 0, len(messages))
	for _, message := range messages {
		switch message.Role {
		case model.RoleUser:
			text := message.Content
			if level >= 3 {
				text = clip(text)
			}
			lines = append(lines, "[User]: "+text)
		case model.RoleAssistant:
			if level >= 3 {
				continue
			}
			if message.Content != "" {
				lines = append(lines, "[Assistant]: "+message.Content)
			}
			for _, call := range message.ToolCalls {
				args := call.Arguments
				if level >= 2 {
					args = clip(args)
				}
				lines = append(lines, fmt.Sprintf("[Assistant tool call]: %s(%s)", call.Name, args))
			}
		case model.RoleTool:
			if level >= 2 {
				continue
			}
			text := message.Content
			if level >= 1 {
				text = clip(text)
			}
			lines = append(lines, fmt.Sprintf("[Tool %s result]: %s", message.Name, text))
		}
	}
	return strings.Join(lines, "\n")
}

// clip keeps the start and end of long text, dropping the middle. It counts
// runes so multi-byte characters are never split.
func clip(text string) string {
	runes := []rune(text)
	if len(runes) <= maxToolOutput {
		return text
	}
	half := maxToolOutput / 2
	return string(runes[:half]) + fmt.Sprintf("\n[... %d chars omitted ...]\n", len(runes)-maxToolOutput) + string(runes[len(runes)-half:])
}
