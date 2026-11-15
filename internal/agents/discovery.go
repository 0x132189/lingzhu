package agents

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"lingzhu/internal/global"
	"lingzhu/internal/permission"
)

// maxAgentBytes bounds one markdown agent file.
const maxAgentBytes = 256 * 1024

var agentIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/-]{0,127}$`)

// Roots returns the default agent directories: the user config directory and
// the project's .lingzhu folder, each with singular and plural forms.
func Roots(configPath, workdir string) []string {
	configDir := ""
	if strings.TrimSpace(configPath) != "" {
		configDir = filepath.Dir(configPath)
	} else {
		configDir = global.Config()
	}
	candidates := []string{
		filepath.Join(configDir, "agent"),
		filepath.Join(configDir, "agents"),
		// Legacy layouts: a mode directory always yields primary agents.
		filepath.Join(configDir, "mode"),
		filepath.Join(configDir, "modes"),
		filepath.Join(workdir, ".lingzhu", "agent"),
		filepath.Join(workdir, ".lingzhu", "agents"),
		filepath.Join(workdir, ".lingzhu", "mode"),
		filepath.Join(workdir, ".lingzhu", "modes"),
	}
	var roots []string
	seen := map[string]bool{}
	for _, candidate := range candidates {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		abs, err := filepath.Abs(candidate)
		if err != nil {
			continue
		}
		abs = filepath.Clean(abs)
		if seen[abs] {
			continue
		}
		seen[abs] = true
		roots = append(roots, abs)
	}
	return roots
}

// Overlay applies markdown definitions over configuration entries. Non-empty
// fields win, permission lists append, and tool switches merge.
func (c Config) Overlay(defs map[string]Definition) Config {
	out := Config{Agents: make(map[string]Definition, len(c.Agents)+len(defs))}
	for id, def := range c.Agents {
		out.Agents[id] = def
	}
	for id, def := range defs {
		base := out.Agents[id]
		if def.Name != "" {
			base.Name = def.Name
		}
		if def.Description != "" {
			base.Description = def.Description
		}
		if def.Mode != "" {
			base.Mode = def.Mode
		}
		if def.Model != "" {
			base.Model = NormalizeModelRef(def.Model)
		}
		if def.Variant != "" {
			base.Variant = def.Variant
			if base.Model != "" {
				base.Model = WithVariant(base.Model, def.Variant)
			}
		}
		if def.System != "" {
			base.System = def.System
		}
		if def.Steps > 0 {
			base.Steps = def.Steps
		}
		base.Hidden = base.Hidden || def.Hidden
		base.HiddenSet = base.HiddenSet || def.HiddenSet
		base.Disabled = base.Disabled || def.Disabled
		if len(def.Permissions) > 0 {
			base.Permissions = append(base.Permissions, def.Permissions...)
		}
		if len(def.Tools) > 0 {
			tools := make(map[string]bool, len(base.Tools)+len(def.Tools))
			for name, enabled := range base.Tools {
				tools[name] = enabled
			}
			for name, enabled := range def.Tools {
				tools[name] = enabled
			}
			base.Tools = tools
		}
		out.Agents[id] = base
	}
	return out
}

// Discover reads markdown agent files from the given roots. Directories are
// walked recursively, so a nested file becomes an id with a slash. Files
// without frontmatter are ignored. Legacy mode/modes directories force the
// primary mode, matching the reference migration rules.
func Discover(roots []string, logf func(string, ...any)) map[string]Definition {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	out := map[string]Definition{}
	for _, root := range roots {
		legacyPrimary := legacyModeRoot(root)
		var paths []string
		err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				if !errors.Is(walkErr, os.ErrNotExist) {
					logf("agents: walk %s: %v", path, walkErr)
				}
				return nil
			}
			if info.IsDir() || !strings.EqualFold(filepath.Ext(info.Name()), ".md") {
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logf("agents: walk %s: %v", root, err)
			}
			continue
		}
		sort.Strings(paths)
		for _, path := range paths {
			def, ok := loadAgentFile(path, logf)
			if !ok {
				continue
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				continue
			}
			id := strings.TrimSuffix(strings.ToLower(filepath.ToSlash(rel)), ".md")
			if !validAgentID(id) {
				logf("agents: skipping %s: %q is not a valid agent id", path, id)
				continue
			}
			if legacyPrimary {
				def.Mode = ModePrimary
			}
			out[id] = def
		}
	}
	return out
}

// validAgentID keeps ids addressable as one map key while allowing the nested
// directory form used by the reference implementation.
func validAgentID(id string) bool {
	if !agentIDPattern.MatchString(id) {
		return false
	}
	for _, part := range strings.Split(id, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
	}
	return true
}

// legacyModeRoot reports whether a discovery root is the V1 mode/modes folder.
func legacyModeRoot(root string) bool {
	switch strings.ToLower(filepath.Base(filepath.Clean(root))) {
	case "mode", "modes":
		return true
	}
	return false
}

func loadAgentFile(path string, logf func(string, ...any)) (Definition, bool) {
	info, err := os.Stat(path)
	if err != nil {
		logf("agents: stat %s: %v", path, err)
		return Definition{}, false
	}
	if info.Size() > maxAgentBytes {
		logf("agents: skipping %s: larger than %d bytes", path, maxAgentBytes)
		return Definition{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		logf("agents: read %s: %v", path, err)
		return Definition{}, false
	}
	values, body, ok := parseDocument(string(data))
	if !ok {
		logf("agents: skipping %s: no frontmatter", path)
		return Definition{}, false
	}
	def := definition(values, body)
	if def.Description == "" {
		logf("agents: skipping %s: frontmatter needs a description", path)
		return Definition{}, false
	}
	return def, true
}

type frontValue struct {
	scalar  string
	mapping map[string]string
	list    []map[string]string
}

func parseDocument(text string) (map[string]frontValue, string, bool) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if !strings.HasPrefix(text, "---\n") {
		return nil, "", false
	}
	rest := text[len("---\n"):]
	index := strings.Index(rest, "\n---")
	if index < 0 {
		return nil, "", false
	}
	raw := rest[:index]
	body := rest[index+len("\n---"):]
	body = strings.TrimPrefix(body, "\n")

	values := map[string]frontValue{}
	current := ""
	mode := ""
	var item map[string]string
	flush := func() {
		if mode == "list" && item != nil && current != "" {
			value := values[current]
			value.list = append(value.list, item)
			values[current] = value
		}
		item = nil
	}

	for _, line := range strings.Split(raw, "\n") {
		trimmedLine := strings.TrimSpace(line)
		if trimmedLine == "" || strings.HasPrefix(trimmedLine, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 0 {
			flush()
			mode = ""
			key, value, ok := strings.Cut(trimmedLine, ":")
			if !ok {
				current = ""
				continue
			}
			key = strings.ToLower(strings.TrimSpace(key))
			value = strings.TrimSpace(value)
			if value == "" {
				current = key
				values[key] = frontValue{mapping: map[string]string{}}
				continue
			}
			current = ""
			values[key] = frontValue{scalar: unquote(value)}
			continue
		}
		if current == "" {
			continue
		}
		if strings.HasPrefix(trimmedLine, "- ") {
			flush()
			mode = "list"
			item = map[string]string{}
			parseKeyValue(strings.TrimSpace(trimmedLine[2:]), item)
			continue
		}
		if mode == "list" {
			if item == nil {
				item = map[string]string{}
			}
			parseKeyValue(trimmedLine, item)
			continue
		}
		mode = "map"
		value := values[current]
		if value.mapping == nil {
			value.mapping = map[string]string{}
		}
		parseKeyValue(trimmedLine, value.mapping)
		values[current] = value
	}
	flush()
	return values, body, true
}

func parseKeyValue(line string, into map[string]string) {
	key, value, ok := strings.Cut(line, ":")
	if !ok {
		return
	}
	into[strings.TrimSpace(key)] = unquote(strings.TrimSpace(value))
}

func unquote(value string) string {
	if len(value) >= 2 {
		if (value[0] == '"' && value[len(value)-1] == '"') || (value[0] == '\'' && value[len(value)-1] == '\'') {
			return value[1 : len(value)-1]
		}
	}
	return value
}

func definition(values map[string]frontValue, body string) Definition {
	def := Definition{System: strings.TrimSpace(body)}
	scalar := func(key string) string {
		return strings.TrimSpace(values[key].scalar)
	}
	def.Name = scalar("name")
	def.Description = scalar("description")
	def.Mode = scalar("mode")
	def.Model = NormalizeModelRef(scalar("model"))
	def.Variant = scalar("variant")
	if def.Variant != "" && def.Model != "" {
		def.Model = WithVariant(def.Model, def.Variant)
	}
	if system := scalar("system"); system != "" {
		def.System = system
	}
	if def.Mode == "" {
		def.Mode = ModeAll
	}
	if steps := scalar("steps"); steps != "" {
		if parsed, err := strconv.Atoi(steps); err == nil && parsed > 0 {
			def.Steps = parsed
		}
	}
	def.Hidden = parseBool(scalar("hidden"))
	def.HiddenSet = values["hidden"].scalar != ""
	def.Disabled = parseBool(scalar("disabled"))

	if tools, ok := values["tools"]; ok && len(tools.mapping) > 0 {
		def.Tools = map[string]bool{}
		for name, value := range tools.mapping {
			enabled, err := strconv.ParseBool(strings.TrimSpace(value))
			if err != nil {
				continue
			}
			def.Tools[name] = enabled
		}
	}

	for _, key := range []string{"permissions", "permission"} {
		value, ok := values[key]
		if !ok {
			continue
		}
		if value.scalar != "" {
			def.Permissions = append(def.Permissions, parseInlineRules(value.scalar)...)
		}
		for _, entry := range value.list {
			rule := permission.Rule{
				Action:   strings.TrimSpace(entry["action"]),
				Resource: strings.TrimSpace(entry["resource"]),
				Effect:   permission.Effect(strings.ToLower(strings.TrimSpace(entry["effect"]))),
			}
			if rule.Action == "" {
				continue
			}
			if rule.Resource == "" {
				rule.Resource = "*"
			}
			switch rule.Effect {
			case permission.Allow, permission.Ask, permission.Deny:
				def.Permissions = append(def.Permissions, rule)
			}
		}
	}
	return def
}

func parseInlineRules(value string) permission.Rules {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "[") {
		return nil
	}
	var rules permission.Rules
	if err := json.Unmarshal([]byte(value), &rules); err != nil {
		return nil
	}
	out := make(permission.Rules, 0, len(rules))
	for _, rule := range rules {
		if strings.TrimSpace(rule.Action) == "" {
			continue
		}
		switch rule.Effect {
		case permission.Allow, permission.Ask, permission.Deny:
			out = append(out, rule)
		}
	}
	return out
}

func parseBool(value string) bool {
	parsed, err := strconv.ParseBool(strings.TrimSpace(value))
	return err == nil && parsed
}
