package mcp

import "sort"

// ServerTool is one tool advertised by a server.
type ServerTool struct {
	Name      string `json:"name"`
	Effective string `json:"effective"`
}

// ServerResource is one resource or resource template of a server.
type ServerResource struct {
	Name        string `json:"name"`
	URI         string `json:"uri,omitempty"`
	URITemplate string `json:"uriTemplate,omitempty"`
	Description string `json:"description,omitempty"`
	MimeType    string `json:"mimeType,omitempty"`
}

// ServerStatus is the public view of one configured MCP server. Connected
// servers include their catalogs; failed and disabled servers explain why they
// are unavailable so the client can offer a retry.
type ServerStatus struct {
	Name      string           `json:"name"`
	Status    string           `json:"status"`
	Error     string           `json:"error,omitempty"`
	Tools     []ServerTool     `json:"tools"`
	Resources []ServerResource `json:"resources"`
}

// Status returns every configured server with its tools and resources. It
// never exposes transport configuration, headers, or OAuth credentials.
func (m *Manager) Status() []ServerStatus {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	names := make([]string, 0, len(m.states))
	for name := range m.states {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]ServerStatus, 0, len(names))
	for _, name := range names {
		out = append(out, m.statusLocked(name))
	}
	return out
}

// StatusFor returns one configured server's public status.
func (m *Manager) StatusFor(name string) (ServerStatus, bool) {
	if m == nil {
		return ServerStatus{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	if _, ok := m.states[name]; !ok {
		return ServerStatus{}, false
	}
	return m.statusLocked(name), true
}

func (m *Manager) statusFor(name string) ServerStatus {
	status, _ := m.StatusFor(name)
	return status
}

func (m *Manager) statusLocked(name string) ServerStatus {
	state := m.states[name]
	status := ServerStatus{
		Name:      name,
		Tools:     []ServerTool{},
		Resources: []ServerResource{},
	}
	if state != nil {
		status.Status = state.status
		status.Error = state.err
	}
	if status.Status != StatusConnected {
		return status
	}
	for _, item := range m.serverTools[name] {
		raw, ok := item.(*mcpTool)
		if !ok {
			continue
		}
		status.Tools = append(status.Tools, ServerTool{Name: raw.rawName, Effective: raw.name})
	}
	for _, resource := range m.resources[name] {
		status.Resources = append(status.Resources, ServerResource{
			Name:        resource.Name,
			URI:         resource.URI,
			URITemplate: resource.URITemplate,
			Description: resource.Description,
			MimeType:    resource.MimeType,
		})
	}
	sort.Slice(status.Tools, func(i, j int) bool { return status.Tools[i].Name < status.Tools[j].Name })
	sort.Slice(status.Resources, func(i, j int) bool {
		if status.Resources[i].URI != status.Resources[j].URI {
			return status.Resources[i].URI < status.Resources[j].URI
		}
		return status.Resources[i].Name < status.Resources[j].Name
	})
	return status
}
