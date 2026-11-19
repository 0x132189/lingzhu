package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"lingzhu/internal/mcp"
)

// SetMCP attaches the MCP manager used by the status endpoint.
func (s *Server) SetMCP(manager *mcp.Manager) {
	s.mcp = manager
}

// listMCP returns connected servers with their tools and resources.
func (s *Server) listMCP(w http.ResponseWriter, r *http.Request) {
	servers := []mcp.ServerStatus{}
	if manager := s.mcpFor(r); manager != nil {
		servers = manager.Status()
	}
	if servers == nil {
		servers = []mcp.ServerStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"servers": servers})
}

// reconnectMCP retries one failed server without restarting the core.
func (s *Server) reconnectMCP(w http.ResponseWriter, r *http.Request) {
	manager := s.mcpFor(r)
	if manager == nil {
		writeError(w, http.StatusNotImplemented, errText("MCP is not available"))
		return
	}
	server := r.PathValue("server")
	status, err := manager.Reconnect(r.Context(), server)
	if err != nil {
		if _, ok := manager.StatusFor(server); !ok {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": status})
}

// callMCPTool invokes one tool on a connected server.
func (s *Server) callMCPTool(w http.ResponseWriter, r *http.Request) {
	if s.mcpFor(r) == nil {
		writeError(w, http.StatusNotImplemented, errText("MCP is not available"))
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
	result, err := s.mcpFor(r).CallTool(r.Context(), r.PathValue("server"), name, args)
	if err != nil {
		if strings.Contains(err.Error(), "is not connected") {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
}

// readMCPResource reads one resource from a connected server.
func (s *Server) readMCPResource(w http.ResponseWriter, r *http.Request) {
	if s.mcpFor(r) == nil {
		writeError(w, http.StatusNotImplemented, errText("MCP is not available"))
		return
	}
	var body struct {
		URI string `json:"uri"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	uri := strings.TrimSpace(body.URI)
	if uri == "" {
		writeError(w, http.StatusBadRequest, errText("uri is empty"))
		return
	}
	contents, err := s.mcpFor(r).ReadResource(r.Context(), r.PathValue("server"), uri)
	if err != nil {
		if strings.Contains(err.Error(), "is not connected") {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if contents == nil {
		contents = []mcp.ResourceContent{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"contents": contents})
}
