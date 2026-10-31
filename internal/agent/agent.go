// Package agent 实现单步执行的会话主循环：一步一次模型调用，
// 工具结果回填后继续下一步，直到模型不再请求工具。
package agent

import (
	"context"
	"errors"
	"io"
	"sort"
	"strings"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/permission"
	"lingzhu/internal/provider"
	"lingzhu/internal/session"
	"lingzhu/internal/tool"
)

type Agent struct {
	store    *session.Store
	provider provider.Provider
	tools    *tool.Registry
	perm     *permission.Manager
	model    string
	maxSteps int
}

func New(store *session.Store, p provider.Provider, tools *tool.Registry, perm *permission.Manager, modelName string, maxSteps int) *Agent {
	return &Agent{store: store, provider: p, tools: tools, perm: perm, model: modelName, maxSteps: maxSteps}
}

// Reply 回答一次待批的权限请求。
func (a *Agent) Reply(id string, decision permission.Decision, message string) error {
	if a.perm == nil {
		return errors.New("permission is not configured")
	}
	return a.perm.Reply(id, decision, message)
}

// Pending 列出待批的权限请求，sessionID 为空时返回全部。
func (a *Agent) Pending(sessionID string) []permission.Request {
	if a.perm == nil {
		return nil
	}
	return a.perm.Pending(sessionID)
}

// Run 异步执行一轮对话，返回事件流。事件流在结束时关闭。
func (a *Agent) Run(ctx context.Context, sessionID, prompt string) (<-chan event.Event, error) {
	if a.model == "" {
		return nil, errors.New("model is not configured")
	}
	events := make(chan event.Event, 64)
	go func() {
		defer close(events)
		a.loop(ctx, sessionID, prompt, events)
	}()
	return events, nil
}

func (a *Agent) loop(ctx context.Context, sessionID, prompt string, events chan<- event.Event) {
	if err := a.record(sessionID, event.User(prompt), events); err != nil {
		a.fail(events, err)
		return
	}

	for step := 0; step < a.maxSteps; step++ {
		if !send(ctx, events, event.Step(step)) {
			return
		}
		messages, err := a.store.Messages(sessionID)
		if err != nil {
			a.fail(events, err)
			return
		}

		text, calls, err := a.stream(ctx, messages, events)
		if err != nil {
			if ctx.Err() != nil {
				send(ctx, events, event.End("interrupted"))
				return
			}
			a.fail(events, err)
			return
		}
		if strings.TrimSpace(text) != "" {
			if err := a.record(sessionID, event.Assistant(text), events); err != nil {
				a.fail(events, err)
				return
			}
		}
		if len(calls) == 0 {
			send(ctx, events, event.End("done"))
			return
		}
		if !a.runTools(ctx, sessionID, calls, events) {
			return
		}
	}
	send(ctx, events, event.End("max_steps"))
}

func (a *Agent) stream(ctx context.Context, messages []model.Message, events chan<- event.Event) (string, []model.ToolCall, error) {
	req := model.Request{Model: a.model, Messages: messages, Tools: a.tools.Definitions()}
	stream, err := a.provider.Stream(ctx, req)
	if err != nil {
		return "", nil, err
	}
	defer stream.Close()

	var text strings.Builder
	calls := map[int]*model.ToolCall{}
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", nil, err
		}
		if chunk.Text != "" {
			text.WriteString(chunk.Text)
			if !send(ctx, events, event.Delta(chunk.Text)) {
				return "", nil, ctx.Err()
			}
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
	return text.String(), ordered(calls), nil
}

func (a *Agent) runTools(ctx context.Context, sessionID string, calls []model.ToolCall, events chan<- event.Event) bool {
	for _, call := range calls {
		if call.ID == "" {
			call.ID = "call_" + call.Name
		}
		if err := a.record(sessionID, event.ToolCallEvent(call.ID, call.Name, call.Arguments), events); err != nil {
			a.fail(events, err)
			return false
		}
		output, err := a.tools.Run(ctx, tool.Call{
			SessionID: sessionID,
			Name:      call.Name,
			Input:     []byte(call.Arguments),
			Perm:      a.perm,
			OnAsk: func(req permission.Request) {
				sendRealtime(events, event.PermissionAskedEvent(req.ID, req.SessionID, req.Action, req.Resources, req.Message))
			},
		})
		failed := err != nil
		if err != nil {
			output = strings.TrimSpace(output + "\n" + err.Error())
		}
		if err := a.record(sessionID, event.ToolResultEvent(call.ID, call.Name, output, failed), events); err != nil {
			a.fail(events, err)
			return false
		}
	}
	return true
}

func (a *Agent) record(sessionID string, ev event.Event, events chan<- event.Event) error {
	if err := a.store.Append(sessionID, ev); err != nil {
		return err
	}
	sendRealtime(events, ev)
	return nil
}

func (a *Agent) fail(events chan<- event.Event, err error) {
	sendRealtime(events, event.ErrorEvent(err.Error()))
	sendRealtime(events, event.End("error"))
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

func send(ctx context.Context, events chan<- event.Event, ev event.Event) bool {
	select {
	case events <- ev:
		return true
	case <-ctx.Done():
		return false
	}
}

func sendRealtime(events chan<- event.Event, ev event.Event) {
	select {
	case events <- ev:
	default:
	}
}
