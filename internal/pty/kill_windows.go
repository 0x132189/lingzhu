//go:build windows

package pty

import (
	"os/exec"
	"strconv"
)

// killTree terminates the process and its children. cmd.exe leaves a child
// process holding inherited pipes when only the shell is killed.
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	taskkill := exec.Command("taskkill", "/PID", strconv.Itoa(pid), "/T", "/F")
	_ = taskkill.Run()
	_ = cmd.Process.Kill()
}
