package server

import (
	"net/http"

	"lingzhu/internal/plugin"
)

// listPlugins returns connected plugins with their tools and commands.
func (s *Server) listPlugins(w http.ResponseWriter, r *http.Request) {
	plugins := []plugin.PluginStatus{}
	if manager := s.pluginsFor(r); manager != nil {
		plugins = manager.Status()
	}
	if plugins == nil {
		plugins = []plugin.PluginStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"plugins": plugins})
}
