// Package tool 定义工具接口、注册表与内置工具。
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"lingzhu/internal/model"
	"lingzhu/internal/permission"
)

var ErrNotFound = errors.New("tool not found")

// Call 是一次工具调用的上下文。
type Call struct {
	SessionID string
	Name      string
	Input     json.RawMessage
	Perm      *permission.Manager
	OnAsk     func(permission.Request)
}

// Allow 在工具执行前向权限管理器申请 action 权限。
func (c Call) Allow(ctx context.Context, action string, resources []string, message string) error {
	if c.Perm == nil {
		return nil
	}
	return c.Perm.Check(ctx, permission.Request{
		SessionID: c.SessionID,
		Action:    action,
		Resources: resources,
		Message:   message,
	}, c.OnAsk)
}

// AllowFile 先处理越界目录，再申请文件动作权限。
func (c Call) AllowFile(ctx context.Context, action, resource string, external bool) error {
	if external {
		if err := c.Allow(ctx, "external_directory", []string{resource}, ""); err != nil {
			return err
		}
	}
	return c.Allow(ctx, action, []string{resource}, "")
}

type Tool interface {
	Name() string
	Description() string
	// Schema 是该工具入参的 JSON Schema。
	Schema() json.RawMessage
	Run(ctx context.Context, call Call) (string, error)
}

type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}}
}

func (r *Registry) Add(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

func (r *Registry) Definitions() []model.ToolDef {
	r.mu.RLock()
	defer r.mu.RUnlock()
	defs := make([]model.ToolDef, 0, len(r.tools))
	for _, t := range r.tools {
		defs = append(defs, model.ToolDef{Name: t.Name(), Description: t.Description(), Schema: t.Schema()})
	}
	sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	return defs
}

func (r *Registry) Run(ctx context.Context, call Call) (string, error) {
	t, ok := r.Get(call.Name)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNotFound, call.Name)
	}
	return t.Run(ctx, call)
}
