package commands

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"lingzhu/internal/global"
)

const (
	maxCommandBytes = 256 * 1024
	maxCommandDepth = 6
)

var commandNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9/_-]{0,127}$`)

// Roots returns the default command directories: the user config directory and
// the project's .lingzhu folder, each with singular and plural forms.
func Roots(configPath, workdir string) []string {
	configDir := ""
	if strings.TrimSpace(configPath) != "" {
		configDir = filepath.Dir(configPath)
	} else {
		configDir = global.Config()
	}
	candidates := []string{
		filepath.Join(configDir, "command"),
		filepath.Join(configDir, "commands"),
		filepath.Join(workdir, ".lingzhu", "command"),
		filepath.Join(workdir, ".lingzhu", "commands"),
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

// Discover reads markdown commands below each root. Later roots win when the
// same name appears more than once.
func Discover(roots []string, config map[string]Definition, logf func(string, ...any)) map[string]Definition {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	out := map[string]Definition{}
	for name, def := range config {
		out[normalizeName(name)] = def
	}
	for _, root := range roots {
		walkRoot(root, out, logf)
	}
	return out
}

func walkRoot(root string, out map[string]Definition, logf func(string, ...any)) {
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				logf("commands: walk %s: %v", path, err)
			}
			return nil
		}
		if entry.IsDir() {
			if path != root && shouldSkip(entry.Name()) {
				return filepath.SkipDir
			}
			relative, relErr := filepath.Rel(root, path)
			if relErr == nil && relative != "." && strings.Count(relative, string(filepath.Separator)) >= maxCommandDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.EqualFold(filepath.Ext(entry.Name()), ".md") || shouldSkip(entry.Name()) {
			return nil
		}
		name, ok := commandName(root, path)
		if !ok {
			logf("commands: skipping %s: invalid command name", path)
			return nil
		}
		def, ok := loadFile(path, logf)
		if !ok {
			return nil
		}
		out[name] = def
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		logf("commands: read %s: %v", root, err)
	}
}

func shouldSkip(name string) bool {
	return strings.HasPrefix(name, ".") || name == "node_modules" || name == "vendor"
}

func commandName(root, path string) (string, bool) {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return "", false
	}
	relative = strings.TrimSuffix(relative, filepath.Ext(relative))
	relative = strings.ReplaceAll(relative, string(filepath.Separator), "/")
	name := normalizeName(relative)
	return name, commandNamePattern.MatchString(name)
}

func normalizeName(value string) string {
	return strings.ToLower(strings.Trim(strings.TrimSpace(value), "/"))
}

func loadFile(path string, logf func(string, ...any)) (Definition, bool) {
	info, err := os.Stat(path)
	if err != nil {
		logf("commands: stat %s: %v", path, err)
		return Definition{}, false
	}
	if info.Size() > maxCommandBytes {
		logf("commands: skipping %s: larger than %d bytes", path, maxCommandBytes)
		return Definition{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		logf("commands: read %s: %v", path, err)
		return Definition{}, false
	}
	fields, body := parseFrontmatter(string(data))
	template := strings.TrimSpace(body)
	if template == "" {
		logf("commands: skipping %s: empty template", path)
		return Definition{}, false
	}
	return Definition{
		Description: strings.TrimSpace(fields["description"]),
		Template:    template,
		Agent:       strings.TrimSpace(fields["agent"]),
		Model:       strings.TrimSpace(fields["model"]),
	}, true
}

// parseFrontmatter reads the simple scalar keys commands use. Files without
// frontmatter keep their whole contents as the template.
func parseFrontmatter(text string) (map[string]string, string) {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	fields := map[string]string{}
	if !strings.HasPrefix(text, "---\n") {
		return fields, text
	}
	rest := text[len("---\n"):]
	index := strings.Index(rest, "\n---")
	if index < 0 {
		return fields, text
	}
	for _, line := range strings.Split(rest[:index], "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		switch key {
		case "description", "agent", "model":
			fields[key] = strings.TrimSpace(strings.Trim(strings.TrimSpace(value), `"'`))
		}
	}
	body := rest[index+len("\n---"):]
	return fields, strings.TrimPrefix(body, "\n")
}
