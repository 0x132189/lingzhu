package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"lingzhu/internal/config"
	"lingzhu/internal/policy"
	"lingzhu/internal/provider"
	"lingzhu/internal/warming"
)

// SetConfig attaches the effective configuration for the read and patch
// endpoints. The catalog keeps serving its startup snapshot until restart.
func (s *Server) SetConfig(cfg config.Config) {
	s.configMu.Lock()
	s.config = cfg
	s.configMu.Unlock()
}

// ModelView is one selectable model across providers.
type ModelView struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ProviderID    string `json:"providerId"`
	ContextWindow int    `json:"contextWindow"`
	Default       bool   `json:"default"`
}

// ConfigView is the sanitized configuration returned to clients.
type ConfigView struct {
	Path          string             `json:"path,omitempty"`
	Workdir       string             `json:"workdir,omitempty"`
	DataDir       string             `json:"dataDir,omitempty"`
	Provider      string             `json:"provider,omitempty"`
	Model         string             `json:"model,omitempty"`
	DefaultAgent  string             `json:"defaultAgent,omitempty"`
	MaxSteps      int                `json:"maxSteps"`
	Shell         []string           `json:"shell,omitempty"`
	Permission    []config.Rule      `json:"permission,omitempty"`
	Policies      []policy.Statement `json:"policies,omitempty"`
	Warming       warming.Config     `json:"warming"`
	ContextWindow int                `json:"contextWindow,omitempty"`
	CompactKeep   int                `json:"compactKeep,omitempty"`
}

type configPatch struct {
	Provider     *string        `json:"provider"`
	Model        *string        `json:"model"`
	DefaultAgent *string        `json:"default_agent"`
	MaxSteps     *int           `json:"max_steps"`
	Shell        *[]string      `json:"shell"`
	Permission   *[]config.Rule `json:"permission"`
}

func (s *Server) listProviders(w http.ResponseWriter, r *http.Request) {
	items := []provider.ProviderInfo{}
	if loop := s.defaultAgentFor(r); loop != nil {
		items = loop.Providers()
	}
	if items == nil {
		items = []provider.ProviderInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"providers": items})
}

func (s *Server) getProvider(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.PathValue("id"))
	if loop := s.defaultAgentFor(r); loop != nil {
		for _, item := range loop.Providers() {
			if item.ID == id {
				writeJSON(w, http.StatusOK, map[string]any{"provider": item})
				return
			}
		}
	}
	writeError(w, http.StatusNotFound, errText("provider not found"))
}

func (s *Server) listModels(w http.ResponseWriter, r *http.Request) {
	views := s.modelViews(r)
	writeJSON(w, http.StatusOK, map[string]any{"models": views})
}

func (s *Server) defaultModel(w http.ResponseWriter, r *http.Request) {
	var found *ModelView
	for _, item := range s.modelViews(r) {
		if item.Default {
			copy := item
			found = &copy
			break
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"model": found})
}

func (s *Server) modelViews(r *http.Request) []ModelView {
	views := []ModelView{}
	loop := s.defaultAgentFor(r)
	if loop == nil {
		return views
	}
	target := loop.DefaultTarget()
	for _, item := range loop.Providers() {
		for _, model := range item.Models {
			views = append(views, ModelView{
				ID:            model.ID,
				Name:          model.Name,
				ProviderID:    item.ID,
				ContextWindow: model.ContextWindow,
				Default:       item.ID == target.ProviderID && model.ID == target.Model,
			})
		}
	}
	return views
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.configFor(r)
	view := ConfigView{
		Path:          cfg.Path,
		Workdir:       cfg.Workdir,
		DataDir:       cfg.DataDir,
		Provider:      cfg.Provider,
		Model:         cfg.Model,
		DefaultAgent:  cfg.DefaultAgent,
		MaxSteps:      cfg.MaxSteps,
		Shell:         cfg.Shell,
		Permission:    cfg.Permission,
		Policies:      cfg.Policies,
		Warming:       cfg.Warming,
		ContextWindow: cfg.ContextWindow,
		CompactKeep:   cfg.CompactKeep,
	}
	writeJSON(w, http.StatusOK, map[string]any{"config": view})
}

func (s *Server) patchConfig(w http.ResponseWriter, r *http.Request) {
	var patch configPatch
	if err := json.NewDecoder(r.Body).Decode(&patch); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	fields := map[string]any{}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	path := s.config.Path
	if strings.TrimSpace(path) == "" {
		path = config.DefaultPath()
	}
	if patch.Provider != nil {
		value := strings.TrimSpace(*patch.Provider)
		fields["provider"] = value
		s.config.Provider = value
	}
	if patch.Model != nil {
		value := strings.TrimSpace(*patch.Model)
		fields["model"] = value
		s.config.Model = value
	}
	if patch.DefaultAgent != nil {
		value := strings.TrimSpace(*patch.DefaultAgent)
		fields["default_agent"] = value
		s.config.DefaultAgent = value
	}
	if patch.MaxSteps != nil {
		if *patch.MaxSteps < 0 {
			writeError(w, http.StatusBadRequest, errText("max_steps must not be negative"))
			return
		}
		fields["max_steps"] = *patch.MaxSteps
		s.config.MaxSteps = *patch.MaxSteps
	}
	if patch.Shell != nil {
		fields["shell"] = *patch.Shell
		s.config.Shell = *patch.Shell
	}
	if patch.Permission != nil {
		fields["permission"] = *patch.Permission
		s.config.Permission = *patch.Permission
	}
	if len(fields) == 0 {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if err := config.Update(path, fields); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.config.Path = path
	w.WriteHeader(http.StatusNoContent)
}
