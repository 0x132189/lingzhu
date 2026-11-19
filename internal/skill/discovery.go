package skill

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	maxDepth        = 6
	maxSkills       = 500
	maxSkillBytes   = 256 * 1024
	defaultFiles    = 10
	maxResourceScan = 4096
)

var skipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	"vendor":       true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
	"dist":         true,
	"build":        true,
}

// resourceSkipDirs matches the reference implementation's file scan: hidden
// resource files stay visible, but well-known dependency and metadata folders
// are not sampled into the model prompt.
var resourceSkipDirs = map[string]bool{
	".git":         true,
	"node_modules": true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
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
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		// Read skills written for the two common agent layouts. Project-level
		// copies below override these because later roots win.
		add(filepath.Join(home, ".claude", "skills"))
		add(filepath.Join(home, ".agents", "skills"))
	}
	for _, base := range projectDirs(workdir) {
		add(filepath.Join(base, ".lingzhu", "skills"))
		add(filepath.Join(base, ".lingzhu", "skill"))
		add(filepath.Join(base, ".claude", "skills"))
		add(filepath.Join(base, ".agents", "skills"))
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

// projectDirs returns workdir and its ancestors from the filesystem root down,
// so skills in a repository root are visible when the workspace is a subfolder.
func projectDirs(workdir string) []string {
	if strings.TrimSpace(workdir) == "" {
		return nil
	}
	path, err := filepath.Abs(workdir)
	if err != nil {
		return nil
	}
	var dirs []string
	for {
		dirs = append(dirs, path)
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	for i, j := 0, len(dirs)-1; i < j; i, j = i+1, j-1 {
		dirs[i], dirs[j] = dirs[j], dirs[i]
	}
	return dirs
}

// Manager holds discovered skills keyed by id. Reload replaces the catalog in
// place so the skill tool and prompt builder see filesystem changes without a
// workspace restart.
type Manager struct {
	mu       sync.RWMutex
	skills   map[string]Info
	list     []Info
	maxFiles int
}

// Discover scans every root in order with the default file sample size. A
// later root wins when two skills share an id, matching the source precedence
// used by the reference implementation.
func Discover(roots []string, logf func(string, ...any)) *Manager {
	return DiscoverWithFiles(roots, logf, defaultFiles)
}

// DiscoverWithFiles is Discover with a configured resource file sample size.
func DiscoverWithFiles(roots []string, logf func(string, ...any), files int) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if files <= 0 {
		files = defaultFiles
	}
	manager := &Manager{skills: map[string]Info{}, maxFiles: files}
	for _, root := range roots {
		if len(manager.skills) >= maxSkills {
			logf("skill: too many skills, stopping at %d", maxSkills)
			break
		}
		info, err := os.Stat(root)
		if err != nil || !info.IsDir() {
			continue
		}
		for _, item := range scan(root, 0, files) {
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
	m.mu.RLock()
	files := m.maxFiles
	m.mu.RUnlock()
	next := DiscoverWithFiles(roots, logf, files)
	m.mu.Lock()
	m.skills = next.skills
	m.list = next.list
	m.maxFiles = next.maxFiles
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
func scan(directory string, depth, files int) []Info {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil
	}
	var dirs []string
	var markdown []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(directory, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.IsDir() {
			if skipDirs[name] {
				continue
			}
			dirs = append(dirs, path)
			continue
		}
		if depth == 0 && strings.HasSuffix(strings.ToLower(name), ".md") {
			markdown = append(markdown, path)
		}
	}

	if file, ok := findSkillFile(entries); ok {
		if item, ok := parseSkill(filepath.Join(directory, file), directory, files); ok {
			return []Info{item}
		}
		return nil
	}

	var out []Info
	for _, file := range markdown {
		if item, ok := parseSkill(file, directory, files); ok {
			out = append(out, item)
		}
	}
	if depth >= maxDepth {
		return out
	}
	for _, path := range dirs {
		out = append(out, scan(path, depth+1, files)...)
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
func parseSkill(file, directory string, maxFiles int) (Info, bool) {
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
	explicit := false
	if value, ok := frontmatter["metadata.lingzhu/autoinvoke"]; ok {
		if parsed, valid := parseBool(value); valid {
			auto = parsed
			explicit = true
		}
	}
	if !explicit {
		for key, value := range frontmatter {
			if !strings.HasSuffix(key, "/autoinvoke") {
				continue
			}
			if parsed, valid := parseBool(value); valid {
				auto = parsed
				explicit = true
				break
			}
		}
	}
	if !explicit {
		if value, ok := frontmatter["disable-model-invocation"]; ok {
			if parsed, valid := parseBool(value); valid && parsed {
				auto = false
			}
		}
	}

	var files []string
	if base == "SKILL.md" {
		files = listFiles(directory, maxFiles)
	}
	return Info{
		ID:           id,
		Name:         name,
		Description:  strings.TrimSpace(frontmatter["description"]),
		Directory:    directory,
		Path:         file,
		Content:      body,
		Files:        files,
		AllowedTools: parseStringList(frontmatter["allowed-tools"]),
		Autoinvoke:   auto,
	}, true
}

// parseStringList accepts a YAML block list, an inline `[a, b]` list, or a
// single scalar. Tool names never contain commas, so this stays unambiguous.
func parseStringList(value string) []string {
	value = strings.TrimSpace(value)
	if value == "" {
		return nil
	}
	if strings.HasPrefix(value, "[") && strings.HasSuffix(value, "]") {
		value = strings.TrimSuffix(strings.TrimPrefix(value, "["), "]")
	}
	parts := strings.FieldsFunc(value, func(r rune) bool {
		return r == ',' || r == '\n'
	})
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		item := strings.TrimSpace(unquote(strings.TrimSpace(part)))
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}

// splitFrontmatter returns flat key/value pairs plus the markdown body. Nested
// keys are flattened as "parent.child" and sequences join with newlines, so the
// skill fields stay readable without exposing a YAML value type.
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
	header := strings.Join(lines[1:end], "\n")
	body := strings.Join(lines[end+1:], "\n")
	var document map[string]any
	if err := yaml.Unmarshal([]byte(header), &document); err != nil {
		if retry := yaml.Unmarshal([]byte(sanitizeFrontmatter(header)), &document); retry != nil {
			return values, body
		}
	}
	flattenFrontmatter("", document, values)
	return values, body
}

// sanitizeFrontmatter retries values written by other agents as plain text, such
// as unquoted colons and leading tags like `[Tag] text`, as YAML block scalars.
func sanitizeFrontmatter(header string) string {
	lines := strings.Split(header, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t") {
			out = append(out, line)
			continue
		}
		key, value, ok := strings.Cut(line, ":")
		key = strings.TrimSpace(key)
		value = strings.TrimSpace(value)
		if !ok || !frontmatterKey(key) || value == "" || value == "|" || value == ">" ||
			strings.HasPrefix(value, "\"") || strings.HasPrefix(value, "'") {
			out = append(out, line)
			continue
		}
		if strings.Contains(value, ":") || taggedText(value) {
			out = append(out, key+": |-", "  "+value)
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func frontmatterKey(value string) bool {
	if value == "" {
		return false
	}
	for index, r := range value {
		letter := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '_'
		digit := r >= '0' && r <= '9'
		if index == 0 && !letter {
			return false
		}
		if index > 0 && !letter && !digit {
			return false
		}
	}
	return true
}

func taggedText(value string) bool {
	closer := ""
	switch {
	case strings.HasPrefix(value, "["):
		closer = "]"
	case strings.HasPrefix(value, "{"):
		closer = "}"
	default:
		return false
	}
	index := strings.Index(value, closer)
	return index >= 0 && strings.TrimSpace(value[index+1:]) != ""
}

func flattenFrontmatter(prefix string, value map[string]any, out map[string]string) {
	for key, item := range value {
		name := key
		if prefix != "" {
			name = prefix + "." + key
		}
		switch typed := item.(type) {
		case map[string]any:
			flattenFrontmatter(name, typed, out)
		case []any:
			parts := make([]string, 0, len(typed))
			for _, element := range typed {
				if text := frontmatterText(element); text != "" {
					parts = append(parts, text)
				}
			}
			out[name] = strings.Join(parts, "\n")
		default:
			out[name] = frontmatterText(item)
		}
	}
}

func frontmatterText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(typed)
	case bool:
		if typed {
			return "true"
		}
		return "false"
	case time.Time:
		return typed.Format(time.RFC3339)
	default:
		return strings.TrimSpace(fmt.Sprint(typed))
	}
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

// listFiles returns a sorted sample of resources beside SKILL.md. It follows
// symlinks and collects up to a bounded number of paths before slicing, so the
// sample is the lexicographically first N rather than traversal order.
func listFiles(directory string, maxFiles int) []string {
	if maxFiles <= 0 {
		maxFiles = defaultFiles
	}
	files := make([]string, 0, maxFiles)
	collectResourceFiles(directory, 0, &files)
	sort.Strings(files)
	if len(files) > maxFiles {
		files = files[:maxFiles]
	}
	return files
}

func collectResourceFiles(directory string, depth int, files *[]string) {
	if depth > maxDepth || len(*files) >= maxResourceScan {
		return
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		path := filepath.Join(directory, name)
		info, err := os.Stat(path)
		if err != nil {
			continue
		}
		if info.IsDir() {
			if resourceSkipDirs[name] {
				continue
			}
			collectResourceFiles(path, depth+1, files)
			continue
		}
		if name == "SKILL.md" || name == ".lingzhu-version" {
			continue
		}
		*files = append(*files, filepath.ToSlash(path))
		if len(*files) >= maxResourceScan {
			return
		}
	}
}
