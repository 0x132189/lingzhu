// Package event 定义运行时事件。持久事件写入会话日志，
// 增量事件只发给当前订阅者。
package event

import "time"

type Type string

const (
	StepStart        Type = "step.start"
	TextDelta        Type = "text.delta"
	UserMessage      Type = "user.message"
	AssistantMessage Type = "assistant.message"
	ToolCall         Type = "tool.call"
	ToolResult       Type = "tool.result"
	RunEnd           Type = "run.end"
	Error            Type = "error"
)

type Event struct {
	Type Type           `json:"type"`
	At   time.Time      `json:"at"`
	Data map[string]any `json:"data,omitempty"`
}

// Durable 表示该事件是否写入会话日志。增量与运行控制事件不落盘。
func (e Event) Durable() bool {
	switch e.Type {
	case StepStart, TextDelta, RunEnd, Error:
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

func Assistant(text string) Event {
	return New(AssistantMessage, map[string]any{"text": text})
}

func ToolCallEvent(id, name, arguments string) Event {
	return New(ToolCall, map[string]any{"id": id, "name": name, "arguments": arguments})
}

func ToolResultEvent(id, name, output string, failed bool) Event {
	return New(ToolResult, map[string]any{"id": id, "name": name, "output": output, "failed": failed})
}

func End(reason string) Event {
	return New(RunEnd, map[string]any{"reason": reason})
}

func ErrorEvent(message string) Event {
	return New(Error, map[string]any{"message": message})
}
