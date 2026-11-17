package tool

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/global"
	"lingzhu/internal/job"
	"lingzhu/internal/media"
	"lingzhu/internal/model"
	"lingzhu/internal/proc"
)

const (
	maxOutput = 50 * 1024
	// maxOutputLines matches the reference tool output limit.
	maxOutputLines = 2000
	// defaultShellTimeout matches the reference implementation's two minutes.
	defaultShellTimeout = 120 * time.Second
	// maxReadLines caps one read call the same way the reference tool does.
	maxReadLines = 2000
	// maxAttachmentBytes bounds one inline image or PDF.
	maxAttachmentBytes = 20 * 1024 * 1024
	// maxStreamBytes caps the output streamed to the UI for one shell call.
	maxStreamBytes = 64 * 1024
	// streamInterval throttles live output events so the event store stays small.
	streamInterval = 100 * time.Millisecond
)

// shellStream captures combined stdout/stderr while publishing throttled
// deltas to the session event stream.
type shellStream struct {
	mu      sync.Mutex
	all     bytes.Buffer
	pending bytes.Buffer
	emit    func(string)
	sink    io.Writer
	last    time.Time
	sent    int
}

func (s *shellStream) Write(p []byte) (int, error) {
	chunk := s.append(p)
	if chunk != "" && s.emit != nil {
		s.emit(chunk)
	}
	return len(p), nil
}

func (s *shellStream) append(p []byte) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.all.Write(p)
	if s.sink != nil {
		_, _ = s.sink.Write(p)
	}
	if s.emit == nil || s.sent >= maxStreamBytes {
		return ""
	}
	s.pending.Write(p)
	if time.Since(s.last) < streamInterval {
		return ""
	}
	return s.takeLocked()
}

func (s *shellStream) takeLocked() string {
	remaining := maxStreamBytes - s.sent
	if remaining <= 0 || s.pending.Len() == 0 {
		s.pending.Reset()
		return ""
	}
	data := s.pending.Bytes()
	if len(data) > remaining {
		data = data[:remaining]
	}
	chunk := string(data)
	s.pending.Next(len(data))
	if s.pending.Len() == 0 {
		s.pending.Reset()
	}
	s.sent += len(chunk)
	s.last = time.Now()
	return chunk
}

func (s *shellStream) finish() {
	s.mu.Lock()
	chunk := s.takeLocked()
	s.mu.Unlock()
	if chunk != "" && s.emit != nil {
		s.emit(chunk)
	}
}

func (s *shellStream) snapshot() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.all.String()
}

// adopt copies the output captured so far into w and sends every later chunk
// there too. The lock keeps a chunk from being duplicated or lost during the
// handoff to a background job.
func (s *shellStream) adopt(w io.Writer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := w.Write(s.all.Bytes()); err != nil {
		return err
	}
	s.sink = w
	return nil
}

// Builtins returns the built-in tools: shell, read, write, edit, glob, grep,
// patch and webfetch. All workspace access is rooted at workdir.
func Builtins(shell []string, workdir string, jobs *job.Manager, imageConfig ...media.Config) []Tool {
	images := media.Default()
	if len(imageConfig) > 0 {
		images = media.Normalize(imageConfig[0])
	}
	return []Tool{
		Direct(&Shell{Command: shell, Dir: workdir, Jobs: jobs}),
		Direct(&Read{Dir: workdir, Media: images}),
		Direct(&Write{Dir: workdir}),
		Direct(&Edit{Dir: workdir}),
		Direct(&Glob{Dir: workdir}),
		Direct(&Grep{Dir: workdir}),
		Direct(&Patch{Dir: workdir}),
		Direct(&WebFetch{}),
	}
}

// Shell runs commands with the configured shell prefix.
type Shell struct {
	Command []string
	Dir     string
	Jobs    *job.Manager
}

func (s *Shell) Name() string { return "shell" }

func (s *Shell) Description() string {
	description := "Execute a shell command and return its output. " +
		"Quote file paths containing spaces or special characters. " +
		"Prefer dedicated tools over shell commands when possible. " +
		"When output is large, the full result is saved to a file and a truncated preview is returned. " +
		"Rely on automatic truncation unless filtering the output is more useful. " +
		"Commands accept an optional timeout, background commands have no timeout by default. " +
		"Background commands return immediately, and you will be notified when they complete."
	if len(s.Command) > 0 {
		description = "Execute a shell command and return its output. " +
			"Commands run on " + shellPlatform() + " using " + filepath.Base(s.Command[0]) + ". " +
			"Quote file paths containing spaces or special characters. " +
			"Prefer dedicated tools over shell commands when possible. " +
			"When output is large, the full result is saved to a file and a truncated preview is returned. " +
			"Rely on automatic truncation unless filtering the output is more useful. " +
			"Commands accept an optional timeout, background commands have no timeout by default. " +
			"Background commands return immediately, and you will be notified when they complete."
	}
	return description
}

// shellPlatform names the current operating system for the shell description.
func shellPlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "Windows"
	case "darwin":
		return "macOS"
	case "linux":
		return "Linux"
	}
	return runtime.GOOS
}

func (s *Shell) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"command": {"type": "string", "description": "Shell command string to execute"},
			"workdir": {"type": "string", "description": "Working directory to execute the command in. Defaults to the current working directory. When possible, avoid changing directories in the command and set the working directory here instead."},
			"timeout": {"type": "integer", "description": "Timeout in milliseconds. Set to 0 to disable the timeout. Defaults to 120000 for foreground commands; background commands have no timeout by default."},
			"background": {"type": "boolean", "description": "Run the command in the background and return immediately (useful for dev servers and long-running builds). You do not need to use '&' at the end of the command when using this parameter. You will be notified when it completes. DO NOT poll for completion."}
		},
		"required": ["command"]
	}`)
}

func (s *Shell) Run(ctx context.Context, call Call) (string, error) {
	var args struct {
		Command    string `json:"command"`
		Workdir    string `json:"workdir"`
		Timeout    *int   `json:"timeout"`
		Background bool   `json:"background"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("shell: %w", err)
	}
	command := strings.TrimSpace(args.Command)
	if command == "" {
		return "", fmt.Errorf("shell: command is empty")
	}
	if err := call.Allow(ctx, "shell", []string{command}, ""); err != nil {
		return "", err
	}
	call.SetMetadata("command", command)

	dir, err := s.resolveWorkdir(ctx, call, args.Workdir)
	if err != nil {
		return "", err
	}
	if args.Background {
		return s.runBackground(call, dir, command, args.Timeout)
	}

	timeout := defaultShellTimeout
	if args.Timeout != nil {
		if *args.Timeout < 0 {
			return "", fmt.Errorf("shell: timeout must not be negative")
		}
		timeout = time.Duration(*args.Timeout) * time.Millisecond
	}
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}

	// The command runs in its own process group and is stopped through
	// proc.Stop, because exec.CommandContext kills only the direct child and
	// would leave everything it spawned behind.
	cmd := exec.Command(s.Command[0], append(s.Command[1:], command)...)
	cmd.Dir = dir
	proc.Configure(cmd)
	stream := &shellStream{}
	if call.Emit != nil && call.ID != "" {
		stream.emit = func(chunk string) {
			_ = call.Emit(event.ToolOutputEvent(call.ID, chunk))
		}
	}
	cmd.Stdout = stream
	cmd.Stderr = stream
	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("shell: %w", err)
	}
	proc.Track(cmd)
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	var runErr error
	finished := false
	select {
	case runErr = <-waitCh:
		finished = true
	case <-ctx.Done():
		// A foreground timeout hands the still-running command to the job
		// manager instead of discarding its work. A user interrupt still
		// stops the tree and settles the call as interrupted.
		if errors.Is(ctx.Err(), context.DeadlineExceeded) && s.Jobs != nil {
			select {
			case runErr = <-waitCh:
				finished = true
			default:
				item, err := s.Jobs.Adopt(call.SessionID, command, cmd, waitCh, stream.adopt)
				if err == nil {
					stream.finish()
					preview, file := boundedTail(stream.snapshot())
					if file != "" {
						call.SetMetadata("truncated", true)
						call.SetMetadata("outputFile", file)
					}
					call.SetMetadata("timeout", true)
					call.SetMetadata("shellID", item.ID)
					call.SetMetadata("background", true)
					notice := fmt.Sprintf("Command exceeded timeout of %d ms and moved to the background (job ID: %s).\nOutput is streaming to: %s\nYou will be notified automatically when it completes. Do not poll for completion; continue with unrelated work or end your response.", timeout.Milliseconds(), item.ID, item.File)
					return withNotice(preview, notice), nil
				}
			}
		}
		if !finished {
			proc.Stop(cmd, proc.Grace)
			runErr = <-waitCh
		}
	}
	// The adopted-timeout path returned above; this call owns the job handle.
	proc.Release(cmd)
	stream.finish()
	preview, file := boundedTail(stream.snapshot())
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		call.SetMetadata("timeout", true)
		message := fmt.Sprintf("Command exceeded timeout of %d ms. Retry with a larger timeout if the command is expected to take longer.", timeout.Milliseconds())
		return withNotice(preview, message), fmt.Errorf("shell: timeout after %s", timeout)
	}
	// A stopped command exits with a non-zero status, which looks like an
	// ordinary command failure. The interrupted turn must win over that.
	if errors.Is(ctx.Err(), context.Canceled) {
		return preview, fmt.Errorf("shell: interrupted")
	}
	if file != "" {
		call.SetMetadata("truncated", true)
		call.SetMetadata("outputFile", file)
		preview += "\n\n(full output: " + file + ")"
	}
	// A detached descendant can keep the output pipe open after the command
	// itself has exited. WaitDelay bounds that drain, and ErrWaitDelay only
	// means the last bytes may be missing; the command result is still valid.
	if errors.Is(runErr, exec.ErrWaitDelay) {
		code := 0
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		call.SetMetadata("exit", code)
		if code != 0 {
			preview = withExitCode(preview, code)
		}
		return preview, nil
	}
	if runErr != nil {
		var exit *exec.ExitError
		if errors.As(runErr, &exit) {
			code := exit.ExitCode()
			call.SetMetadata("exit", code)
			return withExitCode(preview, code), nil
		}
		call.SetMetadata("exit", -1)
		return preview, fmt.Errorf("shell: %w", runErr)
	}
	call.SetMetadata("exit", 0)
	return preview, nil
}

// withExitCode appends the model-facing exit notice to a command's output.
func withExitCode(output string, code int) string {
	return withNotice(output, fmt.Sprintf("Exit code: %d", code))
}

// withNotice appends a model-facing note to command output.
func withNotice(output, notice string) string {
	if strings.TrimSpace(output) == "" {
		return notice
	}
	return strings.TrimRight(output, "\n") + "\n\n" + notice
}

// runBackground starts a command that outlives the current turn. Completion is
// delivered through the job manager, not by blocking here.
func (s *Shell) runBackground(call Call, dir, command string, timeout *int) (string, error) {
	if s.Jobs == nil {
		return "", fmt.Errorf("shell: background jobs are not available")
	}
	limit := time.Duration(0)
	if timeout != nil && *timeout > 0 {
		limit = time.Duration(*timeout) * time.Millisecond
	}
	item, err := s.Jobs.Start(call.SessionID, s.Command, dir, command, limit)
	if err != nil {
		return "", fmt.Errorf("shell: %w", err)
	}
	call.SetMetadata("shellID", item.ID)
	call.SetMetadata("background", true)
	return fmt.Sprintf("Command moved to the background (job ID: %s).\nOutput is streaming to: %s\nYou will be notified automatically when it completes. Do not poll for completion; continue with unrelated work or end your response.", item.ID, item.File), nil
}

// resolveWorkdir turns the requested directory into an absolute path and applies
// the external-directory boundary before the command runs.
func (s *Shell) resolveWorkdir(ctx context.Context, call Call, dir string) (string, error) {
	if strings.TrimSpace(dir) == "" {
		return s.Dir, nil
	}
	abs, err := resolvePath(s.Dir, dir)
	if err != nil {
		return "", fmt.Errorf("shell: %w", err)
	}
	if external(s.Dir, abs) {
		if err := call.Allow(ctx, "external_directory", []string{abs}, ""); err != nil {
			return "", err
		}
	}
	info, err := os.Stat(abs)
	if err != nil {
		return "", fmt.Errorf("shell: %w", err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("shell: workdir is not a directory: %s", abs)
	}
	return abs, nil
}

// boundedTail returns at most maxOutput bytes from the end of the output and,
// when it had to truncate, the path of a file holding the complete output.
func boundedTail(text string) (string, string) {
	if len(text) <= maxOutput {
		return text, ""
	}
	file, err := os.CreateTemp("", "lingzhu-output-*.log")
	if err != nil {
		return "[output truncated]\n" + strings.ToValidUTF8(text[len(text)-maxOutput:], ""), ""
	}
	if _, err := file.WriteString(text); err != nil {
		file.Close()
		return "[output truncated]\n" + strings.ToValidUTF8(text[len(text)-maxOutput:], ""), ""
	}
	file.Close()
	tail := strings.ToValidUTF8(text[len(text)-maxOutput:], "")
	return "[output truncated; showing the tail]\n" + tail, file.Name()
}

// Read returns a text file page or a directory listing.
type Read struct {
	Dir   string
	Media media.Config

	mu     sync.Mutex
	loaded map[string]map[string]struct{}
}

func (r *Read) Name() string { return "read" }

func (r *Read) Description() string {
	return "Read the contents of a file or directory. Supports text files, images, and PDFs. Images and PDFs are presented directly to the model. Each text line is prefixed by its 1-based line number as <line>: <content>. The prefix is for reference and is not part of the file content. Directory entries are returned one per line. Use offset and limit to read large files or directories in sections. Prefer one larger read over many small slices, and use grep to find specific content in large files."
}

func (r *Read) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "File or directory to read"},
			"offset": {"type": "integer", "description": "The line or directory entry to start reading from (1-based)"},
			"limit": {"type": "integer", "description": "The maximum number of lines or directory entries to read (defaults to and capped at 2000)"}
		},
		"required": ["path"]
	}`)
}

func (r *Read) Run(ctx context.Context, call Call) (string, error) {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	path, err := call.Resolve(ctx, "read", r.Dir, args.Path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if info.IsDir() {
		call.SetMetadata("count", 0)
		output, err := readDir(path, args.Offset, args.Limit)
		if err != nil {
			return "", err
		}
		r.injectInstructions(call, path, "")
		return output, nil
	}
	call.SetMetadata("path", path)
	if strings.EqualFold(filepath.Base(path), "AGENTS.md") {
		call.SetMetadata("loaded", []string{path})
	}
	output, err := readFile(call, path, args.Offset, args.Limit, r.Media)
	if err != nil {
		return "", err
	}
	r.injectInstructions(call, filepath.Dir(path), path)
	return output, nil
}

// injectInstructions discovers AGENTS.md files between a read target and the
// workspace root, excluding the root (already in the system prompt) and the file
// just read. Each path is injected once per session as a synthetic instruction
// message, matching the reference's nearby-instruction behavior.
func (r *Read) injectInstructions(call Call, start, skip string) {
	if call.OnInstruction == nil || call.SessionID == "" {
		return
	}
	root, err := filepath.Abs(r.Dir)
	if err != nil {
		return
	}
	dir, err := filepath.Abs(start)
	if err != nil || !withinDir(root, dir) {
		return
	}
	var candidates []string
	for {
		if samePath(dir, root) {
			break
		}
		candidate := filepath.Join(dir, "AGENTS.md")
		if !samePath(candidate, skip) && isFile(candidate) {
			candidates = append(candidates, candidate)
		}
		parent := filepath.Dir(dir)
		if parent == dir || !withinDir(root, parent) {
			break
		}
		dir = parent
	}
	// Walk collected nearest-first; outer files must enter the context first.
	for index := len(candidates) - 1; index >= 0; index-- {
		path := candidates[index]
		if !r.claimInstruction(call.SessionID, path) {
			continue
		}
		data, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		text := strings.TrimSpace(string(data))
		if text == "" {
			continue
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			relative = path
		}
		call.OnInstruction(Instruction{
			Text:        "Instructions from: " + path + "\n" + text,
			Description: "Loaded " + relative,
			Paths:       []string{relative},
		})
	}
}

func (r *Read) claimInstruction(sessionID, path string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.loaded == nil {
		r.loaded = map[string]map[string]struct{}{}
	}
	set := r.loaded[sessionID]
	if set == nil {
		set = map[string]struct{}{}
		r.loaded[sessionID] = set
	}
	if _, exists := set[path]; exists {
		return false
	}
	set[path] = struct{}{}
	return true
}

func withinDir(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

func samePath(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func readDir(path string, offset, limit int) (string, error) {
	entries, err := os.ReadDir(path)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	start := max(offset-1, 0)
	if start > len(entries) {
		return "", fmt.Errorf("read: offset %d is beyond the %d directory entries", offset, len(entries))
	}
	limit = clampLimit(limit, maxReadLines)
	end := min(start+limit, len(entries))

	var b strings.Builder
	if len(entries) == 0 {
		fmt.Fprintf(&b, "Read directory %s, 0 entries\n", path)
	} else {
		fmt.Fprintf(&b, "Read directory %s, entries %d-%d\n", path, start+1, end)
	}
	for _, entry := range entries[start:end] {
		name := entry.Name()
		if entry.IsDir() {
			name += string(filepath.Separator)
		}
		b.WriteString(name + "\n")
	}
	if end < len(entries) {
		fmt.Fprintf(&b, "[Output truncated. Continue reading with offset: %d]\n", end+1)
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

func readFile(call Call, path string, offset, limit int, imageConfig media.Config) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read: %w", err)
	}
	if mime := mediaType(data, path); mime != "" {
		if !call.CanAttach {
			return "", fmt.Errorf("read: the selected model does not accept %s attachments", mime)
		}
		if len(data) > maxAttachmentBytes {
			return "", fmt.Errorf("read: %s is %d bytes, over the %d byte attachment limit", path, len(data), maxAttachmentBytes)
		}
		if strings.HasPrefix(mime, "image/") {
			processed, nextMime, err := media.Process(data, mime, imageConfig)
			if err != nil {
				return "", fmt.Errorf("read: %w", err)
			}
			data, mime = processed, nextMime
		}
		call.SetAttachment(model.Attachment{
			Mime: mime,
			Name: filepath.Base(path),
			Data: base64.StdEncoding.EncodeToString(data),
		})
		kind := "image"
		if mime == "application/pdf" {
			kind = "PDF"
		}
		return fmt.Sprintf("Read %s file %s (%s, %d bytes). The %s is attached to this tool result.", kind, path, mime, len(data), kind), nil
	}
	head := data
	if len(head) > 8192 {
		head = head[:8192]
	}
	if bytes.IndexByte(head, 0) >= 0 {
		return "", fmt.Errorf("read: cannot read binary file: %s", path)
	}
	text := strings.TrimSuffix(string(data), "\n")
	if text == "" {
		return fmt.Sprintf("Read file %s, 0 lines", path), nil
	}
	lines := strings.Split(text, "\n")
	start := max(offset-1, 0)
	if start > len(lines) {
		return "", fmt.Errorf("read: offset %d is beyond the %d lines in %s", offset, len(lines), path)
	}
	limit = clampLimit(limit, maxReadLines)
	end := min(start+limit, len(lines))

	var b strings.Builder
	fmt.Fprintf(&b, "Read file %s, lines %d-%d\n", path, start+1, end)
	for i := start; i < end; i++ {
		fmt.Fprintf(&b, "%d: %s\n", i+1, strings.TrimRight(lines[i], "\r"))
	}
	if end < len(lines) {
		fmt.Fprintf(&b, "[Output truncated. Continue reading with offset: %d]\n", end+1)
	}
	return truncate(strings.TrimRight(b.String(), "\n")), nil
}

// mediaType reports the inline media type of a file, or empty for text and
// other binary files.
func mediaType(data []byte, path string) string {
	if bytes.HasPrefix(data, []byte("%PDF-")) {
		return "application/pdf"
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png":
		return "image/png"
	case ".jpg", ".jpeg":
		return "image/jpeg"
	case ".gif":
		return "image/gif"
	case ".webp":
		return "image/webp"
	case ".pdf":
		return "application/pdf"
	}
	if len(data) > 0 {
		switch http.DetectContentType(data) {
		case "image/png", "image/jpeg", "image/gif", "image/webp":
			return http.DetectContentType(data)
		}
	}
	return ""
}

func clampLimit(limit, fallback int) int {
	if limit <= 0 {
		return fallback
	}
	if limit > fallback {
		return fallback
	}
	return limit
}

// Write overwrites or creates a text file.
type Write struct {
	Dir string
}

func (w *Write) Name() string { return "write" }

func (w *Write) Description() string {
	return "Writes a file to the local filesystem, overwriting if one exists.\n\nMissing parent directories are created automatically.\n\nUse this tool to create new files or overwrite existing files. For partial changes, use the edit tool instead."
}

func (w *Write) Schema() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"path": {"type": "string", "description": "Path to the file to write to"},
			"content": {"type": "string", "description": "Content to write to the file"}
		},
		"required": ["path", "content"]
	}`)
}

func (w *Write) Run(ctx context.Context, call Call) (string, error) {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(call.Input, &args); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	path, err := call.Resolve(ctx, "edit", w.Dir, args.Path)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	old, readErr := os.ReadFile(path)
	existed := readErr == nil
	if err := os.WriteFile(path, []byte(args.Content), 0o644); err != nil {
		return "", fmt.Errorf("write: %w", err)
	}
	formatter, formatErr := call.FormatFile(ctx, path)
	final := args.Content
	if formatter != "" {
		if data, err := os.ReadFile(path); err == nil {
			final = string(data)
		}
	}
	call.SetDiff(lineDiff(w.Dir, path, string(old), final))
	call.SetMetadata("diagnostics", diagnose(path))
	status := "modified"
	if !existed {
		status = "added"
	}
	call.SetMetadata("files", []map[string]any{fileChange(path, lineDiff(w.Dir, path, string(old), final), status)})
	action := "Created"
	if existed {
		action = "Wrote"
	}
	message := fmt.Sprintf("%s file successfully: %s", action, relPath(w.Dir, path))
	if formatter != "" {
		message += " (formatted with " + formatter + ")"
	}
	if formatErr != nil {
		message += "; formatter failed: " + formatErr.Error()
	}
	return message, nil
}

func resolvePath(dir, p string) (string, error) {
	if strings.TrimSpace(p) == "" {
		return "", fmt.Errorf("path is empty")
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p), nil
	}
	return filepath.Join(dir, p), nil
}

// external reports whether path escapes the working directory.
func external(dir, path string) bool {
	base, err := filepath.Abs(dir)
	if err != nil {
		return true
	}
	rel, err := filepath.Rel(base, path)
	if err != nil {
		return true
	}
	return rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// relPath renders a path relative to the workspace when it stays inside.
func relPath(dir, path string) string {
	rel, err := filepath.Rel(dir, path)
	if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return filepath.ToSlash(rel)
	}
	return filepath.ToSlash(path)
}

// Limit bounds any tool result the same way the built-in tools bound their own
// output. It is applied by the agent to every tool, so a result from an MCP
// server, a plugin or a subagent cannot fill the context window.
func Limit(s string) string {
	return truncate(s)
}

func truncate(s string) string {
	// A tool that already bounded its own output keeps its own marker, so the
	// limit never stacks a second truncation notice on one result.
	if strings.HasSuffix(s, "(output truncated)") || strings.Contains(s, "; full output saved to ") {
		return s
	}
	totalLines := strings.Count(s, "\n") + 1
	if len(s) <= maxOutput && totalLines <= maxOutputLines {
		return s
	}
	cut := s
	if len(cut) > maxOutput {
		cut = strings.ToValidUTF8(cut[:maxOutput], "")
	}
	lines := strings.SplitAfter(cut, "\n")
	shown := len(lines)
	if shown > maxOutputLines {
		shown = maxOutputLines
	}
	cut = strings.Join(lines[:shown], "")
	marker := "\n(output truncated)"
	if path := saveToolOutput(s); path != "" {
		marker = fmt.Sprintf("\n[showing lines 1-%d of %d; full output saved to %s]", shown, totalLines, path)
	}
	return cut + marker
}

var toolOutputCleanup sync.Once

// saveToolOutput writes the complete result under the user data directory and
// returns its path. The first cleanup removes files older than seven days.
func saveToolOutput(s string) string {
	dir := filepath.Join(global.Data(), "tool-output")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ""
	}
	file, err := os.CreateTemp(dir, "tool_*")
	if err != nil {
		return ""
	}
	path := file.Name()
	if _, err := file.WriteString(s); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return ""
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return ""
	}
	toolOutputCleanup.Do(func() { cleanupToolOutput(dir) })
	return path
}

func cleanupToolOutput(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		info, err := entry.Info()
		if err != nil || time.Since(info.ModTime()) < 7*24*time.Hour {
			continue
		}
		_ = os.Remove(filepath.Join(dir, entry.Name()))
	}
}
