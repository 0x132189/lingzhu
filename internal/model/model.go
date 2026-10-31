// Package model 定义 provider、tool、session、agent 共用的消息类型。
package model

import "encoding/json"

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

// Message 是发给模型的一条消息。工具调用挂在 assistant 消息上，
// 工具结果单独成为 role=tool 的消息。
type Message struct {
	Role       Role
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	Name       string
}

type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type ToolDef struct {
	Name        string
	Description string
	Schema      json.RawMessage
}

type Request struct {
	Model    string
	Messages []Message
	Tools    []ToolDef
}
