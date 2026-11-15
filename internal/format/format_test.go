package format

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestHelperFormatter is a tiny formatter used by the tests. It rewrites "bad"
// to "good" in the file passed as the last argument.
func TestHelperFormatter(t *testing.T) {
	if os.Getenv("LINGZHU_FORMAT_HELPER") != "1" {
		return
	}
	file := os.Args[len(os.Args)-1]
	data, err := os.ReadFile(file)
	if err != nil {
		os.Exit(1)
	}
	updated := strings.ReplaceAll(string(data), "bad", "good")
	if err := os.WriteFile(file, []byte(updated), 0o644); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func helperConfig(t *testing.T) Config {
	t.Helper()
	return Config{Formatters: map[string]Definition{
		"fixer": {
			Command:    []string{os.Args[0], "-test.run=TestHelperFormatter", "$FILE"},
			Extensions: []string{".txt"},
			Environment: map[string]string{
				"LINGZHU_FORMAT_HELPER": "1",
			},
		},
	}}
}

func TestFormatRunsConfiguredFormatter(t *testing.T) {
	workdir := t.TempDir()
	manager := New(workdir, helperConfig(t), t.Logf)
	path := filepath.Join(workdir, "note.txt")
	if err := os.WriteFile(path, []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := manager.Format(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "fixer" || !result.Changed {
		t.Fatalf("unexpected result: %+v", result)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "good" {
		t.Fatalf("formatter did not run: %q", data)
	}

	status := manager.Status()
	found := false
	for _, item := range status {
		if item.Name == "fixer" && item.Source == "config" {
			found = true
		}
	}
	if !found {
		t.Fatalf("unexpected status: %+v", status)
	}
}

func TestFormatSkipsOtherExtensionsAndDisabled(t *testing.T) {
	workdir := t.TempDir()
	manager := New(workdir, helperConfig(t), t.Logf)
	path := filepath.Join(workdir, "note.md")
	if err := os.WriteFile(path, []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Format(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Name != "" || result.Changed {
		t.Fatalf("unexpected result: %+v", result)
	}

	disabled := false
	off := New(workdir, Config{Enabled: &disabled}, t.Logf)
	if items := off.Status(); len(items) != 0 {
		t.Fatalf("disabled manager should have no formatters: %+v", items)
	}
}

func TestConfiguredCommandFailureIsReported(t *testing.T) {
	workdir := t.TempDir()
	config := Config{Formatters: map[string]Definition{
		"broken": {
			Command:    []string{"definitely-not-a-formatter-command", "$FILE"},
			Extensions: []string{".txt"},
		},
	}}
	manager := New(workdir, config, t.Logf)
	path := filepath.Join(workdir, "note.txt")
	if err := os.WriteFile(path, []byte("bad"), 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := manager.Format(context.Background(), path)
	if err == nil || !strings.Contains(err.Error(), "broken") {
		t.Fatalf("want formatter error, got %v", err)
	}
	if result.Name != "broken" {
		t.Fatalf("unexpected result: %+v", result)
	}
	if data, _ := os.ReadFile(path); string(data) != "bad" {
		t.Fatalf("file changed after failure: %q", data)
	}
}

func TestConfigAcceptsBooleanAndObject(t *testing.T) {
	var disabled Config
	if err := json.Unmarshal([]byte(`false`), &disabled); err != nil || disabled.Enabled == nil || *disabled.Enabled {
		t.Fatalf("unexpected disabled config: %+v %v", disabled, err)
	}
	var enabled Config
	if err := json.Unmarshal([]byte(`true`), &enabled); err != nil || enabled.Enabled == nil || !*enabled.Enabled {
		t.Fatalf("unexpected enabled config: %+v %v", enabled, err)
	}
	var custom Config
	body := `{"fixer":{"command":["fix","$FILE"],"extensions":["txt"],"environment":{"A":"B"}}}`
	if err := json.Unmarshal([]byte(body), &custom); err != nil {
		t.Fatal(err)
	}
	def := custom.Formatters["fixer"]
	if len(def.Command) != 2 || len(def.Extensions) != 1 || def.Environment["A"] != "B" {
		t.Fatalf("unexpected custom config: %+v", custom)
	}
}
