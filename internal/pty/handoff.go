package pty

import (
	"errors"
	"os"
	"strings"
	"time"
)

// HandoffVersion is the current persistent terminal handoff format.
const HandoffVersion = 1

// handoffOutputBytes caps the retained output copied into one export.
const handoffOutputBytes = 1 << 20

// PersistentExport is one terminal record plus its retained output. It can be
// imported by another core process to keep an inspected record after handoff.
type PersistentExport struct {
	Info   PersistentInfo `json:"info"`
	Output string         `json:"output"`
}

// Handoff is a transportable set of persistent terminal records.
type Handoff struct {
	Version    int                `json:"version"`
	ExportedAt time.Time          `json:"exportedAt"`
	Terminals  []PersistentExport `json:"terminals"`
}

// ExportPersistent snapshots every known terminal record and its retained
// output. Running terminals are exported as-is; importing always records them
// as exited because the process cannot cross the process boundary.
func (m *Manager) ExportPersistent() (Handoff, error) {
	m.mu.Lock()
	records := make([]persistentRecord, 0, len(m.persistent))
	for _, record := range m.persistent {
		copied := *record
		copied.file = nil
		copied.info.Args = append([]string(nil), record.info.Args...)
		records = append(records, copied)
	}
	m.mu.Unlock()

	handoff := Handoff{Version: HandoffVersion, ExportedAt: time.Now().UTC(), Terminals: []PersistentExport{}}
	for _, record := range records {
		handoff.Terminals = append(handoff.Terminals, PersistentExport{
			Info:   record.info,
			Output: readTail(m.persistentPath(record.info.ID, ".log"), handoffOutputBytes),
		})
	}
	return handoff, nil
}

// ImportPersistent writes exported terminals as exited records so another core
// can list and inspect them. IDs that already exist are replaced with a fresh
// ID to avoid clobbering live or loaded terminals.
func (m *Manager) ImportPersistent(handoff Handoff) ([]PersistentInfo, error) {
	if handoff.Version != 0 && handoff.Version > HandoffVersion {
		return nil, errors.New("pty: unsupported handoff version")
	}
	imported := make([]PersistentInfo, 0, len(handoff.Terminals))
	for _, entry := range handoff.Terminals {
		info := entry.Info
		if strings.TrimSpace(info.ID) == "" || strings.TrimSpace(info.SessionID) == "" {
			return nil, errors.New("pty: handoff terminal needs id and session")
		}
		m.mu.Lock()
		if m.persistent[info.ID] != nil {
			info.ID = newID("pty_")
		}
		m.mu.Unlock()

		info.Status = "exited"
		info.PID = 0
		info.Output = OutputRange{Head: 0, Tail: int64(len(entry.Output))}
		if err := os.WriteFile(m.persistentPath(info.ID, ".log"), []byte(entry.Output), 0o600); err != nil {
			return nil, err
		}
		data, err := marshalInfo(info)
		if err != nil {
			return nil, err
		}
		if err := os.WriteFile(m.persistentPath(info.ID, ".json"), data, 0o600); err != nil {
			return nil, err
		}

		m.mu.Lock()
		m.persistent[info.ID] = &persistentRecord{info: info, updated: time.Now()}
		m.mu.Unlock()
		imported = append(imported, info)
	}
	return imported, nil
}
