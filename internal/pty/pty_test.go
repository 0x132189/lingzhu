package pty

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func ptyCommand() (string, []string, string) {
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/Q"}, "\r\n"
	}
	return "/bin/sh", nil, "\n"
}

func newManager(t *testing.T) *Manager {
	t.Helper()
	manager, err := New(t.TempDir(), nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return manager
}

func pollPTY(t *testing.T, manager *Manager, id, marker string, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var output strings.Builder
	var after int64
	for time.Now().Before(deadline) {
		chunks, next, _, err := manager.Output(id, after)
		if err != nil {
			t.Fatal(err)
		}
		after = next
		for _, chunk := range chunks {
			output.WriteString(chunk.Data)
		}
		if strings.Contains(output.String(), marker) {
			return output.String()
		}
		time.Sleep(20 * time.Millisecond)
	}
	return output.String()
}

func TestPTYLifecycle(t *testing.T) {
	manager := newManager(t)
	defer manager.CloseAll()
	command, args, newline := ptyCommand()
	info, _, err := manager.StartPTY(Options{Command: []string{command}, Args: args, Title: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(info.ID, "pty_") || info.Status != "running" || info.Title != "test" || info.PID <= 0 {
		t.Fatalf("unexpected info: %+v", info)
	}
	if err := manager.Write(info.ID, "echo PTY_MARKER"+newline); err != nil {
		t.Fatal(err)
	}
	output := pollPTY(t, manager, info.ID, "PTY_MARKER", 5*time.Second)
	if !strings.Contains(output, "PTY_MARKER") {
		t.Fatalf("marker missing: %q", output)
	}

	updated, err := manager.Update(info.ID, "renamed", 120, 40)
	if err != nil || updated.Title != "renamed" {
		t.Fatalf("unexpected update: %+v %v", updated, err)
	}
	if list := manager.ListPTY(); len(list) != 1 || list[0].ID != info.ID {
		t.Fatalf("unexpected list: %+v", list)
	}

	if err := manager.Write(info.ID, "exit"+newline); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, found := manager.GetPTY(info.ID)
		if !found {
			t.Fatal("exited session should remain until removed")
		}
		if got.Status == "exited" {
			if got.ExitCode == nil {
				t.Fatal("exited session should carry an exit code")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not exit: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := manager.Remove(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, found := manager.GetPTY(info.ID); found {
		t.Fatal("removed session should be gone")
	}
	if err := manager.Remove(info.ID); err != ErrNotFound {
		t.Fatalf("second remove should report not found: %v", err)
	}
}

func TestPTYSubscribe(t *testing.T) {
	manager := newManager(t)
	defer manager.CloseAll()
	command, args, newline := ptyCommand()
	info, _, err := manager.StartPTY(Options{Command: []string{command}, Args: args})
	if err != nil {
		t.Fatal(err)
	}
	chunks, cancel, err := manager.Subscribe(info.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer cancel()
	if err := manager.Write(info.ID, "echo SUB_MARKER"+newline); err != nil {
		t.Fatal(err)
	}
	deadline := time.After(5 * time.Second)
	var output strings.Builder
	for {
		select {
		case chunk, ok := <-chunks:
			if !ok {
				t.Fatalf("subscription closed early: %q", output.String())
			}
			output.WriteString(chunk.Data)
			if strings.Contains(output.String(), "SUB_MARKER") {
				return
			}
		case <-deadline:
			t.Fatalf("subscription timeout: %q", output.String())
		}
	}
}

func TestShellLifecycle(t *testing.T) {
	manager := newManager(t)
	defer manager.CloseAll()
	info, err := manager.StartShell("echo SHELL_MARKER", "", map[string]any{"session": "s1"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(info.ID, "sh_") || info.Status != "running" || info.File == "" || info.Metadata["session"] != "s1" {
		t.Fatalf("unexpected shell info: %+v", info)
	}
	deadline := time.Now().Add(5 * time.Second)
	var exited ShellInfo
	for {
		got, found := manager.GetShell(info.ID)
		if !found {
			t.Fatal("shell command disappeared")
		}
		if got.Status != "running" {
			exited = got
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("shell did not exit: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if exited.Exit == nil || *exited.Exit != 0 || exited.Time.Completed == nil {
		t.Fatalf("unexpected exited shell: %+v", exited)
	}
	if list := manager.ListShell(); len(list) != 0 {
		t.Fatalf("exited commands should not be listed: %+v", list)
	}

	var output strings.Builder
	var cursor int64
	for {
		page, err := manager.ShellOutput(info.ID, cursor, 1024)
		if err != nil {
			t.Fatal(err)
		}
		output.WriteString(page.Output)
		cursor = page.Cursor
		if cursor >= page.Size {
			break
		}
	}
	if !strings.Contains(output.String(), "SHELL_MARKER") {
		t.Fatalf("shell output missing marker: %q", output.String())
	}
	if err := manager.RemoveShell(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, found := manager.GetShell(info.ID); found {
		t.Fatal("removed shell should be gone")
	}
	if _, err := os.Stat(info.File); !os.IsNotExist(err) {
		t.Fatalf("output file should be removed: %v", err)
	}
}

func TestShellExitCodeAndTimeout(t *testing.T) {
	manager := newManager(t)
	defer manager.CloseAll()
	command := "exit 7"
	if runtime.GOOS == "windows" {
		command = "exit 7"
	}
	info, err := manager.StartShell(command, "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := manager.GetShell(info.ID)
		if got.Status != "running" {
			if got.Exit == nil || *got.Exit != 7 {
				t.Fatalf("unexpected exit: %+v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("command did not exit: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}

	slow := "sleep 5"
	if runtime.GOOS == "windows" {
		slow = "ping -n 30 127.0.0.1 > NUL"
	}
	timeoutInfo, err := manager.StartShell(slow, "", nil, 150*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		got, _ := manager.GetShell(timeoutInfo.ID)
		if got.Status == "timeout" {
			break
		}
		if got.Status != "running" {
			t.Fatalf("unexpected timeout status: %+v", got)
		}
		if time.Now().After(deadline) {
			t.Fatalf("command did not time out: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestRemoveRunningShell(t *testing.T) {
	manager := newManager(t)
	defer manager.CloseAll()
	command := "sleep 30"
	if runtime.GOOS == "windows" {
		command = "ping -n 30 127.0.0.1 > NUL"
	}
	info, err := manager.StartShell(command, "", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RemoveShell(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, found := manager.GetShell(info.ID); found {
		t.Fatal("removed shell should be gone")
	}
	if _, err := os.Stat(info.File); !os.IsNotExist(err) {
		t.Fatalf("output file should be removed: %v", err)
	}
}

func TestUnknownSessions(t *testing.T) {
	manager := newManager(t)
	if _, ok := manager.GetPTY("pty_missing"); ok {
		t.Fatal("unknown pty should not be found")
	}
	if err := manager.Write("pty_missing", "x"); err != ErrNotFound {
		t.Fatalf("unexpected write error: %v", err)
	}
	if _, _, _, err := manager.Output("pty_missing", 0); err != ErrNotFound {
		t.Fatalf("unexpected output error: %v", err)
	}
	if _, err := manager.ShellOutput("sh_missing", 0, 10); err != ErrNotFound {
		t.Fatalf("unexpected shell output error: %v", err)
	}
}

func TestPersistentTerminalLifecycle(t *testing.T) {
	manager := newManager(t)
	defer manager.CloseAll()
	command, args, newline := ptyCommand()
	info, err := manager.StartPersistent("s1", Options{Command: []string{command}, Args: args, Title: "persist"}, Size{Cols: 100, Rows: 30})
	if err != nil {
		t.Fatal(err)
	}
	if info.ID == "" || info.SessionID != "s1" || info.Status != "running" || info.Size.Cols != 100 || info.Title != "persist" {
		t.Fatalf("unexpected info: %+v", info)
	}
	if err := manager.Write(info.ID, "echo PERSIST_MARKER"+newline); err != nil {
		t.Fatal(err)
	}
	output := pollPTY(t, manager, info.ID, "PERSIST_MARKER", 5*time.Second)
	if !strings.Contains(output, "PERSIST_MARKER") {
		t.Fatalf("marker missing: %q", output)
	}
	if list := manager.ListPersistent("s1"); len(list) != 1 || list[0].ID != info.ID {
		t.Fatalf("unexpected list: %+v", list)
	}
	read, err := manager.ReadPersistent("s1", 50)
	if err != nil || read == nil || !strings.Contains(read.Screen.Text, "PERSIST_MARKER") {
		t.Fatalf("unexpected read: %+v %v", read, err)
	}
	snapshot, err := manager.Snapshot(info.ID, 50)
	if err != nil || !strings.Contains(snapshot.Text, "PERSIST_MARKER") || snapshot.Info.ID != info.ID {
		t.Fatalf("unexpected snapshot: %+v %v", snapshot, err)
	}
	title := "renamed"
	updated, err := manager.UpdatePersistent(info.ID, &title, false, &Size{Cols: 120, Rows: 40})
	if err != nil || updated.Title != "renamed" || updated.Size.Cols != 120 || updated.UserTitle == nil {
		t.Fatalf("unexpected update: %+v %v", updated, err)
	}
	cleared, err := manager.UpdatePersistent(info.ID, nil, true, nil)
	if err != nil || cleared.UserTitle != nil || cleared.Title != cleared.ShellTitle {
		t.Fatalf("title was not cleared: %+v %v", cleared, err)
	}

	ticket, expires, err := manager.IssueTicket(info.ID)
	if err != nil || ticket == "" || expires <= 0 {
		t.Fatalf("unexpected ticket: %q %d %v", ticket, expires, err)
	}
	if !manager.ValidTicket(info.ID, ticket) {
		t.Fatal("ticket should be valid once")
	}
	if manager.ValidTicket(info.ID, ticket) {
		t.Fatal("ticket should be single use")
	}
	if manager.ValidTicket(info.ID, "bogus") {
		t.Fatal("unknown ticket should be rejected")
	}

	manager.ShutdownPersistent()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, _ := manager.GetPersistent(info.ID)
		if got.Status == "exited" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal did not exit: %+v", got)
		}
		time.Sleep(20 * time.Millisecond)
	}
	after, err := manager.Snapshot(info.ID, 50)
	if err != nil || !strings.Contains(after.Text, "PERSIST_MARKER") {
		t.Fatalf("snapshot lost after shutdown: %+v %v", after, err)
	}
	if err := manager.RemovePersistent(info.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.GetPersistent(info.ID); ok {
		t.Fatal("removed terminal should be gone")
	}
}

func TestPersistentTerminalSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	work := t.TempDir()
	manager, err := New(work, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	command, args, newline := ptyCommand()
	info, err := manager.StartPersistent("s1", Options{Command: []string{command}, Args: args, Title: "restart"}, Size{Cols: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Write(info.ID, "echo RESTART_MARKER"+newline); err != nil {
		t.Fatal(err)
	}
	pollPTY(t, manager, info.ID, "RESTART_MARKER", 5*time.Second)
	manager.CloseAll()

	reopened, err := New(work, nil, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.CloseAll()
	got, ok := reopened.GetPersistent(info.ID)
	if !ok || got.Status != "exited" || got.SessionID != "s1" {
		t.Fatalf("terminal did not survive restart: %+v %v", got, ok)
	}
	snapshot, err := reopened.Snapshot(info.ID, 50)
	if err != nil || !strings.Contains(snapshot.Text, "RESTART_MARKER") {
		t.Fatalf("snapshot lost after restart: %+v %v", snapshot, err)
	}
	read, err := reopened.ReadPersistent("s1", 50)
	if err != nil || read == nil || !strings.Contains(read.Screen.Text, "RESTART_MARKER") {
		t.Fatalf("read lost after restart: %+v %v", read, err)
	}
}
