// Package job tracks background shell commands. A job owns an output file and
// reports completion once, so the agent can resume the session with a synthetic
// message instead of polling.
package job

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"lingzhu/internal/proc"
)

// Status is the lifecycle state of one background job.
type Status string

const (
	Running   Status = "running"
	Completed Status = "completed"
	Failed    Status = "failed"
	Cancelled Status = "cancelled"
)

// Job is one background command.
type Job struct {
	ID        string    `json:"id"`
	SessionID string    `json:"sessionId"`
	Command   string    `json:"command"`
	File      string    `json:"file"`
	Status    Status    `json:"status"`
	ExitCode  int       `json:"exitCode"`
	Error     string    `json:"error,omitempty"`
	Started   time.Time `json:"started"`
	Finished  time.Time `json:"finished,omitempty"`
	// Tail is the bounded output included in the completion notification.
	Tail string `json:"-"`
}

// Notification renders the synthetic message handed back to the model.
func (j Job) Notification() string {
	var b strings.Builder
	b.WriteString("[Background shell completed]\n")
	b.WriteString("Job: " + j.ID + "\n")
	b.WriteString("Command: " + j.Command + "\n")
	if j.ExitCode >= 0 {
		fmt.Fprintf(&b, "Status: %s (exit %d)\n", j.Status, j.ExitCode)
	} else {
		fmt.Fprintf(&b, "Status: %s\n", j.Status)
	}
	if j.Error != "" {
		b.WriteString("Error: " + j.Error + "\n")
	}
	b.WriteString("Output file: " + j.File + "\n")
	b.WriteString("Output:\n")
	if strings.TrimSpace(j.Tail) == "" {
		b.WriteString("(no output)")
	} else {
		b.WriteString(strings.TrimRight(j.Tail, "\n"))
	}
	return b.String()
}

// Manager owns the running jobs of one process. Jobs are also recorded on disk,
// so a restart keeps their command, status and captured output instead of
// losing every background command the user started.
type Manager struct {
	mu     sync.Mutex
	jobs   map[string]*Job
	cmds   map[string]*exec.Cmd
	seq    atomic.Int64
	notify func(Job)
	closed bool
	// dir is where job records and their output files live; empty disables
	// persistence, which only tests use.
	dir string
}

// New creates an empty manager that keeps nothing on disk.
func New() *Manager {
	return &Manager{jobs: map[string]*Job{}, cmds: map[string]*exec.Cmd{}}
}

// Open creates a manager that records jobs under dir. An existing record set is
// loaded, and a job that was running when the process stopped is reported as
// interrupted: its process did not survive the restart.
func Open(dir string) (*Manager, error) {
	m := &Manager{jobs: map[string]*Job{}, cmds: map[string]*exec.Cmd{}, dir: dir}
	if strings.TrimSpace(dir) == "" {
		return m, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m.load()
	return m, nil
}

// load reads every recorded job. A job still marked running belongs to a
// process that is gone, so it is closed as interrupted rather than reported as
// running forever.
func (m *Manager) load() {
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), recordSuffix) {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.dir, entry.Name()))
		if err != nil {
			continue
		}
		var item Job
		if err := json.Unmarshal(data, &item); err != nil || item.ID == "" {
			continue
		}
		if item.Status == Running {
			item.Status = Failed
			item.Error = "interrupted by a restart"
			item.ExitCode = -1
			if item.Finished.IsZero() {
				item.Finished = time.Now().UTC()
			}
		}
		stored := item
		m.jobs[item.ID] = &stored
	}
	m.rewrite()
}

// rewrite writes every job record, so a record corrected during load is stored
// with its final status.
func (m *Manager) rewrite() {
	for _, item := range m.jobs {
		m.persistLocked(item)
	}
}

// recordSuffix is the file extension of one stored job.
const recordSuffix = ".json"

// persistLocked writes one job record. The caller holds the lock.
func (m *Manager) persistLocked(item *Job) {
	if m.dir == "" || item == nil {
		return
	}
	data, err := json.Marshal(item)
	if err != nil {
		return
	}
	path := filepath.Join(m.dir, item.ID+recordSuffix)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return
	}
}

// SetNotifier registers the completion callback. It runs on its own goroutine
// so a slow session resume never blocks the command watcher.
func (m *Manager) SetNotifier(fn func(Job)) {
	m.mu.Lock()
	m.notify = fn
	m.mu.Unlock()
}

// Create registers a running job and returns its snapshot.
func (m *Manager) Create(sessionID, command, file string) Job {
	id := fmt.Sprintf("job_%d_%d", time.Now().UnixNano(), m.seq.Add(1))
	return m.register(id, sessionID, command, file)
}

// createOutput opens the file that captures one job's output. It lives beside
// the job records so a restart can still read it, and an empty data directory
// falls back to the system temporary directory.
func (m *Manager) createOutput(id string) (*os.File, error) {
	if m.dir == "" {
		return os.CreateTemp("", "lingzhu-shell-*.log")
	}
	return os.Create(filepath.Join(m.dir, id+".log"))
}

// register records one job and persists it.
func (m *Manager) register(id, sessionID, command, file string) Job {
	item := &Job{
		ID:        id,
		SessionID: sessionID,
		Command:   command,
		File:      file,
		Status:    Running,
		ExitCode:  -1,
		Started:   time.Now().UTC(),
	}
	m.mu.Lock()
	m.jobs[id] = item
	m.persistLocked(item)
	snapshot := *item
	m.mu.Unlock()
	return snapshot
}

// Attach records the process so Close can stop it.
func (m *Manager) Attach(id string, cmd *exec.Cmd) {
	m.mu.Lock()
	m.cmds[id] = cmd
	m.mu.Unlock()
}

// Finish records the terminal state and notifies once. A job that was already
// cancelled keeps the cancelled status.
func (m *Manager) Finish(id string, status Status, exitCode int, errText, tail string) {
	m.mu.Lock()
	item, ok := m.jobs[id]
	if !ok || item.Status != Running {
		m.mu.Unlock()
		return
	}
	item.Status = status
	item.ExitCode = exitCode
	item.Error = errText
	item.Tail = tail
	item.Finished = time.Now().UTC()
	delete(m.cmds, id)
	m.persistLocked(item)
	snapshot := *item
	notify := m.notify
	m.mu.Unlock()
	if notify != nil {
		go notify(snapshot)
	}
}

// Start launches one background command using shell as the prefix. Output goes
// to a file under the manager's data directory, so a restart can still read it;
// completion notifies once, matching the shell tool.
func (m *Manager) Start(sessionID string, shell []string, dir, command string, timeout time.Duration) (Job, error) {
	if len(shell) == 0 {
		return Job{}, errors.New("job: shell is not configured")
	}
	id := fmt.Sprintf("job_%d_%d", time.Now().UnixNano(), m.seq.Add(1))
	file, err := m.createOutput(id)
	if err != nil {
		return Job{}, err
	}
	path := file.Name()
	args := append(append([]string{}, shell[1:]...), command)
	cmd := exec.Command(shell[0], args...)
	cmd.Dir = dir
	proc.Configure(cmd)
	cmd.Stdout = file
	cmd.Stderr = file
	if err := cmd.Start(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return Job{}, err
	}
	proc.Track(cmd)
	item := m.register(id, sessionID, command, path)
	m.Attach(item.ID, cmd)

	var timer *time.Timer
	if timeout > 0 {
		timer = time.AfterFunc(timeout, func() {
			proc.Stop(cmd, proc.Grace)
		})
	}

	go func() {
		waitErr := cmd.Wait()
		if timer != nil {
			timer.Stop()
		}
		_ = file.Close()
		status := Completed
		exit := 0
		message := ""
		if waitErr != nil {
			status = Failed
			exit = -1
			message = waitErr.Error()
			var exitErr *exec.ExitError
			if errors.As(waitErr, &exitErr) {
				exit = exitErr.ExitCode()
			}
		}
		m.Finish(item.ID, status, exit, message, tail(path, 8*1024))
	}()
	return item, nil
}

// Get returns one job snapshot.
func (m *Manager) Get(id string) (Job, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.jobs[id]
	if !ok {
		return Job{}, false
	}
	return *item, true
}

// Output returns up to limit bytes from the end of a job's captured output and
// whether the process is still running.
func (m *Manager) Output(id string, limit int64) (string, bool, bool) {
	item, ok := m.Get(id)
	if !ok {
		return "", false, false
	}
	if limit <= 0 {
		limit = 64 * 1024
	}
	return tail(item.File, limit), item.Status == Running, true
}

// Cancel stops one running job and marks it cancelled without notifying the
// session, because a manual cancellation is already visible to the user.
func (m *Manager) Cancel(id string) (Job, error) {
	m.mu.Lock()
	item, ok := m.jobs[id]
	if !ok {
		m.mu.Unlock()
		return Job{}, errors.New("job not found")
	}
	if item.Status != Running {
		m.mu.Unlock()
		return Job{}, errors.New("job is not running")
	}
	cmd := m.cmds[id]
	delete(m.cmds, id)
	item.Status = Cancelled
	item.ExitCode = -1
	item.Error = "cancelled"
	item.Finished = time.Now().UTC()
	m.persistLocked(item)
	snapshot := *item
	m.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		proc.Stop(cmd, proc.Grace)
	}
	return snapshot, nil
}

// List returns every job of a session, newest first. An empty session returns
// all jobs.
func (m *Manager) List(sessionID string) []Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Job, 0, len(m.jobs))
	for _, item := range m.jobs {
		if sessionID != "" && item.SessionID != sessionID {
			continue
		}
		out = append(out, *item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Started.After(out[j].Started) })
	return out
}

// Close cancels every running job. It is called on process shutdown.
func (m *Manager) Close() {
	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	m.closed = true
	jobs := make([]*Job, 0, len(m.jobs))
	cmds := make([]*exec.Cmd, 0, len(m.cmds))
	for id, item := range m.jobs {
		if item.Status == Running {
			jobs = append(jobs, item)
			if cmd := m.cmds[id]; cmd != nil {
				cmds = append(cmds, cmd)
			}
		}
	}
	m.mu.Unlock()

	for _, cmd := range cmds {
		if cmd.Process != nil {
			proc.Stop(cmd, proc.Grace)
		}
	}
	for _, item := range jobs {
		m.Finish(item.ID, Cancelled, -1, "cancelled at shutdown", "")
	}
}

// tail returns at most limit bytes from the end of a file as valid UTF-8.
func tail(path string, limit int64) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	start := info.Size() - limit
	if start < 0 {
		start = 0
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return ""
	}
	return strings.ToValidUTF8(string(data), "")
}
