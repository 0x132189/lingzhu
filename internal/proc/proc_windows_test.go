//go:build windows

package proc

import (
	"os/exec"
	"testing"
)

func TestConfigureHidesTheConsoleWindow(t *testing.T) {
	cmd := exec.Command("cmd")
	Configure(cmd)
	if cmd.SysProcAttr == nil || cmd.SysProcAttr.CreationFlags&createNoWindow == 0 {
		t.Fatalf("Configure must suppress a new console window: %+v", cmd.SysProcAttr)
	}
}

func TestReleaseDropsTheJobHandle(t *testing.T) {
	cmd := exec.Command("cmd", "/c", "exit 0")
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	Track(cmd)
	if _, ok := jobs.Load(cmd); !ok {
		t.Fatal("Track did not register a job")
	}
	_ = cmd.Wait()
	Release(cmd)
	if _, ok := jobs.Load(cmd); ok {
		t.Fatal("Release left the job handle registered")
	}
}
