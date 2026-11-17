package proc

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// longCommand returns a command that keeps running until it is stopped, plus a
// marker file it creates so the test can tell it actually started.
func longCommand(t *testing.T, dir string) ([]string, string) {
	t.Helper()
	marker := filepath.Join(dir, "started")
	if runtime.GOOS == "windows" {
		// ping is available on every Windows install and ignores Ctrl+C.
		return []string{"cmd", "/c", "echo started> " + marker + " && ping -n 60 127.0.0.1"}, marker
	}
	return []string{"sh", "-c", "touch " + marker + " && sleep 60"}, marker
}

func TestStopEndsTheProcessAndReleasesTheDirectory(t *testing.T) {
	dir := t.TempDir()
	args, marker := longCommand(t, dir)
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	Track(cmd)
	waitForFile(t, marker)
	Stop(cmd, 2*time.Second)
	// A stopped tree must not hold the working directory any more.
	waitForRemoval(t, dir)
}

func TestConfigureBoundsThePostExitDrain(t *testing.T) {
	cmd := exec.Command(shellName())
	Configure(cmd)
	if cmd.WaitDelay != WaitDelay {
		t.Fatalf("WaitDelay = %s, want %s", cmd.WaitDelay, WaitDelay)
	}
}

// TestConfigureBoundsDetachedOutput makes a helper exit while its child still
// holds the helper's stdout pipe. WaitDelay must end the drain instead of
// letting the call block until its own timeout.
func TestConfigureBoundsDetachedOutput(t *testing.T) {
	cmd := exec.Command(os.Args[0], "-test.run=TestWaitDelayHelper")
	cmd.Env = append(os.Environ(), "LINGZHU_WAIT_DELAY_HELPER=1")
	var output bytes.Buffer
	cmd.Stdout = &output
	cmd.Stderr = &output
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	Track(cmd)
	started := time.Now()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-time.After(5 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("Wait blocked on a detached descendant's inherited stdout")
	}
	elapsed := time.Since(started)
	Stop(cmd, 100*time.Millisecond)
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("want ErrWaitDelay, got %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("drain took %s, want at most the one-second WaitDelay", elapsed)
	}
}

// TestWaitDelayHelper is the child process used by
// TestConfigureBoundsDetachedOutput. It exits while its own child keeps the
// inherited stdout pipe open.
func TestWaitDelayHelper(t *testing.T) {
	if os.Getenv("LINGZHU_WAIT_DELAY_HELPER") != "1" {
		return
	}
	var child *exec.Cmd
	if runtime.GOOS == "windows" {
		child = exec.Command("ping", "-n", "6", "127.0.0.1")
	} else {
		child = exec.Command("sleep", "6")
	}
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestStopIsSafeOnAnExitedCommand(t *testing.T) {
	cmd := exec.Command(shellName(), shellArgs("exit 0")...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	// Must not panic or block on a command that already finished.
	Stop(cmd, 100*time.Millisecond)
}

func TestStopIgnoresNilCommand(t *testing.T) {
	Stop(nil, time.Second)
}

func shellName() string {
	if runtime.GOOS == "windows" {
		return "cmd"
	}
	return "sh"
}

func shellArgs(script string) []string {
	if runtime.GOOS == "windows" {
		return []string{"/c", script}
	}
	return []string{"-c", script}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// waitForRemoval asserts the directory can be deleted, which fails while a
// stopped child still holds a handle inside it.
func waitForRemoval(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last error
	for time.Now().Before(deadline) {
		if err := os.RemoveAll(dir); err == nil {
			return
		} else {
			last = err
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("directory is still locked after stop: %v", last)
}

// TestStopFromContextWatcher covers the pattern the shell tool uses: the
// command is started without CommandContext, because that helper kills only
// the direct child and leaves the tree it spawned behind. A watcher goroutine
// calls Stop instead.
func TestStopFromContextWatcher(t *testing.T) {
	dir := t.TempDir()
	args, marker := longCommand(t, dir)
	ctx, cancel := context.WithCancel(context.Background())
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Dir = dir
	Configure(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	Track(cmd)
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		Stop(cmd, 2*time.Second)
		close(done)
	}()
	waitForFile(t, marker)
	cancel()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return after cancellation")
	}
	waitForRemoval(t, dir)
}
