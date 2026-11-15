package skill

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// maxIndexBytes bounds one remote skill index.
	maxIndexBytes = 1024 * 1024
	// maxRemoteFileBytes bounds every downloaded skill file.
	maxRemoteFileBytes = 2 * 1024 * 1024
	// maxRemoteSkills bounds the packages fetched from one index.
	maxRemoteSkills = 100
	// maxRemoteFiles bounds the files fetched for one skill.
	maxRemoteFiles = 32
	// remoteTimeout bounds one configured source, not the whole pull.
	remoteTimeout = 15 * time.Second
	// remoteVersionFile records the index version of a cached package.
	remoteVersionFile = ".lingzhu-version"
)

// remoteFile is one file to place under a cached skill directory.
type remoteFile struct {
	path string
	url  string
	data []byte
}

// Pull downloads skill packages from configured URLs into cacheDir/skills and
// returns the local directories that Discovery can scan. A URL may point at one
// markdown file, an index.json, or a directory containing index.json. Failures
// are logged and never prevent the remaining sources from loading.
func Pull(ctx context.Context, urls []string, cacheDir string, logf func(string, ...any)) []string {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if strings.TrimSpace(cacheDir) == "" || len(urls) == 0 {
		return nil
	}
	cacheRoot := filepath.Join(cacheDir, "skills")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		logf("skill: create cache %s: %v", cacheRoot, err)
		return nil
	}

	var sources []string
	seenSource := map[string]bool{}
	for _, raw := range urls {
		raw = strings.TrimSpace(raw)
		if raw == "" || seenSource[raw] {
			continue
		}
		seenSource[raw] = true
		sources = append(sources, raw)
	}
	results := make([][]string, len(sources))
	var wg sync.WaitGroup
	for index, raw := range sources {
		wg.Add(1)
		go func(index int, raw string) {
			defer wg.Done()
			sourceCtx, cancel := context.WithTimeout(ctx, remoteTimeout)
			defer cancel()
			results[index] = pullSource(sourceCtx, raw, cacheRoot, logf)
		}(index, raw)
	}
	wg.Wait()

	var out []string
	seen := map[string]bool{}
	for _, list := range results {
		for _, item := range list {
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			out = append(out, item)
		}
	}
	return out
}

func pullSource(ctx context.Context, raw, cacheRoot string, logf func(string, ...any)) []string {
	source, err := url.Parse(raw)
	if err != nil || source.Host == "" || (source.Scheme != "http" && source.Scheme != "https") {
		logf("skill: skipping invalid url %q", raw)
		return nil
	}
	if strings.EqualFold(path.Ext(source.Path), ".md") {
		root, ok := pullMarkdown(ctx, raw, source, cacheRoot, logf)
		if !ok {
			return nil
		}
		return []string{root}
	}

	base := *source
	base.RawQuery = ""
	base.Fragment = ""
	if strings.HasSuffix(strings.ToLower(base.Path), "index.json") {
		base.Path = path.Dir(strings.TrimSuffix(base.Path, "/"))
	}
	if !strings.HasSuffix(base.Path, "/") {
		base.Path += "/"
	}
	indexURL := base.String() + "index.json"
	return pullIndex(ctx, indexURL, &base, cacheRoot, logf)
}

// pullMarkdown installs a single markdown file as a skill directory.
func pullMarkdown(ctx context.Context, raw string, source *url.URL, cacheRoot string, logf func(string, ...any)) (string, bool) {
	name := markdownSkillName(source)
	root := filepath.Join(cacheRoot, cacheKey(raw), name)
	data, err := fetch(ctx, raw, maxRemoteFileBytes)
	if err != nil {
		logf("skill: download %q: %v", raw, err)
		return "", false
	}
	if current, err := os.ReadFile(filepath.Join(root, "SKILL.md")); err == nil && bytes.Equal(current, data) {
		return root, true
	}
	files := []remoteFile{{path: "SKILL.md", url: raw, data: data}}
	if err := installFiles(ctx, files, root, ""); err != nil {
		logf("skill: install %q: %v", raw, err)
		return "", false
	}
	return root, true
}

type indexSkill struct {
	Name    string   `json:"name"`
	Version string   `json:"version"`
	Files   []string `json:"files"`
}

type indexDocument struct {
	Skills []indexSkill `json:"skills"`
}

func pullIndex(ctx context.Context, indexURL string, base *url.URL, cacheRoot string, logf func(string, ...any)) []string {
	data, err := fetch(ctx, indexURL, maxIndexBytes)
	if err != nil {
		logf("skill: fetch index %q: %v", indexURL, err)
		return nil
	}
	var index indexDocument
	if err := json.Unmarshal(data, &index); err != nil {
		logf("skill: parse index %q: %v", indexURL, err)
		return nil
	}
	if len(index.Skills) > maxRemoteSkills {
		logf("skill: index %q lists %d skills, using the first %d", indexURL, len(index.Skills), maxRemoteSkills)
		index.Skills = index.Skills[:maxRemoteSkills]
	}

	var out []string
	for _, item := range index.Skills {
		name := strings.TrimSpace(item.Name)
		if !safeSegment(name) {
			logf("skill: skipped unsafe skill name %q from %s", item.Name, indexURL)
			continue
		}
		if !hasSkillMarkdown(item.Files, name) {
			logf("skill: skipped skill %q from %s: no SKILL.md or %s.md", name, indexURL, name)
			continue
		}
		if len(item.Files) > maxRemoteFiles {
			logf("skill: skipped skill %q from %s: more than %d files", name, indexURL, maxRemoteFiles)
			continue
		}
		files := make([]remoteFile, 0, len(item.Files))
		valid := true
		for _, file := range item.Files {
			if !safeRelative(file) {
				logf("skill: skipped skill %q from %s: unsafe file %q", name, indexURL, file)
				valid = false
				break
			}
			resource, err := resolveFile(base, name, file)
			if err != nil {
				logf("skill: skipped skill %q from %s: %v", name, indexURL, err)
				valid = false
				break
			}
			files = append(files, remoteFile{path: file, url: resource})
		}
		if !valid {
			continue
		}

		root := filepath.Join(cacheRoot, cacheKey(base.String()), name)
		version := strings.TrimSpace(item.Version)
		if (item.Version != "" && versionMatches(root, version) && hasSkillFile(root, name)) ||
			(item.Version == "" && hasSkillFile(root, name)) {
			out = append(out, root)
			continue
		}
		if err := installFiles(ctx, files, root, version); err != nil {
			logf("skill: install %q from %s: %v", name, indexURL, err)
			continue
		}
		out = append(out, root)
	}
	return out
}

// installFiles downloads every file beside the skill root, then swaps the
// staging directory into place. A failed download leaves the previous cache
// untouched.
func installFiles(ctx context.Context, files []remoteFile, root, version string) error {
	staging := root + ".tmp-" + nonce()
	defer os.RemoveAll(staging)
	for _, file := range files {
		data := file.data
		if data == nil {
			var err error
			data, err = fetch(ctx, file.url, maxRemoteFileBytes)
			if err != nil {
				return fmt.Errorf("%s: %w", file.url, err)
			}
		}
		destination := filepath.Join(staging, filepath.FromSlash(file.path))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(destination, data, 0o644); err != nil {
			return err
		}
	}
	if !containsSkillFile(staging) {
		return fmt.Errorf("package has no SKILL.md")
	}
	if version != "" {
		if err := os.WriteFile(filepath.Join(staging, remoteVersionFile), []byte(version), 0o644); err != nil {
			return err
		}
	}
	return replaceDir(staging, root)
}

func replaceDir(staging, root string) error {
	if _, err := os.Stat(root); err == nil {
		backup := root + ".old-" + nonce()
		if err := os.Rename(root, backup); err != nil {
			return err
		}
		if err := os.Rename(staging, root); err != nil {
			_ = os.Rename(backup, root)
			return err
		}
		return os.RemoveAll(backup)
	}
	if err := os.MkdirAll(filepath.Dir(root), 0o755); err != nil {
		return err
	}
	return os.Rename(staging, root)
}

func fetch(ctx context.Context, raw string, limit int64) ([]byte, error) {
	parsed, err := url.Parse(raw)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("unsupported url %q", raw)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "lingzhu")
	req.Header.Set("Accept", "text/markdown, application/json, text/plain;q=0.8, */*;q=0.5")
	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, nil
}

func resolveFile(base *url.URL, name, file string) (string, error) {
	skillBase := *base
	skillBase.Path += url.PathEscape(name) + "/"
	resource, err := skillBase.Parse(file)
	if err != nil {
		return "", err
	}
	if resource.Scheme != base.Scheme || resource.Host != base.Host {
		return "", fmt.Errorf("file %q resolves outside the source host", file)
	}
	return resource.String(), nil
}

func hasSkillMarkdown(files []string, name string) bool {
	for _, file := range files {
		if file == "SKILL.md" || file == name+".md" {
			return true
		}
	}
	return false
}

func hasSkillFile(root, name string) bool {
	for _, file := range []string{"SKILL.md", name + ".md"} {
		if info, err := os.Stat(filepath.Join(root, file)); err == nil && !info.IsDir() {
			return true
		}
	}
	return false
}

func containsSkillFile(directory string) bool {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if entry.Name() == "SKILL.md" || strings.HasSuffix(strings.ToLower(entry.Name()), ".md") {
			return true
		}
	}
	return false
}

func versionMatches(root, version string) bool {
	data, err := os.ReadFile(filepath.Join(root, remoteVersionFile))
	return err == nil && strings.TrimSpace(string(data)) == version
}

func markdownSkillName(source *url.URL) string {
	base := path.Base(source.Path)
	name := strings.TrimSuffix(base, path.Ext(base))
	if strings.EqualFold(name, "skill") || name == "" || name == "." || name == "/" {
		parent := path.Base(path.Dir(source.Path))
		if parent != "." && parent != "/" && parent != "" {
			name = parent
		} else {
			name = source.Hostname()
		}
	}
	return sanitizeSegment(name)
}

func sanitizeSegment(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), ".-")
	if out == "" {
		return "remote-skill"
	}
	return out
}

func safeSegment(value string) bool {
	return value != "" && value != "." && value != ".." &&
		!strings.ContainsAny(value, "/\\\x00") && !strings.Contains(value, "..")
}

func safeRelative(value string) bool {
	if value == "" || strings.ContainsAny(value, "\\\x00?#") || strings.HasPrefix(value, "/") {
		return false
	}
	if strings.Contains(value, "//") {
		return false
	}
	for _, segment := range strings.Split(value, "/") {
		if !safeSegment(segment) {
			return false
		}
	}
	return true
}

func cacheKey(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

var nonceSeq int64

func nonce() string {
	return fmt.Sprintf("%d-%d", time.Now().UnixNano(), atomic.AddInt64(&nonceSeq, 1))
}
