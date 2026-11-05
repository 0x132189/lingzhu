package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"lingzhu/internal/event"
)

// migrateLegacy imports JSONL logs written by older versions. The files are
// left in place as a backup; the database becomes the source of truth.
func (s *Store) migrateLegacy() error {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		if !validID.MatchString(id) {
			continue
		}
		events, err := readLegacyEvents(filepath.Join(s.dir, entry.Name()))
		if err != nil {
			// One damaged old file must not stop the core from starting. Its
			// JSONL stays on disk for manual recovery.
			log.Printf("session: skip legacy %s: %v", id, err)
			continue
		}
		meta := readLegacyMeta(filepath.Join(s.dir, id+".meta.json"))
		title := strings.TrimSpace(meta.Title)
		if title == "" {
			title = titleFromEvents(events)
		}
		updated := time.Now().UnixMilli()
		if info, err := entry.Info(); err == nil {
			updated = info.ModTime().UnixMilli()
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO sessions(id, title, provider, model, updated) VALUES(?, ?, ?, ?, ?)`,
			id, title, meta.Provider, meta.Model, updated,
		); err != nil {
			return err
		}
		var last int64
		for _, ev := range events {
			if !ev.Durable() {
				continue
			}
			seq := ev.Seq
			if seq <= last {
				seq = last + 1
			}
			last = seq
			at := int64(0)
			if !ev.At.IsZero() {
				at = ev.At.UnixMilli()
			}
			data, err := json.Marshal(ev.Data)
			if err != nil {
				return err
			}
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO events(session_id, seq, type, at, data) VALUES(?, ?, ?, ?, ?)`,
				id, seq, string(ev.Type), at, string(data),
			); err != nil {
				return err
			}
		}
	}

	// Metadata without a log is a draft selection written by the old client
	// before its first prompt. Keep it addressable through Meta, but List still
	// hides sessions without events.
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".meta.json") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".meta.json")
		if !validID.MatchString(id) {
			continue
		}
		meta := readLegacyMeta(filepath.Join(s.dir, entry.Name()))
		updated := time.Now().UnixMilli()
		if info, err := entry.Info(); err == nil {
			updated = info.ModTime().UnixMilli()
		}
		if _, err := tx.Exec(
			`INSERT OR IGNORE INTO sessions(id, title, provider, model, updated) VALUES(?, ?, ?, ?, ?)`,
			id, strings.TrimSpace(meta.Title), meta.Provider, meta.Model, updated,
		); err != nil {
			return err
		}
	}

	if _, err := tx.Exec("PRAGMA user_version = 1"); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) legacyPath(id string) string {
	return filepath.Join(s.dir, id+".jsonl")
}

func (s *Store) legacyMetaPath(id string) string {
	return filepath.Join(s.dir, id+".meta.json")
}

// readLegacyEvents decodes the old JSONL format. A torn final record is ignored
// because the append that produced it never completed.
func readLegacyEvents(path string) ([]event.Event, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []event.Event
	dec := json.NewDecoder(bufio.NewReaderSize(f, 64*1024))
	for {
		var ev event.Event
		if err := dec.Decode(&ev); err != nil {
			if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
				break
			}
			return nil, err
		}
		events = append(events, ev)
	}
	return events, nil
}

func readLegacyMeta(path string) Meta {
	var meta Meta
	data, err := os.ReadFile(path)
	if err != nil {
		return meta
	}
	if json.Unmarshal(data, &meta) != nil {
		return Meta{}
	}
	return meta
}
