//go:build windows

package proc

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

// helperEnv turns the test binary into the process that owns the job. The test
// cannot join its own job: ending the job would kill the test runner before it
// could report anything.
const helperEnv = "LINGZHU_PROC_ESCAPE_HELPER"

// markerEnv is where the detached child writes proof of survival. It must live
// outside the helper's temp directory, because that directory is removed when
// the helper exits.
const markerEnv = "LINGZHU_PROC_MARKER"

// TestLaunchEscapesKillOnCloseJob proves a detached launch survives the job that
// owns its parent. A self-update installer is started from a shell job that is
// killed when the core exits, so without breakaway it would die in the middle of
// replacing the binary.
func TestLaunchEscapesKillOnCloseJob(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "survived.txt")

	cmd := exec.Command(os.Args[0], "-test.run=TestLaunchEscapesKillOnCloseJobHelper", "-test.timeout=60s")
	cmd.Env = append(os.Environ(), helperEnv+"=1", markerEnv+"="+marker)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the detached child was killed with the job")
}

// TestLaunchEscapesKillOnCloseJobHelper joins a kill-on-close job, starts a
// detached child, then returns. Exiting closes the job handle, which ends every
// process still inside it: the child only survives if it actually left.
func TestLaunchEscapesKillOnCloseJobHelper(t *testing.T) {
	if os.Getenv(helperEnv) != "1" {
		t.Skip("helper only")
	}
	marker := os.Getenv(markerEnv)
	if marker == "" {
		t.Fatal("missing marker path")
	}

	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{
		BasicLimitInformation: windows.JOBOBJECT_BASIC_LIMIT_INFORMATION{
			// Track uses the same flags, so this mirrors production: the job
			// ends its children when it closes, but a child that asks to
			// detach may leave first.
			LimitFlags: windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE | windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK,
		},
	}
	if _, err := windows.SetInformationJobObject(
		job,
		windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	); err != nil {
		t.Fatal(err)
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(os.Getpid()))
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		t.Skipf("this process cannot join a job: %v", err)
	}

	// The child writes the marker after the helper is already gone, so only a
	// process that left the job can still be running.
	script := "Start-Sleep -Seconds 2; Set-Content -Path '" + marker + "' -Value alive"
	child := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	if err := Launch(child); err != nil {
		t.Fatalf("launch: %v", err)
	}
	windows.CloseHandle(job)
}
