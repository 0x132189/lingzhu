package session

import (
	"strings"
)

// maxTitleLength bounds a generated title in the sidebar.
const maxTitleLength = 100

// TitleInput frames the text to name. Framing it as data keeps models from
// answering or carrying out the request instead.
func TitleInput(text string) string {
	return "Here is the first message of the conversation:\n\n<message>\n" + text + "\n</message>\n\nReply with only the title."
}

// CleanTitle keeps the first usable line and drops wrapping quotes or a
// "Title:" label that a model may add despite the instruction.
func CleanTitle(value string) string {
	for _, raw := range strings.Split(value, "\n") {
		line := sanitizeTitleLine(raw)
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > maxTitleLength {
			return strings.TrimSpace(string(runes[:maxTitleLength-1])) + "…"
		}
		return line
	}
	return ""
}

// sanitizeTitleLine normalizes one candidate title line. It returns an empty
// string for a blank line or for a line that is still chat-template syntax, so
// the caller can try the next line and afford to keep the automatic label.
func sanitizeTitleLine(value string) string {
	line := strings.TrimSpace(value)
	if line == "" {
		return ""
	}
	line = strings.TrimPrefix(line, "Title:")
	line = strings.TrimPrefix(line, "title:")
	line = strings.TrimSpace(line)
	line = stripControlTokens(line)
	line = strings.Join(strings.Fields(line), " ")
	line = strings.Trim(line, "\"'`*")
	line = strings.TrimSpace(strings.TrimRight(line, "."))
	line = strings.Trim(line, "\"'`*")
	if line == "" || looksLikeControlToken(line) {
		return ""
	}
	return line
}

// stripControlTokens removes chat-template control spans such as
// <|tool_calls|> or the fullwidth <｜...｜> form that some models emit instead
// of plain text. An unterminated span drops the rest of the line.
func stripControlTokens(line string) string {
	if !strings.Contains(line, "<") {
		return line
	}
	var b strings.Builder
	for i := 0; i < len(line); {
		rest := line[i:]
		switch {
		case strings.HasPrefix(rest, "<|"):
			end := strings.Index(rest[2:], "|>")
			if end < 0 {
				return b.String()
			}
			i += 2 + end + 2
			b.WriteByte(' ')
		case strings.HasPrefix(rest, "<｜"):
			width := len("<｜")
			end := strings.Index(rest[width:], "｜>")
			if end < 0 {
				return b.String()
			}
			i += width + end + len("｜>")
			b.WriteByte(' ')
		default:
			b.WriteByte(line[i])
			i++
		}
	}
	return b.String()
}

// looksLikeControlToken reports whether a line still carries control syntax
// after span removal, so it can be rejected rather than stored as a title.
func looksLikeControlToken(line string) bool {
	for _, marker := range []string{"<|", "|>", "<｜", "｜>", "DSML"} {
		if strings.Contains(line, marker) {
			return true
		}
	}
	return false
}

// Untitled reports whether a title is still the automatic first-message label,
// so a generated title may replace it.
func Untitled(title, firstMessage string) bool {
	trimmed := strings.TrimSpace(title)
	return trimmed == "" || trimmed == titleFromText(firstMessage)
}
