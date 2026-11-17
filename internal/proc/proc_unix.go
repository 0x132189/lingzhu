//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

// Configure starts the command in its own process group, so a signal to the
// group reaches every process the command spawned.
func Configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.WaitDelay = WaitDelay
}

// ConfigureDetached starts a command in a session of its own, so it survives
// the core exiting and never receives the group's signals.
func ConfigureDetached(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

// Track is a no-op: the process group created by Configure already covers the
// tree, because a child inherits its parent's group.
func Track(cmd *exec.Cmd) {}

// Release is a no-op: Unix process groups need no handle to close after exit.
func Release(cmd *exec.Cmd) {}

// terminate asks the whole group to stop. It reports whether a signal was
// delivered.
func terminate(cmd *exec.Cmd) bool {
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) == nil
}

// kill forces the whole group to stop.
func kill(cmd *exec.Cmd) {
	_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// alive reports whether the process group still has a member.
func alive(cmd *exec.Cmd) bool {
	return syscall.Kill(-cmd.Process.Pid, 0) == nil
}
