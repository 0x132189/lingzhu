package server

import (
	"net/http"
	"path/filepath"
)

// migrationStatus reports the store schema, row counts and legacy backups.
func (s *Server) migrationStatus(w http.ResponseWriter, r *http.Request) {
	status, err := s.store.MigrationStatus()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"migration": status,
		"database":  filepath.Join(status.SessionDir, "sessions.db"),
	})
}

// runMigration re-imports legacy JSONL logs and reports the new status.
func (s *Server) runMigration(w http.ResponseWriter, r *http.Request) {
	if err := s.store.MigrateLegacy(); err != nil {
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	s.migrationStatus(w, r)
}
