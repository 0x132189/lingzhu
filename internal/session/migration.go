package session

import (
	"os"
	"sort"
	"strings"
)

// MigrationStatus summarizes the store schema and leftover legacy backups.
type MigrationStatus struct {
	SchemaVersion int      `json:"schemaVersion"`
	SessionDir    string   `json:"sessionDir"`
	Sessions      int      `json:"sessions"`
	Events        int      `json:"events"`
	LegacyBackups []string `json:"legacyBackups"`
}

// MigrationStatus reads the schema version and row counts.
func (s *Store) MigrationStatus() (MigrationStatus, error) {
	status := MigrationStatus{SessionDir: s.dir}
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&status.SchemaVersion); err != nil {
		return status, err
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM sessions").Scan(&status.Sessions); err != nil {
		return status, err
	}
	if err := s.db.QueryRow("SELECT COUNT(*) FROM events").Scan(&status.Events); err != nil {
		return status, err
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil && !os.IsNotExist(err) {
		return status, err
	}
	backups := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasSuffix(name, ".jsonl") || strings.HasSuffix(name, ".meta.json") {
			backups = append(backups, name)
		}
	}
	sort.Strings(backups)
	status.LegacyBackups = backups
	return status, nil
}

// MigrateLegacy re-imports old JSONL logs. Imported rows are skipped, so
// running it again is safe.
func (s *Store) MigrateLegacy() error {
	return s.migrateLegacy()
}
