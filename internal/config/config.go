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
	Addr       string
	DataDir    string
	Model      string
	BaseURL    string
	APIKey     string
	MaxSteps   int
	Shell      []string
	Permission []Rule
}

// Rule is a permission rule; action and resource support wildcards.
type Rule struct {
	Action   string `json:"action"`
	Resource string `json:"resource"`
	Effect   string `json:"effect"`
}

// File is the config file shape; fields use snake_case.
type File struct {
	Addr       string   `json:"addr"`
	DataDir    string   `json:"data_dir"`
	Model      string   `json:"model"`
	BaseURL    string   `json:"base_url"`
	APIKey     string   `json:"api_key"`
	MaxSteps   int      `json:"max_steps"`
	Shell      []string `json:"shell"`
	Permission []Rule   `json:"permission"`
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
		Addr:     "127.0.0.1:8787",
		DataDir:  "data",
		BaseURL:  "https://api.openai.com/v1",
		MaxSteps: 16,
		Shell:    DefaultShell(),
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
	if file.Model != "" {
		cfg.Model = file.Model
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
}

func applyEnv(cfg *Config) {
	set := func(key string, target *string) {
		if v := os.Getenv(key); v != "" {
			*target = v
		}
	}
	set("LINGZHU_ADDR", &cfg.Addr)
	set("LINGZHU_DATA_DIR", &cfg.DataDir)
	set("LINGZHU_MODEL", &cfg.Model)
	set("OPENAI_API_KEY", &cfg.APIKey)
	if v := os.Getenv("OPENAI_BASE_URL"); v != "" {
		cfg.BaseURL = strings.TrimRight(v, "/")
	}
	if n, err := strconv.Atoi(os.Getenv("LINGZHU_MAX_STEPS")); err == nil && n > 0 {
		cfg.MaxSteps = n
	}
}

// DefaultShell returns the command prefix used by the built-in bash tool.
func DefaultShell() []string {
	if runtime.GOOS == "windows" {
		return []string{"powershell", "-NoProfile", "-NonInteractive", "-Command"}
	}
	return []string{"bash", "-lc"}
}
