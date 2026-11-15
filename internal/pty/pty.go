// Package pty owns interactive shell sessions and one-shot shell commands for
// clients. Interactive sessions stream output chunks in memory; one-shot
// commands capture combined stdout/stderr to a file that clients page through.
package pty

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const (
	maxSessions  = 32
	maxBuffer    = 1 << 20
	maxChunk     = 64 << 10
	defaultLimit = 64 << 10
	maxLimit     = 1 << 20
)

// ErrNotFound means the requested session is unknown.
var ErrNotFound = errors.New("pty: session not found")

// Options creates an interactive session.
type Options struct {
	Command []string
	Args    []string
	Cwd     string
	Title   string
	Env     map[string]string
}

// Info is one interactive session.
type Info struct {
	ID       string   `json:"id"`
	Title    string   `json:"title"`
	Command  string   `json:"command"`
	Args     []string `json:"args"`
	Cwd      string   `json:"cwd"`
	Status   string   `json:"status"`
	PID      int      `json:"pid"`
	ExitCode *int     `json:"exitCode,omitempty"`
}

// Chunk is one output fragment with a monotonically increasing sequence.
type Chunk struct {
	Seq  int64  `json:"seq"`
	Data string `json:"data"`
}

// ShellTime is the start and completion time in Unix milliseconds.
type ShellTime struct {
	Started   int64  `json:"started"`
	Completed *int64 `json:"completed,omitempty"`
}

// ShellInfo is one one-shot command.
type ShellInfo struct {
	ID       string         `json:"id"`
	Status   string         `json:"status"`
	Command  string         `json:"command"`
	Cwd      string         `json:"cwd"`
	Shell    string         `json:"shell"`
	File     string         `json:"file"`
	PID      int            `json:"pid,omitempty"`
	Exit     *int           `json:"exit,omitempty"`
	Signal   string         `json:"signal,omitempty"`
	Metadata map[string]any `json:"metadata"`
	Time     ShellTime      `json:"time"`
}

// Output is one page of captured shell output.
type Output struct {
	Output    string `json:"output"`
	Cursor    int64  `json:"cursor"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
}

type sessionState struct {
	info    Info
	shell   ShellInfo
	kind    string
	stdin   io.WriteCloser
	cmd     *exec.Cmd
	file    *os.File
	timer   *time.Timer
	done    chan struct{}
	started time.Time

	chunks   []Chunk
	nextSeq  int64
	buffered int
	subs     map[chan Chunk]struct{}
	copies   sync.WaitGroup
	persist  *persistentRecord
}

// Manager owns every running session for one core process.
type Manager struct {
	workdir    string
	shell      []string
	logDir     string
	persistDir string
	mu         sync.Mutex
	order      []string
	byID       map[string]*sessionState
	persistent map[string]*persistentRecord
	tickets    map[string]persistentTicket
}

// New creates a manager. shell is the configured shell command without the
// script argument; dataDir stores one-shot output files and terminal records.
func New(workdir string, shell []string, dataDir string) (*Manager, error) {
	logDir := filepath.Join(dataDir, "shell")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, err
	}
	persistDir := filepath.Join(dataDir, "terminals")
	if err := os.MkdirAll(persistDir, 0o755); err != nil {
		return nil, err
	}
	manager := &Manager{
		workdir:    workdir,
		shell:      append([]string(nil), shell...),
		logDir:     logDir,
		persistDir: persistDir,
		byID:       map[string]*sessionState{},
		persistent: map[string]*persistentRecord{},
		tickets:    map[string]persistentTicket{},
	}
	manager.loadPersistent()
	return manager, nil
}

// ListPTY returns interactive sessions, newest first.
func (m *Manager) ListPTY() []Info {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Info, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		if state := m.byID[m.order[i]]; state != nil && state.kind == "pty" {
			out = append(out, state.info)
		}
	}
	return out
}

// GetPTY returns one interactive session.
func (m *Manager) GetPTY(id string) (Info, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.byID[id]
	if state == nil || state.kind != "pty" {
		return Info{}, false
	}
	return state.info, true
}

// StartPTY launches an interactive shell and returns its starting sequence.
func (m *Manager) StartPTY(options Options) (Info, int64, error) {
	command, args, err := m.ptyCommand(options)
	if err != nil {
		return Info{}, 0, err
	}
	cwd, err := m.resolveCwd(options.Cwd)
	if err != nil {
		return Info{}, 0, err
	}
	m.mu.Lock()
	if len(m.byID) >= maxSessions {
		m.mu.Unlock()
		return Info{}, 0, errors.New("pty: too many sessions")
	}
	m.mu.Unlock()

	id := newID("pty_")
	cmd := exec.Command(command, args...)
	cmd.Dir = cwd
	cmd.Env = mergedEnv(options.Env)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return Info{}, 0, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Info{}, 0, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Info{}, 0, err
	}
	if err := cmd.Start(); err != nil {
		return Info{}, 0, err
	}
	title := strings.TrimSpace(options.Title)
	if title == "" {
		title = filepath.Base(command)
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
		done: make(chan struct{}),
		subs: map[chan Chunk]struct{}{},
	}
	info := state.info
	m.add(state)
	state.copies.Add(2)
	go m.copyOutput(state, stdout)
	go m.copyOutput(state, stderr)
	go m.wait(state)
	return info, 0, nil
}

// Write sends input to an interactive session.
func (m *Manager) Write(id, data string) error {
	m.mu.Lock()
	state := m.byID[id]
	if state == nil || state.kind != "pty" || state.info.Status != "running" || state.stdin == nil {
		m.mu.Unlock()
		return ErrNotFound
	}
	stdin := state.stdin
	m.mu.Unlock()
	_, err := io.WriteString(stdin, data)
	return err
}

// Output returns chunks after the given sequence, the next sequence and status.
func (m *Manager) Output(id string, after int64) ([]Chunk, int64, string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.byID[id]
	if state == nil || state.kind != "pty" {
		return nil, 0, "", ErrNotFound
	}
	out := make([]Chunk, 0, len(state.chunks))
	for _, chunk := range state.chunks {
		if chunk.Seq > after {
			out = append(out, chunk)
		}
	}
	return out, state.nextSeq, state.info.Status, nil
}

// Subscribe streams new chunks. after replays buffered chunks first. The cancel
// function must be called to release the subscription.
func (m *Manager) Subscribe(id string, after int64) (<-chan Chunk, func(), error) {
	m.mu.Lock()
	state := m.byID[id]
	if state == nil || state.kind != "pty" {
		m.mu.Unlock()
		return nil, nil, ErrNotFound
	}
	ch := make(chan Chunk, 64)
	for _, chunk := range state.chunks {
		if chunk.Seq > after {
			select {
			case ch <- chunk:
			default:
			}
		}
	}
	state.subs[ch] = struct{}{}
	running := state.info.Status == "running"
	m.mu.Unlock()
	if !running {
		close(ch)
	}
	cancel := func() {
		m.mu.Lock()
		if state := m.byID[id]; state != nil {
			if _, ok := state.subs[ch]; ok {
				delete(state.subs, ch)
				close(ch)
			}
		}
		m.mu.Unlock()
	}
	return ch, cancel, nil
}

// Update changes the title or viewport of an interactive session.
func (m *Manager) Update(id, title string, rows, cols int) (Info, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.byID[id]
	if state == nil || state.kind != "pty" {
		return Info{}, ErrNotFound
	}
	if strings.TrimSpace(title) != "" {
		state.info.Title = strings.TrimSpace(title)
	}
	if rows > 0 && cols > 0 {
		_ = rows
		_ = cols
	}
	return state.info, nil
}

// Remove terminates an interactive session and forgets it.
func (m *Manager) Remove(id string) error {
	m.mu.Lock()
	state := m.byID[id]
	if state == nil || state.kind != "pty" {
		m.mu.Unlock()
		return ErrNotFound
	}
	m.dropLocked(id, state)
	m.mu.Unlock()
	kill(state)
	return nil
}

// StartShell runs a one-shot command and returns immediately.
func (m *Manager) StartShell(command, cwd string, metadata map[string]any, timeout time.Duration) (ShellInfo, error) {
	if strings.TrimSpace(command) == "" {
		return ShellInfo{}, errors.New("shell: command is empty")
	}
	shellPath, args := m.shellCommand(command)
	dir, err := m.resolveCwd(cwd)
	if err != nil {
		return ShellInfo{}, err
	}
	m.mu.Lock()
	if len(m.byID) >= maxSessions {
		m.mu.Unlock()
		return ShellInfo{}, errors.New("shell: too many sessions")
	}
	m.mu.Unlock()

	id := newID("sh_")
	filePath := filepath.Join(m.logDir, id+".log")
	file, err := os.OpenFile(filePath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return ShellInfo{}, err
	}
	cmd := exec.Command(shellPath, args...)
	cmd.Dir = dir
	cmd.Env = mergedEnv(nil)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = file.Close()
		return ShellInfo{}, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = file.Close()
		return ShellInfo{}, err
	}
	if err := cmd.Start(); err != nil {
		_ = file.Close()
		return ShellInfo{}, err
	}
	now := time.Now()
	state := &sessionState{
		kind:    "shell",
		cmd:     cmd,
		file:    file,
		done:    make(chan struct{}),
		started: now,
		subs:    map[chan Chunk]struct{}{},
		shell: ShellInfo{
			ID:       id,
			Status:   "running",
			Command:  command,
			Cwd:      dir,
			Shell:    shellPath,
			File:     filePath,
			PID:      cmd.Process.Pid,
			Metadata: metadata,
			Time:     ShellTime{Started: now.UnixMilli()},
		},
	}
	if state.shell.Metadata == nil {
		state.shell.Metadata = map[string]any{}
	}
	info := state.shell
	m.add(state)
	state.copies.Add(2)
	go m.copyOutput(state, stdout)
	go m.copyOutput(state, stderr)
	if timeout > 0 {
		state.timer = time.AfterFunc(timeout, func() { m.expire(state) })
	}
	go m.wait(state)
	return info, nil
}

// ListShell returns running one-shot commands, newest first.
func (m *Manager) ListShell() []ShellInfo {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]ShellInfo, 0, len(m.order))
	for i := len(m.order) - 1; i >= 0; i-- {
		if state := m.byID[m.order[i]]; state != nil && state.kind == "shell" && state.shell.Status == "running" {
			out = append(out, state.shell)
		}
	}
	return out
}

// GetShell returns one one-shot command even after it exits.
func (m *Manager) GetShell(id string) (ShellInfo, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.byID[id]
	if state == nil || state.kind != "shell" {
		return ShellInfo{}, false
	}
	return state.shell, true
}

// ShellOutput pages through captured output by absolute byte cursor.
func (m *Manager) ShellOutput(id string, cursor, limit int64) (Output, error) {
	m.mu.Lock()
	state := m.byID[id]
	if state == nil || state.kind != "shell" {
		m.mu.Unlock()
		return Output{}, ErrNotFound
	}
	path := state.shell.File
	m.mu.Unlock()
	if cursor < 0 {
		cursor = 0
	}
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	file, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Output{Output: "", Cursor: cursor, Size: 0}, nil
		}
		return Output{}, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return Output{}, err
	}
	size := info.Size()
	if cursor > size {
		cursor = size
	}
	if _, err := file.Seek(cursor, io.SeekStart); err != nil {
		return Output{}, err
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return Output{}, err
	}
	return Output{Output: string(data), Cursor: cursor + int64(len(data)), Size: size}, nil
}

// RemoveShell terminates a one-shot command and removes its output file.
func (m *Manager) RemoveShell(id string) error {
	m.mu.Lock()
	state := m.byID[id]
	if state == nil || state.kind != "shell" {
		m.mu.Unlock()
		return ErrNotFound
	}
	path := state.shell.File
	m.dropLocked(id, state)
	state.shell.Status = "killed"
	m.mu.Unlock()
	kill(state)
	select {
	case <-state.done:
	case <-time.After(2 * time.Second):
	}
	_ = os.Remove(path)
	return nil
}

// CloseAll terminates every session. It runs on core shutdown.
func (m *Manager) CloseAll() {
	m.mu.Lock()
	states := make([]*sessionState, 0, len(m.byID))
	for _, state := range m.byID {
		states = append(states, state)
	}
	m.byID = map[string]*sessionState{}
	m.order = nil
	m.mu.Unlock()
	for _, state := range states {
		kill(state)
	}
	for _, state := range states {
		if state.done == nil {
			continue
		}
		select {
		case <-state.done:
		case <-time.After(2 * time.Second):
		}
	}
}

func (m *Manager) add(state *sessionState) {
	m.mu.Lock()
	id := state.info.ID
	if state.kind == "shell" {
		id = state.shell.ID
	}
	m.byID[id] = state
	m.order = append(m.order, id)
	m.mu.Unlock()
}

func (m *Manager) dropLocked(id string, state *sessionState) {
	delete(m.byID, id)
	if state.kind == "shell" {
		delete(m.byID, state.shell.ID)
	}
	for i, item := range m.order {
		if item == id {
			m.order = append(m.order[:i], m.order[i+1:]...)
			break
		}
	}
}

func (m *Manager) copyOutput(state *sessionState, reader io.Reader) {
	defer state.copies.Done()
	buf := make([]byte, 4096)
	for {
		n, err := reader.Read(buf)
		if n > 0 {
			m.appendOutput(state, buf[:n])
		}
		if err != nil {
			return
		}
	}
}

func (m *Manager) appendOutput(state *sessionState, data []byte) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if state.file != nil {
		_, _ = state.file.Write(data)
	}
	if state.persist != nil && state.persist.file != nil {
		_, _ = state.persist.file.Write(data)
		state.persist.info.Output.Tail += int64(len(data))
		state.persist.updated = time.Now()
	}
	text := string(data)
	if len(text) > maxChunk {
		text = text[len(text)-maxChunk:]
	}
	state.nextSeq++
	chunk := Chunk{Seq: state.nextSeq, Data: text}
	state.chunks = append(state.chunks, chunk)
	state.buffered += len(text)
	for state.buffered > maxBuffer && len(state.chunks) > 1 {
		state.buffered -= len(state.chunks[0].Data)
		state.chunks = state.chunks[1:]
	}
	for sub := range state.subs {
		select {
		case sub <- chunk:
		default:
		}
	}
}

func (m *Manager) wait(state *sessionState) {
	err := state.cmd.Wait()
	state.copies.Wait()
	code := 0
	signal := ""
	if err != nil {
		code = -1
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			code = exitErr.ExitCode()
		}
		signal = err.Error()
	}
	status := "exited"
	if code != 0 {
		status = "exited"
	}
	now := time.Now()
	m.mu.Lock()
	if state.timer != nil {
		state.timer.Stop()
	}
	if state.kind == "pty" {
		state.info.Status = "exited"
		state.info.ExitCode = &code
		if state.persist != nil {
			state.persist.info.Status = "exited"
			state.persist.info.ExitCode = &code
			state.persist.info.PID = 0
			if state.persist.file != nil {
				_ = state.persist.file.Close()
				state.persist.file = nil
			}
			if !state.persist.removed {
				m.savePersistentLocked(state.persist)
			}
		}
	} else {
		if state.shell.Status == "killed" || state.shell.Status == "timeout" {
			status = state.shell.Status
		}
		completed := now.UnixMilli()
		state.shell.Status = status
		state.shell.Exit = &code
		state.shell.Signal = signal
		state.shell.Time.Completed = &completed
	}
	if state.file != nil {
		_ = state.file.Close()
		state.file = nil
	}
	for sub := range state.subs {
		close(sub)
	}
	state.subs = nil
	m.mu.Unlock()
	if state.done != nil {
		close(state.done)
	}
}

func (m *Manager) expire(state *sessionState) {
	m.mu.Lock()
	if state.kind != "shell" || state.shell.Status != "running" {
		m.mu.Unlock()
		return
	}
	state.shell.Status = "timeout"
	m.mu.Unlock()
	kill(state)
}

func kill(state *sessionState) {
	killTree(state.cmd)
}

func (m *Manager) ptyCommand(options Options) (string, []string, error) {
	if len(options.Command) > 0 {
		command := strings.TrimSpace(options.Command[0])
		if command == "" {
			return "", nil, errors.New("pty: command is empty")
		}
		args := append(append([]string(nil), options.Command[1:]...), options.Args...)
		return command, args, nil
	}
	if len(m.shell) > 0 {
		return m.shell[0], append([]string(nil), m.shell[1:]...), nil
	}
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/Q"}, nil
	}
	return "/bin/sh", nil, nil
}

func (m *Manager) shellCommand(command string) (string, []string) {
	if len(m.shell) > 0 {
		args := append([]string(nil), m.shell[1:]...)
		return m.shell[0], append(args, command)
	}
	if runtime.GOOS == "windows" {
		return "cmd.exe", []string{"/C", command}
	}
	return "/bin/sh", []string{"-c", command}
}

func (m *Manager) resolveCwd(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return m.workdir, nil
	}
	if filepath.IsAbs(value) {
		return filepath.Clean(value), nil
	}
	return filepath.Join(m.workdir, value), nil
}

func mergedEnv(extra map[string]string) []string {
	env := os.Environ()
	for key, value := range extra {
		env = append(env, key+"="+value)
	}
	return env
}

func newID(prefix string) string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%s%d", prefix, time.Now().UnixNano())
	}
	return prefix + hex.EncodeToString(buf)
}
