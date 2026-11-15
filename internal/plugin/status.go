package plugin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

// PluginTool is one tool contributed by a plugin.
type PluginTool struct {
	Name      string `json:"name"`
	Effective string `json:"effective"`
}

// PluginStatus is the public view of one connected plugin.
type PluginStatus struct {
	Name         string       `json:"name"`
	Capabilities Capabilities `json:"capabilities"`
	Tools        []PluginTool `json:"tools"`
	Commands     []string     `json:"commands"`
	Panels       []PanelInfo  `json:"panels"`
}

// CallTool invokes one plugin tool by plugin name and raw or effective tool
// name. Explicit calls bypass the model-facing permission layer.
func (m *Manager) CallTool(ctx context.Context, plugin, name string, arguments map[string]any) (CallResult, error) {
	if m == nil {
		return CallResult{}, errors.New("plugins are disabled")
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	payload, err := json.Marshal(arguments)
	if err != nil {
		return CallResult{}, err
	}
	m.mu.RLock()
	var target *pluginTool
	for _, item := range m.tools {
		entry, ok := item.(*pluginTool)
		if !ok {
			continue
		}
		if entry.plugin == plugin && (entry.rawName == name || entry.name == name) {
			target = entry
			break
		}
	}
	m.mu.RUnlock()
	if target == nil {
		return CallResult{}, fmt.Errorf("plugin tool %q is not available", name)
	}
	result, err := target.client.CallTool(ctx, target.rawName, payload)
	if err != nil {
		return CallResult{}, err
	}
	return *result, nil
}

// Status returns connected plugins with their tools and commands. It never
// exposes process commands, environment variables, or transport details.
func (m *Manager) Status() []PluginStatus {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()

	byName := make(map[string]*PluginStatus, len(m.clients))
	for name, client := range m.clients {
		byName[name] = &PluginStatus{
			Name:         name,
			Capabilities: client.Capabilities(),
			Tools:        []PluginTool{},
			Commands:     []string{},
			Panels:       []PanelInfo{},
		}
	}
	for name, panels := range m.panels {
		if status := byName[name]; status != nil {
			status.Panels = panels
		}
	}
	for _, item := range m.tools {
		pluginTool, ok := item.(*pluginTool)
		if !ok {
			continue
		}
		status := byName[pluginTool.plugin]
		if status == nil {
			continue
		}
		status.Tools = append(status.Tools, PluginTool{Name: pluginTool.rawName, Effective: pluginTool.name})
	}
	for _, command := range m.commands {
		status := byName[command.Plugin]
		if status == nil {
			continue
		}
		status.Commands = append(status.Commands, command.Name)
	}

	out := make([]PluginStatus, 0, len(byName))
	for _, status := range byName {
		sort.Slice(status.Tools, func(i, j int) bool { return status.Tools[i].Name < status.Tools[j].Name })
		sort.Strings(status.Commands)
		out = append(out, *status)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
