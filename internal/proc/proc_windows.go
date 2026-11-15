//go:build windows

package proc

import (
	"errors"
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// createNewProcessGroup keeps the command from receiving the console's
// Ctrl+C, so the agent decides when it stops.
const createNewProcessGroup = 0x00000200

// createBreakawayFromJob lets a child leave the job object that owns this
// process. Without it a self-update installer would be killed the moment the
// core exits, which is exactly when it has to keep running.
const createBreakawayFromJob = 0x01000000

// createNoWindow starts a console program without allocating a console.
// DETACHED_PROCESS is not used: a console host such as PowerShell fails to run
// at all without one, so the update relay would silently never start.
const createNoWindow = 0x08000000

// jobs maps a running command to the job object that owns its tree. A job is
// the only Windows mechanism that ends a whole tree synchronously: taskkill
// returns before the processes it targeted have actually released their
// handles, which leaves a working directory locked for a moment.
var jobs sync.Map

// Configure gives the command its own process group.
func Configure(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup
}

// launchDetached starts a command that must outlive this process. It leaves the
// job object and gets its own console-less group, so stopping the core never
// reaches it. A job that does not allow breakaway rejects the flag with access
// denied, so the launch falls back to a plain detached start: the update still
// runs, even though an enclosing job could end it.
func launchDetached(cmd *exec.Cmd) error {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup | createBreakawayFromJob | createNoWindow
	cmd.SysProcAttr.HideWindow = true
	err := cmd.Start()
	if err == nil || !errors.Is(err, windows.ERROR_ACCESS_DENIED) {
		return err
	}
	cmd.SysProcAttr.CreationFlags &^= createBreakawayFromJob
	return cmd.Start()
}

// Track places a started command in its own job object. It must run right
// after Start, before the command can spawn children that would escape.
func Track(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			// Closing the last handle ends every process still in the job.
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE,
		},
	}
	// A command that asks to detach (a self-update installer, a relaunch
	// helper) must be allowed to leave, otherwise the core's exit kills the
	// update halfway through.
	info.BasicLimitInformation.LimitFlags |= windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		windows.CloseHandle(job)
		return
	}
	process, err := windows.OpenProcess(
		windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE,
		false,
		uint32(cmd.Process.Pid),
	)
	if err != nil {
		windows.CloseHandle(job)
		return
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		windows.CloseHandle(job)
		return
	}
	jobs.Store(cmd, job)
}

// ConfigureDetached is kept for callers that build their own detached command;
// Launch is the supported entry point.
func ConfigureDetached(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= createNewProcessGroup | createBreakawayFromJob | createNoWindow
	cmd.SysProcAttr.HideWindow = true
}

// terminate has no graceful signal on Windows, so it reports false and the
// caller falls through to a forced tree kill.
func terminate(cmd *exec.Cmd) bool { return false }

// kill ends the whole tree at once. Terminating the job is synchronous: when
// it returns, no process in the tree is still running.
func kill(cmd *exec.Cmd) {
	if value, ok := jobs.LoadAndDelete(cmd); ok {
		job := value.(windows.Handle)
		_ = windows.TerminateJobObject(job, 1)
		windows.CloseHandle(job)
		return
	}
	_ = cmd.Process.Kill()
}

// alive reports whether the process still exists. It waits on the process
// handle rather than calling Wait, so it never races the goroutine that owns
// the command's result.
func alive(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return false
	}
	handle, err := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)
	status, waitErr := windows.WaitForSingleObject(handle, 0)
	return waitErr == nil && status == uint32(windows.WAIT_TIMEOUT)
}
