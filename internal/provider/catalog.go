package provider

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"lingzhu/internal/policy"
)

// ModelSpec is one model in a provider catalog.
type ModelSpec struct {
	ID            string
	Name          string
	ContextWindow int
	// Options are request-body fields merged into every call of this model.
	Options map[string]any
	// Variants are named option overrides; the catalog exposes each one as
	// a selectable alias of the same wire model.
	Variants map[string]map[string]any
	// WireModel is the id sent upstream when ID is a variant alias. Empty
	// means ID itself.
	WireModel string
	// NoTools marks a model the catalog says takes no tool definitions.
	NoTools bool
	// Reasoning and Attachment are catalog capabilities, kept for clients.
	Reasoning  bool
	Attachment bool
	CacheTTL   time.Duration
	MaxOutput  int
	// NativeCompaction reports that the provider compacts its own window.
	NativeCompaction bool
	// CompactionTrigger is the token count at which the provider compacts.
	CompactionTrigger int
	// Type is "chat" or "image"; empty means chat.
	Type string
	// API overrides the provider's wire protocol for this model.
	API string
	// Family groups versions of one model for the catalog tool.
	Family string
	// Released is the release date as Unix milliseconds; zero means unknown.
	Released int64
}

// ProviderSpec is one endpoint and its models. API selects the wire protocol:
// "chat" (default), "responses" or "messages".
type ProviderSpec struct {
	ID      string
	Name    string
	BaseURL string
	APIKey  string
	// API is the provider-wide wire protocol; a model can override it.
	API string
	// OAuth marks a provider whose bearer token comes from a token source
	// installed after the catalog is built.
	OAuth  bool
	Models []ModelSpec
	// Blacklist hides model ids; a non-empty whitelist keeps only the ids
	// it names. Both apply before variants expand.
	Blacklist []string
	Whitelist []string
}

// VariantID is the selectable id of one model variant. The separator is not
// valid in a normal upstream id, so the base model stays unambiguous.
func VariantID(model, variant string) string { return model + "::" + variant }

// Selection names a provider and model; empty fields fall back to the default.
type Selection struct {
	Provider string
	Model    string
}

// Target is a resolved provider, model and context window.
type Target struct {
	ProviderID   string
	ProviderName string
	// Model is the catalog id, which may name a variant.
	Model string
	// ModelName is the catalog display name, used by model-specific prompts.
	ModelName string
	// RequestModel is the upstream model id sent to the provider.
	RequestModel string
	// Options are merged into the request body.
	Options map[string]any
	// NoTools omits tool definitions for a model that rejects them.
	NoTools bool
	// Attachment reports whether the model accepts image/PDF input.
	Attachment bool
	// CacheTTL is the provider prompt-cache lifetime; zero disables auto
	// warming intervals.
	CacheTTL time.Duration
	// MaxOutput is the catalog's reply limit; zero means unknown.
	MaxOutput int
	Window    int
	Provider  Provider
	// Headers are per-agent request headers.
	Headers map[string]string
	// NativeCompaction reports that the provider compacts its own window, so
	// the agent asks for a provider checkpoint instead of a summary.
	NativeCompaction bool
	// CompactionTrigger is the token count at which the provider compacts.
	CompactionTrigger int
}

// ModelInfo is the catalog view of one model.
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int    `json:"contextWindow"`
	Type          string `json:"type,omitempty"`
	Family        string `json:"family,omitempty"`
	Released      int64  `json:"released,omitempty"`
}

// ProviderInfo is the catalog view of one provider; it never carries a key.
type ProviderInfo struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	BaseURL string      `json:"baseUrl"`
	Ready   bool        `json:"ready"`
	Models  []ModelInfo `json:"models"`
}

// SetTokenSource installs a dynamic bearer-token supplier for one provider.
// It returns false when the provider is unknown.
func (c *Catalog) SetTokenSource(id string, source func(ctx context.Context) (string, error)) bool {
	entry, ok := c.entries[id]
	if !ok {
		return false
	}
	applied := false
	for _, provider := range entry.providers {
		switch item := provider.(type) {
		case *OpenAI:
			item.TokenSource = source
			applied = true
		case *OpenAIResponses:
			item.TokenSource = source
			applied = true
		case *Anthropic:
			item.TokenSource = source
			applied = true
		}
	}
	return applied
}

type catalogEntry struct {
	spec     ProviderSpec
	provider Provider
	// providers caches one adapter per wire protocol.
	providers  map[string]Provider
	models     map[string]ModelSpec
	modelOrder []string
}

// adapter returns the Provider for a wire protocol, building it lazily.
func (e *catalogEntry) adapter(api string) Provider {
	api = normalizeAPI(api)
	if e.providers == nil {
		e.providers = map[string]Provider{}
	}
	if existing, ok := e.providers[api]; ok {
		return existing
	}
	provider := newProvider(api, e.spec.BaseURL, e.spec.APIKey)
	e.providers[api] = provider
	return provider
}

func normalizeAPI(api string) string {
	switch strings.ToLower(strings.TrimSpace(api)) {
	case "responses", "openai-responses":
		return "responses"
	case "messages", "anthropic", "anthropic-messages":
		return "messages"
	default:
		return "chat"
	}
}

// validAPI reports whether a configured API name is supported. An empty value
// keeps the default chat/completions transport.
func validAPI(api string) bool {
	switch strings.ToLower(strings.TrimSpace(api)) {
	case "", "chat", "chat-completions", "openai", "responses", "openai-responses",
		"messages", "anthropic", "anthropic-messages":
		return true
	default:
		return false
	}
}

func newProvider(api, baseURL, apiKey string) Provider {
	switch normalizeAPI(api) {
	case "responses":
		return NewOpenAIResponses(baseURL, apiKey)
	case "messages":
		return NewAnthropic(baseURL, apiKey)
	default:
		return NewOpenAI(baseURL, apiKey)
	}
}

// Catalog resolves provider/model selections to a provider implementation.
type Catalog struct {
	entries       map[string]*catalogEntry
	order         []string
	defaultTarget Target
	policies      policy.Rules
}

// NewCatalog validates the provider specs and resolves the default selection.
func NewCatalog(specs []ProviderSpec, defaultProvider, defaultModel string) (*Catalog, error) {
	catalog := &Catalog{entries: map[string]*catalogEntry{}}
	for _, spec := range specs {
		spec.ID = strings.TrimSpace(spec.ID)
		if spec.ID == "" {
			return nil, fmt.Errorf("provider id is required")
		}
		if _, exists := catalog.entries[spec.ID]; exists {
			return nil, fmt.Errorf("duplicate provider %q", spec.ID)
		}
		if spec.Name = strings.TrimSpace(spec.Name); spec.Name == "" {
			spec.Name = spec.ID
		}
		spec.BaseURL = strings.TrimRight(strings.TrimSpace(spec.BaseURL), "/")
		if err := validBaseURL(spec.BaseURL); err != nil {
			return nil, fmt.Errorf("provider %s: %w", spec.ID, err)
		}
		if !validAPI(spec.API) {
			return nil, fmt.Errorf("provider %s: unsupported api %q", spec.ID, spec.API)
		}
		entry := &catalogEntry{spec: spec, models: map[string]ModelSpec{}}
		for _, model := range filterModels(spec.Models, spec.Blacklist, spec.Whitelist) {
			model.ID = strings.TrimSpace(model.ID)
			if model.ID == "" {
				return nil, fmt.Errorf("provider %s: model id is required", spec.ID)
			}
			if !validAPI(model.API) {
				return nil, fmt.Errorf("provider %s: model %s: unsupported api %q", spec.ID, model.ID, model.API)
			}
			if model.Name = strings.TrimSpace(model.Name); model.Name == "" {
				model.Name = model.ID
			}
			for _, selectable := range expandModel(model) {
				if _, exists := entry.models[selectable.ID]; exists {
					return nil, fmt.Errorf("provider %s: duplicate model %q", spec.ID, selectable.ID)
				}
				entry.models[selectable.ID] = selectable
				entry.modelOrder = append(entry.modelOrder, selectable.ID)
			}
		}
		entry.provider = entry.adapter(spec.API)
		catalog.entries[spec.ID] = entry
		catalog.order = append(catalog.order, spec.ID)
	}
	if len(catalog.order) == 0 {
		// An explicit empty providers block leaves a core running with no
		// model; a prompt fails with a clear error instead of a broken start.
		return catalog, nil
	}

	defaultProvider = strings.TrimSpace(defaultProvider)
	if defaultProvider == "" {
		defaultProvider = catalog.guessProvider(defaultModel)
	}
	entry, ok := catalog.entries[defaultProvider]
	if !ok {
		return nil, fmt.Errorf("unknown provider %q", defaultProvider)
	}
	defaultModel = strings.TrimSpace(defaultModel)
	if defaultModel == "" && len(entry.modelOrder) > 0 {
		defaultModel = entry.modelOrder[0]
	}
	if defaultModel != "" && !entry.hasModel(defaultModel) {
		return nil, fmt.Errorf("provider %s has no model %q", defaultProvider, defaultModel)
	}
	catalog.defaultTarget = entry.targetFor(defaultModel)
	return catalog, nil
}

// variantAlias splits a selectable id into its base model and variant name.
func variantAlias(id string) (base, variant string, ok bool) {
	base, variant, ok = strings.Cut(id, "::")
	base = strings.TrimSpace(base)
	variant = strings.TrimSpace(variant)
	return base, variant, ok && base != "" && variant != ""
}

// hasModel reports whether an id names a base model or a reasoning alias that
// the catalog can derive on the fly.
func (e *catalogEntry) hasModel(id string) bool {
	if _, ok := e.models[id]; ok {
		return true
	}
	base, _, ok := variantAlias(id)
	if !ok {
		return false
	}
	_, exists := e.models[base]
	return exists
}

// targetFor resolves a selectable id. An alias the config did not expand keeps
// the base request model and derives its reasoning options, so a client can
// advertise levels from the provider catalog without rewriting the config.
func (e *catalogEntry) targetFor(id string) Target {
	if _, ok := e.models[id]; ok {
		return e.target(id)
	}
	base, variant, ok := variantAlias(id)
	if !ok {
		return e.target(id)
	}
	target := e.target(base)
	target.Model = VariantID(base, variant)
	target.Options = mergeOptions(e.models[base].Options, reasoningVariantOptions(variant))
	return target
}

// reasoningVariantOptions maps a derived alias onto OpenAI-compatible fields.
// Toggle aliases use the shared thinking flag; every other name is an effort.
func reasoningVariantOptions(variant string) map[string]any {
	switch strings.ToLower(variant) {
	case "thinking":
		return map[string]any{"thinking": map[string]any{"type": "enabled"}}
	case "off":
		return map[string]any{"thinking": map[string]any{"type": "disabled"}}
	default:
		return map[string]any{"reasoning_effort": strings.ToLower(variant)}
	}
}

// target builds the resolved view of one model. An unknown id keeps the id as
// the wire model with no window, which is how a legacy single-model config
// behaves when the catalog did not describe it.
func (e *catalogEntry) target(modelID string) Target {
	window := 0
	maxOutput := 0
	modelName := ""
	requestModel := modelID
	var options map[string]any
	noTools := false
	attachment := false
	cacheTTL := time.Duration(0)
	nativeCompaction := false
	compactionTrigger := 0
	if model, ok := e.models[modelID]; ok {
		window = model.ContextWindow
		maxOutput = model.MaxOutput
		modelName = model.Name
		requestModel = model.WireModel
		options = model.Options
		noTools = model.NoTools
		attachment = model.Attachment
		cacheTTL = model.CacheTTL
		nativeCompaction = model.NativeCompaction
		compactionTrigger = model.CompactionTrigger
	}
	provider := e.adapter(e.spec.API)
	if model, ok := e.models[modelID]; ok && model.API != "" {
		provider = e.adapter(model.API)
	}
	return Target{
		ProviderID:   e.spec.ID,
		ProviderName: e.spec.Name,
		Model:        modelID,
		ModelName:    modelName,
		RequestModel: requestModel,
		Options:      options,
		NoTools:      noTools,
		Attachment:   attachment,
		CacheTTL:     cacheTTL,
		MaxOutput:    maxOutput,
		Window:       window,
		Provider:     provider,
		// Native compaction is a provider-side window replacement.
		NativeCompaction:  nativeCompaction,
		CompactionTrigger: compactionTrigger,
	}
}

// Static builds a catalog around an existing provider implementation; tests
// use it to avoid constructing a real HTTP client.
func Static(id, name string, p Provider, models []ModelSpec, defaultModel string) *Catalog {
	entry := &catalogEntry{models: map[string]ModelSpec{}, provider: p, providers: map[string]Provider{"chat": p}}
	entry.spec = ProviderSpec{ID: id, Name: name, APIKey: "static", Models: models}
	catalog := &Catalog{entries: map[string]*catalogEntry{id: entry}, order: []string{id}}
	for _, model := range models {
		for _, selectable := range expandModel(model) {
			entry.models[selectable.ID] = selectable
			entry.modelOrder = append(entry.modelOrder, selectable.ID)
		}
	}
	catalog.defaultTarget = entry.target(defaultModel)
	return catalog
}

func (c *Catalog) guessProvider(model string) string {
	model = strings.TrimSpace(model)
	if _, ok := c.entries["default"]; ok {
		return "default"
	}
	if len(c.order) == 1 {
		return c.order[0]
	}
	if model != "" {
		found := ""
		for id, entry := range c.entries {
			if !entry.hasModel(model) {
				continue
			}
			if found != "" {
				return c.order[0]
			}
			found = id
		}
		if found != "" {
			return found
		}
	}
	return c.order[0]
}

// Default returns the default provider/model target.
func (c *Catalog) Default() Target { return c.defaultTarget }

// SetPolicies installs hard provider.use policy statements.
func (c *Catalog) SetPolicies(rules policy.Rules) {
	c.policies = append(policy.Rules(nil), rules...)
}

// Resolve turns a selection into a target. A provider must have an API key so a
// call can succeed; the returned error is shown instead of failing mid-stream.
func (c *Catalog) Resolve(selection Selection) (Target, error) {
	if len(c.order) == 0 {
		return Target{}, errors.New("no providers configured")
	}
	providerID := strings.TrimSpace(selection.Provider)
	if providerID == "" {
		providerID = c.defaultTarget.ProviderID
	}
	entry, ok := c.entries[providerID]
	if !ok {
		return Target{}, fmt.Errorf("unknown provider %q", providerID)
	}
	if c.policies.Denied("provider.use", providerID) {
		return Target{}, fmt.Errorf("provider %q is blocked by policy", providerID)
	}
	modelID := strings.TrimSpace(selection.Model)
	if modelID == "" && len(entry.modelOrder) > 0 {
		modelID = entry.modelOrder[0]
	}
	if modelID != "" && !entry.hasModel(modelID) {
		return Target{}, fmt.Errorf("provider %s has no model %q", providerID, modelID)
	}
	if entry.spec.APIKey == "" && !entry.spec.OAuth {
		return Target{}, fmt.Errorf("provider %s has no API key", providerID)
	}
	return entry.targetFor(modelID), nil
}

// smallFamilies is the reference preference order for title models.
var smallFamilies = []string{"gpt-luna", "gemini-flash-lite", "gemini-flash", "claude-haiku"}

// Small returns the provider's small text model, or false when the catalog has
// none of the reference families.
func (c *Catalog) Small(providerID string) (Target, bool) {
	entry, ok := c.entries[providerID]
	if !ok {
		return Target{}, false
	}
	for _, family := range smallFamilies {
		for _, id := range entry.modelOrder {
			spec, ok := entry.models[id]
			if ok && spec.Family == family {
				return entry.target(id), true
			}
		}
	}
	return Target{}, false
}

// LowerReasoning resolves the same model at its least reasoning variant when
// the catalog offers one. Auxiliary calls such as titles use it so they do not
// pay for reasoning the task does not need.
func (c *Catalog) LowerReasoning(selection Selection) (Target, bool) {
	target, err := c.Resolve(selection)
	if err != nil {
		return Target{}, false
	}
	base := target.Model
	if index := strings.Index(base, "::"); index > 0 {
		base = base[:index]
	}
	entry, ok := c.entries[target.ProviderID]
	if !ok {
		return Target{}, false
	}
	spec, ok := entry.models[base]
	if !ok {
		return Target{}, false
	}
	for _, name := range []string{"none", "minimal", "low"} {
		if _, ok := spec.Variants[name]; !ok {
			continue
		}
		lowered, err := c.Resolve(Selection{Provider: target.ProviderID, Model: VariantID(base, name)})
		if err == nil {
			return lowered, true
		}
	}
	return Target{}, false
}

// Providers returns the catalog for clients, in configuration order.
func (c *Catalog) Providers() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(c.order))
	for _, id := range c.order {
		if c.policies.Denied("provider.use", id) {
			continue
		}
		entry := c.entries[id]
		info := ProviderInfo{
			ID:      entry.spec.ID,
			Name:    entry.spec.Name,
			BaseURL: entry.spec.BaseURL,
			Ready:   entry.spec.APIKey != "" || entry.spec.OAuth,
		}
		for _, modelID := range entry.modelOrder {
			model := entry.models[modelID]
			info.Models = append(info.Models, ModelInfo{
				ID:            model.ID,
				Name:          model.Name,
				ContextWindow: model.ContextWindow,
				Type:          model.Type,
				Family:        model.Family,
				Released:      model.Released,
			})
		}
		out = append(out, info)
	}
	return out
}

// filterModels applies a provider's blacklist and whitelist to base model ids
// before variants expand.
func filterModels(models []ModelSpec, blacklist, whitelist []string) []ModelSpec {
	block := make(map[string]bool, len(blacklist))
	for _, id := range blacklist {
		if id = strings.TrimSpace(id); id != "" {
			block[id] = true
		}
	}
	allow := make(map[string]bool, len(whitelist))
	for _, id := range whitelist {
		if id = strings.TrimSpace(id); id != "" {
			allow[id] = true
		}
	}
	out := make([]ModelSpec, 0, len(models))
	for _, model := range models {
		id := strings.TrimSpace(model.ID)
		if block[id] {
			continue
		}
		if len(allow) > 0 && !allow[id] {
			continue
		}
		out = append(out, model)
	}
	return out
}

// expandModel adds one selectable entry per variant, each carrying the base
// options overridden by that variant's. The base model keeps its own id.
func expandModel(model ModelSpec) []ModelSpec {
	if model.WireModel == "" {
		model.WireModel = model.ID
	}
	out := []ModelSpec{model}
	if len(model.Variants) == 0 {
		return out
	}
	names := make([]string, 0, len(model.Variants))
	for name := range model.Variants {
		if strings.TrimSpace(name) != "" {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		variant := model
		variant.ID = VariantID(model.ID, name)
		variant.Name = model.Name + " 璺?" + name
		variant.Options = mergeOptions(model.Options, model.Variants[name])
		variant.Variants = nil
		out = append(out, variant)
	}
	return out
}

func mergeOptions(base, over map[string]any) map[string]any {
	if len(base) == 0 && len(over) == 0 {
		return nil
	}
	out := make(map[string]any, len(base)+len(over))
	for key, value := range base {
		out[key] = value
	}
	for key, value := range over {
		out[key] = value
	}
	return out
}

func validBaseURL(raw string) error {
	if raw == "" {
		return errors.New("base URL is required")
	}
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return errors.New("base URL must be an absolute http(s) URL")
	}
	return nil
}
