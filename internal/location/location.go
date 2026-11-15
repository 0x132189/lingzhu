// Package location keeps one workspace's services apart from every other
// workspace in the same process. A core can serve several directories at once;
// each one gets its own tools, snapshots, formatter, worktrees, terminals, MCP
// servers, plugins, skills, commands and agent profiles, so nothing leaks from
// one project into another.
package location

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Ref identifies one workspace. Directory is absolute and cleaned; a caller
// never compares raw user input against it.
type Ref struct {
	Directory string `json:"directory"`
}

// Make normalizes a directory into a Ref.
func Make(directory string) (Ref, error) {
	dir := strings.TrimSpace(directory)
	if dir == "" {
		return Ref{}, fmt.Errorf("location: directory is empty")
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Ref{}, err
	}
	return Ref{Directory: filepath.Clean(abs)}, nil
}

// Key is the map key and on-disk slug for this location. Windows and macOS are
// case-insensitive, so the key lowercases the path to stop one project from
// being opened twice under two spellings.
func (r Ref) Key() string {
	return strings.ToLower(filepath.Clean(r.Directory))
}

// Name is the display name: the final path element.
func (r Ref) Name() string {
	return filepath.Base(filepath.Clean(r.Directory))
}

// DataDir returns the per-location directory under the core's data directory.
// Two workspaces never share a session store, a snapshot repository or a
// terminal log directory.
func (r Ref) DataDir(base string) string {
	sum := sha256.Sum256([]byte(r.Key()))
	slug := slugify(r.Name())
	return filepath.Join(base, "locations", fmt.Sprintf("%s-%s", slug, hex.EncodeToString(sum[:4])))
}

// slugify reduces a directory name to a safe path element.
func slugify(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == '.':
			b.WriteRune(r)
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "workspace"
	}
	if len(slug) > 32 {
		slug = slug[:32]
	}
	return slug
}

// Exists reports whether the directory is still on disk.
func (r Ref) Exists() bool {
	info, err := os.Stat(r.Directory)
	return err == nil && info.IsDir()
}
