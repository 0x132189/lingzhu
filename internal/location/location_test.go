package location

import (
	"os"
	"path/filepath"
	"testing"
)

func TestMakeNormalizesAndKeysCaseInsensitively(t *testing.T) {
	dir := t.TempDir()
	ref, err := Make(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Directory != filepath.Clean(dir) {
		t.Fatalf("want cleaned directory, got %q", ref.Directory)
	}
	upper, err := Make(strings_ToUpper(dir))
	if err != nil {
		t.Fatal(err)
	}
	if upper.Key() != ref.Key() {
		t.Fatalf("case variants must share a key: %q vs %q", upper.Key(), ref.Key())
	}
}

// strings_ToUpper keeps the test independent of the platform's case folding
// rules: only an ASCII directory can be uppercased meaningfully.
func strings_ToUpper(value string) string {
	out := []byte(value)
	for i, b := range out {
		if b >= 'a' && b <= 'z' {
			out[i] = b - ('a' - 'A')
		}
	}
	return string(out)
}

func TestDataDirSeparatesWorkspaces(t *testing.T) {
	base := t.TempDir()
	first, err := Make(filepath.Join(t.TempDir(), "project-a"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Make(filepath.Join(t.TempDir(), "project-b"))
	if err != nil {
		t.Fatal(err)
	}
	if first.DataDir(base) == second.DataDir(base) {
		t.Fatal("two workspaces must not share a data directory")
	}
	if filepath.Dir(first.DataDir(base)) != filepath.Join(base, "locations") {
		t.Fatalf("unexpected layout: %s", first.DataDir(base))
	}
}

func TestSetTracksDefaultAndRemoval(t *testing.T) {
	set := NewSet()
	first := &Services{Ref: Ref{Directory: filepath.Clean(os.TempDir())}}
	second := &Services{Ref: Ref{Directory: filepath.Clean(filepath.Join(os.TempDir(), "other"))}}
	set.Put(first)
	set.Put(second)

	if set.Default() != first {
		t.Fatal("the first workspace inserted must be the default")
	}
	if got, ok := set.Resolve(first.Root()); !ok || got != first {
		t.Fatal("resolve must find a known directory")
	}
	if set.SetDefault(second.Root()) != true || set.Default() != second {
		t.Fatal("SetDefault must move the default")
	}
	if set.Remove(second.Root()) != second {
		t.Fatal("remove must return the workspace it dropped")
	}
	if set.Default() != first {
		t.Fatal("removing the default must fall back to the first remaining workspace")
	}
}

func TestAddRejectsEmptyAndDuplicates(t *testing.T) {
	if _, err := Make("   "); err == nil {
		t.Fatal("an empty directory must be rejected")
	}
}

func TestExistsReportsMissingDirectory(t *testing.T) {
	dir := t.TempDir()
	ref, err := Make(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ref.Exists() {
		t.Fatal("a temp directory must exist")
	}
	if err := os.Remove(dir); err != nil {
		t.Fatal(err)
	}
	if ref.Exists() {
		t.Fatal("a removed directory must not report existing")
	}
}
