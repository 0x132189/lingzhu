package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const maxOutput = 64 * 1024

// Builtins 返回内置工具：bash、read_file、write_file。
func Builtins(shell []string, workdir string) []Tool {
	return []Tool{
		&Bash{Shell: shell, Dir: workdir},
		&ReadFile{Dir: workdir},
		&WriteFile{Dir: workdir},
	}
}

type Bash struct {
	Shell []string
	Dir   string
}

func (b *Bash) Name() string { return "bash" }

func (b *Bash) Description() string {
	return "运行一条 shell 命令，返回合并后的 stdout 与 stderr。"
}

func (b *Bash) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "要执行的命令"},
			"timeout_seconds": {"type": "integer", "description": "超时秒数，默认 60"}
		},
		"required": ["command"]
	}`)
}

func (b *Bash) Run(ctx context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Command        string `json:"command"`
		TimeoutSeconds int    `json:"timeout_seconds"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("bash: %w", err)
	}
	if strings.TrimSpace(args.Command) == "" {
		return "", fmt.Errorf("bash: command is empty")
	}
	timeout := 60 * time.Second
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, b.Shell[0], append(b.Shell[1:], args.Command)...)
	cmd.Dir = b.Dir
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	text := truncate(out.String())
	if ctx.Err() == context.DeadlineExceeded {
		return text + "\n(timeout)", fmt.Errorf("bash: timeout after %s", timeout)
	}
	if err != nil {
		return text, fmt.Errorf("bash: %w", err)
	}
	return text, nil
}

type ReadFile struct {
	Dir string
}

func (r *ReadFile) Name() string { return "read_file" }

func (r *ReadFile) Description() string {
	return "读取文本文件，按行返回，可指定 offset 与 limit。"
}

func (r *ReadFile) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string"},
			"offset": {"type": "integer", "description": "起始行号，从 1 开始"},
			"limit": {"type": "integer", "description": "最多返回行数，默认 2000"}
		},
		"required": ["path"]
	}`)
}

func (r *ReadFile) Run(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	path, err := r.resolve(args.Path)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read_file: %w", err)
	}
	lines := strings.Split(string(data), "\n")
	start := max(args.Offset-1, 0)
	if start >= len(lines) {
		return "", nil
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 2000
	}
	end := min(start+limit, len(lines))

	var sb strings.Builder
	for i := start; i < end; i++ {
		fmt.Fprintf(&sb, "%d\t%s\n", i+1, strings.TrimRight(lines[i], "\r"))
	}
	return truncate(sb.String()), nil
}

type WriteFile struct {
	Dir string
}

func (w *WriteFile) Name() string { return "write_file" }

func (w *WriteFile) Description() string {
	return "写入文本文件，父目录不存在时自动创建。"
}

func (w *WriteFile) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string"},
			"content": {"type": "string"}
		},
		"required": ["path", "content"]
	}`)
}

func (w *WriteFile) Run(_ context.Context, input json.RawMessage) (string, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	path, err := w.resolve(args.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	if err := os.WriteFile(path, []byte(args.Content), 0o644); err != nil {
		return "", fmt.Errorf("write_file: %w", err)
	}
	return fmt.Sprintf("wrote %d bytes to %s", len(args.Content), path), nil
}

func (r *ReadFile) resolve(p string) (string, error) { return resolvePath(r.Dir, p) }

func (w *WriteFile) resolve(p string) (string, error) { return resolvePath(w.Dir, p) }

func resolvePath(dir, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path is empty")
	}
	if filepath.IsAbs(p) {
		return p, nil
	}
	return filepath.Join(dir, p), nil
}

func truncate(s string) string {
	if len(s) <= maxOutput {
		return s
	}
	return s[:maxOutput] + "\n(output truncated)"
}
