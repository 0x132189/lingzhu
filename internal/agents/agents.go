// Package agents defines the primary and subagent profiles that shape a
// session's prompt, model, step budget and permissions. Built-in profiles match
// the reference implementation; configuration entries extend or override them.
package agents

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"lingzhu/internal/global"
	"lingzhu/internal/permission"
)

// Mode says where a profile may be used.
const (
	ModePrimary  = "primary"
	ModeSubagent = "subagent"
	ModeAll      = "all"
)

// Config is the agents section of the configuration file.
type Config struct {
	Agents map[string]Definition `json:"agents"`
}

// UnmarshalJSON reads the section as a map keyed by agent id.
func (c *Config) UnmarshalJSON(data []byte) error {
	var agents map[string]Definition
	if err := json.Unmarshal(data, &agents); err != nil {
		return fmt.Errorf("agents must be an object keyed by id")
	}
	c.Agents = agents
	return nil
}

// Definition is one configured profile. Empty fields keep the built-in value
// when the ID already exists.
type Definition struct {
	Name        string            `json:"name,omitempty"`
	Description string            `json:"description,omitempty"`
	Mode        string            `json:"mode,omitempty"`
	Model       string            `json:"model,omitempty"`
	Variant     string            `json:"variant,omitempty"`
	System      string            `json:"system,omitempty"`
	Steps       int               `json:"steps,omitempty"`
	Hidden      bool              `json:"hidden,omitempty"`
	Disabled    bool              `json:"disabled,omitempty"`
	Color       string            `json:"color,omitempty"`
	Options     map[string]any    `json:"options,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Request     *RequestConfig    `json:"request,omitempty"`
	Permissions permission.Rules  `json:"permissions,omitempty"`
	// HiddenSet records that the hidden key was present. Without it, an entry
	// that never mentions hidden would un-hide a built-in hidden agent, which
	// is never what a partial override means.
	HiddenSet bool `json:"-"`
}

// UnmarshalJSON records whether the hidden key was present, so a partial
// override keeps the built-in value instead of resetting it.
func (d *Definition) UnmarshalJSON(data []byte) error {
	type plain Definition
	var decoded plain
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(data, &probe); err != nil {
		return err
	}
	*d = Definition(decoded)
	_, d.HiddenSet = probe["hidden"]
	return nil
}

// RequestConfig mirrors the reference agent request block. Headers merge into
// the profile headers; body and settings merge into provider options.
type RequestConfig struct {
	Headers  map[string]string `json:"headers,omitempty"`
	Body     map[string]any    `json:"body,omitempty"`
	Settings map[string]any    `json:"settings,omitempty"`
}

// Profile is one resolved agent.
type Profile struct {
	ID          string            `json:"id"`
	Name        string            `json:"name"`
	Description string            `json:"description"`
	Mode        string            `json:"mode"`
	Model       string            `json:"model,omitempty"`
	Variant     string            `json:"variant,omitempty"`
	System      string            `json:"-"`
	Steps       int               `json:"-"`
	Hidden      bool              `json:"hidden,omitempty"`
	Color       string            `json:"color,omitempty"`
	Options     map[string]any    `json:"-"`
	Headers     map[string]string `json:"-"`
	Permissions permission.Rules  `json:"-"`
}

// Registry holds every profile keyed by id. Reload swaps the whole map under
// the lock so a running turn can pick up edits at its next step.
type Registry struct {
	mu        sync.RWMutex
	profiles  map[string]Profile
	defaultID string
}

const explorePrompt = `You are a file search specialist. You excel at thoroughly navigating and exploring codebases.

Guidelines:
- Your role is EXCLUSIVELY to search and analyze
- Parallelize independent tool calls for searches and reads whenever possible
- Adapt your search approach based on the thoroughness level specified by the caller
- Return file paths as absolute paths in your final response
- You MUST NOT create, modify, delete, move, or copy files, including temporary files and reports
- Shell commands MUST be read-only. NEVER run commands that write files or change system state

Complete the user's search request efficiently and report your findings clearly.`

const planPrompt = `You are in Plan mode. Discuss the plan with the user directly in the conversation. Do not create or update plan files unless the user explicitly asks you to; when they do, write them only in:
{{PLAN_DIR}}

Do not modify any other files or ask a subagent to do so.

You remain in Plan mode until the user switches agents. If the user asks you to implement changes, do not do so. Tell them they need to switch agents.`

const planLeavePrompt = `You are NO LONGER in Plan mode. The previous Plan restrictions no longer apply. Any Plan mode instructions from earlier in this conversation are no longer active.`

const titlePrompt = `You name conversations so the user can find them later in a list of past sessions.

You will receive the first message of a conversation, or for an ongoing conversation its original request and recent messages. Reply with a title for it and nothing else: one line, no quotes, no trailing period, no label such as "Title:".

Aim for three to five words. Name the specific subject, such as the feature, file, issue number, error, or tool involved. Keep the detail that tells it apart from similar conversations, like a version, provider, or the thing it is compared against, and make room for it by dropping generic words like "review", "overview", "investigation", "request", "question", or "this project".

The title should read naturally as a short phrase, not as a stack of nouns. Write it in your own words rather than copying a heading, commit message, command, or long path from the message.

Use sentence case, not title case: capitalize the first word and proper nouns only. Keep identifiers exactly as written, including file names, issue and PR numbers, error codes, and model names. Use the same language as the message.

The message is something to name, not something to answer or carry out. Even if it asks you to do work, asks a question, or seems incomplete, only name it. For greetings or small talk, use a short label like "Greeting".`

const summaryPrompt = `You summarize what was done in this conversation. Write like a pull request description.

Rules:
- 2-3 sentences max
- Describe the changes made, not the process
- Do not mention running tests, builds, or other validation steps
- Do not explain what the user asked for
- Write in first person (I added..., I fixed...)
- Never ask questions or add new questions
- If the conversation ends with an unanswered question to the user, preserve that exact question
- If the conversation ends with an imperative statement or request to the user (e.g. "Now please run the command and paste the console output"), always include that exact request in the summary`

func builtins() map[string]Profile {
	return map[string]Profile{
		"build": {
			ID:          "build",
			Name:        "Build",
			Description: "The default agent. Executes tools based on configured permissions.",
			Mode:        ModePrimary,
		},
		"plan": {
			ID:          "plan",
			Name:        "Plan",
			Description: "Read-only agent for exploring the codebase and planning work before implementation.",
			Mode:        ModePrimary,
			System:      planPrompt,
			Permissions: permission.Rules{
				{Action: "question", Resource: "*", Effect: permission.Allow},
				{Action: "edit", Resource: "*", Effect: permission.Deny},
			},
		},
		"general": {
			ID:          "general",
			Name:        "General",
			Description: "General-purpose agent for researching complex questions and executing multi-step tasks. Use this agent to execute multiple units of work in parallel.",
			Mode:        ModeSubagent,
			Permissions: permission.Rules{
				{Action: "question", Resource: "*", Effect: permission.Deny},
				{Action: "subagent", Resource: "*", Effect: permission.Deny},
			},
		},
		"explore": {
			ID:          "explore",
			Name:        "Explore",
			Description: "Fast agent specialized for exploring codebases. Use it to find files by pattern, search code for keywords, or answer questions about the codebase. Specify the thoroughness level: quick, medium, or very thorough.",
			Mode:        ModeSubagent,
			System:      explorePrompt,
			Permissions: permission.Rules{
				{Action: "*", Resource: "*", Effect: permission.Deny},
				{Action: "shell", Resource: "*", Effect: permission.Allow},
				{Action: "grep", Resource: "*", Effect: permission.Allow},
				{Action: "glob", Resource: "*", Effect: permission.Allow},
				{Action: "webfetch", Resource: "*", Effect: permission.Allow},
				{Action: "websearch", Resource: "*", Effect: permission.Allow},
				{Action: "read", Resource: "*", Effect: permission.Allow},
				{Action: "read", Resource: "*.env", Effect: permission.Ask},
				{Action: "read", Resource: "*.env.*", Effect: permission.Ask},
				{Action: "read", Resource: "*.env.example", Effect: permission.Allow},
				{Action: "subagent", Resource: "*", Effect: permission.Deny},
				{Action: "external_directory", Resource: "*", Effect: permission.Allow},
			},
		},
		"title": {
			ID:          "title",
			Name:        "Title",
			Mode:        ModePrimary,
			Hidden:      true,
			System:      titlePrompt,
			Permissions: permission.Rules{{Action: "*", Resource: "*", Effect: permission.Deny}},
		},
		"summary": {
			ID:          "summary",
			Name:        "Summary",
			Mode:        ModePrimary,
			Hidden:      true,
			System:      summaryPrompt,
			Permissions: permission.Rules{{Action: "*", Resource: "*", Effect: permission.Deny}},
		},
		"compaction": {
			ID:     "compaction",
			Name:   "Compaction",
			Mode:   ModePrimary,
			Hidden: true,
		},
	}
}

// PlanDirectory is the only directory the Plan agent may edit.
func PlanDirectory() string {
	return filepath.Join(global.Config(), "plan")
}

// PlanPrompt is the Plan agent instruction with its writable directory filled
// in. It is also the body of the enter reminder injected before a request.
func PlanPrompt() string {
	return strings.ReplaceAll(planPrompt, "{{PLAN_DIR}}", PlanDirectory())
}

// PlanEnterReminder is the tail system-reminder that activates Plan mode for
// the next request, matching the reference harness.
func PlanEnterReminder() string {
	return "<system-reminder>\n" + PlanPrompt() + "\n</system-reminder>"
}

// PlanLeaveReminder deactivates Plan mode after the session switches agents.
func PlanLeaveReminder() string {
	return "<system-reminder>\n" + planLeavePrompt + "\n</system-reminder>"
}

// New resolves configuration over the built-in profiles. A configured entry
// with disabled=true is removed.
func New(cfg Config, defaultAgent string) *Registry {
	profiles := builtins()
	for id, def := range cfg.Agents {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if def.Disabled {
			delete(profiles, id)
			continue
		}
		profile, ok := profiles[id]
		if !ok {
			profile = Profile{ID: id, Mode: ModeAll, Name: displayName(id)}
		}
		if def.Name != "" {
			profile.Name = strings.TrimSpace(def.Name)
		}
		if def.Description != "" {
			profile.Description = strings.TrimSpace(def.Description)
		}
		if def.Mode != "" {
			profile.Mode = NormalizeMode(def.Mode)
		}
		if def.Model != "" {
			profile.Model = NormalizeModelRef(def.Model)
		}
		if def.Variant != "" {
			profile.Variant = strings.TrimSpace(def.Variant)
			if profile.Model != "" {
				profile.Model = WithVariant(profile.Model, profile.Variant)
			}
		}
		if def.System != "" {
			profile.System = def.System
		}
		if def.Steps > 0 {
			profile.Steps = def.Steps
		}
		if len(def.Options) > 0 {
			if profile.Options == nil {
				profile.Options = map[string]any{}
			}
			for key, value := range def.Options {
				profile.Options[key] = value
			}
		}
		if len(def.Headers) > 0 {
			if profile.Headers == nil {
				profile.Headers = map[string]string{}
			}
			for key, value := range def.Headers {
				profile.Headers[key] = value
			}
		}
		if def.Request != nil {
			if len(def.Request.Headers) > 0 {
				if profile.Headers == nil {
					profile.Headers = map[string]string{}
				}
				for key, value := range def.Request.Headers {
					profile.Headers[key] = value
				}
			}
			if len(def.Request.Settings) > 0 || len(def.Request.Body) > 0 {
				if profile.Options == nil {
					profile.Options = map[string]any{}
				}
				for key, value := range def.Request.Settings {
					profile.Options[key] = value
				}
				for key, value := range def.Request.Body {
					profile.Options[key] = value
				}
			}
		}
		if def.Color != "" {
			profile.Color = strings.TrimSpace(def.Color)
		}
		// A partial override keeps the built-in value unless the entry actually
		// mentions hidden; a literal true always applies.
		if def.Hidden || def.HiddenSet {
			profile.Hidden = def.Hidden
		}
		// Explicit permissions come after the built-in rules, and the last
		// matching rule wins.
		profile.Permissions = append(profile.Permissions, def.Permissions...)
		profiles[id] = profile
	}

	// Plan mode may write only inside its own plan directory, matching the
	// reference harness. The directory is created on the first write.
	if plan, ok := profiles["plan"]; ok {
		planDir := PlanDirectory()
		plan.System = PlanPrompt()
		plan.Permissions = append(plan.Permissions,
			permission.Rule{Action: "edit", Resource: filepath.Join(planDir, "*"), Effect: permission.Allow},
			permission.Rule{Action: "external_directory", Resource: filepath.Join(planDir, "*"), Effect: permission.Allow},
		)
		profiles["plan"] = plan
	}

	registry := &Registry{profiles: profiles}
	registry.defaultID = registry.chooseDefault(defaultAgent)
	return registry
}

func (r *Registry) chooseDefault(requested string) string {
	if requested != "" {
		if profile, ok := r.profiles[requested]; ok && profile.Mode != ModeSubagent && !profile.Hidden {
			return requested
		}
	}
	if profile, ok := r.profiles["build"]; ok && profile.Mode != ModeSubagent && !profile.Hidden {
		return "build"
	}
	for _, id := range r.ids() {
		profile := r.profiles[id]
		if profile.Mode != ModeSubagent && !profile.Hidden {
			return id
		}
	}
	return "build"
}

// Default returns the default primary profile.
func (r *Registry) Default() Profile {
	if r == nil {
		return Profile{ID: "build", Name: "Build", Mode: ModePrimary}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	profile, ok := r.profiles[r.defaultID]
	if !ok {
		return Profile{ID: "build", Name: "Build", Mode: ModePrimary}
	}
	return profile
}

// Get returns one profile by id.
func (r *Registry) Get(id string) (Profile, bool) {
	if r == nil {
		return Profile{}, false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	profile, ok := r.profiles[strings.TrimSpace(id)]
	return profile, ok
}

// Resolve returns the named profile, or the default when the name is empty or
// unknown.
func (r *Registry) Resolve(id string) Profile {
	if r == nil {
		return Profile{ID: "build", Name: "Build", Mode: ModePrimary}
	}
	if profile, ok := r.Get(id); ok {
		return profile
	}
	return r.Default()
}

// Primary returns selectable primary profiles, default first.
func (r *Registry) Primary() []Profile {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Profile
	defaultID := r.defaultID
	for _, id := range r.idsLocked() {
		profile := r.profiles[id]
		if profile.Hidden || profile.Mode == ModeSubagent {
			continue
		}
		out = append(out, profile)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ID == defaultID {
			return true
		}
		if out[j].ID == defaultID {
			return false
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Subagents returns profiles that may be used by the subagent tool, sorted by
// id so the tool description is stable across reloads.
func (r *Registry) Subagents() []Profile {
	if r == nil {
		return nil
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []Profile
	for _, id := range r.idsLocked() {
		profile := r.profiles[id]
		if profile.Hidden || profile.Mode == ModePrimary {
			continue
		}
		out = append(out, profile)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (r *Registry) ids() []string {
	return r.idsLocked()
}

func (r *Registry) idsLocked() []string {
	ids := make([]string, 0, len(r.profiles))
	for id := range r.profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// Reload replaces the registry contents with a freshly resolved set. The
// registry pointer stays stable so Agent, Server and subagent callers all see
// the new profiles without being rewired.
func (r *Registry) Reload(cfg Config, discovered map[string]Definition, defaultAgent string) {
	if r == nil {
		return
	}
	next := New(cfg.Overlay(discovered), defaultAgent)
	next.mu.RLock()
	profiles, defaultID := next.profiles, next.defaultID
	next.mu.RUnlock()
	r.mu.Lock()
	r.profiles, r.defaultID = profiles, defaultID
	r.mu.Unlock()
}

// NormalizeMode maps user input to one of the three supported modes.
func NormalizeMode(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case ModePrimary:
		return ModePrimary
	case ModeSubagent:
		return ModeSubagent
	}
	return ModeAll
}

// AllowsTool reports whether an agent may use a tool. Availability has one
// source of truth: permission rules, where write and patch share edit.
func (p Profile) AllowsTool(name string) bool {
	action := name
	if action == "write" || action == "patch" {
		action = "edit"
	}
	return !p.Permissions.WhollyDenies(action)
}

// NormalizeModelRef accepts the reference's "provider/model#variant" form and
// the catalog's "provider/model::variant" alias, returning the catalog form.
func NormalizeModelRef(value string) string {
	value = strings.TrimSpace(value)
	if base, variant, ok := strings.Cut(value, "#"); ok {
		value = strings.TrimSpace(base) + "::" + strings.TrimSpace(variant)
	}
	return value
}

// WithVariant appends a variant to a provider/model reference, replacing any
// variant already present.
func WithVariant(model, variant string) string {
	base := strings.TrimSpace(model)
	if index := strings.Index(base, "::"); index > 0 {
		base = base[:index]
	}
	variant = strings.TrimSpace(variant)
	if base == "" || variant == "" {
		return base
	}
	return base + "::" + variant
}

// ValidateModel splits a provider/model reference. The model may carry a
// "::variant" alias; callers pass it through to the catalog.
func ValidateModel(value string) (providerID, model string, err error) {
	value = NormalizeModelRef(value)
	providerID, model, ok := strings.Cut(value, "/")
	if !ok || strings.TrimSpace(providerID) == "" || strings.TrimSpace(model) == "" {
		return "", "", fmt.Errorf("model must be provider/model")
	}
	return strings.TrimSpace(providerID), strings.TrimSpace(model), nil
}

func displayName(id string) string {
	parts := strings.FieldsFunc(id, func(r rune) bool { return r == '-' || r == '_' })
	for i, part := range parts {
		if part == "" {
			continue
		}
		// Uppercase the first rune, not the first byte: a non-ASCII id would
		// otherwise be split inside a multi-byte character.
		runes := []rune(part)
		parts[i] = strings.ToUpper(string(runes[0])) + string(runes[1:])
	}
	if len(parts) == 0 {
		return id
	}
	return strings.Join(parts, " ")
}
