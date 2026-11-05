// Package global resolves the per-user directories the core uses. Nothing in
// the source tree is a runtime location: configuration, data, state and cache
// live under the user's own directories, following the XDG layout on Unix and
// AppData/LocalAppData on Windows.
package global

import (
	"os"
	"path/filepath"
	"runtime"
)

const app = "lingzhu"

// Override returns the directory LINGZHU_HOME points at, if any. It redirects
// data, state, cache and tmp so a development run never writes to the real
// user directories; configuration stays shared unless LINGZHU_CONFIG is set.
func Override() string { return os.Getenv("LINGZHU_HOME") }

// Config is the directory holding config.json.
func Config() string {
	if p := os.Getenv("LINGZHU_CONFIG_DIR"); p != "" {
		return p
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(userConfigDir(), app)
	}
	if p := os.Getenv("XDG_CONFIG_HOME"); p != "" {
		return filepath.Join(p, app)
	}
	return filepath.Join(homeDir(), ".config", app)
}

// Data is the directory holding durable agent data, such as session databases.
func Data() string {
	if p := Override(); p != "" {
		return filepath.Join(p, "data")
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(localAppData(), "data")
	}
	if p := os.Getenv("XDG_DATA_HOME"); p != "" {
		return filepath.Join(p, app)
	}
	return filepath.Join(homeDir(), ".local", "share", app)
}

// State is the directory holding machine-local state that is not user content.
func State() string {
	if p := Override(); p != "" {
		return filepath.Join(p, "state")
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(localAppData(), "state")
	}
	if p := os.Getenv("XDG_STATE_HOME"); p != "" {
		return filepath.Join(p, app)
	}
	return filepath.Join(homeDir(), ".local", "state", app)
}

// Cache is the directory holding disposable files, including logs.
func Cache() string {
	if p := Override(); p != "" {
		return filepath.Join(p, "cache")
	}
	if runtime.GOOS == "windows" {
		return filepath.Join(localAppData(), "cache")
	}
	if p := os.Getenv("XDG_CACHE_HOME"); p != "" {
		return filepath.Join(p, app)
	}
	return filepath.Join(homeDir(), ".cache", app)
}

// Log is the directory holding process logs.
func Log() string { return filepath.Join(Cache(), "log") }

// Tmp is the directory holding scratch files that may be deleted at any time.
func Tmp() string {
	if p := Override(); p != "" {
		return filepath.Join(p, "tmp")
	}
	return filepath.Join(os.TempDir(), app)
}

// localAppData is %LOCALAPPDATA% on Windows and the XDG cache root elsewhere.
func localAppData() string {
	if p := os.Getenv("LOCALAPPDATA"); p != "" {
		return filepath.Join(p, app)
	}
	if base, err := os.UserCacheDir(); err == nil {
		return filepath.Join(base, app)
	}
	return filepath.Join(homeDir(), app)
}

func userConfigDir() string {
	if base, err := os.UserConfigDir(); err == nil {
		return base
	}
	return homeDir()
}

func homeDir() string {
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		return home
	}
	return os.TempDir()
}
