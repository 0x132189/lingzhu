//go:build !windows

package pty

import "os/exec"

// killTree terminates the process group so children do not keep inherited
// pipes open after the shell exits.
func killTree(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
