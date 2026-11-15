package tool

import (
	"fmt"
	"strings"
)

// maxDiffBytes bounds the UI-only diff carried by one tool result.
const maxDiffBytes = 24 * 1024

// lineDiff renders the changed middle of two texts as a unified hunk. Common
// leading and trailing lines are omitted, which is enough for the small edits
// an agent usually makes and keeps the cost linear.
func lineDiff(dir, path, before, after string) string {
	oldLines := splitDiffLines(before)
	newLines := splitDiffLines(after)

	prefix := 0
	for prefix < len(oldLines) && prefix < len(newLines) && oldLines[prefix] == newLines[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(oldLines)-prefix && suffix < len(newLines)-prefix &&
		oldLines[len(oldLines)-1-suffix] == newLines[len(newLines)-1-suffix] {
		suffix++
	}

	oldCount := len(oldLines) - prefix - suffix
	newCount := len(newLines) - prefix - suffix
	if oldCount == 0 && newCount == 0 {
		return ""
	}

	name := relPath(dir, path)
	var b strings.Builder
	fmt.Fprintf(&b, "--- a/%s\n+++ b/%s\n", name, name)
	fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@\n", diffStart(prefix, oldCount), oldCount, diffStart(prefix, newCount), newCount)
	for _, line := range oldLines[prefix : len(oldLines)-suffix] {
		b.WriteString("-" + line + "\n")
	}
	for _, line := range newLines[prefix : len(newLines)-suffix] {
		b.WriteString("+" + line + "\n")
	}

	out := b.String()
	if len(out) > maxDiffBytes {
		out = strings.ToValidUTF8(out[:maxDiffBytes], "") + "\n… diff truncated\n"
	}
	return out
}

// diffStart uses the unified-diff convention of zero for an empty side.
func diffStart(prefix, count int) int {
	if count == 0 {
		return prefix
	}
	return prefix + 1
}

func splitDiffLines(text string) []string {
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i := range lines {
		lines[i] = strings.TrimSuffix(lines[i], "\r")
	}
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}
