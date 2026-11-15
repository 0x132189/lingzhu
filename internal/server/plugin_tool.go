package server

import (
	"encoding/json"
	"net/http"
	"strings"
)

// callPluginTool invokes one tool contributed by a plugin.
func (s *Server) callPluginTool(w http.ResponseWriter, r *http.Request) {
	if s.plugins == nil {
		writeError(w, http.StatusNotImplemented, errText("plugins are not available"))
		return
	}
	var body struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, errText("tool name is empty"))
		return
	}
	args := map[string]any{}
	if len(body.Arguments) > 0 && string(body.Arguments) != "null" {
		if err := json.Unmarshal(body.Arguments, &args); err != nil {
			writeError(w, http.StatusBadRequest, errText("arguments must be a JSON object"))
			return
		}
	}
	result, err := s.pluginsFor(r).CallTool(r.Context(), r.PathValue("plugin"), name, args)
	if err != nil {
		if strings.Contains(err.Error(), "not available") {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}
