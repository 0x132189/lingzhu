package pty

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	terminalTicketTTL = time.Minute
	terminalTailBytes = 256 << 10
)

// Size is the terminal viewport.
type Size struct {
	Cols int `json:"cols"`
	Rows int `json:"rows"`
}

// OutputRange is the retained output byte range.
type OutputRange struct {
	Head int64 `json:"head"`
	Tail int64 `json:"tail"`
}

// Cursor is the screen cursor position.
type Cursor struct {
	X int `json:"x"`
	Y int `json:"y"`
}

// PersistentInfo is one terminal that survives core restarts as a record.
type PersistentInfo struct {
	ID                string      `json:"id"`
	Title             string      `json:"title"`
	ShellTitle        string      `json:"shellTitle,omitempty"`
	Command           string      `json:"command"`
	Args              []string    `json:"args"`
	Cwd               string      `json:"cwd"`
	SessionID         string      `json:"sessionID"`
	Status            string      `json:"status"`
	PID               int         `json:"pid"`
	ExitCode          *int        `json:"exitCode,omitempty"`
	UserTitle         *string     `json:"userTitle"`
	ForegroundProcess *string     `json:"foregroundProcess"`
	Size              Size        `json:"size"`
	Output            OutputRange `json:"output"`
}

// PersistentSnapshot is a rendered screen plus metadata.
type PersistentSnapshot struct {
	Info       PersistentInfo `json:"info"`
	Text       string         `json:"text"`
	Checkpoint string         `json:"checkpoint"`
	Cursor     Cursor         `json:"cursor"`
}

// PersistentRead is the last physical rows of a session terminal.
type PersistentRead struct {
	PTYID             string  `json:"ptyID"`
	Title             string  `json:"title"`
	Cwd               string  `json:"cwd"`
	ForegroundProcess *string `json:"foregroundProcess"`
	Screen            struct {
		Text   string `json:"text"`
		Cols   int    `json:"cols"`
		Rows   int    `json:"rows"`
		Cursor Cursor `json:"cursor"`
	} `json:"screen"`
}

type persistentRecord struct {
	info    PersistentInfo
	file    *os.File
	updated time.Time
	removed bool
}

type persistentTicket struct {
	id      string
	expires time.Time
}

func (m *Manager) persistentPath(id, extension string) string {
	return filepath.Join(m.persistDir, id+extension)
}

// StartPersistent starts a terminal attached to a session and persists its
// metadata and output under the core data directory.
func (m *Manager) StartPersistent(sessionID string, options Options, size Size) (PersistentInfo, error) {
	if strings.TrimSpace(sessionID) == "" {
		return PersistentInfo{}, errors.New("terminal needs a session")
	}
	command, args, err := m.ptyCommand(options)
	if err != nil {
		return PersistentInfo{}, err
	}
	cwd, err := m.resolveCwd(options.Cwd)
	if err != nil {
		return PersistentInfo{}, err
	}
	if size.Cols <= 0 {
		size.Cols = 80
	}
	if size.Rows <= 0 {
		size.Rows = 24
	}
	m.mu.Lock()
	if len(m.byID) >= maxSessions {
		m.mu.Unlock()
		return PersistentInfo{}, errors.New("pty: too many sessions")
	}
	m.mu.Unlock()

	id := newID("pty_")
	file, err := os.OpenFile(m.persistentPath(id, ".log"), os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return PersistentInfo{}, err
	}
	cmd := exec.Command(command, args...)
	cmd.Dir = cwd
	cmd.Env = mergedEnv(options.Env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = file.Close()
		return PersistentInfo{}, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = file.Close()
		return PersistentInfo{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = file.Close()
		return PersistentInfo{}, err
	}
	if err := cmd.Start(); err != nil {
		_ = file.Close()
		return PersistentInfo{}, err
	}
	title := strings.TrimSpace(options.Title)
	if title == "" {
		title = filepath.Base(command)
	}
	foreground := filepath.Base(command)
	info := PersistentInfo{
		ID:                id,
		Title:             title,
		ShellTitle:        filepath.Base(command),
		Command:           command,
		Args:              append([]string(nil), args...),
		Cwd:               cwd,
		SessionID:         sessionID,
		Status:            "running",
		PID:               cmd.Process.Pid,
		ForegroundProcess: &foreground,
		Size:              size,
	}
	state := &sessionState{
		kind:  "pty",
		stdin: stdin,
		cmd:   cmd,
		info: Info{
			ID:      id,
			Title:   title,
			Command: command,
			Args:    append([]string(nil), args...),
			Cwd:     cwd,
			Status:  "running",
			PID:     cmd.Process.Pid,
		},
		done:    make(chan struct{}),
		subs:    map[chan Chunk]struct{}{},
		persist: &persistentRecord{info: info, file: file, updated: time.Now()},
	}
	m.mu.Lock()
	m.byID[id] = state
	m.order = append(m.order, id)
	m.persistent[id] = state.persist
	m.savePersistentLocked(state.persist)
	m.mu.Unlock()

	state.copies.Add(2)
	go m.copyOutput(state, stdout)
	go m.copyOutput(state, stderr)
	go m.wait(state)
	return info, nil
}

// ListPersistent lists terminals of one session, newest first.
func (m *Manager) ListPersistent(sessionID string) []PersistentInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]PersistentInfo, 0, len(m.persistent))
	for _, record := range m.persistent {
		if record.info.SessionID == sessionID {
			out = append(out, record.info)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID > out[j].ID
		}
		return out[i].Title > out[j].Title
	})
	return out
}

// GetPersistent returns one terminal record.
func (m *Manager) GetPersistent(id string) (PersistentInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.persistent[id]
	if record == nil {
		return PersistentInfo{}, false
	}
	return record.info, true
}

// UpdatePersistent renames or resizes a terminal. clearTitle returns to the
// shell title; a nil title leaves it unchanged.
func (m *Manager) UpdatePersistent(id string, title *string, clearTitle bool, size *Size) (PersistentInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	record := m.persistent[id]
	if record == nil {
		return PersistentInfo{}, ErrNotFound
	}
	if clearTitle {
		record.info.UserTitle = nil
		record.info.Title = record.info.ShellTitle
	} else if title != nil {
		value := strings.TrimSpace(*title)
		if value == "" {
			record.info.UserTitle = nil
			record.info.Title = record.info.ShellTitle
		} else {
			record.info.UserTitle = &value
			record.info.Title = value
		}
	}
	if size != nil {
		if size.Cols <= 0 || size.Rows <= 0 {
			return PersistentInfo{}, errors.New("terminal size must be positive")
		}
		record.info.Size = *size
	}
	record.updated = time.Now()
	m.savePersistentLocked(record)
	return record.info, nil
}

// Snapshot renders the terminal's retained output.
func (m *Manager) Snapshot(id string, lines int) (PersistentSnapshot, error) {
	m.mu.Lock()
	record := m.persistent[id]
	if record == nil {
		m.mu.Unlock()
		return PersistentSnapshot{}, ErrNotFound
	}
	info := record.info
	path := m.persistentPath(id, ".log")
	m.mu.Unlock()
	text := readTail(path, linesFor(info, lines))
	return PersistentSnapshot{Info: info, Text: text, Cursor: cursorFor(text)}, nil
}

// ReadPersistent returns the most recently updated terminal for a session.
func (m *Manager) ReadPersistent(sessionID string, lines int) (*PersistentRead, error) {
	m.mu.Lock()
	var record *persistentRecord
	for _, item := range m.persistent {
		if item.info.SessionID != sessionID {
			continue
		}
		if record == nil || item.updated.After(record.updated) {
			record = item
		}
	}
	if record == nil {
		m.mu.Unlock()
		return nil, nil
	}
	info := record.info
	m.mu.Unlock()
	text := readTail(m.persistentPath(info.ID, ".log"), linesFor(info, lines))
	read := &PersistentRead{PTYID: info.ID, Title: info.Title, Cwd: info.Cwd, ForegroundProcess: info.ForegroundProcess}
	read.Screen.Text = text
	read.Screen.Cols = info.Size.Cols
	read.Screen.Rows = info.Size.Rows
	read.Screen.Cursor = cursorFor(text)
	return read, nil
}

// RemovePersistent terminates and deletes a terminal record.
func (m *Manager) RemovePersistent(id string) error {
	m.mu.Lock()
	record := m.persistent[id]
	if record == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	record.removed = true
	state := m.byID[id]
	delete(m.persistent, id)
	if state != nil {
		m.dropLocked(id, state)
	}
	m.mu.Unlock()
	if state != nil {
		kill(state)
		select {
		case <-state.done:
		case <-time.After(2 * time.Second):
		}
	}
	_ = os.Remove(m.persistentPath(id, ".json"))
	_ = os.Remove(m.persistentPath(id, ".log"))
	return nil
}

// IssueTicket creates a short-lived connect ticket for one terminal.
func (m *Manager) IssueTicket(id string) (string, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.persistent[id] == nil {
		return "", 0, ErrNotFound
	}
	now := time.Now()
	for key, ticket := range m.tickets {
		if ticket.expires.Before(now) {
			delete(m.tickets, key)
		}
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", 0, err
	}
	ticket := hex.EncodeToString(buf)
	m.tickets[ticket] = persistentTicket{id: id, expires: now.Add(terminalTicketTTL)}
	return ticket, int(terminalTicketTTL / time.Second), nil
}

// ValidTicket reports whether a connect ticket belongs to the terminal.
func (m *Manager) ValidTicket(id, ticket string) bool {
	if ticket == "" {
		return true
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item, ok := m.tickets[ticket]
	if !ok || item.id != id || item.expires.Before(time.Now()) {
		return false
	}
	delete(m.tickets, ticket)
	return true
}

func marshalInfo(info PersistentInfo) ([]byte, error) {
	return json.MarshalIndent(info, "", "  ")
}

func (m *Manager) savePersistentLocked(record *persistentRecord) {
	data, err := marshalInfo(record.info)
	if err != nil {
		return
	}
	path := m.persistentPath(record.info.ID, ".json")
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(temp, path)
}

func (m *Manager) loadPersistent() {
	entries, err := os.ReadDir(m.persistDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(m.persistDir, entry.Name()))
		if err != nil {
			continue
		}
		var info PersistentInfo
		if err := json.Unmarshal(data, &info); err != nil || info.ID == "" {
			continue
		}
		info.Status = "exited"
		info.PID = 0
		if info.ShellTitle == "" {
			info.ShellTitle = filepath.Base(info.Command)
		}
		if info.UserTitle != nil {
			info.Title = *info.UserTitle
		} else {
			info.Title = info.ShellTitle
		}
		record := &persistentRecord{info: info, updated: time.Now()}
		if stat, err := os.Stat(m.persistentPath(info.ID, ".log")); err == nil {
			info.Output.Tail = stat.Size()
			record.updated = stat.ModTime()
		}
		m.persistent[info.ID] = record
	}
}

func linesFor(info PersistentInfo, lines int) int {
	if lines > 0 {
		if lines > 65535 {
			return 65535
		}
		return lines
	}
	if info.Size.Rows > 0 {
		return info.Size.Rows
	}
	return 24
}

func readTail(path string, lines int) string {
	if lines <= 0 {
		lines = 24
	}
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return ""
	}
	size := info.Size()
	start := int64(0)
	if size > terminalTailBytes {
		start = size - terminalTailBytes
	}
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(file)
	if err != nil {
		return ""
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	parts := strings.Split(text, "\n")
	if len(parts) > 0 && parts[len(parts)-1] == "" {
		parts = parts[:len(parts)-1]
	}
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func cursorFor(text string) Cursor {
	lines := strings.Split(text, "\n")
	if len(lines) == 0 {
		return Cursor{}
	}
	return Cursor{X: len([]rune(lines[len(lines)-1])), Y: len(lines) - 1}
}

// ShutdownPersistent terminates running persistent terminals but keeps their
// records and captured output for later inspection.
func (m *Manager) ShutdownPersistent() {
	m.mu.Lock()
	states := make([]*sessionState, 0, len(m.persistent))
	for _, record := range m.persistent {
		if state := m.byID[record.info.ID]; state != nil {
			states = append(states, state)
		}
	}
	m.mu.Unlock()
	for _, state := range states {
		kill(state)
	}
	for _, state := range states {
		select {
		case <-state.done:
		case <-time.After(2 * time.Second):
		}
	}
}
