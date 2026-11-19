package skill

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	maxDepth      = 6
	maxSkills     = 500
	maxSkillBytes = 256 * 1024
	maxFiles      = 10
)

var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".venv":        true,
	"__pycache__":  true,
	"dist":         true,
	"build":        true,
}

// Roots returns the default skill directories plus configured paths. Defaults
// cover the directory beside the config file and the project's .lingzhu folder;
// configured paths are resolved from workdir and override earlier duplicates.
func Roots(configured []string, configPath, workdir string) []string {
	var roots []string
	seen := map[string]bool{}
	add := func(path string) {
		if strings.TrimSpace(path) == "" {
			return
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			return
		}
		abs = filepath.Clean(abs)
		if seen[abs] {
			return
		}
		seen[abs] = true
		roots = append(roots, abs)
	}
	if configPath != "" {
		dir := filepath.Dir(configPath)
		add(filepath.Join(dir, "skills"))
		add(filepath.Join(dir, "skill"))
	}
	for _, base := range []string{workdir} {
		if base == "" {
			continue
		}
		add(filepath.Join(base, ".lingzhu", "skills"))
		add(filepath.Join(base, ".lingzhu", "skill"))
	}
	for _, item := range configured {
		path := strings.TrimSpace(item)
		if path == "" {
			continue
		}
		if strings.HasPrefix(path, "~/") {
			if home, err := os.UserHomeDir(); err == nil {
				path = filepath.Join(home, path[2:])
			}
		}
		if !filepath.IsAbs(path) {
			path = filepath.Join(workdir, path)
		}
		add(path)
	}
	return roots
}

// Manager holds discovered skills keyed by id. Reload replaces the catalog in
// place so the skill tool and prompt builder see filesystem changes without a
// workspace restart.
type Manager struct {
	mu     sync.RWMutex
	skills map[string]Info
	list   []Info
}

// Discover scans every root in order. A later root wins when two skills share
// an id, matching the reference implementation's source precedence.
func Discover(roots []string, logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	manager := &Manager{skills: map[string]Info{}}
	for _, root := range roots {
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		for _, item := range scan(root, 0) {
			if len(manager.skills) >= maxSkills {
				logf("skill: too many skills, stopping at %d", maxSkills)
				break
			}
			if _, exists := manager.skills[item.ID]; exists {
				logf("skill: %s from %s overrides an earlier definition", item.ID, root)
			}
			manager.skills[item.ID] = item
		}
	}
	manager.list = make([]Info, 0, len(manager.skills))
	for _, item := range manager.skills {
		manager.list = append(manager.list, item)
	}
	sort.Slice(manager.list, func(i, j int) bool { return manager.list[i].ID < manager.list[j].ID })
	return manager
}

// Get returns one skill by id.
func (m *Manager) Get(id string) (Info, bool) {
	if m == nil {
		return Info{}, false
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	item, ok := m.skills[id]
	return item, ok
}

// List returns every discovered skill in id order.
func (m *Manager) List() []Info {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return append([]Info(nil), m.list...)
}

// Reload replaces the catalog with a fresh scan of roots. Existing skills that
// disappeared are removed; later roots still win on duplicate ids.
func (m *Manager) Reload(roots []string, logf func(string, ...any)) {
	if m == nil {
		return
	}
	next := Discover(roots, logf)
	m.mu.Lock()
	m.skills = next.skills
	m.list = next.list
	m.mu.Unlock()
}

// Filter removes skills whose id is rejected and rebuilds the ordered list.
func (m *Manager) Filter(keep func(id string) bool) {
	if m == nil || keep == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for id := range m.skills {
		if !keep(id) {
			delete(m.skills, id)
		}
	}
	m.list = make([]Info, 0, len(m.skills))
	for _, item := range m.skills {
		m.list = append(m.list, item)
	}
	sort.Slice(m.list, func(i, j int) bool { return m.list[i].ID < m.list[j].ID })
}

// Summaries returns the prompt-facing list.
func (m *Manager) Summaries() []Summary {
	if m == nil {
		return nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Summaries(m.list)
}

// scan walks one root. A directory containing SKILL.md is a single skill and is
// not descended further; at the root, standalone .md files are skills too.
func scan(directory string, depth int) []Info {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	var dirs []os.DirEntry
	var markdown []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if entry.IsDir() {
			if skipDirs[name] {
				continue
			}
			dirs = append(dirs, entry)
			continue
		}
		if depth == 0 && strings.HasSuffix(strings.ToLower(name), ".md") {
			markdown = append(markdown, filepath.Join(directory, name))
		}
	}

	if file, ok := findSkillFile(entries); ok {
		if item, ok := parseSkill(filepath.Join(directory, file), directory); ok {
			return []Info{item}
		}
		return nil
	}

	var out []Info
	for _, file := range markdown {
		if item, ok := parseSkill(file, directory); ok {
			out = append(out, item)
		}
	}
	if depth >= maxDepth {
		return out
	}
	for _, entry := range dirs {
		out = append(out, scan(filepath.Join(directory, entry.Name()), depth+1)...)
		if len(out) >= maxSkills {
			break
		}
	}
	return out
}

func findSkillFile(entries []os.DirEntry) (string, bool) {
	for _, entry := range entries {
		if !entry.IsDir() && entry.Name() == "SKILL.md" {
			return entry.Name(), true
		}
	}
	return "", false
}

// parseSkill reads one markdown file and builds its Info. The file is skipped
// when it cannot be read or has no content.
func parseSkill(file, directory string) (Info, bool) {
	data, err := os.ReadFile(file)
	if err != nil {
		return Info{}, false
	}
	if len(data) > maxSkillBytes {
		data = data[:maxSkillBytes]
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	frontmatter, body := splitFrontmatter(text)
	body = strings.TrimSpace(body)
	if body == "" && frontmatter["description"] == "" {
		return Info{}, false
	}

	base := filepath.Base(file)
	id := filepath.Base(directory)
	if base != "SKILL.md" && filepath.Dir(file) == directory {
		id = strings.TrimSuffix(base, filepath.Ext(base))
	}
	name := strings.TrimSpace(frontmatter["name"])
	if name == "" {
		name = id
	}
	auto := true
	if value, ok := frontmatter["metadata.lingzhu/autoinvoke"]; ok {
		if parsed, valid := parseBool(value); valid {
			auto = parsed
		}
	}
	if value, ok := frontmatter["disable-model-invocation"]; ok {
		if parsed, valid := parseBool(value); valid && parsed {
			auto = false
		}
	}

	var files []string
	if base == "SKILL.md" {
		files = listFiles(directory)
	}
	return Info{
		ID:          id,
		Name:        name,
		Description: strings.TrimSpace(frontmatter["description"]),
		Directory:   directory,
		Path:        file,
		Content:     body,
		Files:       files,
		Autoinvoke:  auto,
	}, true
}

// splitFrontmatter returns flat key/value pairs plus the markdown body. Nested
// metadata keys are flattened as "metadata.<key>" so autoinvoke stays readable.
func splitFrontmatter(text string) (map[string]string, string) {
	values := map[string]string{}
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return values, text
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end < 0 {
		return values, text
	}
	header := lines[1:end]
	body := strings.Join(lines[end+1:], "\n")
	metadata := false
	for i := 0; i < len(header); i++ {
		line := header[i]
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := line[:len(line)-len(strings.TrimLeft(line, " \t"))]
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if metadata {
			if indent == "" {
				metadata = false
			} else if key != "" {
				values["metadata."+key] = unquote(value)
				continue
			}
		}
		if key == "metadata" && value == "" {
			metadata = true
			continue
		}
		if value == "|" || value == ">" {
			var parts []string
			for i+1 < len(header) {
				next := header[i+1]
				if next == "" {
					i++
					parts = append(parts, "")
					continue
				}
				if !strings.HasPrefix(next, " ") && !strings.HasPrefix(next, "\t") {
					break
				}
				i++
				parts = append(parts, strings.TrimSpace(next))
			}
			if value == ">" {
				value = strings.Join(parts, " ")
			} else {
				value = strings.Join(parts, "\n")
			}
		}
		values[key] = unquote(value)
	}
	return values, body
}

func unquote(value string) string {
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		if parsed, err := strconv.Unquote(value); err == nil {
			return parsed
		}
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1]
	}
	return value
}

func parseBool(value string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "true", "yes", "on", "1":
		return true, true
	case "false", "no", "off", "0":
		return false, true
	}
	return false, false
}

// listFiles returns a bounded, sorted sample of resources beside SKILL.md.
func listFiles(directory string) []string {
	var files []string
	_ = filepath.WalkDir(directory, func(path string, entry os.DirEntry, err error) error {
		if err != nil || len(files) >= maxFiles {
			return nil
		}
		if path == directory {
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if skipDirs[name] || strings.HasPrefix(name, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(name, ".") || name == "SKILL.md" {
			return nil
		}
		files = append(files, filepath.ToSlash(path))
		return nil
	})
	sort.Strings(files)
	if len(files) > maxFiles {
		files = files[:maxFiles]
	}
	return files
}
