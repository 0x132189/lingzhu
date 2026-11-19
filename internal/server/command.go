package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"lingzhu/internal/agent"
	"lingzhu/internal/agents"
	"lingzhu/internal/commands"
	"lingzhu/internal/plugin"
	"lingzhu/internal/provider"
)

func (s *Server) runCommand(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !s.requireSession(w, r, id) {
		return
	}
	var body struct {
		Name      string `json:"name"`
		Text      string `json:"text"`
		Arguments string `json:"arguments"`
		Provider  string `json:"provider"`
		Model     string `json:"model"`
		Agent     string `json:"agent"`
		Delivery  string `json:"delivery"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimPrefix(strings.TrimSpace(body.Name), "/")
	if name == "" {
		writeError(w, http.StatusBadRequest, errText("command name is empty"))
		return
	}
	arguments := strings.TrimSpace(body.Arguments)
	extra := strings.TrimSpace(body.Text)
	if arguments == "" {
		arguments = extra
		extra = ""
	}
	_, hasPlugin := s.commandIn(r, name)
	custom, hasCustom := s.customCommandIn(r, name)
	hasMCP := false
	mcpPrompt := ""
	if !hasPlugin && !hasCustom {
		if manager := s.mcpFor(r); manager != nil {
			rendered, ok, err := manager.ExpandPrompt(r.Context(), name, arguments)
			if err != nil {
				writeError(w, http.StatusBadGateway, err)
				return
			}
			if ok {
				hasMCP = true
				mcpPrompt = rendered
			}
		}
	}
	if !hasPlugin && !hasCustom && !hasMCP {
		writeError(w, http.StatusNotFound, errText("command not found"))
		return
	}
	if hasPlugin {
		hasCustom = false
	}
	text := mcpPrompt
	if !hasMCP {
		text = s.expandIn(r, "/"+name+" "+arguments)
	}
	if extra != "" {
		text = strings.TrimSpace(text) + "\n\n" + extra
	}
	if strings.TrimSpace(text) == "" {
		writeError(w, http.StatusBadRequest, errText("command produced an empty prompt"))
		return
	}

	meta, err := s.store.Meta(id)
	if err != nil {
		writeStoreError(w, err)
		return
	}
	agentID := strings.TrimSpace(body.Agent)
	if agentID == "" && hasCustom {
		agentID = custom.Agent
	}
	if agentID == "" {
		agentID = meta.Agent
	}
	selection := provider.Selection{Provider: meta.Provider, Model: meta.Model}
	if agentID != "" {
		profile, ok := profileIn(s.agentsFor(r), agentID)
		if !ok {
			writeError(w, http.StatusBadRequest, errText("unknown agent"))
			return
		}
		previous := s.sessionAgent(id)
		if err := s.store.SetAgent(id, profile.ID); err != nil {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		s.announceAgent(id, profile.ID, previous)
		if strings.TrimSpace(profile.Model) != "" && (body.Provider == "" || body.Model == "") {
			providerID, modelID, err := agents.ValidateModel(profile.Model)
			if err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			selection = provider.Selection{Provider: providerID, Model: modelID}
		}
	}
	if hasCustom && custom.Model != "" && body.Provider == "" && body.Model == "" {
		providerID, modelID, err := agents.ValidateModel(custom.Model)
		if err != nil {
			writeError(w, http.StatusBadRequest, err)
			return
		}
		selection = provider.Selection{Provider: providerID, Model: modelID}
	}
	if body.Provider != "" || body.Model != "" {
		selection = provider.Selection{Provider: body.Provider, Model: body.Model}
	}
	target, err := s.agentFor(id).Resolve(selection)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := s.store.SetModel(id, target.ProviderID, target.Model); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}

	delivery := strings.TrimSpace(body.Delivery)
	if delivery == "" {
		delivery = "steer"
	}
	if delivery != "steer" && delivery != "queue" {
		writeError(w, http.StatusBadRequest, errText("delivery must be steer or queue"))
		return
	}
	events, err := s.agentFor(id).Run(context.Background(), id, text, provider.Selection{Provider: target.ProviderID, Model: target.Model})
	if err != nil {
		if errors.Is(err, agent.ErrBusy) {
			if _, enqueueErr := s.agentFor(id).EnqueueDelivery(id, text, delivery); enqueueErr != nil {
				if errors.Is(enqueueErr, agent.ErrNotRunning) {
					writeError(w, http.StatusConflict, errText("session is running"))
					return
				}
				writeError(w, http.StatusTooManyRequests, enqueueErr)
				return
			}
			writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	go func() {
		for range events {
		}
	}()
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) command(name string) (plugin.Command, bool) {
	return s.commandIn(nil, name)
}

// commandIn finds a plugin command inside one workspace.
func (s *Server) commandIn(r *http.Request, name string) (plugin.Command, bool) {
	manager := s.pluginsFor(r)
	if manager == nil {
		return plugin.Command{}, false
	}
	for _, item := range manager.Commands() {
		if item.Name == name {
			return item, true
		}
	}
	return plugin.Command{}, false
}

func (s *Server) customCommand(name string) (commands.Command, bool) {
	return s.customCommandIn(nil, name)
}

// customCommandIn finds a configured command inside one workspace.
func (s *Server) customCommandIn(r *http.Request, name string) (commands.Command, bool) {
	registry := s.commandsFor(r)
	if registry == nil {
		return commands.Command{}, false
	}
	return registry.Get(name)
}

// slashName returns the command portion of a leading slash prompt.
func slashName(text string) string {
	text = strings.TrimSpace(text)
	if !strings.HasPrefix(text, "/") {
		return ""
	}
	body := strings.TrimPrefix(text, "/")
	if index := strings.IndexFunc(body, func(r rune) bool { return r == ' ' || r == '\t' || r == '\n' }); index >= 0 {
		body = body[:index]
	}
	return body
}
