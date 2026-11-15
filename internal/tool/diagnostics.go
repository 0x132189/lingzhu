package tool

import (
	"context"
	"encoding/json"
	"go/parser"
	"go/scanner"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Diagnostic is one compiler or parser problem shown under an edit.
type Diagnostic struct {
	Severity string `json:"severity"`
	Message  string `json:"message"`
	Line     int    `json:"line"`
	Column   int    `json:"column"`
}

var linePattern = regexp.MustCompile(`(?i)(?:line\s+|:)(\d+)`)

// diagnose reports local syntax problems after a write. Go uses the standard
// parser; JSON uses encoding/json; JavaScript, Python and shell use their own
// available checkers. Missing checkers simply produce no diagnostics.
func diagnose(path string) []Diagnostic {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".go":
		return diagnoseGo(path)
	case ".json":
		return diagnoseJSON(path)
	case ".js", ".mjs", ".cjs":
		return diagnoseCommand(path, "node", "--check", path)
	case ".py":
		return diagnoseCommand(path, "python", "-m", "py_compile", path)
	case ".sh", ".bash":
		return diagnoseCommand(path, "bash", "-n", path)
	default:
		return nil
	}
}

func diagnoseGo(path string) []Diagnostic {
	fset := token.NewFileSet()
	if _, err := parser.ParseFile(fset, path, nil, parser.AllErrors); err != nil {
		list, ok := err.(scanner.ErrorList)
		if !ok {
			return []Diagnostic{{Severity: "error", Message: err.Error(), Line: 1, Column: 1}}
		}
		out := make([]Diagnostic, 0, min(len(list), 20))
		for _, item := range list {
			if len(out) >= 20 {
				break
			}
			out = append(out, Diagnostic{
				Severity: "error",
				Message:  item.Msg,
				Line:     item.Pos.Line,
				Column:   item.Pos.Column,
			})
		}
		return out
	}
	return nil
}

func diagnoseJSON(path string) []Diagnostic {
	data, err := os.ReadFile(path)
	if err != nil {
		return []Diagnostic{{Severity: "error", Message: err.Error(), Line: 1, Column: 1}}
	}
	if err := json.Unmarshal(data, &json.RawMessage{}); err != nil {
		line, column := 1, 1
		if syntax, ok := err.(*json.SyntaxError); ok {
			line, column = offsetPosition(string(data), syntax.Offset)
		}
		return []Diagnostic{{Severity: "error", Message: err.Error(), Line: line, Column: column}}
	}
	return nil
}

func diagnoseCommand(path, name string, args ...string) []Diagnostic {
	if _, err := exec.LookPath(name); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err == nil {
		return nil
	}
	text := strings.TrimSpace(string(output))
	line := 0
	message := ""
	for _, item := range strings.Split(text, "\n") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		if match := linePattern.FindStringSubmatch(item); len(match) == 2 {
			if value, convErr := strconv.Atoi(match[1]); convErr == nil {
				line = value
			}
		}
		if strings.Contains(item, "Error:") || strings.Contains(item, "error:") || strings.Contains(item, "SyntaxError") || strings.Contains(item, "syntax error") {
			message = item
		}
	}
	if message == "" {
		message = text
		if len(message) > 500 {
			message = message[:500]
		}
	}
	return []Diagnostic{{Severity: "error", Message: message, Line: max(line, 1), Column: 1}}
}

func offsetPosition(text string, offset int64) (int, int) {
	if offset < 0 {
		return 1, 1
	}
	if offset > int64(len(text)) {
		offset = int64(len(text))
	}
	line, column := 1, 1
	for _, char := range text[:offset] {
		if char == '\n' {
			line++
			column = 1
		} else {
			column++
		}
	}
	return line, column
}

func diagnosePaths(paths []string) []Diagnostic {
	seen := map[string]bool{}
	var out []Diagnostic
	for _, path := range paths {
		if seen[path] {
			continue
		}
		seen[path] = true
		out = append(out, diagnose(path)...)
	}
	return out
}
