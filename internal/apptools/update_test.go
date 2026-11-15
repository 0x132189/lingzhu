package apptools

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"lingzhu/internal/permission"
	"lingzhu/internal/tool"
)

// TestSelfUpdateRejectsNonInstaller proves the tool only accepts an existing
// executable: a typo must fail before anything is launched.
func TestSelfUpdateRejectsNonInstaller(t *testing.T) {
	dir := t.TempDir()
	text := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(text, []byte("not an installer"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Paths go through JSON, so backslashes must be escaped: a raw Windows path
	// would otherwise be read as an escape sequence.
	input := func(path string) string {
		data, err := json.Marshal(map[string]string{"installer": path})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}

	cases := []struct {
		name    string
		input   string
		wantErr string
	}{
		{"empty", `{"installer":""}`, "installer is empty"},
		{"missing", input(filepath.Join(dir, "gone.exe")), "cannot find the file"},
		{"not exe", input(text), "is not an installer executable"},
		{"directory", input(dir), "is not an installer executable"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := (&selfUpdateTool{}).Run(context.Background(), tool.Call{
				SessionID: "s1",
				Input:     []byte(c.input),
				Perm:      permission.New(nil),
			})
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Fatalf("want error containing %q, got %v", c.wantErr, err)
			}
		})
	}
}

// TestInstallerRelayWaitsThenRuns proves the relay waits before starting the
// installer. The delay is what lets the core persist the tool result before the
// installer replaces and stops it.
func TestInstallerRelayWaitsThenRuns(t *testing.T) {
	installer := filepath.Join(t.TempDir(), "lingzhu installer.exe")
	cmd := installerCmd(installer, 7)
	script := strings.Join(cmd.Args, " ")

	if !strings.Contains(script, "Start-Sleep -Seconds 7") {
		t.Fatalf("relay does not wait: %s", script)
	}
	if !strings.Contains(script, "-ArgumentList '/S'") {
		t.Fatalf("relay does not run a silent install: %s", script)
	}
	// A single quote in the path would end the PowerShell string early, so it
	// must be doubled.
	if !strings.Contains(script, "lingzhu installer.exe") {
		t.Fatalf("relay lost the installer path: %s", script)
	}
	quoted := installerCmd(filepath.Join(t.TempDir(), "it's.exe"), 1)
	if !strings.Contains(strings.Join(quoted.Args, " "), "it''s.exe") {
		t.Fatalf("relay does not escape a quote in the path: %v", quoted.Args)
	}
	if cmd.Dir != filepath.Dir(installer) {
		t.Fatalf("relay does not run beside the installer: %q", cmd.Dir)
	}
}
