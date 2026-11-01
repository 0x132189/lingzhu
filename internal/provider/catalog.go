package provider

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

// ModelSpec is one model in a provider catalog.
type ModelSpec struct {
	ID            string
	Name          string
	ContextWindow int
}

// ProviderSpec is one OpenAI-compatible endpoint and its models.
type ProviderSpec struct {
	ID      string
	Name    string
	BaseURL string
	APIKey  string
	Models  []ModelSpec
}

// Selection names a provider and model; empty fields fall back to the default.
type Selection struct {
	Provider string
	Model    string
}

// Target is a resolved provider, model and context window.
type Target struct {
	ProviderID   string
	ProviderName string
	Model        string
	Window       int
	Provider     Provider
}

// ModelInfo is the catalog view of one model.
type ModelInfo struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int    `json:"contextWindow"`
}

// ProviderInfo is the catalog view of one provider; it never carries a key.
type ProviderInfo struct {
	ID      string      `json:"id"`
	Name    string      `json:"name"`
	BaseURL string      `json:"baseUrl"`
	Ready   bool        `json:"ready"`
	Models  []ModelInfo `json:"models"`
}

type catalogEntry struct {
	spec       ProviderSpec
	provider   Provider
	models     map[string]ModelSpec
	modelOrder []string
}

// Catalog resolves provider/model selections to a provider implementation.
type Catalog struct {
	entries       map[string]*catalogEntry
	order         []string
	defaultTarget Target
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
		entry := &catalogEntry{spec: spec, models: map[string]ModelSpec{}}
		for _, model := range spec.Models {
			model.ID = strings.TrimSpace(model.ID)
			if model.ID == "" {
				return nil, fmt.Errorf("provider %s: model id is required", spec.ID)
			}
			if _, exists := entry.models[model.ID]; exists {
				return nil, fmt.Errorf("provider %s: duplicate model %q", spec.ID, model.ID)
			}
			if model.Name = strings.TrimSpace(model.Name); model.Name == "" {
				model.Name = model.ID
			}
			entry.models[model.ID] = model
			entry.modelOrder = append(entry.modelOrder, model.ID)
		}
		entry.provider = NewOpenAI(spec.BaseURL, spec.APIKey)
		catalog.entries[spec.ID] = entry
		catalog.order = append(catalog.order, spec.ID)
	}
	if len(catalog.order) == 0 {
		return nil, errors.New("no providers configured")
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
	if defaultModel != "" {
		if _, ok := entry.models[defaultModel]; !ok {
			return nil, fmt.Errorf("provider %s has no model %q", defaultProvider, defaultModel)
		}
	}
	window := 0
	if model, ok := entry.models[defaultModel]; ok {
		window = model.ContextWindow
	}
	catalog.defaultTarget = Target{
		ProviderID:   entry.spec.ID,
		ProviderName: entry.spec.Name,
		Model:        defaultModel,
		Window:       window,
		Provider:     entry.provider,
	}
	return catalog, nil
}

// Static builds a catalog around an existing provider implementation; tests
// use it to avoid constructing a real HTTP client.
func Static(id, name string, p Provider, models []ModelSpec, defaultModel string) *Catalog {
	entry := &catalogEntry{models: map[string]ModelSpec{}, provider: p}
	entry.spec = ProviderSpec{ID: id, Name: name, APIKey: "static", Models: models}
	catalog := &Catalog{entries: map[string]*catalogEntry{id: entry}, order: []string{id}}
	for _, model := range models {
		entry.models[model.ID] = model
		entry.modelOrder = append(entry.modelOrder, model.ID)
	}
	window := 0
	if model, ok := entry.models[defaultModel]; ok {
		window = model.ContextWindow
	}
	catalog.defaultTarget = Target{
		ProviderID:   id,
		ProviderName: name,
		Model:        defaultModel,
		Window:       window,
		Provider:     p,
	}
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
			if _, ok := entry.models[model]; !ok {
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

// Resolve turns a selection into a target. A provider must have an API key so a
// call can succeed; the returned error is shown instead of failing mid-stream.
func (c *Catalog) Resolve(selection Selection) (Target, error) {
	providerID := strings.TrimSpace(selection.Provider)
	if providerID == "" {
		providerID = c.defaultTarget.ProviderID
	}
	entry, ok := c.entries[providerID]
	if !ok {
		return Target{}, fmt.Errorf("unknown provider %q", providerID)
	}
	modelID := strings.TrimSpace(selection.Model)
	if modelID == "" && len(entry.modelOrder) > 0 {
		modelID = entry.modelOrder[0]
	}
	window := 0
	if modelID != "" {
		model, ok := entry.models[modelID]
		if !ok {
			return Target{}, fmt.Errorf("provider %s has no model %q", providerID, modelID)
		}
		window = model.ContextWindow
	}
	if entry.spec.APIKey == "" {
		return Target{}, fmt.Errorf("provider %s has no API key", providerID)
	}
	return Target{
		ProviderID:   entry.spec.ID,
		ProviderName: entry.spec.Name,
		Model:        modelID,
		Window:       window,
		Provider:     entry.provider,
	}, nil
}

// Providers returns the catalog for clients, in configuration order.
func (c *Catalog) Providers() []ProviderInfo {
	out := make([]ProviderInfo, 0, len(c.order))
	for _, id := range c.order {
		entry := c.entries[id]
		info := ProviderInfo{
			ID:      entry.spec.ID,
			Name:    entry.spec.Name,
			BaseURL: entry.spec.BaseURL,
			Ready:   entry.spec.APIKey != "",
		}
		for _, modelID := range entry.modelOrder {
			model := entry.models[modelID]
			info.Models = append(info.Models, ModelInfo{
				ID:            model.ID,
				Name:          model.Name,
				ContextWindow: model.ContextWindow,
			})
		}
		out = append(out, info)
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
