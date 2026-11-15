// Package skill discovers SKILL.md packages and exposes them to the model as a
// loadable tool. A skill is one directory with optional resources beside its
// SKILL.md; packages may come from local roots or be pulled from a remote index
// into the user cache.
package skill

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Config is the skills section of the configuration file. Both a plain array
// of paths and the reference implementation's {"paths": [...]} object are
// accepted.
type Config struct {
	Paths []string `json:"paths"`
	URLs  []string `json:"urls"`
}

// UnmarshalJSON accepts either form used by existing configurations.
func (c *Config) UnmarshalJSON(data []byte) error {
	var paths []string
	if err := json.Unmarshal(data, &paths); err == nil {
		c.Paths = paths
		return nil
	}
	var object struct {
		Paths []string `json:"paths"`
		URLs  []string `json:"urls"`
	}
	if err := json.Unmarshal(data, &object); err != nil {
		return fmt.Errorf("skills must be an array of paths or an object with paths and urls")
	}
	c.Paths = object.Paths
	c.URLs = object.URLs
	return nil
}

// Summary is the small shape rendered into the system prompt.
type Summary struct {
	ID          string
	Name        string
	Description string
}

// Info is one discovered skill.
type Info struct {
	ID          string
	Name        string
	Description string
	Directory   string
	Path        string
	Content     string
	Files       []string
	// Autoinvoke is false when the skill opts out of the automatic prompt list.
	Autoinvoke bool
}

// Loaded renders one skill in the shape the model expects, matching the
// reference implementation's <skill_content> block.
func (s Info) Loaded() string {
	var b strings.Builder
	fmt.Fprintf(&b, "<skill_content name=%q>\n", s.Name)
	fmt.Fprintf(&b, "# Skill: %s\n\n", s.Name)
	b.WriteString(strings.TrimSpace(s.Content))
	b.WriteString("\n\n")
	fmt.Fprintf(&b, "Base directory for this skill: %s\n", s.Directory)
	b.WriteString("Relative paths in this skill (e.g., scripts/, reference/) are relative to this base directory.\n")
	if len(s.Files) > 0 {
		b.WriteString("Note: file list is sampled.\n\n")
		b.WriteString("<skill_files>\n")
		for _, file := range s.Files {
			fmt.Fprintf(&b, "<file>%s</file>\n", file)
		}
		b.WriteString("</skill_files>\n")
	}
	b.WriteString("</skill_content>")
	return b.String()
}

// Summaries returns auto-invocable skills in stable id order.
func Summaries(list []Info) []Summary {
	out := make([]Summary, 0, len(list))
	for _, item := range list {
		if !item.Autoinvoke || strings.TrimSpace(item.Description) == "" {
			continue
		}
		out = append(out, Summary{ID: item.ID, Name: item.Name, Description: item.Description})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
