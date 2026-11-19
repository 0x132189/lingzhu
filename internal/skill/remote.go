package skill

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
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
	// maxArchiveBytes bounds one downloaded zip or tarball.
	maxArchiveBytes = 64 * 1024 * 1024
	// maxArchiveFiles bounds the entries extracted from one archive.
	maxArchiveFiles = 1000
	// remoteTimeout bounds one configured source, not the whole pull.
	remoteTimeout = 60 * time.Second
	// remoteVersionFile records the index version of a cached package.
	remoteVersionFile = ".lingzhu-version"
)

// remoteFile is one file to place under a cached skill directory.
type remoteFile struct {
	path   string
	url    string
	data   []byte
	sha256 string
}

// SourceError is one configured remote source that could not be loaded.
type SourceError struct {
	URL   string `json:"url"`
	Error string `json:"error"`
}

// PullResult is the outcome of one remote pull.
type PullResult struct {
	Roots  []string      `json:"roots"`
	Errors []SourceError `json:"errors,omitempty"`
}

// Pull downloads skill packages from configured URLs into cacheDir/skills and
// returns the local directories that Discovery can scan. A URL may point at one
// markdown file, an index.json, or a directory containing index.json. Failures
// are logged and never prevent the remaining sources from loading.
func Pull(ctx context.Context, urls []string, cacheDir string, logf func(string, ...any)) []string {
	return PullStatus(ctx, urls, cacheDir, logf).Roots
}

// PullStatus is Pull with one error entry per failed source.
func PullStatus(ctx context.Context, urls []string, cacheDir string, logf func(string, ...any)) PullResult {
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if strings.TrimSpace(cacheDir) == "" || len(urls) == 0 {
		return PullResult{}
	}
	cacheRoot := filepath.Join(cacheDir, "skills")
	if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
		logf("skill: create cache %s: %v", cacheRoot, err)
		return PullResult{Errors: []SourceError{{Error: err.Error()}}}
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
	results := make([]PullResult, len(sources))
	var wg sync.WaitGroup
	for index, raw := range sources {
		wg.Add(1)
		go func(index int, raw string) {
			defer wg.Done()
			sourceCtx, cancel := context.WithTimeout(ctx, remoteTimeout)
			defer cancel()
			roots, err := pullSource(sourceCtx, raw, cacheRoot, logf)
			results[index] = PullResult{Roots: roots}
			if err != nil {
				results[index].Errors = []SourceError{{URL: raw, Error: err.Error()}}
			}
		}(index, raw)
	}
	wg.Wait()

	var out PullResult
	seen := map[string]bool{}
	for _, result := range results {
		for _, item := range result.Roots {
			if item == "" || seen[item] {
				continue
			}
			seen[item] = true
			out.Roots = append(out.Roots, item)
		}
		out.Errors = append(out.Errors, result.Errors...)
	}
	return out
}

func pullSource(ctx context.Context, raw, cacheRoot string, logf func(string, ...any)) ([]string, error) {
	gitSource := strings.HasPrefix(raw, "git+")
	parsedRaw := strings.TrimPrefix(raw, "git+")
	source, err := url.Parse(parsedRaw)
	if err != nil || source.Host == "" || (source.Scheme != "http" && source.Scheme != "https") {
		logf("skill: skipping invalid url %q", raw)
		return nil, fmt.Errorf("invalid skill url %q", raw)
	}
	lower := strings.ToLower(source.Path)
	switch {
	case gitSource || strings.HasSuffix(lower, ".git"):
		return pullGit(ctx, raw, cacheRoot, logf)
	case strings.HasSuffix(lower, ".zip"):
		return pullArchive(ctx, raw, cacheRoot, "zip", logf)
	case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
		return pullArchive(ctx, raw, cacheRoot, "tar", logf)
	case strings.HasSuffix(lower, ".tar"):
		return pullArchive(ctx, raw, cacheRoot, "tarraw", logf)
	}
	if strings.EqualFold(path.Ext(source.Path), ".md") {
		root, err := pullMarkdown(ctx, raw, source, cacheRoot, logf)
		if err != nil {
			return nil, err
		}
		return []string{root}, nil
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
func pullMarkdown(ctx context.Context, raw string, source *url.URL, cacheRoot string, logf func(string, ...any)) (string, error) {
	name := markdownSkillName(source)
	root := filepath.Join(cacheRoot, cacheKey(raw), name)
	data, err := fetch(ctx, raw, maxRemoteFileBytes)
	if err != nil {
		logf("skill: download %q: %v", raw, err)
		return "", err
	}
	if current, err := os.ReadFile(filepath.Join(root, "SKILL.md")); err == nil && bytes.Equal(current, data) {
		return root, nil
	}
	files := []remoteFile{{path: "SKILL.md", url: raw, data: data}}
	if err := installFiles(ctx, files, root, ""); err != nil {
		logf("skill: install %q: %v", raw, err)
		return "", err
	}
	return root, nil
}

// pullArchive extracts a zip or tarball into the cache and returns every
// directory that contains a SKILL.md.
func pullArchive(ctx context.Context, raw, cacheRoot, kind string, logf func(string, ...any)) ([]string, error) {
	data, err := fetch(ctx, raw, maxArchiveBytes)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(cacheRoot, cacheKey(raw))
	staging := root + ".tmp-" + nonce()
	defer os.RemoveAll(staging)
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return nil, err
	}
	switch kind {
	case "zip":
		err = extractZip(data, staging)
	case "tarraw":
		err = extractTarData(data, staging)
	default:
		err = extractTar(data, staging)
	}
	if err != nil {
		return nil, err
	}
	roots, err := collectSkillRoots(staging)
	if err != nil {
		return nil, err
	}
	mapped := mapRoots(staging, root, roots)
	if err := replaceDir(staging, root); err != nil {
		return nil, err
	}
	logf("skill: installed archive %q with %d skill(s)", raw, len(mapped))
	return mapped, nil
}

// pullGit clones a repository and treats every SKILL.md directory inside it as
// a skill. The git executable is used so credentials and proxies keep working.
func pullGit(ctx context.Context, raw, cacheRoot string, logf func(string, ...any)) ([]string, error) {
	url := strings.TrimPrefix(raw, "git+")
	root := filepath.Join(cacheRoot, cacheKey(raw))
	staging := root + ".tmp-" + nonce()
	defer os.RemoveAll(staging)
	cmd := exec.CommandContext(ctx, "git", "clone", "--depth", "1", "--", url, staging)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		detail := strings.TrimSpace(string(output))
		if len(detail) > 512 {
			detail = detail[:512]
		}
		return nil, fmt.Errorf("git clone %s: %v: %s", url, err, detail)
	}
	_ = os.RemoveAll(filepath.Join(staging, ".git"))
	roots, err := collectSkillRoots(staging)
	if err != nil {
		return nil, err
	}
	mapped := mapRoots(staging, root, roots)
	if err := replaceDir(staging, root); err != nil {
		return nil, err
	}
	logf("skill: cloned %q with %d skill(s)", url, len(mapped))
	return mapped, nil
}

func extractZip(data []byte, destination string) error {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return err
	}
	files := 0
	for _, entry := range reader.File {
		if entry.FileInfo().IsDir() {
			continue
		}
		name := filepath.ToSlash(entry.Name)
		if !safeRelative(name) {
			return fmt.Errorf("archive contains unsafe path %q", entry.Name)
		}
		if entry.UncompressedSize64 > maxArchiveBytes {
			return fmt.Errorf("archive entry %q is too large", name)
		}
		files++
		if files > maxArchiveFiles {
			return fmt.Errorf("archive has more than %d files", maxArchiveFiles)
		}
		body, err := entry.Open()
		if err != nil {
			return err
		}
		err = writeArchiveFile(destination, name, body)
		body.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func extractTar(data []byte, destination string) error {
	gz, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer gz.Close()
	return extractTarReader(gz, destination)
}

func extractTarData(data []byte, destination string) error {
	return extractTarReader(bytes.NewReader(data), destination)
}

func extractTarReader(source io.Reader, destination string) error {
	reader := tar.NewReader(source)
	files := 0
	for {
		header, err := reader.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if header.Typeflag != tar.TypeReg {
			continue
		}
		name := filepath.ToSlash(header.Name)
		if !safeRelative(name) {
			return fmt.Errorf("archive contains unsafe path %q", header.Name)
		}
		if header.Size > maxArchiveBytes {
			return fmt.Errorf("archive entry %q is too large", name)
		}
		files++
		if files > maxArchiveFiles {
			return fmt.Errorf("archive has more than %d files", maxArchiveFiles)
		}
		if err := writeArchiveFile(destination, name, io.LimitReader(reader, maxArchiveBytes+1)); err != nil {
			return err
		}
	}
}

func writeArchiveFile(destination, name string, body io.Reader) error {
	target := filepath.Join(destination, filepath.FromSlash(name))
	if !strings.HasPrefix(target, destination+string(filepath.Separator)) {
		return fmt.Errorf("archive path escapes destination: %q", name)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	file, err := os.Create(target)
	if err != nil {
		return err
	}
	written, copyErr := io.Copy(file, io.LimitReader(body, maxArchiveBytes+1))
	closeErr := file.Close()
	if copyErr != nil {
		return copyErr
	}
	if closeErr != nil {
		return closeErr
	}
	if written > maxArchiveBytes {
		return fmt.Errorf("archive entry %q is too large", name)
	}
	return nil
}

func collectSkillRoots(root string) ([]string, error) {
	var roots []string
	err := filepath.WalkDir(root, func(current string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if !entry.IsDir() {
			return nil
		}
		if _, err := os.Stat(filepath.Join(current, "SKILL.md")); err == nil {
			roots = append(roots, current)
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("package has no SKILL.md")
	}
	return roots, nil
}

func mapRoots(staging, root string, roots []string) []string {
	out := make([]string, 0, len(roots))
	for _, item := range roots {
		rel, err := filepath.Rel(staging, item)
		if err != nil {
			continue
		}
		out = append(out, filepath.Join(root, rel))
	}
	return out
}

type indexSkill struct {
	Name      string            `json:"name"`
	Version   string            `json:"version"`
	Files     []string          `json:"files"`
	Checksums map[string]string `json:"checksums,omitempty"`
}

type indexDocument struct {
	Skills []indexSkill `json:"skills"`
}

func pullIndex(ctx context.Context, indexURL string, base *url.URL, cacheRoot string, logf func(string, ...any)) ([]string, error) {
	data, err := fetch(ctx, indexURL, maxIndexBytes)
	if err != nil {
		logf("skill: fetch index %q: %v", indexURL, err)
		return nil, err
	}
	var index indexDocument
	if err := json.Unmarshal(data, &index); err != nil {
		logf("skill: parse index %q: %v", indexURL, err)
		return nil, err
	}
	if len(index.Skills) > maxRemoteSkills {
		logf("skill: index %q lists %d skills, using the first %d", indexURL, len(index.Skills), maxRemoteSkills)
		index.Skills = index.Skills[:maxRemoteSkills]
	}

	var out []string
	var firstErr error
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
			files = append(files, remoteFile{
				path:   file,
				url:    resource,
				sha256: strings.TrimSpace(item.Checksums[file]),
			})
		}
		if !valid {
			continue
		}

		root := filepath.Join(cacheRoot, cacheKey(base.String()), name)
		version := strings.TrimSpace(item.Version)
		if hasSkillFile(root, name) && (version == "" || versionMatches(root, version)) {
			// The version did not change, but the source may have edited a file
			// without bumping it. Refresh changed files in place; a version
			// change still uses the atomic staging path below.
			if err := syncFiles(ctx, files, root); err != nil {
				logf("skill: update %q from %s: %v", name, indexURL, err)
				if firstErr == nil {
					firstErr = err
				}
			}
			out = append(out, root)
			continue
		}
		if err := installFiles(ctx, files, root, version); err != nil {
			logf("skill: install %q from %s: %v", name, indexURL, err)
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		out = append(out, root)
	}
	return out, firstErr
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
		if err := verifyChecksum(file, data); err != nil {
			return err
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

// syncFiles downloads listed files and writes only the ones whose bytes
// changed. A version bump still uses the atomic staging path; this function
// handles edits made without one.
func syncFiles(ctx context.Context, files []remoteFile, root string) error {
	for _, file := range files {
		destination := filepath.Join(root, filepath.FromSlash(file.path))
		data, err := fetch(ctx, file.url, maxRemoteFileBytes)
		if err != nil {
			return fmt.Errorf("%s: %w", file.url, err)
		}
		if err := verifyChecksum(file, data); err != nil {
			return err
		}
		if current, err := os.ReadFile(destination); err == nil && bytes.Equal(current, data) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(destination, data, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func verifyChecksum(file remoteFile, data []byte) error {
	want := strings.TrimSpace(file.sha256)
	if want == "" {
		return nil
	}
	sum := sha256.Sum256(data)
	got := hex.EncodeToString(sum[:])
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("%s: checksum mismatch", file.url)
	}
	return nil
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
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			timer := time.NewTimer(time.Duration(attempt) * 200 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C:
			}
		}
		data, retry, err := fetchOnce(ctx, raw, limit)
		if err == nil {
			return data, nil
		}
		last = err
		if !retry || ctx.Err() != nil {
			break
		}
	}
	return nil, last
}

func fetchOnce(ctx context.Context, raw string, limit int64) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, raw, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", "lingzhu")
	req.Header.Set("Accept", "text/markdown, application/json, text/plain;q=0.8, */*;q=0.5")
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retry := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return nil, retry, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, true, err
	}
	if int64(len(data)) > limit {
		return nil, false, fmt.Errorf("file exceeds %d bytes", limit)
	}
	return data, false, nil
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
