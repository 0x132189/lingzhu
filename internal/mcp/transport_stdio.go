package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"lingzhu/internal/rpc"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
)

// stdioTransport runs a local MCP server and speaks newline-delimited JSON-RPC
// over its stdin and stdout.
type stdioTransport struct {
	cmd      *exec.Cmd
	stdin    io.WriteCloser
	writeMu  sync.Mutex
	stderr   *tailBuffer
	waitOnce sync.Once
}

func startStdio(name string, cfg Server, workdir string, onMessage func(rpc.Message), onClose func()) (*stdioTransport, error) {
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("mcp: server %q has no command", name)
	}

	cwd := workdir
	if cfg.Cwd != "" {
		if filepath.IsAbs(cfg.Cwd) {
			cwd = cfg.Cwd
		} else {
			cwd = filepath.Join(workdir, cfg.Cwd)
		}
	}
	cmd := exec.Command(cfg.Command[0], cfg.Command[1:]...)
	cmd.Dir = cwd
	env := os.Environ()
	for key, value := range cfg.Environment {
		env = append(env, key+"="+value)
	}
	cmd.Env = env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("mcp: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp: %w", err)
	}
	stderr := &tailBuffer{limit: 4096}
	cmd.Stderr = stderr

	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return nil, fmt.Errorf("mcp: start %q: %w", name, err)
	}

	transport := &stdioTransport{cmd: cmd, stdin: stdin, stderr: stderr}
	go transport.readLoop(stdout, onMessage, onClose)
	return transport, nil
}

func (t *stdioTransport) Send(ctx context.Context, message []byte, expectID int64) error {
	data := make([]byte, 0, len(message)+1)
	data = append(data, message...)
	data = append(data, '\n')
	t.writeMu.Lock()
	defer t.writeMu.Unlock()
	if _, err := t.stdin.Write(data); err != nil {
		return fmt.Errorf("mcp: write: %w", err)
	}
	return nil
}

func (t *stdioTransport) readLoop(stdout io.Reader, onMessage func(rpc.Message), onClose func()) {
	defer onClose()
	defer t.wait()
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 32*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var message rpc.Message
		if err := json.Unmarshal([]byte(line), &message); err != nil {
			continue
		}
		onMessage(message)
	}
}

func (t *stdioTransport) wait() {
	t.waitOnce.Do(func() { _ = t.cmd.Wait() })
}

// Close stops the server process.
func (t *stdioTransport) Close() error {
	_ = t.stdin.Close()
	killProcess(t.cmd)
	go t.wait()
	return nil
}

// killProcess ends the server and its children. On Windows taskkill is used so
// wrappers such as npx do not leave node behind; elsewhere the direct process
// is enough for the stdio servers we start.
func killProcess(cmd *exec.Cmd) {
	if cmd.Process == nil {
		return
	}
	if runtime.GOOS == "windows" {
		if err := exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run(); err == nil {
			return
		}
	}
	_ = cmd.Process.Kill()
}

// tailBuffer keeps the last bytes written to stderr for diagnostics.
type tailBuffer struct {
	mu    sync.Mutex
	limit int
	data  []byte
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.data = append(b.data, p...)
	if b.limit > 0 && len(b.data) > b.limit {
		b.data = b.data[len(b.data)-b.limit:]
	}
	return len(p), nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return string(b.data)
}
