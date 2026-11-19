package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
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

// disconnectMCP stops one server without changing its configuration.
func (s *Server) disconnectMCP(w http.ResponseWriter, r *http.Request) {
	manager := s.mcpFor(r)
	if manager == nil {
		writeError(w, http.StatusNotImplemented, errText("MCP is not available"))
		return
	}
	server := r.PathValue("server")
	status, err := manager.Disconnect(server)
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

// startMCPAuth discovers the authorization server and returns a consent URL.
func (s *Server) startMCPAuth(w http.ResponseWriter, r *http.Request) {
	manager := s.mcpFor(r)
	if manager == nil {
		writeError(w, http.StatusNotImplemented, errText("MCP is not available"))
		return
	}
	url, state, err := manager.AuthStart(r.Context(), r.PathValue("server"))
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": url, "state": state})
}

// exchangeMCPAuth completes a pasted-code OAuth flow.
func (s *Server) exchangeMCPAuth(w http.ResponseWriter, r *http.Request) {
	manager := s.mcpFor(r)
	if manager == nil {
		writeError(w, http.StatusNotImplemented, errText("MCP is not available"))
		return
	}
	var body struct {
		Code  string `json:"code"`
		State string `json:"state"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	status, err := manager.AuthExchange(r.Context(), r.PathValue("server"), body.Code, body.State)
	if err != nil {
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"server": status})
}

// mcpOAuthCallback is the loopback redirect used by automatic MCP login. It is
// public because the browser cannot attach the API token to a redirect.
func (s *Server) mcpOAuthCallback(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if s.oauth == nil {
		http.Error(w, "OAuth is not available", http.StatusNotImplemented)
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if code == "" || state == "" {
		http.Error(w, "Missing authorization code or state.", http.StatusBadRequest)
		return
	}
	authName, ok := s.oauth.PendingName(state)
	if !ok {
		http.Error(w, "Authorization state is invalid or expired.", http.StatusBadRequest)
		return
	}
	authName, _, err := s.oauth.ExchangeState(r.Context(), state, code)
	if err != nil {
		http.Error(w, "Authorization failed: "+err.Error(), http.StatusBadGateway)
		return
	}
	// Several workspaces may use the same MCP server. The token is shared, so
	// reconnect every manager that references it instead of only the first.
	reconnected := false
	var firstErr error
	for _, manager := range s.mcpManagers() {
		name, ok := manager.ServerForAuth(authName)
		if !ok {
			continue
		}
		reconnected = true
		if _, err := manager.Reconnect(r.Context(), name); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if !reconnected {
		http.Error(w, "No MCP server uses this authorization.", http.StatusNotFound)
		return
	}
	if firstErr != nil {
		http.Error(w, "Authorization failed: "+firstErr.Error(), http.StatusBadGateway)
		return
	}
	_, _ = w.Write([]byte("<!doctype html><meta charset=\"utf-8\"><title>Lingzhu</title><p>Authorization complete. You can close this window.</p>"))
}

// streamMCPEvents follows one event type as server-sent events.
func (s *Server) streamMCPEvents(w http.ResponseWriter, r *http.Request) {
	manager := s.mcpFor(r)
	if manager == nil {
		writeError(w, http.StatusNotImplemented, errText("MCP is not available"))
		return
	}
	server := r.PathValue("server")
	query := r.URL.Query()
	name := strings.TrimSpace(query.Get("name"))
	if name == "" {
		writeError(w, http.StatusBadRequest, errText("event name is empty"))
		return
	}
	var cursor *string
	if values, ok := query["cursor"]; ok && len(values) > 0 {
		value := values[0]
		cursor = &value
	}
	args := map[string]any{}
	if raw := strings.TrimSpace(query.Get("arguments")); raw != "" {
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			writeError(w, http.StatusBadRequest, errText("arguments must be a JSON object"))
			return
		}
	}
	maxAge := 0
	if raw := strings.TrimSpace(query.Get("maxAgeMs")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, errText("maxAgeMs must be a non-negative integer"))
			return
		}
		maxAge = parsed
	}
	stream, stop, err := manager.StreamEvents(r.Context(), server, name, args, cursor, maxAge)
	if err != nil {
		if _, ok := manager.StatusFor(server); !ok {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	defer stop()
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, errText("streaming is not supported"))
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case message, open := <-stream:
			if !open {
				return
			}
			data, err := json.Marshal(message)
			if err != nil {
				continue
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) mcpManagers() []*mcp.Manager {
	var out []*mcp.Manager
	seen := map[*mcp.Manager]bool{}
	add := func(manager *mcp.Manager) {
		if manager != nil && !seen[manager] {
			seen[manager] = true
			out = append(out, manager)
		}
	}
	add(s.mcp)
	if s.locations != nil {
		for _, services := range s.locations.List() {
			add(services.MCP)
		}
	}
	return out
}

// completeMCP asks a server for prompt or resource argument completions.
func (s *Server) completeMCP(w http.ResponseWriter, r *http.Request) {
	manager := s.mcpFor(r)
	if manager == nil {
		writeError(w, http.StatusNotImplemented, errText("MCP is not available"))
		return
	}
	var body struct {
		Type     string `json:"type"`
		Name     string `json:"name"`
		URI      string `json:"uri"`
		Argument string `json:"argument"`
		Value    string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	ref := strings.TrimSpace(body.Name)
	if strings.EqualFold(body.Type, "resource") {
		ref = strings.TrimSpace(body.URI)
	}
	if ref == "" || strings.TrimSpace(body.Argument) == "" {
		writeError(w, http.StatusBadRequest, errText("reference and argument are required"))
		return
	}
	result, err := manager.Complete(r.Context(), r.PathValue("server"), body.Type, ref, body.Argument, body.Value)
	if err != nil {
		if strings.Contains(err.Error(), "is not connected") {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"completion": result})
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

// listMCPEvents returns the event types a connected server advertises.
func (s *Server) listMCPEvents(w http.ResponseWriter, r *http.Request) {
	manager := s.mcpFor(r)
	if manager == nil {
		writeError(w, http.StatusNotImplemented, errText("MCP is not available"))
		return
	}
	server := r.PathValue("server")
	events, err := manager.EventTypes(r.Context(), server)
	if err != nil {
		if _, ok := manager.StatusFor(server); !ok {
			writeError(w, http.StatusNotFound, err)
			return
		}
		writeError(w, http.StatusBadGateway, err)
		return
	}
	if events == nil {
		events = []mcp.EventType{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}
