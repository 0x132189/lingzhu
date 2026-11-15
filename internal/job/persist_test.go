package job

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestOpenRestoresRecordedJobs proves a restart keeps every background command
// the user started. Without the record, the transcript card outlives its job
// and can never show output again.
func TestOpenRestoresRecordedJobs(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	output, err := first.createOutput("job_1_1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := output.WriteString("server listening on 5173\n"); err != nil {
		t.Fatal(err)
	}
	if err := output.Close(); err != nil {
		t.Fatal(err)
	}
	first.register("job_1_1", "s1", "npm run dev", output.Name())
	first.Finish("job_1_1", Completed, 0, "", "server listening on 5173\n")

	// A second manager over the same directory stands in for a restarted core.
	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := second.Get("job_1_1")
	if !ok {
		t.Fatal("a recorded job must survive a restart")
	}
	if item.Command != "npm run dev" || item.Status != Completed || item.ExitCode != 0 {
		t.Fatalf("unexpected restored job: %+v", item)
	}
	text, running, ok := second.Output("job_1_1", 64*1024)
	if !ok || running {
		t.Fatalf("a finished job must read back as finished: ok=%v running=%v", ok, running)
	}
	if text != "server listening on 5173\n" {
		t.Fatalf("output must survive a restart, got %q", text)
	}
}

// TestOpenClosesJobsThatWereRunning proves a job whose process died with the
// previous core is reported as interrupted instead of running forever.
func TestOpenClosesJobsThatWereRunning(t *testing.T) {
	dir := t.TempDir()
	first, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	first.register("job_1_1", "s1", "npm run dev", filepath.Join(dir, "job_1_1.log"))

	second, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	item, ok := second.Get("job_1_1")
	if !ok {
		t.Fatal("the job must be restored")
	}
	if item.Status != Failed {
		t.Fatalf("a job whose process died must not report running, got %s", item.Status)
	}
	if item.Error == "" {
		t.Fatal("an interrupted job must explain why it stopped")
	}
	if item.Finished.IsZero() || item.Finished.After(time.Now()) {
		t.Fatalf("an interrupted job needs a plausible finish time, got %v", item.Finished)
	}
	// The corrected record is written back, so a second restart does not redo it.
	again, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	restored, _ := again.Get("job_1_1")
	if restored.Status != Failed || restored.Error != item.Error {
		t.Fatalf("the corrected status must be stored: %+v", restored)
	}
}

// TestStartWritesOutputBesideTheRecords proves the captured output lives in the
// data directory, so it is not lost when the system cleans its temp directory.
func TestStartWritesOutputBesideTheRecords(t *testing.T) {
	dir := t.TempDir()
	manager, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	item, err := manager.Start("s1", []string{"cmd", "/c", "echo hello"}, dir, "echo hello", 0)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(item.File) != dir {
		t.Fatalf("output must live beside the records, got %s", item.File)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		current, ok := manager.Get(item.ID)
		if ok && current.Status != Running {
			if current.Status != Completed {
				t.Fatalf("unexpected status: %+v", current)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the command did not finish")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(item.File); err != nil {
		t.Fatalf("the output file must stay on disk: %v", err)
	}
}

// TestNewKeepsJobsInMemory proves the test constructor still works without a
// data directory, so callers that do not want persistence are unaffected.
func TestNewKeepsJobsInMemory(t *testing.T) {
	manager := New()
	if _, err := manager.createOutput("job_1_1"); err != nil {
		t.Fatal(err)
	}
	manager.register("job_1_1", "s1", "true", "")
	if _, ok := manager.Get("job_1_1"); !ok {
		t.Fatal("an in-memory manager must still track its jobs")
	}
}
