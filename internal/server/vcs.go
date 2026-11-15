package server

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"lingzhu/internal/vcs"
)

// SetVCS attaches the version-control reader; nil disables the endpoints.
func (s *Server) SetVCS(manager *vcs.Manager) {
	s.vcs = manager
}

func vcsInfo(w http.ResponseWriter, r *http.Request, manager *vcs.Manager) {
	info, err := manager.Info(r.Context())
	if err != nil {
		writeVCSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"info": info})
}

func vcsBase(w http.ResponseWriter, r *http.Request, manager *vcs.Manager) {
	base, err := manager.Base(r.Context())
	if err != nil {
		writeVCSError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"base": base})
}

func vcsStatus(w http.ResponseWriter, r *http.Request, manager *vcs.Manager) {
	files, err := manager.Status(r.Context())
	if err != nil {
		writeVCSError(w, err)
		return
	}
	if files == nil {
		files = []vcs.FileStatus{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": files})
}

func vcsBranches(w http.ResponseWriter, r *http.Request, manager *vcs.Manager) {
	limit := 0
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		if parsed, err := strconv.Atoi(value); err == nil {
			limit = parsed
		}
	}
	branches, err := manager.Branches(r.Context(), r.URL.Query().Get("search"), limit)
	if err != nil {
		writeVCSError(w, err)
		return
	}
	if branches == nil {
		branches = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"branches": branches})
}

func vcsDiff(w http.ResponseWriter, r *http.Request, manager *vcs.Manager) {
	query := r.URL.Query()
	mode := strings.ToLower(strings.TrimSpace(query.Get("mode")))
	switch mode {
	case "", "working", "branch", "committed":
	default:
		writeError(w, http.StatusBadRequest, errText("mode must be working, branch or committed"))
		return
	}
	contextLines := -1
	if value := strings.TrimSpace(query.Get("context")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil || parsed < 0 {
			writeError(w, http.StatusBadRequest, errText("context must be a non-negative integer"))
			return
		}
		contextLines = parsed
	}
	diffs, err := manager.Diff(r.Context(), vcs.DiffOptions{
		Mode:      query.Get("mode"),
		Base:      query.Get("base"),
		Context:   contextLines,
		Untracked: query.Get("untracked"),
	})
	if err != nil {
		writeVCSError(w, err)
		return
	}
	if diffs == nil {
		diffs = []vcs.FileDiff{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"diffs": diffs})
}

func vcsInit(w http.ResponseWriter, r *http.Request, manager *vcs.Manager) {
	if err := manager.Init(r.Context()); err != nil {
		writeVCSError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// withVCS answers 501 when version control is unavailable and hands the handler
// the manager otherwise, so no handler repeats the guard.
func (s *Server) withVCS(fn func(http.ResponseWriter, *http.Request, *vcs.Manager)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.vcs == nil {
			writeError(w, http.StatusNotImplemented, errText("version control is not available"))
			return
		}
		fn(w, r, s.vcs)
	}
}

func writeVCSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, vcs.ErrNotRepository):
		writeError(w, http.StatusBadRequest, errText("not a git repository"))
	case errors.Is(err, vcs.ErrAlreadyRepository):
		writeError(w, http.StatusConflict, errText("directory already has a repository"))
	default:
		writeError(w, http.StatusInternalServerError, err)
	}
}
