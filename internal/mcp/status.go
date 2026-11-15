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

// ServerStatus is the public view of one connected MCP server.
type ServerStatus struct {
	Name      string           `json:"name"`
	Tools     []ServerTool     `json:"tools"`
	Resources []ServerResource `json:"resources"`
}

// Status returns connected servers with their tools and resources. It never
// exposes transport configuration, headers, or OAuth credentials.
func (m *Manager) Status() []ServerStatus {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	byName := make(map[string]*ServerStatus, len(m.clients))
	for name := range m.clients {
		byName[name] = &ServerStatus{Name: name, Tools: []ServerTool{}, Resources: []ServerResource{}}
	}
	for _, item := range m.tools {
		raw, ok := item.(*mcpTool)
		if !ok {
			continue
		}
		status := byName[raw.server]
		if status == nil {
			continue
		}
		status.Tools = append(status.Tools, ServerTool{Name: raw.rawName, Effective: raw.name})
	}
	for name, resources := range m.resources {
		status := byName[name]
		if status == nil {
			continue
		}
		for _, resource := range resources {
			status.Resources = append(status.Resources, ServerResource{
				Name:        resource.Name,
				URI:         resource.URI,
				URITemplate: resource.URITemplate,
				Description: resource.Description,
				MimeType:    resource.MimeType,
			})
		}
	}
	out := make([]ServerStatus, 0, len(byName))
	for _, status := range byName {
		sort.Slice(status.Tools, func(i, j int) bool { return status.Tools[i].Name < status.Tools[j].Name })
		sort.Slice(status.Resources, func(i, j int) bool {
			if status.Resources[i].URI != status.Resources[j].URI {
				return status.Resources[i].URI < status.Resources[j].URI
			}
			return status.Resources[i].Name < status.Resources[j].Name
		})
		out = append(out, *status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
