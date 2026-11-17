// Package proc starts child processes that can be stopped together with
// everything they spawned. A shell command such as `npm run dev` forks node
// children, so killing only the direct process would leave them running.
package proc

import (
	"os/exec"
	"time"
)

// Grace is how long a stop waits for a child to exit on its own before it is
// forced. It matches the reference harness.
const Grace = 3 * time.Second

// WaitDelay bounds how long Wait spends draining output pipes after the
// process has exited. A detached descendant can inherit those pipes and keep
// them open; without a bound the tool call would wait until its timeout.
const WaitDelay = time.Second

// Launch starts a command that must outlive this process, such as a self-update
// installer or a relaunch helper. The command leaves the caller's job object
// and is never tracked, so shutting the core down cannot kill it. It returns as
// soon as the process exists; the caller gets no exit status.
func Launch(cmd *exec.Cmd) error {
	return launchDetached(cmd)
}

// Stop terminates a running command and its whole process tree. It sends the
// graceful signal first, waits up to grace, then forces the remainder. It is
// safe on a command that never started or already exited.
func Stop(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	if grace <= 0 {
		grace = Grace
	}
	if terminate(cmd) {
		// Wait for the group to leave on its own, bounded by the grace period.
		if exited(cmd, grace) {
			return
		}
	}
	kill(cmd)
	// The forced stop is asynchronous: the caller may delete the command's
	// working directory right after, which fails while the process still holds
	// it. Waiting here keeps Stop's postcondition "the tree is gone".
	_ = exited(cmd, grace)
}

// exited reports whether the process is gone within grace. It never waits for
// the caller's own Wait call, so the caller keeps ownership of the result.
func exited(cmd *exec.Cmd, grace time.Duration) bool {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !alive(cmd) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return !alive(cmd)
}
