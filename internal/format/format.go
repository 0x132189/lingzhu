// Package format runs configured formatters on files after they are written.
// Built-in formatters are detected from PATH and local node_modules; explicit
// configuration can add, replace or disable them.
package format

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

const (
	formatTimeout = 20 * time.Second
	maxOutput     = 8 * 1024
)

// Config is the formatter section. `false` disables formatting, `true` or an
// absent value enables built-ins, and an object adds or overrides formatters.
type Config struct {
	Enabled    *bool                 `json:"-"`
	Formatters map[string]Definition `json:"-"`
}

// Definition is one explicitly configured formatter.
type Definition struct {
	Command     []string          `json:"command"`
	Extensions  []string          `json:"extensions"`
	Environment map[string]string `json:"environment"`
	Disabled    bool              `json:"disabled"`
}

// UnmarshalJSON accepts a boolean or a map keyed by formatter name.
func (c *Config) UnmarshalJSON(data []byte) error {
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" || trimmed == "null" {
		return nil
	}
	var enabled bool
	if err := json.Unmarshal(data, &enabled); err == nil {
		c.Enabled = &enabled
		c.Formatters = nil
		return nil
	}
	var formatters map[string]Definition
	if err := json.Unmarshal(data, &formatters); err != nil {
		return fmt.Errorf("formatter must be false or an object keyed by name")
	}
	c.Formatters = formatters
	return nil
}

// Status is one resolved formatter for the status endpoint.
type Status struct {
	Name       string   `json:"name"`
	Command    []string `json:"command"`
	Extensions []string `json:"extensions"`
	Source     string   `json:"source"`
}

// Result is one format run.
type Result struct {
	Name    string `json:"name,omitempty"`
	Changed bool   `json:"changed"`
	Output  string `json:"output,omitempty"`
}

type formatter struct {
	name        string
	command     []string
	extensions  []string
	environment []string
	source      string
}

// Manager resolves formatters once for a workspace.
type Manager struct {
	workdir    string
	formatters []formatter
	logf       func(string, ...any)
}

// New resolves built-ins and configuration. A missing formatter binary simply
// removes that built-in.
func New(workdir string, cfg Config, logf func(string, ...any)) *Manager {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	m := &Manager{workdir: workdir, logf: logf}
	if cfg.Enabled != nil && !*cfg.Enabled {
		return m
	}

	builtins := availableBuiltins(workdir)
	configured := make([]formatter, 0, len(cfg.Formatters))
	names := make([]string, 0, len(cfg.Formatters))
	for name := range cfg.Formatters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		def := cfg.Formatters[name]
		if def.Disabled {
			continue
		}
		base, found := builtinNamed(builtins, name)
		item := formatter{name: name, source: "config"}
		if found {
			item = base
			item.source = "config"
		}
		if len(def.Command) > 0 {
			item.command = append([]string(nil), def.Command...)
		}
		if len(def.Extensions) > 0 {
			item.extensions = normalizeExtensions(def.Extensions)
		}
		item.environment = mergeEnv(item.environment, def.Environment)
		if len(item.command) == 0 || len(item.extensions) == 0 {
			logf("formatter: %q skipped: command and extensions are required", name)
			continue
		}
		configured = append(configured, item)
	}

	var out []formatter
	seen := map[string]bool{}
	for _, item := range configured {
		out = append(out, item)
		seen[item.name] = true
	}
	for _, item := range builtins {
		if seen[item.name] {
			continue
		}
		if def, ok := cfg.Formatters[item.name]; ok && def.Disabled {
			continue
		}
		out = append(out, item)
	}
	m.formatters = out
	if len(out) > 0 {
		names := make([]string, 0, len(out))
		for _, item := range out {
			names = append(names, item.name)
		}
		logf("formatters: %s", strings.Join(names, ", "))
	}
	return m
}

// Root returns the work directory formatters run in.
func (m *Manager) Root() string {
	if m == nil {
		return ""
	}
	return m.workdir
}

// Format runs the first matching formatter for path.
func (m *Manager) Format(ctx context.Context, path string) (Result, error) {
	if m == nil || len(m.formatters) == 0 {
		return Result{}, nil
	}
	extension := strings.ToLower(filepath.Ext(path))
	var selected *formatter
	for i := range m.formatters {
		for _, candidate := range m.formatters[i].extensions {
			if candidate == extension {
				selected = &m.formatters[i]
				break
			}
		}
		if selected != nil {
			break
		}
	}
	if selected == nil {
		return Result{}, nil
	}

	before, err := os.ReadFile(path)
	if err != nil {
		return Result{Name: selected.name}, err
	}
	command := expand(selected.command, path)
	runCtx, cancel := context.WithTimeout(ctx, formatTimeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, command[0], command[1:]...)
	cmd.Dir = m.workdir
	cmd.Env = append(os.Environ(), selected.environment...)
	out, err := cmd.CombinedOutput()
	result := Result{Name: selected.name, Output: clip(string(out))}
	if err != nil {
		return result, fmt.Errorf("%s: %w", selected.name, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	result.Changed = string(before) != string(after)
	return result, nil
}

// FormatFile is the hook shape used by tools: it returns the formatter name.
func (m *Manager) FormatFile(ctx context.Context, path string) (string, error) {
	result, err := m.Format(ctx, path)
	return result.Name, err
}

// Status lists the resolved formatters.
func (m *Manager) Status() []Status {
	if m == nil {
		return []Status{}
	}
	out := make([]Status, 0, len(m.formatters))
	for _, item := range m.formatters {
		out = append(out, Status{
			Name:       item.name,
			Command:    append([]string(nil), item.command...),
			Extensions: append([]string(nil), item.extensions...),
			Source:     item.source,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func availableBuiltins(workdir string) []formatter {
	type spec struct {
		name        string
		extensions  []string
		args        []string
		environment map[string]string
		detect      func(base string) (string, bool)
	}
	execOnly := func(name string) func(string) (string, bool) {
		return func(base string) (string, bool) { return findExecutable(name, base) }
	}
	jsEnv := map[string]string{"BUN_BE_BUN": "1"}
	jsExtensions := []string{
		".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts",
		".html", ".htm", ".css", ".scss", ".sass", ".less", ".vue", ".svelte",
		".json", ".jsonc", ".yaml", ".yml", ".toml", ".xml",
		".md", ".mdx", ".graphql", ".gql",
	}

	specs := []spec{
		{name: "gofmt", extensions: []string{".go"}, args: []string{"-w", "$FILE"}, detect: execOnly("gofmt")},
		{name: "mix", extensions: []string{".ex", ".exs", ".eex", ".heex", ".leex", ".neex", ".sface"}, args: []string{"format", "$FILE"}, detect: execOnly("mix")},
		{name: "oxfmt", extensions: []string{".js", ".jsx", ".mjs", ".cjs", ".ts", ".tsx", ".mts", ".cts"}, args: []string{"$FILE"}, environment: jsEnv, detect: func(base string) (string, bool) {
			if !packageDependency(base, "oxfmt") {
				return "", false
			}
			return findExecutable("oxfmt", base)
		}},
		{name: "prettier", extensions: jsExtensions, args: []string{"--write", "$FILE"}, environment: jsEnv, detect: func(base string) (string, bool) {
			if !packageDependency(base, "prettier") {
				return "", false
			}
			return findExecutable("prettier", base)
		}},
		{name: "biome", extensions: jsExtensions, args: []string{"format", "--write", "$FILE"}, environment: jsEnv, detect: func(base string) (string, bool) {
			if !findUpExists(base, "biome.json") && !findUpExists(base, "biome.jsonc") {
				return "", false
			}
			if bin, ok := findExecutable("@biomejs/biome", base); ok {
				return bin, true
			}
			return findExecutable("biome", base)
		}},
		{name: "zig", extensions: []string{".zig", ".zon"}, args: []string{"fmt", "$FILE"}, detect: execOnly("zig")},
		{name: "clang-format", extensions: []string{".c", ".cc", ".cpp", ".cxx", ".c++", ".h", ".hh", ".hpp", ".hxx", ".h++", ".ino"}, args: []string{"-i", "$FILE"}, detect: func(base string) (string, bool) {
			if !findUpExists(base, ".clang-format") {
				return "", false
			}
			return findExecutable("clang-format", base)
		}},
		{name: "ktlint", extensions: []string{".kt", ".kts"}, args: []string{"-F", "$FILE"}, detect: execOnly("ktlint")},
		{name: "ruff", extensions: []string{".py", ".pyi"}, args: []string{"format", "$FILE"}, detect: ruffDetect},
		{name: "air", extensions: []string{".r"}, args: []string{"format", "$FILE"}, detect: airDetect},
		{name: "uv", extensions: []string{".py", ".pyi"}, args: []string{"format", "--", "$FILE"}, detect: uvDetect},
		{name: "rubocop", extensions: []string{".rb", ".rake", ".gemspec", ".ru"}, args: []string{"--autocorrect", "$FILE"}, detect: execOnly("rubocop")},
		{name: "standardrb", extensions: []string{".rb", ".rake", ".gemspec", ".ru"}, args: []string{"--fix", "$FILE"}, detect: execOnly("standardrb")},
		{name: "htmlbeautifier", extensions: []string{".erb", ".html.erb"}, args: []string{"$FILE"}, detect: execOnly("htmlbeautifier")},
		{name: "dart", extensions: []string{".dart"}, args: []string{"format", "$FILE"}, detect: execOnly("dart")},
		{name: "ocamlformat", extensions: []string{".ml", ".mli"}, args: []string{"-i", "$FILE"}, detect: func(base string) (string, bool) {
			if !findUpExists(base, ".ocamlformat") {
				return "", false
			}
			return findExecutable("ocamlformat", base)
		}},
		{name: "terraform", extensions: []string{".tf", ".tfvars"}, args: []string{"fmt", "$FILE"}, detect: execOnly("terraform")},
		{name: "latexindent", extensions: []string{".tex"}, args: []string{"-w", "-s", "$FILE"}, detect: execOnly("latexindent")},
		{name: "gleam", extensions: []string{".gleam"}, args: []string{"format", "$FILE"}, detect: execOnly("gleam")},
		{name: "shfmt", extensions: []string{".sh", ".bash"}, args: []string{"-w", "$FILE"}, detect: execOnly("shfmt")},
		{name: "nixfmt", extensions: []string{".nix"}, args: []string{"$FILE"}, detect: execOnly("nixfmt")},
		{name: "rustfmt", extensions: []string{".rs"}, args: []string{"$FILE"}, detect: execOnly("rustfmt")},
		{name: "pint", extensions: []string{".php"}, args: []string{"$FILE"}, detect: pintDetect},
		{name: "php-cs-fixer", extensions: []string{".php"}, args: []string{"fix", "$FILE"}, detect: phpFixerDetect},
		{name: "ormolu", extensions: []string{".hs"}, args: []string{"-i", "$FILE"}, detect: execOnly("ormolu")},
		{name: "cljfmt", extensions: []string{".clj", ".cljs", ".cljc", ".edn"}, args: []string{"fix", "--quiet", "$FILE"}, detect: execOnly("cljfmt")},
		{name: "dfmt", extensions: []string{".d"}, args: []string{"-i", "$FILE"}, detect: execOnly("dfmt")},
	}

	var out []formatter
	for _, item := range specs {
		binary, ok := item.detect(workdir)
		if !ok || binary == "" {
			continue
		}
		out = append(out, formatter{
			name:        item.name,
			command:     append([]string{binary}, item.args...),
			extensions:  normalizeExtensions(item.extensions),
			environment: mergeEnv(nil, item.environment),
			source:      "builtin",
		})
	}
	return out
}

func findUpFile(workdir, name string) (string, bool) {
	path := workdir
	for depth := 0; depth < 10; depth++ {
		candidate := filepath.Join(path, name)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate, true
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	return "", false
}

func findUpExists(workdir, name string) bool {
	_, ok := findUpFile(workdir, name)
	return ok
}

func readTextFile(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, 1<<20))
	if err != nil {
		return ""
	}
	return string(data)
}

func packageDependency(workdir, dependency string) bool {
	file, ok := findUpFile(workdir, "package.json")
	if !ok {
		return false
	}
	var payload struct {
		Dependencies    map[string]any `json:"dependencies"`
		DevDependencies map[string]any `json:"devDependencies"`
	}
	if err := json.Unmarshal([]byte(readTextFile(file)), &payload); err != nil {
		return false
	}
	if _, ok := payload.Dependencies[dependency]; ok {
		return true
	}
	_, ok = payload.DevDependencies[dependency]
	return ok
}

func composerDependency(workdir, dependency string) (string, bool) {
	file, ok := findUpFile(workdir, "composer.json")
	if !ok {
		return "", false
	}
	var payload struct {
		Require    map[string]any `json:"require"`
		RequireDev map[string]any `json:"require-dev"`
	}
	if err := json.Unmarshal([]byte(readTextFile(file)), &payload); err != nil {
		return "", false
	}
	if _, ok := payload.Require[dependency]; ok {
		return filepath.Dir(file), true
	}
	if _, ok := payload.RequireDev[dependency]; ok {
		return filepath.Dir(file), true
	}
	return "", false
}

func ruffDetect(base string) (string, bool) {
	binary, ok := findExecutable("ruff", base)
	if !ok {
		return "", false
	}
	for _, name := range []string{"pyproject.toml", "ruff.toml", ".ruff.toml"} {
		file, found := findUpFile(base, name)
		if !found {
			continue
		}
		if name != "pyproject.toml" || strings.Contains(readTextFile(file), "[tool.ruff]") {
			return binary, true
		}
	}
	for _, name := range []string{"requirements.txt", "pyproject.toml", "Pipfile"} {
		file, found := findUpFile(base, name)
		if found && strings.Contains(readTextFile(file), "ruff") {
			return binary, true
		}
	}
	return "", false
}

func airDetect(base string) (string, bool) {
	binary, ok := findExecutable("air", base)
	if !ok {
		return "", false
	}
	output, ok := commandOutput(base, binary, "--help")
	if !ok {
		return "", false
	}
	first := strings.SplitN(output, "\n", 2)[0]
	if strings.Contains(first, "R language") && strings.Contains(first, "formatter") {
		return binary, true
	}
	return "", false
}

func uvDetect(base string) (string, bool) {
	binary, ok := findExecutable("uv", base)
	if !ok {
		return "", false
	}
	if _, ok := commandOutput(base, binary, "format", "--help"); !ok {
		return "", false
	}
	return binary, true
}

func pintDetect(base string) (string, bool) {
	root, ok := composerDependency(base, "laravel/pint")
	if !ok {
		return "", false
	}
	binary := filepath.Join(root, "vendor", "bin", "pint")
	if info, err := os.Stat(binary); err == nil && !info.IsDir() {
		return binary, true
	}
	return "", false
}

func phpFixerDetect(base string) (string, bool) {
	configured := false
	for _, name := range []string{".php-cs-fixer.php", ".php-cs-fixer.dist.php"} {
		if findUpExists(base, name) {
			configured = true
			break
		}
	}
	root, dependency := composerDependency(base, "friendsofphp/php-cs-fixer")
	if !configured && !dependency {
		return "", false
	}
	if binary, ok := findExecutable("php-cs-fixer", base); ok {
		return binary, true
	}
	if dependency {
		binary := filepath.Join(root, "vendor", "bin", "php-cs-fixer")
		if info, err := os.Stat(binary); err == nil && !info.IsDir() {
			return binary, true
		}
	}
	return "", false
}

func commandOutput(dir, name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	if err != nil {
		return "", false
	}
	return string(output), true
}

func builtinNamed(items []formatter, name string) (formatter, bool) {
	for _, item := range items {
		if item.name == name {
			return item, true
		}
	}
	return formatter{}, false
}

func findExecutable(name, workdir string) (string, bool) {
	candidates := []string{name}
	if runtime.GOOS == "windows" {
		for _, extension := range []string{".exe", ".cmd", ".bat"} {
			if !strings.HasSuffix(strings.ToLower(name), extension) {
				candidates = append(candidates, name+extension)
			}
		}
	}
	path := workdir
	for depth := 0; depth < 5; depth++ {
		for _, candidate := range candidates {
			full := filepath.Join(path, "node_modules", ".bin", candidate)
			if info, err := os.Stat(full); err == nil && !info.IsDir() {
				return full, true
			}
		}
		parent := filepath.Dir(path)
		if parent == path {
			break
		}
		path = parent
	}
	for _, candidate := range candidates {
		if found, err := exec.LookPath(candidate); err == nil {
			return found, true
		}
	}
	return "", false
}

func normalizeExtensions(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		item = strings.ToLower(strings.TrimSpace(item))
		if item == "" {
			continue
		}
		if !strings.HasPrefix(item, ".") {
			item = "." + item
		}
		out = append(out, item)
	}
	return out
}

func mergeEnv(base []string, extra map[string]string) []string {
	out := append([]string(nil), base...)
	keys := make([]string, 0, len(extra))
	for key := range extra {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		out = append(out, key+"="+extra[key])
	}
	return out
}

func expand(command []string, path string) []string {
	out := make([]string, 0, len(command))
	replaced := false
	for _, arg := range command {
		if strings.Contains(arg, "$FILE") {
			out = append(out, strings.ReplaceAll(arg, "$FILE", path))
			replaced = true
			continue
		}
		out = append(out, arg)
	}
	if !replaced {
		out = append(out, path)
	}
	return out
}

func clip(text string) string {
	if len(text) <= maxOutput {
		return strings.TrimSpace(text)
	}
	return strings.TrimSpace(text[:maxOutput]) + "\n(output truncated)"
}
