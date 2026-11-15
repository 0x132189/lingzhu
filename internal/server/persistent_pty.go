package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"lingzhu/internal/pty"
)

func listSessionTerminals(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	id := r.PathValue("id")
	if _, err := s.store.Info(id); err != nil {
		writeStoreError(w, err)
		return
	}
	items := manager.ListPersistent(id)
	if items == nil {
		items = []pty.PersistentInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": items})
}

func createSessionTerminal(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	id := r.PathValue("id")
	if _, err := s.store.Info(id); err != nil {
		writeStoreError(w, err)
		return
	}
	var body struct {
		Command string            `json:"command"`
		Args    []string          `json:"args"`
		Cwd     string            `json:"cwd"`
		Title   string            `json:"title"`
		Env     map[string]string `json:"env"`
		Size    *pty.Size         `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	options := pty.Options{Cwd: body.Cwd, Title: body.Title, Args: body.Args, Env: body.Env}
	if strings.TrimSpace(body.Command) != "" {
		options.Command = []string{body.Command}
	}
	size := pty.Size{Cols: 80, Rows: 24}
	if body.Size != nil {
		size = *body.Size
	}
	info, err := manager.StartPersistent(id, options, size)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": info})
}

func readSessionTerminal(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	id := r.PathValue("id")
	if _, err := s.store.Info(id); err != nil {
		writeStoreError(w, err)
		return
	}
	lines := parseLines(r.URL.Query().Get("lines"))
	read, err := manager.ReadPersistent(id, lines)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": read})
}

func shutdownPersistentPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	manager.ShutdownPersistent()
	w.WriteHeader(http.StatusNoContent)
}

func handoffPersistentPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	handoff, err := manager.ExportPersistent()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"handoff": handoff})
}

// importPersistentPTY restores exported records from another core process as
// exited terminals. It accepts either {"handoff": ...} or a bare handoff body.
func importPersistentPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	var body struct {
		Handoff   *pty.Handoff           `json:"handoff"`
		Version   int                    `json:"version"`
		Terminals []pty.PersistentExport `json:"terminals"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	handoff := body.Handoff
	if handoff == nil {
		if len(body.Terminals) == 0 {
			writeError(w, http.StatusBadRequest, errText("handoff is empty"))
			return
		}
		handoff = &pty.Handoff{Version: body.Version, Terminals: body.Terminals}
	}
	imported, err := manager.ImportPersistent(*handoff)
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"data": imported})
}

func getPersistentPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	info, found := manager.GetPersistent(r.PathValue("id"))
	if !found {
		writeError(w, http.StatusNotFound, errText("terminal not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": info})
}

func updatePersistentPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	var body struct {
		Title json.RawMessage `json:"title"`
		Size  *pty.Size       `json:"size"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	var title *string
	clearTitle := false
	if len(body.Title) > 0 {
		if string(body.Title) == "null" {
			clearTitle = true
		} else {
			var value string
			if err := json.Unmarshal(body.Title, &value); err != nil {
				writeError(w, http.StatusBadRequest, err)
				return
			}
			title = &value
		}
	}
	info, err := manager.UpdatePersistent(r.PathValue("id"), title, clearTitle, body.Size)
	if err != nil {
		if errors.Is(err, pty.ErrNotFound) {
			writeError(w, http.StatusNotFound, errText("terminal not found"))
			return
		}
		writeError(w, http.StatusBadRequest, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": info})
}

func snapshotPersistentPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	snapshot, err := manager.Snapshot(r.PathValue("id"), parseLines(r.URL.Query().Get("lines")))
	if err != nil {
		writeError(w, http.StatusNotFound, errText("terminal not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": snapshot})
}

func deletePersistentPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	if err := manager.RemovePersistent(r.PathValue("id")); err != nil {
		writeError(w, http.StatusNotFound, errText("terminal not found"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func persistentPTYToken(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	ticket, expires, err := manager.IssueTicket(r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, errText("terminal not found"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": map[string]any{"ticket": ticket, "expires_in": expires}})
}

func connectPersistentPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	id := r.PathValue("id")
	info, found := manager.GetPersistent(id)
	if !found {
		writeError(w, http.StatusNotFound, errText("terminal not found"))
		return
	}
	if !manager.ValidTicket(id, r.URL.Query().Get("ticket")) {
		writeError(w, http.StatusForbidden, errText("invalid terminal ticket"))
		return
	}
	stream, err := newSSEWriter(w)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	_ = stream.write("status", info)
	stream.flush()
	if info.Status != "running" {
		snapshot, snapErr := manager.Snapshot(id, 0)
		if snapErr == nil {
			_ = stream.write("snapshot", snapshot)
		}
		_ = stream.write("exit", info)
		stream.flush()
		return
	}
	after, _ := parseAfter(r.URL.Query().Get("after"))
	chunks, cancel, err := manager.Subscribe(id, after)
	if err != nil {
		snapshot, snapErr := manager.Snapshot(id, 0)
		if snapErr == nil {
			_ = stream.write("snapshot", snapshot)
		}
		_ = stream.write("exit", info)
		stream.flush()
		return
	}
	defer cancel()
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
	info, _ = manager.GetPersistent(id)
	_ = stream.write("exit", info)
	stream.flush()
}

func inputPersistentPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	var body struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if err := manager.Write(r.PathValue("id"), body.Data); err != nil {
		writeError(w, http.StatusNotFound, errText("terminal not found or exited"))
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": true})
}

func outputPersistentPTY(w http.ResponseWriter, r *http.Request, s *Server, manager *pty.Manager) {
	after, err := parseAfter(r.URL.Query().Get("after"))
	if err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	chunks, next, status, err := manager.Output(r.PathValue("id"), after)
	if err != nil {
		writeError(w, http.StatusNotFound, errText("terminal not found or exited"))
		return
	}
	if chunks == nil {
		chunks = []pty.Chunk{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"chunks": chunks, "next": next, "status": status})
}

func parseLines(value string) int {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < 0 {
		return 0
	}
	return parsed
}
