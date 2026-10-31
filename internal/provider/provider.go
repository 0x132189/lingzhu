// Package provider 定义模型适配接口与 OpenAI 兼容实现。
package provider

import (
	"context"

	"lingzhu/internal/model"
)

type Chunk struct {
	Text      string
	ToolCalls []ToolCallDelta
}

// ToolCallDelta 是流式返回中的一个工具调用片段，按 Index 合并。
type ToolCallDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

type Stream interface {
	Recv() (Chunk, error)
	Close() error
}

type Provider interface {
	Stream(ctx context.Context, req model.Request) (Stream, error)
}
