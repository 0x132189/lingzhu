// Package config provides all runtime configuration.
// Precedence: defaults < config file < environment < command line.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

type Config struct {
	Addr          string
	DataDir       string
	Workdir       string
	Model         string
	Provider      string
	Providers     map[string]Provider
	BaseURL       string
	APIKey        string
	MaxSteps      int
	Shell         []string
	Permission    []Rule
	SystemPrompt  string
	ContextWindow int
	CompactKeep   int
}

// Model is one selectable model inside a provider.
type Model struct {
	ID            string `json:"id"`
	Name          string `json:"name,omitempty"`
	ContextWindow *int   `json:"context_window,omitempty"`
}

// Provider is one OpenAI-compatible endpoint and its model catalog.
type Provider struct {
	Name      string  `json:"name,omitempty"`
	BaseURL   string  `json:"base_url"`
	APIKey    string  `json:"api_key,omitempty"`
	APIKeyEnv string  `json:"api_key_env,omitempty"`
	Models    []Model `json:"models"`
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
	BaseURL       string              `json:"base_url"`
	APIKey        string              `json:"api_key"`
	MaxSteps      int                 `json:"max_steps"`
	Shell         []string            `json:"shell"`
	Permission    []Rule              `json:"permission"`
	SystemPrompt  string              `json:"system_prompt"`
	ContextWindow *int                `json:"context_window"`
	CompactKeep   int                 `json:"compact_keep"`
}

// DefaultPath returns the config file path: an explicit path wins, otherwise
// config.json, build/config.json and ../build/config.json are tried in order.
func DefaultPath() string {
	if p := os.Getenv("LINGZHU_CONFIG"); p != "" {
		return p
	}
	for _, candidate := range []string{"config.json", "build/config.json", filepath.Join("..", "build", "config.json")} {
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

func Load(path string) (Config, error) {
	cfg := Config{
		Addr:          "127.0.0.1:8787",
		DataDir:       "data",
		Workdir:       "",
		BaseURL:       "https://api.openai.com/v1",
		MaxSteps:      16,
		Shell:         DefaultShell(),
		ContextWindow: 128000,
		CompactKeep:   15000,
	}

	if path != "" {
		file, err := read(path)
		if err != nil {
			return Config{}, err
		}
		applyFile(&cfg, file, filepath.Dir(path))
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
	var file File
	if err := json.Unmarshal(data, &file); err != nil {
		return File{}, err
	}
	return file, nil
}

func applyFile(cfg *Config, file File, dir string) {
	if file.Addr != "" {
		cfg.Addr = file.Addr
	}
	if file.DataDir != "" {
		cfg.DataDir = filepath.Join(dir, file.DataDir)
	}
	if file.Workdir != "" {
		cfg.Workdir = filepath.Join(dir, file.Workdir)
	}
	if file.Model != "" {
		cfg.Model = file.Model
	}
	if file.Provider != "" {
		cfg.Provider = strings.TrimSpace(file.Provider)
	}
	if len(file.Providers) > 0 {
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
	if len(file.Shell) > 0 {
		cfg.Shell = file.Shell
	}
	if len(file.Permission) > 0 {
		cfg.Permission = file.Permission
	}
	if file.SystemPrompt != "" {
		cfg.SystemPrompt = file.SystemPrompt
	}
	if file.ContextWindow != nil && *file.ContextWindow >= 0 {
		cfg.ContextWindow = *file.ContextWindow
	}
	if file.CompactKeep > 0 {
		cfg.CompactKeep = file.CompactKeep
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
	if n, err := strconv.Atoi(os.Getenv("LINGZHU_CONTEXT_WINDOW")); err == nil && n >= 0 {
		cfg.ContextWindow = n
	}
	if n, err := strconv.Atoi(os.Getenv("LINGZHU_COMPACT_KEEP")); err == nil && n > 0 {
		cfg.CompactKeep = n
	}
}

// DefaultShell returns the command prefix used by the built-in bash tool.
func DefaultShell() []string {
	if runtime.GOOS == "windows" {
		return []string{"powershell", "-NoProfile", "-NonInteractive", "-Command"}
	}
	return []string{"bash", "-lc"}
}
