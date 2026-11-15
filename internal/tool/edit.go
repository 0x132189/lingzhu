package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"unicode"
)

// Edit replaces exact text in a file. It matches against the LF view Read shows
// the model, retries with typographic variants and trailing whitespace, and
// preserves the file's line endings.
type Edit struct {
	Dir string
}

func (e *Edit) Name() string { return "edit" }

func (e *Edit) Description() string {
	return "Edit the contents of a file by finding and replacing exact text. When editing text from Read output, preserve the exact indentation (tabs or spaces) and omit the line-number prefix, such as `1: `. Never include the prefix in oldString or newString. Write line breaks as \\n; the file's existing CRLF or LF line endings are matched and preserved. To change line endings themselves, make oldString and newString differ only in \\r; that edit applies to the exact bytes. If there is no exact match, the edit retries while ignoring typographic quote, dash, and space variants, then trailing whitespace on each line. The edit fails if oldString is not found. By default, oldString must identify a UNIQUE location. Multiple matches FAIL unless replaceAll is true. Add more surrounding context to disambiguate, or set replaceAll to true to replace every occurrence. Use replaceAll when the change should apply to every occurrence, such as renaming a variable."
}

func (e *Edit) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File to edit"},
			"oldString": {"type": "string", "description": "Exact text to find and replace"},
			"newString": {"type": "string", "description": "Text to replace oldString with (must differ from oldString)"},
			"replaceAll": {"type": "boolean", "description": "Whether to replace every occurrence of oldString. When false, oldString must match exactly once. Defaults to false."}
		},
		"required": ["path", "oldString", "newString"]
	}`)
}

type textMatch struct {
	start int
	end   int
}

// normalizeForMatch folds typographic variants that models commonly emit but
// files rarely store. Every replacement is one rune, so rune offsets stay stable.
func normalizeForMatch(value rune) rune {
	switch value {
	case '‘', '’', '‚', '‛':
		return '\''
	case '“', '”', '„', '‟':
		return '"'
	case '‐', '‑', '‒', '–', '—', '―', '−':
		return '-'
	case '\u00A0', '\u202F', '\u205F', '\u3000':
		return ' '
	}
	if value >= '\u2002' && value <= '\u200A' {
		return ' '
	}
	return value
}

func normalizeString(value string) string {
	runes := []rune(value)
	for index, item := range runes {
		runes[index] = normalizeForMatch(item)
	}
	return string(runes)
}

func runeByteOffsets(value string) []int {
	offsets := make([]int, 0, len(value)+1)
	for index := range value {
		offsets = append(offsets, index)
	}
	return append(offsets, len(value))
}

func findExact(content, search string) []textMatch {
	if search == "" {
		return nil
	}
	var matches []textMatch
	offset := 0
	for {
		index := strings.Index(content[offset:], search)
		if index < 0 {
			return matches
		}
		start := offset + index
		matches = append(matches, textMatch{start: start, end: start + len(search)})
		offset = start + len(search)
	}
}

func findNormalized(content, search string) []textMatch {
	haystack := []rune(content)
	needle := []rune(search)
	if len(needle) == 0 || len(needle) > len(haystack) {
		return nil
	}
	offsets := runeByteOffsets(content)
	var matches []textMatch
	for start := 0; start+len(needle) <= len(haystack); start++ {
		matched := true
		for index := range needle {
			if normalizeForMatch(haystack[start+index]) != normalizeForMatch(needle[index]) {
				matched = false
				break
			}
		}
		if matched {
			matches = append(matches, textMatch{start: offsets[start], end: offsets[start+len(needle)]})
		}
	}
	return matches
}

type viewLine struct {
	start      int
	end        int
	contentEnd int
	newline    bool
	text       string
}

func findLineMatches(content, search string) []textMatch {
	trailingNewline := strings.HasSuffix(search, "\n")
	expected := strings.Split(search, "\n")
	if trailingNewline {
		expected = expected[:len(expected)-1]
	}
	if len(expected) == 0 {
		return nil
	}

	var lines []viewLine
	for start := 0; start < len(content); {
		newline := strings.IndexByte(content[start:], '\n')
		if newline < 0 {
			lines = append(lines, viewLine{start: start, end: len(content), contentEnd: len(content), text: content[start:]})
			break
		}
		end := start + newline + 1
		lines = append(lines, viewLine{start: start, end: end, contentEnd: start + newline, newline: true, text: content[start : start+newline]})
		start = end
	}

	key := func(value string) string {
		return normalizeString(strings.TrimRightFunc(value, unicode.IsSpace))
	}

	var matches []textMatch
	for start := 0; start+len(expected) <= len(lines); start++ {
		matched := true
		for index := range expected {
			if key(lines[start+index].text) != key(expected[index]) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		last := lines[start+len(expected)-1]
		if trailingNewline && !last.newline {
			continue
		}
		end := last.contentEnd
		if trailingNewline {
			end = last.end
		}
		candidate := textMatch{start: lines[start].start, end: end}
		if len(matches) > 0 && matches[len(matches)-1].end > candidate.start {
			continue
		}
		matches = append(matches, candidate)
	}
	return matches
}

func lineEnding(text, fallback string) string {
	crlf := strings.Count(text, "\r\n")
	lf := strings.Count(text, "\n") - crlf
	switch {
	case crlf == lf:
		return fallback
	case crlf > lf:
		return "\r\n"
	default:
		return "\n"
	}
}

func withEnding(replacement, ending string) string {
	if ending == "\n" {
		return replacement
	}
	return strings.ReplaceAll(replacement, "\n", ending)
}

// viewOffsets returns the LF view of source and the view positions of the '\n'
// bytes whose preceding '\r' was removed, so offsets can map back to source.
func viewOffsets(source string) (string, []int) {
	var view strings.Builder
	view.Grow(len(source))
	var removed []int
	for index := 0; index < len(source); index++ {
		if source[index] == '\r' && index+1 < len(source) && source[index+1] == '\n' {
			removed = append(removed, view.Len())
			continue
		}
		view.WriteByte(source[index])
	}
	return view.String(), removed
}

func countBefore(positions []int, offset int) int {
	return sort.SearchInts(positions, offset)
}

func matchView(source, oldString, newString string) ([]textMatch, func(textMatch) string) {
	view, removed := viewOffsets(source)
	search := strings.ReplaceAll(oldString, "\r\n", "\n")
	replacement := strings.ReplaceAll(newString, "\r\n", "\n")

	matches := findExact(view, search)
	if len(matches) == 0 {
		matches = findNormalized(view, search)
	}
	if len(matches) == 0 {
		matches = findLineMatches(view, search)
	}

	mapped := make([]textMatch, len(matches))
	for index, match := range matches {
		mapped[index] = textMatch{
			start: match.start + countBefore(removed, match.start),
			end:   match.end + countBefore(removed, match.end),
		}
	}

	fileEnding := lineEnding(source, "\n")
	replace := func(match textMatch) string {
		region := source[match.start:match.end]
		return withEnding(replacement, lineEnding(region, fileEnding))
	}
	return mapped, replace
}

func applyMatches(source string, matches []textMatch, replaceAll bool, replace func(textMatch) string) (string, int) {
	selected := matches
	if !replaceAll {
		selected = matches[:1]
	}
	ordered := append([]textMatch(nil), selected...)
	sort.Slice(ordered, func(left, right int) bool { return ordered[left].start > ordered[right].start })

	updated := source
	for _, match := range ordered {
		updated = updated[:match.start] + replace(match) + updated[match.end:]
	}
	return updated, len(selected)
}

func (e *Edit) Run(ctx context.Context, call Call) (string, error) {
	var args struct {
		Path       string `json:"path"`
		OldString  string `json:"oldString"`
		NewString  string `json:"newString"`
		ReplaceAll bool   `json:"replaceAll"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	if args.OldString == "" {
		return "", fmt.Errorf("edit: oldString must not be empty. Use write to create or overwrite a file.")
	}
	if args.OldString == args.NewString {
		return "", fmt.Errorf("edit: No changes to apply: oldString and newString are identical.")
	}

	path, err := call.Resolve(ctx, "edit", e.Dir, args.Path)
	if err != nil {
		return "", err
	}

	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("edit: %s is a directory", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	source := string(data)

	// Strings that differ only in \r ask to change line endings themselves, so
	// they apply to the exact bytes instead of the LF view.
	endingsOnly := strings.ReplaceAll(args.OldString, "\r", "") == strings.ReplaceAll(args.NewString, "\r", "")
	var matches []textMatch
	var replace func(textMatch) string
	if endingsOnly {
		matches = findExact(source, args.OldString)
		replace = func(textMatch) string { return args.NewString }
	} else {
		matches, replace = matchView(source, args.OldString, args.NewString)
	}

	if len(matches) == 0 {
		hint := ""
		replacement := strings.ReplaceAll(strings.TrimSpace(args.NewString), "\r\n", "\n")
		if len([]rune(replacement)) >= 20 && strings.Contains(normalizeString(strings.ReplaceAll(source, "\r\n", "\n")), normalizeString(replacement)) {
			hint = " newString is already in the file, so this edit may already be applied."
		}
		return "", fmt.Errorf("edit: Could not find oldString in %s. It must match exactly, including whitespace and indentation.%s", relPath(e.Dir, path), hint)
	}
	if len(matches) > 1 && !args.ReplaceAll {
		return "", fmt.Errorf("edit: Found %d matches for oldString, but expected exactly one. Add more surrounding context to make oldString unique, or set replaceAll to true to replace every occurrence.", len(matches))
	}

	updated, replacements := applyMatches(source, matches, args.ReplaceAll, replace)
	if updated == source {
		return "", fmt.Errorf("edit: No changes to apply: the edit leaves %s unchanged. Line endings follow the file; to change them, write \\r explicitly in oldString or newString.", relPath(e.Dir, path))
	}
	if err := os.WriteFile(path, []byte(updated), info.Mode().Perm()); err != nil {
		return "", fmt.Errorf("edit: %w", err)
	}
	formatter, formatErr := call.FormatFile(ctx, path)
	final := updated
	if formatter != "" {
		if data, err := os.ReadFile(path); err == nil {
			final = string(data)
		}
	}
	call.SetDiff(lineDiff(e.Dir, path, source, final))
	call.SetMetadata("diagnostics", diagnose(path))
	call.SetMetadata("files", []map[string]any{fileChange(path, lineDiff(e.Dir, path, source, final), "modified")})
	message := fmt.Sprintf("Edited file successfully: %s (%d replacement(s))", relPath(e.Dir, path), replacements)
	if formatter != "" {
		message += " (formatted with " + formatter + ")"
	}
	if formatErr != nil {
		message += "; formatter failed: " + formatErr.Error()
	}
	return message, nil
}
