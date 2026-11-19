// Package tool defines the tool interface, registry and built-in tools.
package tool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
	"lingzhu/internal/permission"
)

var ErrNotFound = errors.New("tool not found")

// Instruction is one AGENTS.md file discovered while reading nearby files.
type Instruction struct {
	Text        string
	Description string
	Paths       []string
}

// Call is the context of one tool invocation.
type Call struct {
	SessionID string
	ID        string
	Name      string
	Input     json.RawMessage
	Perm      *permission.Manager
	OnAsk     func(permission.Request)
	// OnDiff receives a unified diff for tools that change files.
	OnDiff func(string)
	// Emit persists and publishes a durable event for this tool call.
	Emit func(event.Event) error
	// OnInstruction receives a nearby instruction file discovered by a read.
	OnInstruction func(Instruction)
	// OnAttachment receives one inline media file for this call.
	OnAttachment func(model.Attachment)
	// CanAttach reports whether the selected model accepts media attachments.
	CanAttach bool
	// OnFormat formats a file after a successful write. It returns the
	// formatter name, or an empty string when no formatter applies.
	OnFormat func(ctx context.Context, path string) (string, error)
	// Metadata carries UI-only result fields such as counts, providers,
	// diagnostics and nested calls. Providers never receive it.
	Metadata map[string]any
	// OnProgress publishes a live UI-only metadata snapshot while a tool runs.
	// The final result still carries the complete Metadata map.
	OnProgress func(map[string]any)
	// ToolFilter limits nested Code Mode calls to the tools an agent allows.
	// Nil means every registered tool is available.
	ToolFilter func(string) bool
	// Rules are the calling agent's permission rules, including the session
	// overlay. Tools use them to filter what they offer the model without
	// prompting; nil means no agent rules are known.
	Rules permission.Rules
	// Unattended marks a turn nobody is watching, such as a scheduled wake-up.
	// An ask becomes a deny instead of blocking forever.
	Unattended bool
}

// Allows reports whether a nested Code Mode call may use name.
func (c Call) Allows(name string) bool {
	return c.ToolFilter == nil || c.ToolFilter(name)
}

// SetMetadata records one UI-only result field for this call.
func (c Call) SetMetadata(key string, value any) {
	if c.Metadata == nil {
		return
	}
	c.Metadata[key] = value
}

// rawInputTool opts out of the object-schema check for a tool that can also
// accept raw text, such as a freeform Code Mode program.
type rawInputTool interface {
	AcceptsRawInput() bool
}

// FormatFile runs the configured formatter for a just-written file.
func (c Call) FormatFile(ctx context.Context, path string) (string, error) {
	if c.OnFormat == nil {
		return "", nil
	}
	return c.OnFormat(ctx, path)
}

// SetAttachment reports inline media for the UI and the model.
func (c Call) SetAttachment(attachment model.Attachment) {
	if c.OnAttachment != nil && attachment.Data != "" {
		c.OnAttachment(attachment)
	}
}

// SetDiff reports the file change for the UI without making the model read it.
func (c Call) SetDiff(diff string) {
	if c.OnDiff != nil && diff != "" {
		c.OnDiff(diff)
	}
}

// Allow requests permission for action before the tool runs.
func (c Call) Allow(ctx context.Context, action string, resources []string, message string) error {
	if c.Perm == nil {
		return nil
	}
	return c.Perm.Check(ctx, permission.Request{
		SessionID:  c.SessionID,
		Action:     action,
		Resources:  resources,
		Message:    message,
		Unattended: c.Unattended,
	}, c.OnAsk)
}

// AllowFile checks the external directory first, then the file action.
func (c Call) AllowFile(ctx context.Context, action, resource string, external bool) error {
	if external {
		if err := c.Allow(ctx, "external_directory", []string{resource}, ""); err != nil {
			return err
		}
	}
	return c.Allow(ctx, action, []string{resource}, "")
}

// Resolve turns a tool path into an absolute path and asks for permission.
// A path outside root is checked against external_directory first, so every
// file tool applies the same boundary policy.
func (c Call) Resolve(ctx context.Context, action, root, path string) (string, error) {
	abs, err := resolvePath(root, path)
	if err != nil {
		return "", err
	}
	if err := c.AllowFile(ctx, action, abs, external(root, abs)); err != nil {
		return "", err
	}
	return abs, nil
}

type Tool interface {
	Name() string
	Description() string
	// Schema is the JSON Schema of the tool input.
	Schema() json.RawMessage
	Run(ctx context.Context, call Call) (string, error)
}

// RulesDescriber lets a tool adapt its description to the calling agent's
// permission rules, so it can hide options the caller may not use.
type RulesDescriber interface {
	DescriptionFor(rules permission.Rules) string
}

// Describe returns the description a specific agent should see.
func Describe(t Tool, rules permission.Rules) string {
	if value, ok := t.(RulesDescriber); ok {
		return value.DescriptionFor(rules)
	}
	return t.Description()
}

// codemodeTool marks tools that execute can call. Tools are direct by default,
// so only plugin, MCP and other host tools opt into Code Mode.
type codemodeTool interface {
	Codemode() bool
}

func callableInCodemode(t Tool) bool {
	if value, ok := t.(codemodeTool); ok {
		return value.Codemode()
	}
	return false
}

type directTool struct{ Tool }

func (d directTool) Codemode() bool { return false }

// Direct marks a tool as callable only directly, never inside execute.
func Direct(t Tool) Tool { return directTool{Tool: t} }

// fullCodemodeDirect lists the built-in tools the reference moves into Code
// Mode when full_codemode is enabled. Other direct tools stay direct.
var fullCodemodeDirect = map[string]struct{}{
	"shell": {},
	"read":  {},
	"write": {},
	"edit":  {},
	"patch": {},
}

func promotedByFullCodemode(name string) bool {
	_, ok := fullCodemodeDirect[name]
	return ok
}

// SetFullCodemode toggles the reference's full Code Mode layout. Built-in file
// and shell tools become callable inside execute and leave the direct list.
func (r *Registry) SetFullCodemode(enabled bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fullCodemode == enabled {
		return
	}
	r.fullCodemode = enabled
	r.dirty = true
}

type codeTool struct{ Tool }

func (c codeTool) Codemode() bool { return true }

// Codemode marks a tool as reachable through execute.
func Codemode(t Tool) Tool { return codeTool{Tool: t} }

type Registry struct {
	mu    sync.RWMutex
	tools map[string]Tool

	// fullCodemode moves the base file and shell tools into Code Mode while
	// leaving them out of the direct tool list, matching the reference's
	// full_codemode mode.
	fullCodemode bool

	// defs and catalogs are rebuilt only when the tool set changes. The agent
	// asks for them on every model step, and re-rendering every schema each
	// time was pure overhead once the registry is stable.
	defs []model.ToolDef
	// codeDefs lists tools execute can call; allDefs lists every tool for the
	// CodemodeOnly switch, where execute is the only direct tool.
	codeDefs    []model.ToolDef
	allDefs     []model.ToolDef
	catalog     string
	catalogOnly string
	dirty       bool
}

func NewRegistry() *Registry {
	return &Registry{tools: map[string]Tool{}, dirty: true}
}

func (r *Registry) Add(t Tool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.tools[t.Name()] = t
	r.dirty = true
}

// Remove deletes one tool from the registry and reports whether it existed.
// Tools loaded dynamically, such as MCP tools, use it when a server stops
// advertising them.
func (r *Registry) Remove(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.tools[name]; !ok {
		return false
	}
	delete(r.tools, name)
	r.dirty = true
	return true
}

func (r *Registry) Get(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	return t, ok
}

// Definitions returns the direct tools plus execute. Code Mode-only tools are
// reachable through execute and are not listed as direct definitions.
func (r *Registry) Definitions() []model.ToolDef {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked()
	out := make([]model.ToolDef, len(r.defs))
	copy(out, r.defs)
	return out
}

// CodemodeDefinitions returns the tools execute can call.
func (r *Registry) CodemodeDefinitions() []model.ToolDef {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked()
	out := make([]model.ToolDef, len(r.codeDefs))
	copy(out, r.codeDefs)
	return out
}

// AllDefinitions returns every non-execute tool, for the CodemodeOnly prompt.
func (r *Registry) AllDefinitions() []model.ToolDef {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked()
	out := make([]model.ToolDef, len(r.allDefs))
	copy(out, r.allDefs)
	return out
}

// CodemodeGet resolves a tool that execute may call by name.
func (r *Registry) CodemodeGet(name string) (Tool, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.tools[name]
	if !ok || name == "execute" {
		return nil, false
	}
	if callableInCodemode(t) || (r.fullCodemode && promotedByFullCodemode(name)) {
		return t, true
	}
	return nil, false
}

// CatalogFor renders the Code Mode prompt section for a filtered tool set.
func (r *Registry) CatalogFor(filter func(string) bool) string {
	if filter == nil {
		return r.Catalog()
	}
	defs := r.CodemodeDefinitions()
	filtered := make([]model.ToolDef, 0, len(defs))
	for _, def := range defs {
		if filter(def.Name) {
			filtered = append(filtered, def)
		}
	}
	if len(filtered) == 0 {
		return noCodemodeTools
	}
	return sectionFromDefs(filtered)
}

// CatalogExecuteOnlyFor renders every non-execute tool for the CodemodeOnly
// switch, filtered to the tools an agent allows.
func (r *Registry) CatalogExecuteOnlyFor(filter func(string) bool) string {
	if filter == nil {
		return r.CatalogExecuteOnly()
	}
	defs := r.AllDefinitions()
	filtered := make([]model.ToolDef, 0, len(defs))
	for _, def := range defs {
		if filter(def.Name) {
			filtered = append(filtered, def)
		}
	}
	return sectionFromDefs(filtered)
}

// Catalog returns the Code Mode prompt section, cached until a tool changes.
func (r *Registry) Catalog() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked()
	if r.catalog == "" {
		r.catalog = sectionFromDefs(r.codeDefs)
		if len(r.codeDefs) == 0 {
			r.catalog = noCodemodeTools
		}
	}
	return r.catalog
}

// CatalogExecuteOnly returns the reference harness wording where every catalog
// tool is reachable only through execute.
func (r *Registry) CatalogExecuteOnly() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.refreshLocked()
	if r.catalogOnly == "" {
		r.catalogOnly = sectionFromDefs(r.allDefs)
	}
	return r.catalogOnly
}

// refreshLocked rebuilds the cached tool lists. Callers must hold r.mu.
func (r *Registry) refreshLocked() {
	if !r.dirty && r.defs != nil {
		return
	}
	direct := make([]model.ToolDef, 0, len(r.tools))
	code := make([]model.ToolDef, 0, len(r.tools))
	all := make([]model.ToolDef, 0, len(r.tools))
	var execute *model.ToolDef
	for _, t := range r.tools {
		def := model.ToolDef{Name: t.Name(), Description: t.Description(), Schema: t.Schema()}
		if def.Name == "execute" {
			execute = &def
			continue
		}
		all = append(all, def)
		promoted := r.fullCodemode && promotedByFullCodemode(def.Name)
		if callableInCodemode(t) || promoted {
			code = append(code, def)
		}
		if !callableInCodemode(t) && !promoted {
			direct = append(direct, def)
		}
	}
	if execute != nil {
		direct = append(direct, *execute)
	}
	sortDefs := func(defs []model.ToolDef) {
		sort.Slice(defs, func(i, j int) bool { return defs[i].Name < defs[j].Name })
	}
	sortDefs(direct)
	sortDefs(code)
	sortDefs(all)
	r.defs = direct
	r.codeDefs = code
	r.allDefs = all
	r.catalog = ""
	r.catalogOnly = ""
	r.dirty = false
}

func (r *Registry) Run(ctx context.Context, call Call) (string, error) {
	t, ok := r.Get(call.Name)
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNotFound, call.Name)
	}
	// Arguments are checked against the tool's own schema before it runs, so a
	// malformed call fails with a field list the model can correct instead of a
	// tool-specific decode error. Freeform tools validate their own input.
	if raw, ok := t.(rawInputTool); !ok || !raw.AcceptsRawInput() {
		if err := Validate(call.Name, t.Schema(), call.Input); err != nil {
			return "", err
		}
	}
	return t.Run(ctx, call)
}
