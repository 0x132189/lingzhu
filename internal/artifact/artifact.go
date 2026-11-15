// Package artifact stores self-contained file bundles and serves them over
// HTTP. Artifacts are addressed by an unguessable id and live under the user
// data directory, so nothing is published outside the local core.
package artifact

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	maxFiles      = 64
	maxFileBytes  = 5 * 1024 * 1024
	maxTotalBytes = 20 * 1024 * 1024
	metaFile      = "artifact.json"
	filesDir      = "files"
)

var idPattern = regexp.MustCompile(`^art_[0-9a-f]{16}$`)

// Config is the artifact section of the configuration file.
type Config struct {
	Enabled *bool  `json:"enabled,omitempty"`
	BaseURL string `json:"base_url,omitempty"`
}

// Info is artifact metadata.
type Info struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Entry     string    `json:"entry"`
	Files     []string  `json:"files"`
	URL       string    `json:"url"`
	SessionID string    `json:"sessionId,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

// Content is metadata plus every file body.
type Content struct {
	Info
	Files map[string]string `json:"contents"`
}

type record struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Entry     string    `json:"entry"`
	SessionID string    `json:"sessionId,omitempty"`
	Created   time.Time `json:"created"`
	Updated   time.Time `json:"updated"`
}

// Manager owns the artifact directory.
type Manager struct {
	dir     string
	baseURL string
	mu      sync.Mutex
}

// New prepares the artifact store. A nil or false enabled flag disables it.
func New(dataDir string, cfg Config) (*Manager, error) {
	if cfg.Enabled != nil && !*cfg.Enabled {
		return nil, nil
	}
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("artifact store needs a data directory")
	}
	dir := filepath.Join(dataDir, "artifacts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Manager{dir: dir, baseURL: strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")}, nil
}

// Enabled reports whether artifacts can be created.
func (m *Manager) Enabled() bool { return m != nil }

// Create stores a new artifact and returns its metadata.
func (m *Manager) Create(title string, files map[string]string, entry, sessionID string) (Info, error) {
	if m == nil {
		return Info{}, errors.New("artifacts are disabled")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return Info{}, errors.New("artifact title is empty")
	}
	clean, order, err := normalizeFiles(files)
	if err != nil {
		return Info{}, err
	}
	resolved, err := resolveEntry(entry, order)
	if err != nil {
		return Info{}, err
	}
	id, err := newID()
	if err != nil {
		return Info{}, err
	}
	now := time.Now().UTC()
	meta := record{ID: id, Title: title, Entry: resolved, SessionID: strings.TrimSpace(sessionID), Created: now, Updated: now}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := install(m.dir, id, meta, clean, order); err != nil {
		return Info{}, err
	}
	return m.infoLocked(meta), nil
}

// Update replaces an artifact's files at the same id and URL.
func (m *Manager) Update(id, title string, files map[string]string, entry, sessionID string) (Info, error) {
	if m == nil {
		return Info{}, errors.New("artifacts are disabled")
	}
	if !idPattern.MatchString(strings.TrimSpace(id)) {
		return Info{}, fmt.Errorf("invalid artifact id %q", id)
	}
	clean, order, err := normalizeFiles(files)
	if err != nil {
		return Info{}, err
	}
	resolved, err := resolveEntry(entry, order)
	if err != nil {
		return Info{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	meta, err := m.readMetaLocked(id)
	if err != nil {
		return Info{}, err
	}
	if strings.TrimSpace(title) != "" {
		meta.Title = strings.TrimSpace(title)
	}
	if strings.TrimSpace(sessionID) != "" {
		meta.SessionID = strings.TrimSpace(sessionID)
	}
	meta.Entry = resolved
	meta.Updated = time.Now().UTC()
	if err := install(m.dir, id, meta, clean, order); err != nil {
		return Info{}, err
	}
	return m.infoLocked(meta), nil
}

// Get returns metadata and every file body.
func (m *Manager) Get(id string) (Content, error) {
	if m == nil {
		return Content{}, errors.New("artifacts are disabled")
	}
	if !idPattern.MatchString(strings.TrimSpace(id)) {
		return Content{}, fmt.Errorf("invalid artifact id %q", id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	meta, err := m.readMetaLocked(id)
	if err != nil {
		return Content{}, err
	}
	order, err := m.fileOrderLocked(id)
	if err != nil {
		return Content{}, err
	}
	contents := make(map[string]string, len(order))
	for _, name := range order {
		data, err := os.ReadFile(filepath.Join(m.dir, id, filesDir, filepath.FromSlash(name)))
		if err != nil {
			return Content{}, err
		}
		contents[name] = string(data)
	}
	return Content{Info: m.infoLocked(meta), Files: contents}, nil
}

// Delete removes an artifact and its stored files.
func (m *Manager) Delete(id string) error {
	if m == nil {
		return errors.New("artifacts are disabled")
	}
	if !idPattern.MatchString(strings.TrimSpace(id)) {
		return fmt.Errorf("invalid artifact id %q", id)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.readMetaLocked(id); err != nil {
		return err
	}
	return os.RemoveAll(filepath.Join(m.dir, id))
}

// List returns metadata for every stored artifact, newest first.
func (m *Manager) List() ([]Info, error) {
	if m == nil {
		return []Info{}, nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return []Info{}, nil
		}
		return nil, err
	}
	out := make([]Info, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || !idPattern.MatchString(entry.Name()) {
			continue
		}
		meta, err := m.readMetaLocked(entry.Name())
		if err != nil {
			continue
		}
		out = append(out, m.infoLocked(meta))
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.After(out[j].Updated)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// OpenFile opens one stored file for serving. It rejects ids and paths that
// could escape the artifact directory.
func (m *Manager) OpenFile(id, name string) (io.ReadCloser, string, error) {
	if m == nil {
		return nil, "", errors.New("artifacts are disabled")
	}
	if !idPattern.MatchString(strings.TrimSpace(id)) {
		return nil, "", fmt.Errorf("invalid artifact id %q", id)
	}
	name = strings.TrimSpace(name)
	if !safeRelative(name) {
		return nil, "", fmt.Errorf("invalid artifact path %q", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, err := m.readMetaLocked(id); err != nil {
		return nil, "", err
	}
	file, err := os.Open(filepath.Join(m.dir, id, filesDir, filepath.FromSlash(name)))
	if err != nil {
		return nil, "", err
	}
	return file, MediaType(name), nil
}

func (m *Manager) infoLocked(meta record) Info {
	order, _ := m.fileOrderLocked(meta.ID)
	entry := meta.Entry
	url := ""
	if entry != "" {
		url = m.baseURL + "/artifacts/" + meta.ID + "/raw/" + escapePath(entry)
	}
	return Info{
		ID:        meta.ID,
		Title:     meta.Title,
		Entry:     entry,
		Files:     order,
		URL:       url,
		SessionID: meta.SessionID,
		Created:   meta.Created,
		Updated:   meta.Updated,
	}
}

func (m *Manager) readMetaLocked(id string) (record, error) {
	data, err := os.ReadFile(filepath.Join(m.dir, id, metaFile))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return record{}, fmt.Errorf("artifact %s not found", id)
		}
		return record{}, err
	}
	var meta record
	if err := json.Unmarshal(data, &meta); err != nil {
		return record{}, err
	}
	if meta.ID != id {
		return record{}, fmt.Errorf("artifact %s is corrupted", id)
	}
	return meta, nil
}

func (m *Manager) fileOrderLocked(id string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(m.dir, id, metaFile))
	if err != nil {
		return nil, err
	}
	var meta struct {
		Order []string `json:"order"`
	}
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, err
	}
	return append([]string(nil), meta.Order...), nil
}

func install(root, id string, meta record, files map[string]string, order []string) error {
	staging := filepath.Join(root, ".tmp-"+id+"-"+fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := os.MkdirAll(filepath.Join(staging, filesDir), 0o755); err != nil {
		return err
	}
	for _, name := range order {
		destination := filepath.Join(staging, filesDir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(destination, []byte(files[name]), 0o644); err != nil {
			return err
		}
	}
	payload, err := json.MarshalIndent(struct {
		record
		Order []string `json:"order"`
	}{record: meta, Order: order}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(staging, metaFile), payload, 0o644); err != nil {
		return err
	}

	target := filepath.Join(root, id)
	backup := target + ".old-" + fmt.Sprintf("%d", time.Now().UnixNano())
	if _, err := os.Stat(target); err == nil {
		if err := os.Rename(target, backup); err != nil {
			return err
		}
	}
	if err := os.Rename(staging, target); err != nil {
		_ = os.Rename(backup, target)
		return err
	}
	if backup != "" {
		_ = os.RemoveAll(backup)
	}
	return nil
}

func normalizeFiles(files map[string]string) (map[string]string, []string, error) {
	if len(files) == 0 {
		return nil, nil, errors.New("artifact needs at least one file")
	}
	if len(files) > maxFiles {
		return nil, nil, fmt.Errorf("artifact has more than %d files", maxFiles)
	}
	clean := make(map[string]string, len(files))
	order := make([]string, 0, len(files))
	total := 0
	for name, body := range files {
		name = strings.TrimSpace(name)
		if !safeRelative(name) {
			return nil, nil, fmt.Errorf("invalid artifact path %q", name)
		}
		if _, exists := clean[name]; exists {
			return nil, nil, fmt.Errorf("duplicate artifact path %q", name)
		}
		if len(body) > maxFileBytes {
			return nil, nil, fmt.Errorf("artifact file %q exceeds 5 MiB", name)
		}
		total += len(body)
		clean[name] = body
		order = append(order, name)
	}
	if total > maxTotalBytes {
		return nil, nil, fmt.Errorf("artifact exceeds 20 MiB")
	}
	sort.Strings(order)
	return clean, order, nil
}

func resolveEntry(entry string, order []string) (string, error) {
	entry = strings.TrimSpace(entry)
	if entry != "" {
		if !safeRelative(entry) {
			return "", fmt.Errorf("invalid artifact entry %q", entry)
		}
		for _, name := range order {
			if name == entry {
				return entry, nil
			}
		}
		return "", fmt.Errorf("artifact entry %q is not one of the files", entry)
	}
	for _, candidate := range []string{"index.html", "index.md"} {
		for _, name := range order {
			if name == candidate {
				return candidate, nil
			}
		}
	}
	if len(order) == 1 {
		return order[0], nil
	}
	return "", errors.New("artifact entry is required when index.html and index.md are absent")
}

func safeRelative(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\\x00?#") || strings.HasPrefix(value, "/") {
		return false
	}
	if path.Clean(value) != value || strings.HasPrefix(value, "../") || value == ".." {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func escapePath(value string) string {
	parts := strings.Split(value, "/")
	for i, part := range parts {
		parts[i] = pathEscape(part)
	}
	return strings.Join(parts, "/")
}

func pathEscape(value string) string {
	var b strings.Builder
	for i := 0; i < len(value); i++ {
		c := value[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}

func newID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "art_" + hex.EncodeToString(buf), nil
}

// MediaType maps a file extension to a content type.
func MediaType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".html", ".htm":
		return "text/html; charset=utf-8"
	case ".css":
		return "text/css; charset=utf-8"
	case ".js", ".mjs":
		return "text/javascript; charset=utf-8"
	case ".json":
		return "application/json; charset=utf-8"
	case ".md":
		return "text/markdown; charset=utf-8"
	case ".txt":
		return "text/plain; charset=utf-8"
	case ".svg":
		return "image/svg+xml"
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".pdf":
		return "application/pdf"
	case ".wasm":
		return "application/wasm"
	}
	return "application/octet-stream"
}
