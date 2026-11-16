// Package event defines runtime events. Durable events are persisted to the
// session store; delta and control events only reach current subscribers.
package event

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"lingzhu/internal/model"
	"lingzhu/internal/permission"
)

type Type string

const (
	StepStart         Type = "step.start"
	TextDelta         Type = "text.delta"
	UserMessage       Type = "user.message"
	AssistantMessage  Type = "assistant.message"
	ToolCall          Type = "tool.call"
	ToolResult        Type = "tool.result"
	ToolOutput        Type = "tool.output"
	ReasoningDelta    Type = "reasoning.delta"
	PermissionAsked   Type = "permission.asked"
	PermissionReplied Type = "permission.replied"
	Usage             Type = "usage"
	CompactionStart   Type = "compaction.start"
	Compaction        Type = "compaction"
	CompactionFailed  Type = "compaction.failed"
	CompactionQueued  Type = "compaction.queued"
	RunEnd            Type = "run.end"
	RunRetry          Type = "run.retry"
	Error             Type = "error"
	InputQueued       Type = "input.queued"
	InputUpdated      Type = "input.updated"
	InputCancelled    Type = "input.cancelled"
	// SyntheticQueued admits synthetic input for delivery at a boundary. The
	// visible event is published when it is delivered, so it stays outside
	// model history until then.
	SyntheticQueued Type = "synthetic.queued"
	TurnStarted     Type = "turn.started"
	TurnFinished    Type = "turn.finished"
	TurnCancelled   Type = "turn.cancelled"
	// TurnReleased closes a claim without cancelling pending input. A user
	// interruption releases the turn and leaves steering and queued prompts
	// deliverable by the next execution.
	TurnReleased Type = "turn.released"
	// TurnResumed records that a surviving turn claim was picked up again.
	// Repeated resumes without a terminal mark a crash loop.
	TurnResumed Type = "turn.resumed"
	// JobCompleted delivers a background job result to the model.
	JobCompleted Type = "job.completed"
	// SubagentCompleted delivers a finished child session result to its parent.
	SubagentCompleted Type = "subagent.completed"
	// SubagentStarted records a background child so a restarted core can still
	// deliver its completion.
	SubagentStarted Type = "subagent.started"
	// ScheduleFired delivers a due schedule prompt to its session.
	ScheduleFired Type = "schedule.fired"
	// Synthetic injects text from an external producer without a user message.
	Synthetic Type = "synthetic"
	// InstructionState stores the frozen instruction baseline and the latest
	// observed values for one session.
	InstructionState Type = "instruction.state"
	// FormAsked records a structured form waiting for an answer.
	FormAsked Type = "form.asked"
	// FormReplied records a form answer or cancellation, visible to the model.
	FormReplied Type = "form.replied"
	// QuestionAsked records a question waiting for the user.
	QuestionAsked Type = "question.asked"
	// QuestionReplied records the user's answer or dismissal.
	QuestionReplied Type = "question.replied"
	// TurnSnapshot records the shadow commit captured before a user turn.
	TurnSnapshot Type = "turn.snapshot"
	// RunFailed records why a turn ended in an error. It is durable so a client
	// that reconnects, or a reader after a restart, still sees the failure
	// instead of a transcript that stops mid-reply.
	RunFailed Type = "run.failed"
	// SessionReverted stages a revert at one user turn and restores its files.
	SessionReverted Type = "session.reverted"
	// SessionReapplied undoes a staged revert.
	SessionReapplied Type = "session.reapplied"
	// ShellEnded records a command the user ran directly and its output.
	ShellEnded Type = "shell.ended"
	// SessionRenamed announces a title change so clients refresh their lists.
	SessionRenamed Type = "session.renamed"
	// SessionDeleted announces a removed session so every client drops it from
	// its lists and closes its tab, instead of keeping a row that opens empty.
	SessionDeleted Type = "session.deleted"
	// AgentSelected announces an agent switch so clients and plugins can react.
	AgentSelected Type = "session.agent.selected"
	// ModelSelected announces a provider/model switch so the transcript can
	// draw the same separator the agent switch draws.
	ModelSelected Type = "session.model.selected"
	// SessionPermissions replaces the session-level permission overlay.
	SessionPermissions Type = "session.permissions"
	// MoveQueued records a session transfer waiting for the turn boundary.
	MoveQueued Type = "move.queued"
	// MoveFailed closes a queued move that could not reach its target.
	MoveFailed Type = "move.failed"
	// SessionMoved records a session that changed workspace inside one core.
	SessionMoved Type = "session.moved"
)

type Event struct {
	Type Type `json:"type"`
	// Seq is the durable sequence number within one session; zero for events
	// that are not persisted.
	Seq int64 `json:"seq,omitempty"`
	// Global is the durable sequence number across every session. A client
	// following the all-session stream resumes from it after a dropped
	// connection, so no event in the gap is lost.
	Global int64          `json:"global,omitempty"`
	At     time.Time      `json:"at"`
	Data   map[string]any `json:"data,omitempty"`
}

// Durable reports whether the event is written to the session store.
func (e Event) Durable() bool {
	switch e.Type {
	case StepStart, TextDelta, RunEnd, RunRetry, Error, PermissionAsked, PermissionReplied, CompactionStart, SessionRenamed, SessionDeleted:
		return false
	}
	return true
}

func New(t Type, data map[string]any) Event {
	return Event{Type: t, At: time.Now().UTC(), Data: data}
}

func Delta(text string) Event {
	return New(TextDelta, map[string]any{"text": text})
}

func Step(index int) Event {
	return New(StepStart, map[string]any{"step": index})
}

// RetryScheduled announces a retryable model failure and the next attempt.
func RetryScheduled(attempt int, next time.Time, message string) Event {
	return New(RunRetry, map[string]any{
		"attempt": attempt,
		"next":    next.UTC().Format(time.RFC3339Nano),
		"message": message,
	})
}

func User(text string) Event {
	return New(UserMessage, map[string]any{"text": text})
}

// UserWithAttachments is a user turn carrying inline images, PDFs, or text. The
// attachments are persisted with the event so replay rebuilds the same prompt.
func UserWithAttachments(text string, attachments []model.Attachment) Event {
	ev := User(text)
	if len(attachments) > 0 {
		ev.Data["attachments"] = attachments
	}
	return ev
}

// ShellEndedEvent records a shell command the user ran directly. The command
// and its merged output join the conversation on the next model request.
func ShellEndedEvent(command, output string, exit int, timedOut, truncated bool) Event {
	data := map[string]any{
		"command": command,
		"output":  output,
		"exit":    exit,
	}
	if timedOut {
		data["timedOut"] = true
	}
	if truncated {
		data["truncated"] = true
	}
	return New(ShellEnded, data)
}

// UserInput is a queued input being delivered. The id marks the queue item as
// delivered so a restart never delivers it twice.
func UserInput(text, inputID string) Event {
	return New(UserMessage, map[string]any{"text": text, "inputId": inputID})
}

// TurnStarted claims a session turn before any work runs. A claim that survives
// a process exit is the signal to resume the turn on the next start.
func TurnStartedEvent(turnID string) Event {
	return New(TurnStarted, map[string]any{"id": turnID})
}

// TurnFinished releases a completed or failed turn claim.
func TurnFinishedEvent(turnID string) Event {
	return New(TurnFinished, map[string]any{"id": turnID})
}

// TurnCancelled releases a claim and cancels every input queued before it.
func TurnCancelledEvent(turnID string) Event {
	return New(TurnCancelled, map[string]any{"id": turnID})
}

// TurnResumedEvent records one attempt to continue a surviving claim.
func TurnResumedEvent(turnID string) Event {
	return New(TurnResumed, map[string]any{"id": turnID})
}

func Assistant(text string) Event {
	return AssistantWithReasoning(text, "")
}

// AssistantWithReasoning stores UI-only reasoning beside the reply. Providers
// never receive the reasoning field.
func AssistantWithReasoning(text, reasoning string) Event {
	data := map[string]any{"text": text}
	if reasoning != "" {
		data["reasoning"] = reasoning
	}
	return New(AssistantMessage, data)
}

// ReasoningDeltaEvent is a live-only reasoning fragment; it is not persisted.
func ReasoningDeltaEvent(text string) Event {
	return New(ReasoningDelta, map[string]any{"text": text})
}

// SyntheticInstruction injects one instruction file discovered after a read.
// The text enters the model context; the description and relative paths drive
// the timeline's "Loaded ..." row.
func SyntheticInstruction(text, description string, paths []string) Event {
	return New(Synthetic, map[string]any{
		"text":        text,
		"description": description,
		"paths":       paths,
	})
}

func ToolCallEvent(id, name, arguments string) Event {
	return New(ToolCall, map[string]any{"id": id, "name": name, "arguments": arguments})
}

// ToolOutputEvent streams output from a running tool. Deltas are appended by
// the client; the final tool.result replaces them with the complete output.
func ToolOutputEvent(id, output string) Event {
	return New(ToolOutput, map[string]any{"id": id, "output": output})
}

func ToolResultEvent(id, name, output string, failed bool) Event {
	return ToolResultWithDiff(id, name, output, "", failed)
}

// ToolResultWithDiff records a UI-only unified diff beside the model-visible
// output.
func ToolResultWithDiff(id, name, output, diff string, failed bool) Event {
	return ToolResultWithAttachments(id, name, output, diff, failed, nil)
}

// ToolResultWithAttachments records inline media and UI-only metadata returned
// by a tool. Metadata never reaches model providers.
func ToolResultWithAttachments(id, name, output, diff string, failed bool, attachments []model.Attachment, metadata ...map[string]any) Event {
	data := map[string]any{"id": id, "name": name, "output": output, "failed": failed}
	if diff != "" {
		data["diff"] = diff
	}
	if len(attachments) > 0 {
		data["attachments"] = attachments
	}
	for _, fields := range metadata {
		if len(fields) == 0 {
			continue
		}
		data["metadata"] = fields
	}
	return New(ToolResult, data)
}

// JobCompletedEvent delivers a finished background job to the model as a
// synthetic turn while keeping the job metadata for clients.
func JobCompletedEvent(id, command, status, outputFile, text string, exitCode int) Event {
	return New(JobCompleted, map[string]any{
		"id":          id,
		"command":     command,
		"status":      status,
		"exit_code":   exitCode,
		"output_file": outputFile,
		"text":        text,
	})
}

// SubagentCompletedEvent delivers a finished child session to its parent as a
// synthetic turn.
func SubagentCompletedEvent(sessionID, description, status, text, errorText string) Event {
	data := map[string]any{
		"id":          sessionID,
		"description": description,
		"status":      status,
		"text":        text,
	}
	if errorText != "" {
		data["error"] = errorText
	}
	return New(SubagentCompleted, data)
}

// SubagentStartedEvent records the child sequence before a background run so
// recovery reads only that run's events.
func SubagentStartedEvent(sessionID, description, agentID string, childSeq int64) Event {
	return New(SubagentStarted, map[string]any{
		"id":          sessionID,
		"description": description,
		"agent":       agentID,
		"childSeq":    childSeq,
	})
}

// SyntheticEvent injects externally produced text as a synthetic turn. The
// model sees it as a user message but clients see no user-authored message.
func SyntheticEvent(text string) Event {
	return New(Synthetic, map[string]any{"text": text})
}

// AgentSelectedEvent records a session agent switch with the previous id.
func AgentSelectedEvent(agent, previous string) Event {
	return New(AgentSelected, map[string]any{"agent": agent, "previous": previous})
}

// ModelSelectedEvent records a session provider/model switch with the previous
// selection, so a client can tell a model change from a variant change.
func ModelSelectedEvent(provider, model, previousProvider, previousModel string) Event {
	return New(ModelSelected, map[string]any{
		"provider": provider,
		"model":    model,
		"previous": map[string]any{"provider": previousProvider, "model": previousModel},
	})
}

// PermissionsEvent replaces the session-level permission overlay. Agent rules
// are prepended by the runner, so these rules can only narrow them.
func PermissionsEvent(rules permission.Rules) Event {
	return New(SessionPermissions, map[string]any{"permissions": rules})
}

// MoveQueuedEvent records a transfer target waiting for the current turn.
func MoveQueuedEvent(target string) Event {
	return MoveQueuedItemEvent("", "steer", target, "")
}

// MoveQueuedItemEvent is MoveQueuedEvent with the inbox identity and delivery
// mode, so the request can be cancelled or ordered like any other control.
func MoveQueuedItemEvent(inputID, delivery, target, directory string) Event {
	data := map[string]any{}
	if inputID != "" {
		data["id"] = inputID
	}
	if delivery != "" {
		data["delivery"] = delivery
	}
	if target != "" {
		data["target"] = target
	}
	if directory != "" {
		data["directory"] = directory
	}
	return New(MoveQueued, data)
}

// MoveFailedEvent closes a queued move that could not be applied.
func MoveFailedEvent(message string) Event {
	return MoveFailedEventForInput(message, "")
}

// MoveFailedEventForInput also names the inbox item that failed.
func MoveFailedEventForInput(message, inputID string) Event {
	data := map[string]any{"message": message}
	if inputID != "" {
		data["inputId"] = inputID
	}
	return New(MoveFailed, data)
}

// SessionMoved records a session that changed workspace inside one core. The
// durable event keeps the transcript truthful after a restart, and a client
// reads it to update the project a session belongs to.
func SessionMovedEvent(directory string) Event {
	return SessionMovedForInput(directory, "")
}

// SessionMovedForInput also names the move control it consumed.
func SessionMovedForInput(directory, inputID string) Event {
	data := map[string]any{"directory": directory}
	if inputID != "" {
		data["inputId"] = inputID
	}
	return New(SessionMoved, data)
}

// InstructionUpdatedEvent announces changed instruction sections. It travels as
// a synthetic user message so the model sees the change without the system
// prefix being rewritten. The text stays model-facing; description drives the
// transcript notice.
func InstructionUpdatedEvent(text string) Event {
	return New(Synthetic, map[string]any{
		"text":        text,
		"source":      "instructions",
		"description": "Instructions updated",
	})
}

// InstructionStateEvent freezes the baseline sections and records the latest
// observed sections. Both maps are keyed by prompt section name, and the hash
// maps let a reader detect which sections changed without diffing the text.
func InstructionStateEvent(initial, current map[string]string) Event {
	return New(InstructionState, map[string]any{
		"initial":       cloneStringMap(initial),
		"current":       cloneStringMap(current),
		"initialHashes": sectionHashes(initial),
		"currentHashes": sectionHashes(current),
	})
}

func sectionHashes(sections map[string]string) map[string]string {
	out := make(map[string]string, len(sections))
	for key, text := range sections {
		sum := sha256.Sum256([]byte(text))
		out[key] = hex.EncodeToString(sum[:])
	}
	return out
}

func cloneStringMap(value map[string]string) map[string]string {
	out := make(map[string]string, len(value))
	for key, text := range value {
		out[key] = text
	}
	return out
}

// FormField is one field of a structured form request.
type FormField struct {
	Name        string   `json:"name"`
	Label       string   `json:"label,omitempty"`
	Type        string   `json:"type,omitempty"`
	Required    bool     `json:"required,omitempty"`
	Options     []string `json:"options,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
	Default     string   `json:"default,omitempty"`
}

// FormAskedEvent records a form waiting for an answer.
func FormAskedEvent(id, title string, fields []FormField) Event {
	return New(FormAsked, map[string]any{"id": id, "title": title, "fields": fields})
}

// FormRepliedEvent records the answer or cancellation. Text is what the model
// sees as a synthetic user message.
func FormRepliedEvent(id, title, text string, values map[string]any, cancelled bool) Event {
	return New(FormReplied, map[string]any{
		"id":        id,
		"title":     title,
		"text":      text,
		"values":    values,
		"cancelled": cancelled,
	})
}

// ScheduleFiredEvent delivers a due schedule prompt as a synthetic turn while
// keeping the schedule metadata for clients.
func ScheduleFiredEvent(id, name, prompt string) Event {
	return New(ScheduleFired, map[string]any{
		"id":     id,
		"name":   name,
		"prompt": prompt,
		"text":   prompt,
	})
}

// QuestionAskedEvent records a question before the tool blocks for an answer.
func QuestionAskedEvent(id, sessionID string, questions any) Event {
	return New(QuestionAsked, map[string]any{
		"id":        id,
		"sessionId": sessionID,
		"questions": questions,
	})
}

// QuestionRepliedEvent records the answer or dismissal for one question.
func QuestionRepliedEvent(id, sessionID string, answers any, cancelled bool, message string) Event {
	data := map[string]any{
		"id":        id,
		"sessionId": sessionID,
		"answers":   answers,
		"cancelled": cancelled,
	}
	if message != "" {
		data["message"] = message
	}
	return New(QuestionReplied, data)
}

// TurnSnapshotEvent records the worktree commit captured when a turn started.
func TurnSnapshotEvent(turnSeq int64, snapshot string) Event {
	return New(TurnSnapshot, map[string]any{"turnSeq": turnSeq, "snapshot": snapshot})
}

// RevertStagedEvent records a revert boundary and both sides of the restore.
func RevertStagedEvent(turnSeq int64, snapshot, current string, files []string) Event {
	return New(SessionReverted, map[string]any{
		"turnSeq":  turnSeq,
		"snapshot": snapshot,
		"current":  current,
		"files":    files,
	})
}

// ReapplyStagedEvent undoes the latest staged revert.
func ReapplyStagedEvent(turnSeq int64) Event {
	return New(SessionReapplied, map[string]any{"turnSeq": turnSeq})
}

// SessionRenamedEvent announces a generated or manual title change.
func SessionRenamedEvent(sessionID, title string) Event {
	return New(SessionRenamed, map[string]any{"sessionID": sessionID, "title": title})
}

// SessionDeletedEvent announces a session that no longer exists. It is live
// only: the session's own event log is gone with it, so the notice travels on
// the all-session stream instead.
func SessionDeletedEvent(sessionID string) Event {
	return New(SessionDeleted, map[string]any{"sessionID": sessionID})
}

// UsageEvent records what the provider reported for one model call. Input
// excludes cached tokens, so input + cacheRead is the whole prompt.
func UsageEvent(input, output, cacheRead, cacheWrite int) Event {
	return New(Usage, map[string]any{
		"input":      input,
		"output":     output,
		"cacheRead":  cacheRead,
		"cacheWrite": cacheWrite,
	})
}

// CompactionEvent replaces all earlier messages with checkpoint.
func CompactionEvent(checkpoint string, covered int) Event {
	return New(Compaction, map[string]any{"checkpoint": checkpoint, "covered": covered})
}

// CompactionEventWithReason records why the checkpoint was produced.
func CompactionEventWithReason(checkpoint string, covered int, reason string) Event {
	event := CompactionEvent(checkpoint, covered)
	event.Data["reason"] = reason
	return event
}

// CompactionEventForInput names the inbox item a manual compaction consumed, so
// recovery can settle exactly that request.
func CompactionEventForInput(checkpoint string, covered int, reason, inputID string) Event {
	event := CompactionEventWithReason(checkpoint, covered, reason)
	if inputID != "" {
		event.Data["inputId"] = inputID
	}
	return event
}

func CompactionStartEvent() Event {
	return New(CompactionStart, nil)
}

// CompactionFailedEvent closes a running compaction with its failure reason.
func CompactionFailedEvent(message string) Event {
	return New(CompactionFailed, map[string]any{"message": message})
}

// CompactionFailedForInput also names the inbox item that failed.
func CompactionFailedForInput(message, inputID string) Event {
	event := CompactionFailedEvent(message)
	if inputID != "" {
		event.Data["inputId"] = inputID
	}
	return event
}

// CompactionQueuedEvent records a manual compaction waiting for the turn's next
// safe boundary. The id is the inbox identity a client cancels or changes.
func CompactionQueuedEvent(inputID, delivery string) Event {
	data := map[string]any{}
	if inputID != "" {
		data["id"] = inputID
	}
	if delivery != "" {
		data["delivery"] = delivery
	}
	return New(CompactionQueued, data)
}

func End(reason string) Event {
	return New(RunEnd, map[string]any{"reason": reason})
}

// RunFailedEvent records the message a turn failed with. The client keeps the
// row after a reconnect and a restarted core can still explain the gap.
func RunFailedEvent(message string) Event {
	return New(RunFailed, map[string]any{"message": message})
}

func PermissionAskedEvent(id, sessionID, action string, resources []string, message string) Event {
	data := map[string]any{"id": id, "sessionId": sessionID, "action": action, "resources": resources}
	if message != "" {
		data["message"] = message
	}
	return New(PermissionAsked, data)
}

func ErrorEvent(message string) Event {
	return New(Error, map[string]any{"message": message})
}

// InputUpdated changes the delivery preference of a pending input.
func InputUpdatedEvent(inputID, delivery string) Event {
	return New(InputUpdated, map[string]any{"id": inputID, "delivery": delivery})
}

// InputCancelled removes one pending input before it is delivered.
func InputCancelledEvent(inputID string) Event {
	return New(InputCancelled, map[string]any{"id": inputID})
}

// InputQueuedDeliveryEvent persists an input together with its delivery mode.
func InputQueuedDeliveryEvent(inputID, text, delivery string) Event {
	data := map[string]any{"id": inputID, "text": text}
	if delivery != "" {
		data["delivery"] = delivery
	}
	return New(InputQueued, data)
}

// InputQueuedPayloadEvent also stores UI-only payload such as attachments or
// review metadata, so a recovered turn rebuilds the same user message.
func InputQueuedPayloadEvent(inputID, text, delivery string, payload map[string]any) Event {
	event := InputQueuedDeliveryEvent(inputID, text, delivery)
	if len(payload) > 0 {
		event.Data["payload"] = payload
	}
	return event
}

// SyntheticQueuedEvent admits one externally produced event for delivery at a
// boundary. eventType and data are replayed on the visible event, so a
// job.completed or schedule.fired keeps its transcript rendering.
func SyntheticQueuedEvent(inputID, text, delivery, eventType string, data map[string]any) Event {
	payload := map[string]any{"event": eventType}
	if len(data) > 0 {
		payload["data"] = data
	}
	return SyntheticQueuedEventWith(inputID, text, delivery, payload)
}

// SyntheticQueuedEventWith is SyntheticQueuedEvent with an explicit payload map.
func SyntheticQueuedEventWith(inputID, text, delivery string, payload map[string]any) Event {
	event := New(SyntheticQueued, map[string]any{"id": inputID, "text": text})
	if delivery != "" {
		event.Data["delivery"] = delivery
	}
	if len(payload) > 0 {
		event.Data["payload"] = payload
	}
	return event
}

// SyntheticInputEvent is a delivered synthetic inbox item. The input id marks
// the pending item delivered, so replay never publishes it twice.
func SyntheticInputEvent(inputID, text, delivery string, data map[string]any) Event {
	event := New(Synthetic, map[string]any{"text": text})
	for key, value := range data {
		event.Data[key] = value
	}
	if inputID != "" {
		event.Data["inputId"] = inputID
	}
	if delivery != "" {
		event.Data["delivery"] = delivery
	}
	return event
}

// TurnReleasedEvent closes a turn claim without cancelling pending input.
func TurnReleasedEvent(turnID string) Event {
	return New(TurnReleased, map[string]any{"id": turnID})
}
