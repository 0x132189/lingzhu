package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	maxFileEntries = 1000
	maxFileBytes   = 256 * 1024
	maxWriteBytes  = 5 * 1024 * 1024
)

var errOutsideWorkspace = errors.New("path is outside the workspace")

type fsEntry struct {
	Name     string    `json:"name"`
	Path     string    `json:"path"`
	Type     string    `json:"type"`
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified,omitempty"`
	Hidden   bool      `json:"hidden,omitempty"`
}

// listFiles lists one directory inside the workspace.
// fsSearchHit is one file matched by a workspace-wide name search.
type fsSearchHit struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
}

var fsSearchSkip = map[string]bool{
	".git":         true,
	".hg":          true,
	".svn":         true,
	"node_modules": true,
	"vendor":       true,
	"dist":         true,
	"build":        true,
	".venv":        true,
	"venv":         true,
	"__pycache__":  true,
	".next":        true,
	"target":       true,
}

// searchFiles finds files whose relative path contains the query.
func (s *Server) searchFiles(w http.ResponseWriter, r *http.Request) {
	query := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("q")))
	if query == "" {
		writeError(w, http.StatusBadRequest, errText("query is empty"))
		return
	}
	root := s.rootFor(r)
	if strings.TrimSpace(root) == "" {
		writeFileError(w, errors.New("workspace is not configured"))
		return
	}
	limit := 50
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, errText("limit must be a positive integer"))
			return
		}
		limit = parsed
	}
	if limit > 200 {
		limit = 200
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		writeFileError(w, err)
		return
	}
	root = absRoot
	hits := []fsSearchHit{}
	truncated := false
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || fsSearchSkip[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)
		if !strings.Contains(strings.ToLower(relative), query) {
			return nil
		}
		if len(hits) >= limit {
			truncated = true
			return filepath.SkipAll
		}
		size := int64(0)
		if info, err := entry.Info(); err == nil {
			size = info.Size()
		}
		hits = append(hits, fsSearchHit{Path: relative, Size: size})
		return nil
	})
	if err != nil {
		writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": hits, "truncated": truncated})
}

// fsGrepHit is one matching line in a workspace file.
type fsGrepHit struct {
	Path string `json:"path"`
	Line int    `json:"line"`
	Text string `json:"text"`
}

const (
	maxGrepFileBytes = 1 << 20
	maxGrepLineRunes = 300
)

// grepFiles searches text file contents for a case-insensitive substring.
func (s *Server) grepFiles(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("q"))
	if query == "" {
		writeError(w, http.StatusBadRequest, errText("query is empty"))
		return
	}
	root := s.rootFor(r)
	if strings.TrimSpace(root) == "" {
		writeFileError(w, errors.New("workspace is not configured"))
		return
	}
	limit := 50
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed <= 0 {
			writeError(w, http.StatusBadRequest, errText("limit must be a positive integer"))
			return
		}
		limit = parsed
	}
	if limit > 200 {
		limit = 200
	}
	root, err := filepath.Abs(root)
	if err != nil {
		writeFileError(w, err)
		return
	}
	needle := strings.ToLower(query)
	hits := []fsGrepHit{}
	truncated := false
	err = filepath.WalkDir(root, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			if entry != nil && entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		name := entry.Name()
		if entry.IsDir() {
			if path != root && (strings.HasPrefix(name, ".") || fsSearchSkip[name]) {
				return filepath.SkipDir
			}
			return nil
		}
		info, err := entry.Info()
		if err != nil || info.Size() > maxGrepFileBytes {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil || bytes.IndexByte(data, 0) >= 0 {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		relative = filepath.ToSlash(relative)
		for index, line := range strings.Split(string(data), "\n") {
			if !strings.Contains(strings.ToLower(line), needle) {
				continue
			}
			if len(hits) >= limit {
				truncated = true
				return filepath.SkipAll
			}
			text := strings.TrimSpace(line)
			if runes := []rune(text); len(runes) > maxGrepLineRunes {
				text = string(runes[:maxGrepLineRunes]) + "…"
			}
			hits = append(hits, fsGrepHit{Path: relative, Line: index + 1, Text: text})
		}
		return nil
	})
	if err != nil {
		writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"hits": hits, "truncated": truncated})
}

func (s *Server) listFiles(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	abs, rel, err := resolveWorkspacePath(s.rootFor(r), query.Get("path"))
	if err != nil {
		writeFileError(w, err)
		return
	}
	showHidden := isTrue(query.Get("hidden"))
	items, err := os.ReadDir(abs)
	if err != nil {
		writeFileError(w, err)
		return
	}
	entries := make([]fsEntry, 0, len(items))
	truncated := false
	for _, item := range items {
		name := item.Name()
		hidden := strings.HasPrefix(name, ".")
		if hidden && !showHidden {
			continue
		}
		if len(entries) >= maxFileEntries {
			truncated = true
			break
		}
		info, err := item.Info()
		if err != nil {
			continue
		}
		kind := "file"
		switch {
		case item.IsDir():
			kind = "dir"
		case item.Type()&fs.ModeSymlink != 0:
			kind = "symlink"
		}
		child := name
		if rel != "" {
			child = rel + "/" + name
		}
		entry := fsEntry{Name: name, Path: child, Type: kind, Hidden: hidden}
		if kind == "file" {
			entry.Size = info.Size()
		}
		entry.Modified = info.ModTime().UTC()
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool {
		if (entries[i].Type == "dir") != (entries[j].Type == "dir") {
			return entries[i].Type == "dir"
		}
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"path":      rel,
		"entries":   entries,
		"truncated": truncated,
	})
}

// readFile returns a bounded text preview of one workspace file.
func (s *Server) readFile(w http.ResponseWriter, r *http.Request) {
	abs, rel, err := resolveWorkspacePath(s.rootFor(r), r.URL.Query().Get("path"))
	if err != nil {
		writeFileError(w, err)
		return
	}
	info, err := os.Lstat(abs)
	if err != nil {
		writeFileError(w, err)
		return
	}
	if info.IsDir() {
		writeError(w, http.StatusBadRequest, errText("path is a directory"))
		return
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		writeError(w, http.StatusBadRequest, errText("symlinks are not supported"))
		return
	}
	file, err := os.Open(abs)
	if err != nil {
		writeFileError(w, err)
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxFileBytes+1))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	truncated := len(data) > maxFileBytes
	if truncated {
		data = data[:maxFileBytes]
	}
	binary := bytes.IndexByte(data, 0) >= 0
	content := ""
	if !binary {
		content = strings.ToValidUTF8(string(data), "")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"path":      rel,
		"content":   content,
		"truncated": truncated,
		"binary":    binary,
		"size":      info.Size(),
	})
}

// writeFile replaces one workspace file with request content.
func (s *Server) writeFile(w http.ResponseWriter, r *http.Request) {
	abs, rel, err := resolveWorkspacePath(s.rootFor(r), r.URL.Query().Get("path"))
	if err != nil {
		writeFileError(w, err)
		return
	}
	if rel == "" {
		writeError(w, http.StatusBadRequest, errText("path is required"))
		return
	}
	mode := fs.FileMode(0o644)
	info, err := os.Lstat(abs)
	switch {
	case err == nil:
		if info.IsDir() {
			writeError(w, http.StatusBadRequest, errText("path is a directory"))
			return
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			writeError(w, http.StatusBadRequest, errText("symlinks are not supported"))
			return
		}
		mode = info.Mode().Perm()
	case !errors.Is(err, os.ErrNotExist):
		writeFileError(w, err)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxWriteBytes+64*1024)
	var body struct {
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, errText("content is too large"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if len(body.Content) > maxWriteBytes {
		writeError(w, http.StatusRequestEntityTooLarge, errText("content is too large"))
		return
	}
	if err := os.WriteFile(abs, []byte(body.Content), mode); err != nil {
		writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"path": rel, "size": len(body.Content)})
}

// createEntry creates one file or directory inside the workspace.
func (s *Server) createEntry(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWriteBytes+64*1024)
	var body struct {
		Path    string `json:"path"`
		Type    string `json:"type"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			writeError(w, http.StatusRequestEntityTooLarge, errText("content is too large"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	kind := strings.TrimSpace(body.Type)
	if kind == "" {
		kind = "file"
	}
	if kind != "file" && kind != "dir" {
		writeError(w, http.StatusBadRequest, errText("type must be file or dir"))
		return
	}
	if len(body.Content) > maxWriteBytes {
		writeError(w, http.StatusRequestEntityTooLarge, errText("content is too large"))
		return
	}
	abs, rel, err := resolveWorkspacePath(s.rootFor(r), body.Path)
	if err != nil {
		writeFileError(w, err)
		return
	}
	if rel == "" {
		writeError(w, http.StatusBadRequest, errText("path is required"))
		return
	}
	if _, err := os.Lstat(abs); err == nil {
		writeError(w, http.StatusConflict, errText("path already exists"))
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		writeFileError(w, err)
		return
	}
	if kind == "dir" {
		err = os.Mkdir(abs, 0o755)
	} else {
		err = os.WriteFile(abs, []byte(body.Content), 0o644)
	}
	if err != nil {
		writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"path": rel,
		"type": kind,
		"size": len(body.Content),
	})
}

// deleteFile removes one workspace file, symlink or empty directory.
func (s *Server) deleteFile(w http.ResponseWriter, r *http.Request) {
	abs, rel, err := resolveWorkspacePath(s.rootFor(r), r.URL.Query().Get("path"))
	if err != nil {
		writeFileError(w, err)
		return
	}
	if rel == "" {
		writeError(w, http.StatusBadRequest, errText("cannot delete the workspace root"))
		return
	}
	if _, err := os.Lstat(abs); err != nil {
		writeFileError(w, err)
		return
	}
	if err := os.Remove(abs); err != nil {
		writeFileError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// moveFile renames or moves one workspace entry. The destination must not exist.
func (s *Server) moveFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	fromAbs, fromRel, err := resolveWorkspacePath(s.rootFor(r), body.From)
	if err != nil {
		writeFileError(w, err)
		return
	}
	toAbs, toRel, err := resolveWorkspacePath(s.rootFor(r), body.To)
	if err != nil {
		writeFileError(w, err)
		return
	}
	if fromRel == "" || toRel == "" {
		writeError(w, http.StatusBadRequest, errText("from and to are required"))
		return
	}
	if _, err := os.Lstat(fromAbs); err != nil {
		writeFileError(w, err)
		return
	}
	if _, err := os.Lstat(toAbs); err == nil {
		writeError(w, http.StatusConflict, errText("destination already exists"))
		return
	} else if !errors.Is(err, os.ErrNotExist) {
		writeFileError(w, err)
		return
	}
	if err := os.Rename(fromAbs, toAbs); err != nil {
		writeFileError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"from": fromRel, "to": toRel})
}

// resolveWorkspacePath confines a relative or absolute request path to the
// workspace root. The second result is the slash-separated relative path.
func resolveWorkspacePath(workspace, raw string) (string, string, error) {
	if strings.TrimSpace(workspace) == "" {
		return "", "", errors.New("workspace is not configured")
	}
	root, err := filepath.Abs(workspace)
	if err != nil {
		return "", "", err
	}
	target := strings.TrimSpace(raw)
	if target == "" {
		target = "."
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(root, filepath.FromSlash(target))
	}
	abs, err := filepath.Abs(target)
	if err != nil {
		return "", "", err
	}
	rel, err := filepath.Rel(root, abs)
	if err != nil {
		return "", "", errOutsideWorkspace
	}
	if rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", "", errOutsideWorkspace
	}
	if rel == "." {
		rel = ""
	}
	return abs, filepath.ToSlash(rel), nil
}

func writeFileError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errOutsideWorkspace):
		writeError(w, http.StatusBadRequest, err)
	case errors.Is(err, os.ErrNotExist):
		writeError(w, http.StatusNotFound, errText("path not found"))
	case isFileConflict(err):
		writeError(w, http.StatusConflict, errText("directory is not empty or path already exists"))
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}

// isFileConflict matches ENOTEMPTY and EEXIST across platforms, including the
// Windows 145/183 codes that errors.Is does not always map.
func isFileConflict(err error) bool {
	if errors.Is(err, syscall.ENOTEMPTY) || errors.Is(err, syscall.EEXIST) {
		return true
	}
	var errno syscall.Errno
	if errors.As(err, &errno) {
		return errno == 145 || errno == 183
	}
	return false
}

func isTrue(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes":
		return true
	}
	if parsed, err := strconv.ParseBool(value); err == nil {
		return parsed
	}
	return false
}
