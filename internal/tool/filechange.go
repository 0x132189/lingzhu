package tool

import "strings"

// fileChange is the UI-facing description of one changed file. The frontend
// prefers this over reconstructing a diff from tool arguments.
func fileChange(path, patch, status string) map[string]any {
	added, removed := diffCounts(patch)
	return map[string]any{
		"file":      path,
		"patch":     patch,
		"additions": added,
		"deletions": removed,
		"status":    status,
	}
}

// diffCounts counts content lines in a unified diff without treating the
// ---/+++ headers as changes.
func diffCounts(patch string) (int, int) {
	added, removed := 0, 0
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++ "), strings.HasPrefix(line, "--- "):
			continue
		case strings.HasPrefix(line, "+"):
			added++
		case strings.HasPrefix(line, "-"):
			removed++
		}
	}
	return added, removed
}
