package tool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func editFixture(t *testing.T, content string) (string, *Edit) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, &Edit{Dir: dir}
}

func runEdit(t *testing.T, edit *Edit, input string) (string, error) {
	t.Helper()
	return edit.Run(context.Background(), Call{Name: "edit", Input: []byte(input), Perm: allowAll()})
}

func readEditFile(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "a.txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestEditFuzzyVariantsAndCRLF(t *testing.T) {
	dir, edit := editFixture(t, "first\r\n“quoted” — dash\r\nlast\r\n")

	if _, err := runEdit(t, edit, `{"path":"a.txt","oldString":"\"quoted\" - dash","newString":"changed"}`); err != nil {
		t.Fatal(err)
	}
	if got := readEditFile(t, dir); got != "first\r\nchanged\r\nlast\r\n" {
		t.Fatalf("unexpected content: %q", got)
	}

	if _, err := runEdit(t, edit, `{"path":"a.txt","oldString":"changed","newString":"next\nline"}`); err != nil {
		t.Fatal(err)
	}
	if got := readEditFile(t, dir); got != "first\r\nnext\r\nline\r\nlast\r\n" {
		t.Fatalf("CRLF not preserved: %q", got)
	}
}

func TestEditTrailingWhitespaceTier(t *testing.T) {
	dir, edit := editFixture(t, "alpha   \nbeta\n")

	if _, err := runEdit(t, edit, `{"path":"a.txt","oldString":"alpha\nbeta","newString":"gamma\nbeta"}`); err != nil {
		t.Fatal(err)
	}
	if got := readEditFile(t, dir); got != "gamma\nbeta\n" {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestEditMultipleMatchesRequireReplaceAll(t *testing.T) {
	dir, edit := editFixture(t, "same\nsame\n")

	_, err := runEdit(t, edit, `{"path":"a.txt","oldString":"same","newString":"other"}`)
	if err == nil || !strings.Contains(err.Error(), "expected exactly one") {
		t.Fatalf("want unique-match error, got %v", err)
	}

	if _, err := runEdit(t, edit, `{"path":"a.txt","oldString":"same","newString":"other","replaceAll":true}`); err != nil {
		t.Fatal(err)
	}
	if got := readEditFile(t, dir); got != "other\nother\n" {
		t.Fatalf("unexpected content: %q", got)
	}
}

func TestEditLineEndingOnlyChangesExactBytes(t *testing.T) {
	dir, edit := editFixture(t, "one\ntwo\n")

	if _, err := runEdit(t, edit, `{"path":"a.txt","oldString":"one\ntwo","newString":"one\r\ntwo"}`); err != nil {
		t.Fatal(err)
	}
	if got := readEditFile(t, dir); got != "one\r\ntwo\n" {
		t.Fatalf("line endings not written: %q", got)
	}
}
