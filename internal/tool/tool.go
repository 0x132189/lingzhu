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
)

var ErrNotFound = errors.New("tool not found")

type Tool interface {
	Name() string
	Description() string
	// Schema 是该工具入参的 JSON Schema。
	Schema() json.RawMessage
	Run(ctx context.Context, input json.RawMessage) (string, error)
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

func (r *Registry) Run(ctx context.Context, name string, input json.RawMessage) (string, error) {
	t, ok := r.Get(name)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return t.Run(ctx, input)
}
