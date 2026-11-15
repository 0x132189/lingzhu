package tool

import (
	"bufio"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

// maxReferences caps one reference search.
const maxReferences = 200

// Reference is one literal text match in a workspace file.
type Reference struct {
	Path   string `json:"path"`
	Line   int    `json:"line"`
	Column int    `json:"column"`
	Text   string `json:"text"`
}

var errReferenceLimit = errors.New("reference limit reached")

// SearchReferences scans dir for a literal term and returns line matches. It
// skips hidden entries and build/dependency directories, ignores files larger
// than the grep cap, and stops once limit matches were collected.
func SearchReferences(dir, subdir, query string, limit int, caseSensitive bool) ([]Reference, bool, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, false, errors.New("query is empty")
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > maxReferences {
		limit = maxReferences
	}
	root := dir
	if strings.TrimSpace(subdir) != "" {
		root = filepath.Join(dir, filepath.FromSlash(subdir))
	}
	needle := query
	if !caseSensitive {
		needle = strings.ToLower(query)
	}
	references := []Reference{}
	err := walk(root, false, func(path, _ string, info os.FileInfo) error {
		if info.Size() > maxFileBytes {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return nil
		}
		defer file.Close()
		relative := relPath(dir, path)
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 64*1024), maxFileBytes)
		line := 0
		for scanner.Scan() {
			line++
			text := scanner.Text()
			haystack := text
			if !caseSensitive {
				haystack = strings.ToLower(text)
			}
			index := strings.Index(haystack, needle)
			if index < 0 {
				continue
			}
			references = append(references, Reference{
				Path:   relative,
				Line:   line,
				Column: utf8.RuneCountInString(text[:index]) + 1,
				Text:   clampLine(text),
			})
			if len(references) >= limit {
				return errReferenceLimit
			}
		}
		return nil
	})
	truncated := errors.Is(err, errReferenceLimit)
	if err != nil && !truncated {
		return nil, false, err
	}
	return references, truncated, nil
}

// clampLine keeps one long source line readable in the response.
func clampLine(line string) string {
	if utf8.RuneCountInString(line) <= maxLineRunes {
		return line
	}
	runes := []rune(line)
	return string(runes[:maxLineRunes]) + "…"
}
