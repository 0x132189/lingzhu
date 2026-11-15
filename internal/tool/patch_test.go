package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPatchAddUpdateMoveDelete(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "app.py", "def greet():\n    print(\"Hi\")\n")
	write(t, dir, "obsolete.txt", "old\n")

	patch := strings.Join([]string{
		"*** Begin Patch",
		"*** Add File: hello.txt",
		"+Hello world",
		"*** Update File: app.py",
		"*** Move to: main.py",
		"@@ def greet():",
		" def greet():",
		"-    print(\"Hi\")",
		"+    print(\"Hello, world!\")",
		"*** Delete File: obsolete.txt",
		"*** End Patch",
	}, "\n")

	out, err := run(t, registry, "patch", `{"patchText":`+quoteJSON(patch)+`}`, allowAll())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "A hello.txt") || !strings.Contains(out, "M main.py") || !strings.Contains(out, "D obsolete.txt") {
		t.Fatalf("unexpected patch output: %q", out)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "hello.txt")); err != nil || string(data) != "Hello world\n" {
		t.Fatalf("add failed: %q %v", data, err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "main.py")); err != nil || !strings.Contains(string(data), `print("Hello, world!")`) {
		t.Fatalf("update/move failed: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "obsolete.txt")); !os.IsNotExist(err) {
		t.Fatalf("delete failed: %v", err)
	}
}

func TestPatchRejectsAmbiguousHunk(t *testing.T) {
	registry, dir := newRegistry(t, nil)
	write(t, dir, "a.txt", "same\nsame\n")
	patch := "*** Begin Patch\n*** Update File: a.txt\n same\n*** End Patch\n"
	if _, err := run(t, registry, "patch", `{"patchText":`+quoteJSON(patch)+`}`, allowAll()); err == nil || !strings.Contains(err.Error(), "more than one") {
		t.Fatalf("want ambiguity error, got %v", err)
	}
}

func quoteJSON(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}
