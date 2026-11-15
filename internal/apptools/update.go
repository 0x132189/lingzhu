// Package apptools exposes model-facing tools for managing lingzhu itself:
// session titles, git worktrees, the model catalog, and installing a build the
// model produced.
package apptools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"lingzhu/internal/proc"
	"lingzhu/internal/tool"
)

// selfUpdateTool installs a build the model produced. It exists because the
// shell tool runs its children inside a job object that is killed with the
// core: an installer started that way dies halfway through replacing the very
// binary that started it. This tool detaches the installer and delays it, so
// the core can finish writing the tool result before it is stopped.
type selfUpdateTool struct{}

func (t *selfUpdateTool) Name() string { return "lingzhu_self_update" }

func (t *selfUpdateTool) Description() string {
	return "Install a Windows installer for lingzhu itself and let the app restart into it. " +
		"The installer runs detached from this process, so the core being replaced cannot kill it. " +
		"The app and this core are stopped and started again by the installer, so the current turn ends here. " +
		"Use this only for an installer you built for this app."
}

func (t *selfUpdateTool) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"installer": {"type": "string", "description": "Absolute path to the installer executable, usually lingzhu-app/bin/lingzhu-app-amd64-installer.exe."},
			"delay": {"type": "integer", "description": "Seconds to wait before installing, so this tool result is saved first. Defaults to 5, minimum 1."}
		},
		"required": ["installer"]
	}`)
}

func (t *selfUpdateTool) Run(ctx context.Context, call tool.Call) (string, error) {
	var args struct {
		Installer string `json:"installer"`
		Delay     int    `json:"delay"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("lingzhu_self_update: %w", err)
	}
	path := strings.TrimSpace(args.Installer)
	if path == "" {
		return "", fmt.Errorf("lingzhu_self_update: installer is empty")
	}
	if err := call.Allow(ctx, "lingzhu_self_update", []string{path}, ""); err != nil {
		return "", err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("lingzhu_self_update: %w", err)
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("lingzhu_self_update: %w", err)
	}
	if info.IsDir() || !strings.EqualFold(filepath.Ext(abs), ".exe") {
		return "", fmt.Errorf("lingzhu_self_update: %s is not an installer executable", abs)
	}

	delay := args.Delay
	if delay <= 0 {
		delay = 5
	}
	if err := launchDetached(abs, delay); err != nil {
		return "", fmt.Errorf("lingzhu_self_update: %w", err)
	}
	return fmt.Sprintf(
		"Installer %s will run in %d seconds and restart lingzhu. "+
			"This turn ends when the core stops; the session resumes in the new build.",
		filepath.Base(abs), delay,
	), nil
}

// launchDetached starts a relay that waits, then runs the installer. The relay
// is a separate detached process, so nothing this core owns can take it down.
func launchDetached(installer string, delay int) error {
	if err := proc.Launch(installerCmd(installer, delay)); err != nil {
		return err
	}
	return nil
}

// installerCmd builds the waiting relay. PowerShell ships with Windows and
// needs no helper binary beside the core.
func installerCmd(installer string, delay int) *exec.Cmd {
	script := fmt.Sprintf(
		"Start-Sleep -Seconds %d; Start-Process -FilePath '%s' -ArgumentList '/S'",
		delay, strings.ReplaceAll(installer, "'", "''"),
	)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	cmd.Dir = filepath.Dir(installer)
	return cmd
}
