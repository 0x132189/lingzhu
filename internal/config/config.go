// Package config provides all runtime configuration.
// Precedence: defaults < config file < environment < command line.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"lingzhu/internal/agents"
	"lingzhu/internal/artifact"
	"lingzhu/internal/commands"
	"lingzhu/internal/format"
	"lingzhu/internal/global"
	"lingzhu/internal/imagegen"
	"lingzhu/internal/mcp"
	"lingzhu/internal/media"
	"lingzhu/internal/oauth"
	"lingzhu/internal/plugin"
	"lingzhu/internal/policy"
	"lingzhu/internal/schedule"
	"lingzhu/internal/share"
	"lingzhu/internal/skill"
	"lingzhu/internal/warming"
	"lingzhu/internal/websearch"
)

type Config struct {
	Addr      string
	DataDir   string
	Workdir   string
	Model     string
	Provider  string
	Providers map[string]Provider
	// ProvidersSet distinguishes an explicit empty providers block from an
	// old config that never had one.
	ProvidersSet bool
	BaseURL      string
	APIKey       string
	MaxSteps     int
	// SubagentDepth is how many nested subagent levels are allowed. The
	// reference default is one, so a subagent cannot spawn another subagent.
	SubagentDepth int
	// CodemodeOnly exposes only the execute tool directly and keeps every other
	// tool behind Code Mode, matching the reference harness.
	CodemodeOnly bool
	Shell        []string
	Permission   []Rule
	// PermissionAlways holds the allows learned from "always" answers. They are
	// separate from Permission so an approval can never lift a configured deny.
	PermissionAlways []Rule
	Policies         policy.Rules
	SystemPrompt     string
	Instructions     []string
	// References list extra directories the model may consult when relevant.
	References []Reference
	// ModelPrompts selects per-family prompt presets instead of the unified
	// harness prompt. It is off by default for v2 parity.
	ModelPrompts  bool
	ContextWindow int
	CompactKeep   int
	// Titles enables model-generated session titles.
	Titles       bool
	MCP          mcp.Config
	OAuth        oauth.Config
	Formatter    format.Config
	WebSearch    websearch.Config
	Image        imagegen.Config
	Media        media.Config
	Warming      warming.Config
	Artifact     artifact.Config
	Schedule     schedule.Config
	Share        share.Config
	Agents       agents.Config
	DefaultAgent string
	Commands     commands.Config
	Plugins      plugin.Config
	Skills       skill.Config
	// Path is the file the configuration was loaded from, empty when no file was
	// used. It is not part of the file format.
	Path string
	// LogLevel is the minimum level written to the log: debug, info, warn or
	// error. An empty value keeps the default of info.
	LogLevel string
}

// Model is one selectable model inside a provider.
type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow *int   `json:"context_window,omitempty"`
	// MaxOutputTokens is the reply limit the catalog reports.
	MaxOutputTokens int `json:"max_output_tokens,omitempty"`
	// CacheTTL is the provider prompt-cache lifetime, such as "5m". Warming
	// with interval "auto" uses 90% of this value.
	CacheTTL string `json:"cache_ttl,omitempty"`
	// Options are request-body fields for this model, such as
	// reasoning_effort or thinking.
	Options map[string]any `json:"options,omitempty"`
	// Variants are named option overrides selectable for this model.
	Variants map[string]map[string]any `json:"variants,omitempty"`
	// ToolCall is whether the model accepts tool definitions; nil means the
	// catalog did not say, which keeps tools on.
	ToolCall *bool `json:"tool_call,omitempty"`
	// Compaction selects the provider's in-band context compaction. "native"
	// asks the provider to compact the window instead of writing a summary.
	Compaction *ModelCompaction `json:"compaction,omitempty"`
	// Reasoning reports whether the model exposes reasoning output.
	Reasoning bool `json:"reasoning,omitempty"`
	// Attachment reports whether the model takes image input.
	Attachment bool `json:"attachment,omitempty"`
	// Cost is the catalog's price per million tokens.
	Cost *ModelCost `json:"cost,omitempty"`
	// Modalities are the input and output kinds the model takes.
	Modalities *ModelModalities `json:"modalities,omitempty"`
	// Type distinguishes chat models from image models; empty means chat.
	Type string `json:"type,omitempty"`
	// API overrides the provider's wire protocol for this model.
	API string `json:"api,omitempty"`
	// Family groups versions of one model; only the newest stays listed
	// unless a caller asks for all of them.
	Family string `json:"family,omitempty"`
	// Released is the release date as Unix milliseconds; zero means unknown.
	Released int64 `json:"released,omitempty"`
}

// ModelCompaction selects how a model compacts its context. Type "native"
// asks the provider for an in-band window replacement instead of a summary.
type ModelCompaction struct {
	Type string `json:"type,omitempty"`
	// TriggerTokens is the token count that makes the provider compact.
	TriggerTokens int `json:"trigger_tokens,omitempty"`
}

// ModelCost is one model's price per million tokens.
type ModelCost struct {
	Input      float64 `json:"input,omitempty"`
	Output     float64 `json:"output,omitempty"`
	CacheRead  float64 `json:"cache_read,omitempty"`
	CacheWrite float64 `json:"cache_write,omitempty"`
}

// ModelModalities are the media kinds a model takes and returns.
type ModelModalities struct {
	Input  []string `json:"input,omitempty"`
	Output []string `json:"output,omitempty"`
}

// Provider is one OpenAI-compatible endpoint and its model catalog.
type Provider struct {
	Name      string `json:"name,omitempty"`
	BaseURL   string `json:"base_url"`
	APIKey    string `json:"api_key,omitempty"`
	APIKeyEnv string `json:"api_key_env,omitempty"`
	// API selects the wire protocol: "chat" (default), "responses" or
	// "messages".
	API    string  `json:"api,omitempty"`
	OAuth  string  `json:"oauth,omitempty"`
	Models []Model `json:"models"`
	// Blacklist hides model ids from the picker; a non-empty whitelist
	// keeps only the ids it names. Both apply before variants expand.
	Blacklist []string `json:"blacklist,omitempty"`
	Whitelist []string `json:"whitelist,omitempty"`
}

// Reference is one extra directory the model may consult when relevant. A
// local reference is a path; a remote one names a git repository and branch.
type Reference struct {
	Name        string `json:"name"`
	Path        string `json:"path,omitempty"`
	Repository  string `json:"repository,omitempty"`
	Branch      string `json:"branch,omitempty"`
	Description string `json:"description,omitempty"`
	Hidden      bool   `json:"hidden,omitempty"`
}

// Rule is a permission rule; action and resource support wildcards.
type Rule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// File is the config file shape; fields use snake_case.
type File struct {
	Addr          string              `json:"addr"`
	DataDir       string              `json:"data_dir"`
	Workdir       string              `json:"workdir"`
	Model         string              `json:"model"`
	Provider      string              `json:"provider"`
	Providers     map[string]Provider `json:"providers"`
	ProvidersSet  bool                `json:"-"`
	BaseURL       string              `json:"base_url"`
	APIKey        string              `json:"api_key"`
	MaxSteps      int                 `json:"max_steps"`
	SubagentDepth int                 `json:"subagent_depth"`
	CodemodeOnly  *bool               `json:"codemode_only"`
	Shell         []string            `json:"shell"`
	Permission    []Rule              `json:"permission"`
	// PermissionAlways keeps approvals separate from configured rules, so a
	// saved allow can never override a deny set on purpose.
	PermissionAlways []Rule             `json:"permission_always"`
	Policies         []policy.Statement `json:"policies"`
	SystemPrompt     string             `json:"system_prompt"`
	Instructions     []string           `json:"instructions"`
	References       []Reference        `json:"references"`
	ModelPrompts     *bool              `json:"model_prompts"`
	ContextWindow    *int               `json:"context_window"`
	CompactKeep      int                `json:"compact_keep"`
	Titles           *bool              `json:"titles"`
	MCP              mcp.Config         `json:"mcp"`
	OAuth            oauth.Config       `json:"oauth"`
	Formatter        format.Config      `json:"formatter"`
	WebSearch        websearch.Config   `json:"websearch"`
	Image            imagegen.Config    `json:"image"`
	Media            *media.Config      `json:"media,omitempty"`
	Warming          *warming.Config    `json:"warming,omitempty"`
	Artifact         artifact.Config    `json:"artifact"`
	Schedule         schedule.Config    `json:"schedule"`
	Share            share.Config       `json:"share"`
	Agents           agents.Config      `json:"agents"`
	DefaultAgent     string             `json:"default_agent"`
	Commands         commands.Config    `json:"commands"`
	Plugins          plugin.Config      `json:"plugins"`
	Skills           skill.Config       `json:"skills"`
	LogLevel         string             `json:"log_level,omitempty"`
}

// DefaultPath returns the config file to load: an explicit LINGZHU_CONFIG wins,
// then a project lingzhu.json beside the working directory, then the user
// config file. The project file replaces the user file for that project; the
// returned path may not exist yet, so settings can be written to it.
func DefaultPath() string {
	return DefaultPathFor("")
}

// DefaultPathFor is DefaultPath for one workspace: an explicit LINGZHU_CONFIG
// wins, then that directory's own lingzhu.json, then the user config file.
func DefaultPathFor(directory string) string {
	if p := os.Getenv("LINGZHU_CONFIG"); p != "" {
		return p
	}
	dir := directory
	if dir == "" {
		if wd, err := os.Getwd(); err == nil {
			dir = wd
		}
	}
	if dir != "" {
		path := filepath.Join(dir, "lingzhu.json")
		if _, err := os.Stat(path); err == nil {
			return path
		}
	}
	return filepath.Join(global.Config(), "config.json")
}

// ForDir loads the configuration that applies to one workspace. A project file
// inside that directory replaces the shared config for the workspace, so two
// projects in one process keep their own providers, agents and tools.
func ForDir(directory, fallbackPath string) (Config, error) {
	path := filepath.Join(directory, "lingzhu.json")
	if _, err := os.Stat(path); err != nil {
		path = fallbackPath
	}
	cfg, err := Load(path)
	if err != nil {
		return Config{}, err
	}
	cfg.Workdir = directory
	return cfg, nil
}

// Load reads a config document and fills in defaults for missing fields.
func Load(path string) (Config, error) {
	cfg := Config{
		Addr:          "127.0.0.1:8787",
		DataDir:       global.Data(),
		Workdir:       "",
		BaseURL:       "https://api.openai.com/v1",
		MaxSteps:      0,
		SubagentDepth: 1,
		Shell:         DefaultShell(),
		ContextWindow: 128000,
		CompactKeep:   15000,
		Titles:        true,
		Media:         media.Default(),
		Warming:       warming.Default(),
	}

	if path != "" {
		file, err := read(path)
		if err != nil {
			return Config{}, err
		}
		applyFile(&cfg, file, filepath.Dir(path))
		cfg.Path = path
	}
	applyEnv(&cfg)
	return cfg, nil
}

func read(path string) (File, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return File{}, nil
	}
	if err != nil {
		return File{}, err
	}
	data = trimBOM(data)
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return File{}, err
	}
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return File{}, err
	}
	_, file.ProvidersSet = raw["providers"]
	return file, nil
}

// trimBOM drops a UTF-8 byte-order mark so a hand-edited config still decodes.
func trimBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
}

// resolvePath joins a relative config path with the config directory but keeps
// absolute paths untouched, including Windows drive-letter paths.
func resolvePath(dir, value string) string {
	value = strings.TrimSpace(value)
	if filepath.IsAbs(value) {
		return filepath.Clean(value)
	}
	return filepath.Join(dir, value)
}

func applyFile(cfg *Config, file File, dir string) {
	if file.Addr != "" {
		cfg.Addr = file.Addr
	}
	if file.DataDir != "" {
		cfg.DataDir = resolvePath(dir, file.DataDir)
	}
	if file.Workdir != "" {
		cfg.Workdir = resolvePath(dir, file.Workdir)
	}
	if file.Model != "" {
		cfg.Model = file.Model
	}
	if file.Provider != "" {
		cfg.Provider = strings.TrimSpace(file.Provider)
	}
	if file.ProvidersSet {
		cfg.ProvidersSet = true
		cfg.Providers = make(map[string]Provider, len(file.Providers))
		for id, provider := range file.Providers {
			provider.BaseURL = strings.TrimRight(strings.TrimSpace(provider.BaseURL), "/")
			for i := range provider.Models {
				provider.Models[i].ID = strings.TrimSpace(provider.Models[i].ID)
				if provider.Models[i].Name == "" {
					provider.Models[i].Name = provider.Models[i].ID
				}
			}
			cfg.Providers[id] = provider
		}
	}
	if file.BaseURL != "" {
		cfg.BaseURL = strings.TrimRight(file.BaseURL, "/")
	}
	if file.APIKey != "" {
		cfg.APIKey = file.APIKey
	}
	if file.MaxSteps > 0 {
		cfg.MaxSteps = file.MaxSteps
	}
	if value := strings.TrimSpace(file.LogLevel); value != "" {
		cfg.LogLevel = value
	}
	if file.SubagentDepth > 0 {
		cfg.SubagentDepth = file.SubagentDepth
	}
	if len(file.Shell) > 0 {
		cfg.Shell = file.Shell
	}
	if len(file.Permission) > 0 {
		cfg.Permission = file.Permission
	}
	if len(file.PermissionAlways) > 0 {
		cfg.PermissionAlways = file.PermissionAlways
	}
	if len(file.Policies) > 0 {
		cfg.Policies = policy.Normalize(file.Policies)
	}
	if file.SystemPrompt != "" {
		cfg.SystemPrompt = file.SystemPrompt
	}
	if file.CodemodeOnly != nil {
		cfg.CodemodeOnly = *file.CodemodeOnly
	}
	if file.ModelPrompts != nil {
		cfg.ModelPrompts = *file.ModelPrompts
	}
	if len(file.Instructions) > 0 {
		cfg.Instructions = append([]string(nil), file.Instructions...)
	}
	if len(file.References) > 0 {
		cfg.References = append([]Reference(nil), file.References...)
	}
	if file.ContextWindow != nil && *file.ContextWindow >= 0 {
		cfg.ContextWindow = *file.ContextWindow
	}
	if file.CompactKeep > 0 {
		cfg.CompactKeep = file.CompactKeep
	}
	if file.Titles != nil {
		cfg.Titles = *file.Titles
	}
	if len(file.MCP.Servers) > 0 {
		cfg.MCP = file.MCP
	}
	if len(file.Agents.Agents) > 0 {
		cfg.Agents = file.Agents
	}
	cfg.OAuth = file.OAuth
	cfg.Formatter = file.Formatter
	cfg.WebSearch = file.WebSearch
	cfg.Image = file.Image
	cfg.Artifact = file.Artifact
	cfg.Schedule = file.Schedule
	cfg.Share = file.Share
	if file.Media != nil {
		cfg.Media = media.Normalize(*file.Media)
	}
	if file.Warming != nil {
		cfg.Warming = warming.Normalize(*file.Warming)
	}
	if file.DefaultAgent != "" {
		cfg.DefaultAgent = file.DefaultAgent
	}
	if len(file.Commands) > 0 {
		cfg.Commands = file.Commands
	}
	if len(file.Plugins.Plugins) > 0 {
		cfg.Plugins = file.Plugins
	}
	if len(file.Skills.Paths) > 0 || len(file.Skills.URLs) > 0 {
		cfg.Skills = file.Skills
	}
}

func applyEnv(cfg *Config) {
	set := func(key string, target *string) {
		if v := os.Getenv(key); v != "" {
			*target = v
		}
	}
	set("LINGZHU_ADDR", &cfg.Addr)
	set("LINGZHU_DATA_DIR", &cfg.DataDir)
	set("LINGZHU_WORKDIR", &cfg.Workdir)
	set("LINGZHU_MODEL", &cfg.Model)
	set("LINGZHU_PROVIDER", &cfg.Provider)
	set("LINGZHU_SYSTEM_PROMPT", &cfg.SystemPrompt)
	set("OPENAI_API_KEY", &cfg.APIKey)
	if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
		cfg.BaseURL = strings.TrimRight(v, "/")
	}
	if n, err := strconv.Atoi(os.Getenv("LINGZHU_MAX_STEPS")); err == nil && n > 0 {
		cfg.MaxSteps = n
	}
	if value := strings.TrimSpace(os.Getenv("LINGZHU_LOG_LEVEL")); value != "" {
		cfg.LogLevel = value
	}
	if value, err := strconv.ParseBool(os.Getenv("LINGZHU_MODEL_PROMPTS")); err == nil {
		cfg.ModelPrompts = value
	}
	if n, err := strconv.Atoi(os.Getenv("LINGZHU_CONTEXT_WINDOW")); err == nil && n >= 0 {
		cfg.ContextWindow = n
	}
	if n, err := strconv.Atoi(os.Getenv("LINGZHU_COMPACT_KEEP")); err == nil && n > 0 {
		cfg.CompactKeep = n
	}
}

// SavePermission replaces the learned-approval list in the config file and
// keeps every other field, so an "always allow" answer survives a restart
// without touching the configured permission rules. The write goes through a
// temp file in the same directory.
func SavePermission(path string, rules []Rule) error {
	if path == "" {
		return errors.New("config path is unknown")
	}
	file := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(trimBOM(data), &file); err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
	}
	file["permission_always"] = rules
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(out); err != nil {
		tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		_ = os.Remove(name)
		return err
	}
	return nil
}

// DefaultShell returns the command prefix used by the built-in shell tool.
func DefaultShell() []string {
	if runtime.GOOS == "windows" {
		return defaultWindowsShell()
	}
	return []string{"bash", "-lc"}
}

// defaultWindowsShell mirrors the reference harness: PowerShell 7 when it is
// installed, then Windows PowerShell, Git Bash, and finally the command
// interpreter. A missing candidate is skipped instead of treated as an error.
func defaultWindowsShell() []string {
	if path, err := exec.LookPath("pwsh"); err == nil {
		return []string{path, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command"}
	}
	if path, err := exec.LookPath("powershell"); err == nil {
		return []string{path, "-NoLogo", "-NoProfile", "-NonInteractive", "-Command"}
	}
	if git, err := exec.LookPath("git"); err == nil {
		bash := filepath.Join(git, "..", "..", "bin", "bash.exe")
		if info, statErr := os.Stat(bash); statErr == nil && info.Size() > 0 {
			return []string{bash, "-c"}
		}
	}
	cmd := strings.TrimSpace(os.Getenv("COMSPEC"))
	if cmd == "" {
		cmd = "cmd.exe"
	}
	return []string{cmd, "/c"}
}

// Update merges top-level fields into the config file, creating it if needed.
// A nil value removes the field; other values are stored as JSON. Existing
// unrelated fields and credentials are preserved.
func Update(path string, fields map[string]any) error {
	if strings.TrimSpace(path) == "" {
		return errors.New("config path is unknown")
	}
	file := map[string]any{}
	data, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if len(data) > 0 {
		if err := json.Unmarshal(trimBOM(data), &file); err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if file == nil {
			file = map[string]any{}
		}
	}
	for key, value := range fields {
		if value == nil {
			delete(file, key)
			continue
		}
		file[key] = value
	}
	out, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return err
	}
	out = append(out, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	temp := path + ".tmp"
	if err := os.WriteFile(temp, out, 0o600); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		_ = os.Remove(temp)
		return err
	}
	return nil
}
