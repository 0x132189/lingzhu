package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"lingzhu/internal/job"
	"lingzhu/internal/pty"
)

// SetPTY attaches the terminal/shell manager; nil disables the endpoints.
func (s *Server) SetPTY(manager *pty.Manager) {
	s.pty = manager
}

// withPTY answers 501 when terminal support is unavailable and hands the
// handler the manager otherwise, so no handler repeats the guard.
func (s *Server) withPTY(fn func(http.ResponseWriter, *http.Request, *Server, *pty.Manager)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.pty == nil {
			writeError(w, http.StatusNotImplemented, errText("terminal sessions are not available"))
			return
		}
		fn(w, r, s, s.pty)
	}
}

func listPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	items := manager.ListPTY()
	if items == nil {
		items = []pty.Info{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ptys": items})
}

func createPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	var body struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Cwd     string            `json:"cwd"`
		Title   string            `json:"title"`
		Env     map[string]string `json:"env"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	options := pty.Options{Cwd: body.Cwd, Title: body.Title, Args: body.Args, Env: body.Env}
	if strings.TrimSpace(body.Command) != "" {
		options.Command = []string{body.Command}
	}
	info, _, err := manager.StartPTY(options)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"pty": info})
}

func getPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	info, found := manager.GetPTY(r.PathValue("id"))
	if !found {
		writeError(w, http.StatusNotFound, errText("terminal session not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pty": info})
}

func updatePTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	var body struct {
		Title string `json:"title"`
		Size  *struct {
			Rows int `json:"rows"`
			Cols int `json:"cols"`
		} `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	rows, cols := 0, 0
	if body.Size != nil {
		rows, cols = body.Size.Rows, body.Size.Cols
	}
	info, err := manager.Update(r.PathValue("id"), body.Title, rows, cols)
	if err != nil {
		writeError(w, http.StatusNotFound, errText("terminal session not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"pty": info})
}

func deletePTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	if err := manager.Remove(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, errText("terminal session not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func ptyInput(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	var body struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := manager.Write(r.PathValue("id"), body.Data); err != nil {
		writeError(w, http.StatusNotFound, errText("terminal session not found or exited"))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

func ptyOutput(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	after, err := parseAfter(r.URL.Query().Get("after"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	chunks, next, status, err := manager.Output(r.PathValue("id"), after)
	if err != nil {
		writeError(w, http.StatusNotFound, errText("terminal session not found"))
		return
	}
	if chunks == nil {
		chunks = []pty.Chunk{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"chunks": chunks, "next": next, "status": status})
}

func ptyConnect(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	id := r.PathValue("id")
	after, err := parseAfter(r.URL.Query().Get("after"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	chunks, cancel, err := manager.Subscribe(id, after)
	if err != nil {
		writeError(w, http.StatusNotFound, errText("terminal session not found"))
		return
	}
	defer cancel()
	stream, err := newSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	info, _ := manager.GetPTY(id)
	_ = stream.write("status", info)
	stream.flush()
	ctx, done := r.Context(), make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			cancel()
		case <-done:
		}
	}()
	for chunk := range chunks {
		if !stream.write("output", chunk) {
			return
		}
		stream.flush()
	}
	info, _ = manager.GetPTY(id)
	_ = stream.write("exit", info)
	stream.flush()
}

func listShell(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	items := manager.ListShell()
	if items == nil {
		items = []pty.ShellInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"shells": items})
}

func createShell(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	var body struct {
		Command  string         `json:"command"`
		Cwd      string         `json:"cwd"`
		Timeout  int64          `json:"timeout"`
		Metadata map[string]any `json:"metadata"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var timeout time.Duration
	if body.Timeout > 0 {
		timeout = time.Duration(body.Timeout) * time.Millisecond
	}
	info, err := manager.StartShell(body.Command, body.Cwd, body.Metadata, timeout)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"shell": info})
}

func getShell(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	info, found := manager.GetShell(r.PathValue("id"))
	if !found {
		writeError(w, http.StatusNotFound, errText("shell command not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"shell": info})
}

func shellOutput(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	query := r.URL.Query()
	cursor, err := parseNonNegative(query.Get("cursor"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	limit, err := parseNonNegative(query.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	output, err := manager.ShellOutput(r.PathValue("id"), cursor, limit)
	if err != nil {
		if !errors.Is(err, pty.ErrNotFound) {
			writeError(w, http.StatusInternalServerError, err)
			return
		}
		// A background job is not a pty shell: the agent's shell tool registers
		// it with the job manager, and the transcript card asks this endpoint for
		// its output. Fall through to that store instead of reporting a missing
		// command.
		if s.jobOutputByID(w, r.PathValue("id"), cursor, limit) {
			return
		}
		writeError(w, http.StatusNotFound, errText("shell command not found"))
		return
	}
	writeJSON(w, http.StatusOK, output)
}

// jobOutputByID answers a background job's output on the shell endpoint. It
// reports false when no job has that id, so the caller can keep its own error.
func (s *Server) jobOutputByID(w http.ResponseWriter, id string, cursor, limit int64) bool {
	if s.jobs == nil {
		return false
	}
	item, ok := s.jobs.Get(id)
	if !ok {
		return false
	}
	if limit <= 0 {
		limit = 64 * 1024
	}
	file, err := os.Open(item.File)
	if err != nil {
		// The record survives a restart even when its output file was cleaned
		// up, so an empty page is the truthful answer rather than a 404.
		writeJSON(w, http.StatusOK, map[string]any{
			"output":  "",
			"cursor":  cursor,
			"size":    0,
			"running": false,
		})
		return true
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return false
	}
	size := info.Size()
	if cursor < 0 {
		cursor = 0
	}
	if cursor > size {
		cursor = size
	}
	if _, err := file.Seek(cursor, io.SeekStart); err != nil {
		return false
	}
	data, err := io.ReadAll(io.LimitReader(file, limit))
	if err != nil {
		return false
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"output":  string(data),
		"cursor":  cursor + int64(len(data)),
		"size":    size,
		"running": item.Status == job.Running,
	})
	return true
}

func deleteShell(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	if err := manager.RemoveShell(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, errText("shell command not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func parseAfter(value string) (int64, error) {
	return parseNonNegative(value)
}

func parseNonNegative(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, nil
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed < 0 {
		return 0, errText("cursor must be a non-negative integer")
	}
	return parsed, nil
}
