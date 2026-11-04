// Package event defines runtime events. Durable events are persisted to the
// session log; delta and control events only reach current subscribers.
package event

import "time"

type Type string

const (
	StepStart         Type = "step.start"
	TextDelta         Type = "text.delta"
	UserMessage       Type = "user.message"
	AssistantMessage  Type = "assistant.message"
	ToolCall          Type = "tool.call"
	ToolResult        Type = "tool.result"
	PermissionAsked   Type = "permission.asked"
	PermissionReplied Type = "permission.replied"
	Usage             Type = "usage"
	CompactionStart   Type = "compaction.start"
	Compaction        Type = "compaction"
	RunEnd            Type = "run.end"
	Error             Type = "error"
	InputQueued       Type = "input.queued"
	TurnStarted       Type = "turn.started"
	TurnFinished      Type = "turn.finished"
	TurnCancelled     Type = "turn.cancelled"
)

type Event struct {
	Type Type `json:"type"`
	// Seq is the durable sequence number within one session; zero for events
	// that are not persisted.
	Seq  int64          `json:"seq,omitempty"`
	At   time.Time      `json:"at"`
	Data map[string]any `json:"data,omitempty"`
}

// Durable reports whether the event is written to the session log.
func (e Event) Durable() bool {
	switch e.Type {
	case StepStart, TextDelta, RunEnd, Error, PermissionAsked, PermissionReplied, CompactionStart:
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

func User(text string) Event {
	return New(UserMessage, map[string]any{"text": text})
}

// UserInput is a queued input being delivered. The id marks the queue item as
// delivered so a restart never delivers it twice.
func UserInput(text, inputID string) Event {
	return New(UserMessage, map[string]any{"text": text, "inputId": inputID})
}

// InputQueued persists an input before it can be delivered.
func InputQueuedEvent(inputID, text string) Event {
	return New(InputQueued, map[string]any{"id": inputID, "text": text})
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

func Assistant(text string) Event {
	return New(AssistantMessage, map[string]any{"text": text})
}

func ToolCallEvent(id, name, arguments string) Event {
	return New(ToolCall, map[string]any{"id": id, "name": name, "arguments": arguments})
}

func ToolResultEvent(id, name, output string, failed bool) Event {
	return ToolResultWithDiff(id, name, output, "", failed)
}

// ToolResultWithDiff records a UI-only unified diff beside the model-visible
// output.
func ToolResultWithDiff(id, name, output, diff string, failed bool) Event {
	data := map[string]any{"id": id, "name": name, "output": output, "failed": failed}
	if diff != "" {
		data["diff"] = diff
	}
	return New(ToolResult, data)
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

func CompactionStartEvent() Event {
	return New(CompactionStart, nil)
}

func End(reason string) Event {
	return New(RunEnd, map[string]any{"reason": reason})
}

func PermissionAskedEvent(id, sessionID, action string, resources []string, message string) Event {
	data := map[string]any{"id": id, "sessionId": sessionID, "action": action, "resources": resources}
	if message != "" {
		data["message"] = message
	}
	return New(PermissionAsked, data)
}

func PermissionRepliedEvent(id, sessionID, decision string) Event {
	return New(PermissionReplied, map[string]any{"id": id, "sessionId": sessionID, "decision": decision})
}

func ErrorEvent(message string) Event {
	return New(Error, map[string]any{"message": message})
}
