// Package session persists session events and rebuilds messages from them.
package session

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"lingzhu/internal/event"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Store keeps durable events in SQLite. A transaction commits a whole event, so
// readers never observe a half-written record; WAL lets a read run while a
// write is in progress.
type Store struct {
	dir string
	db  *sql.DB
}

const sessionSchema = `
CREATE TABLE IF NOT EXISTS sessions (
	id       TEXT PRIMARY KEY,
	title    TEXT NOT NULL DEFAULT '',
	provider TEXT NOT NULL DEFAULT '',
	model    TEXT NOT NULL DEFAULT '',
	updated  INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
	session_id TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	type       TEXT NOT NULL,
	at         INTEGER NOT NULL,
	data       TEXT NOT NULL,
	PRIMARY KEY (session_id, seq),
	FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);`

func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(path, "sessions.db"))
	if err != nil {
		return nil, err
	}
	// One connection serializes writers and keeps per-connection pragmas
	// predictable. Reads are short enough that WAL parallelism is not needed.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	store := &Store{dir: path, db: db}
	if err := store.setup(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

// Close releases the database handle. It is used by tests and by callers that
// reopen the same data directory.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) setup() error {
	var mode string
	if err := s.db.QueryRow("PRAGMA journal_mode=WAL").Scan(&mode); err != nil {
		return err
	}
	for _, pragma := range []string{
		"PRAGMA busy_timeout=5000",
		// NORMAL in WAL mode keeps a committed event across an app crash while
		// avoiding an fsync per append.
		"PRAGMA synchronous=NORMAL",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := s.db.Exec(pragma); err != nil {
			return err
		}
	}
	if _, err := s.db.Exec(sessionSchema); err != nil {
		return err
	}

	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version >= 1 {
		return nil
	}
	return s.migrateLegacy()
}

// Append writes a durable event and returns its sequence number. Non-durable
// events are ignored and return zero.
func (s *Store) Append(id string, ev event.Event) (int64, error) {
	if !ev.Durable() {
		return 0, nil
	}
	if !validID.MatchString(id) {
		return 0, fmt.Errorf("invalid session id %q", id)
	}
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return 0, err
	}
	at := int64(0)
	if !ev.At.IsZero() {
		at = ev.At.UnixMilli()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UnixMilli()
	if _, err := tx.Exec(
		`INSERT INTO sessions(id, updated) VALUES(?, ?)
		 ON CONFLICT(id) DO UPDATE SET updated = excluded.updated`,
		id, now,
	); err != nil {
		return 0, err
	}

	var seq int64
	if err := tx.QueryRow(
		`INSERT INTO events(session_id, seq, type, at, data)
		 VALUES(?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE session_id = ?), ?, ?, ?)
		 RETURNING seq`,
		id, id, string(ev.Type), at, string(data),
	).Scan(&seq); err != nil {
		return 0, err
	}

	// The first user message becomes the title unless the user already named
	// the session. Keeping it in the session row means listing never reads the
	// event log.
	if ev.Type == event.UserMessage {
		if title := titleFromText(text(ev)); title != "" {
			if _, err := tx.Exec(
				`UPDATE sessions SET title = ? WHERE id = ? AND title = ''`,
				title, id,
			); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return seq, nil
}

// Events returns every durable event of a session in sequence order.
func (s *Store) Events(id string) ([]event.Event, error) {
	return s.events(id, 0)
}

// EventsAfter returns durable events with a sequence greater than after.
func (s *Store) EventsAfter(id string, after int64) ([]event.Event, error) {
	return s.events(id, after)
}

func (s *Store) events(id string, after int64) ([]event.Event, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	rows, err := s.db.Query(
		`SELECT seq, type, at, data FROM events WHERE session_id = ? AND seq > ? ORDER BY seq`,
		id, after,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []event.Event
	for rows.Next() {
		var (
			seq  int64
			typ  string
			at   int64
			data string
		)
		if err := rows.Scan(&seq, &typ, &at, &data); err != nil {
			return nil, err
		}
		ev := event.Event{Type: event.Type(typ), Seq: seq}
		if at != 0 {
			ev.At = time.UnixMilli(at).UTC()
		}
		if data != "" && data != "null" {
			if err := json.Unmarshal([]byte(data), &ev.Data); err != nil {
				return nil, fmt.Errorf("session %s event %d: %w", id, seq, err)
			}
		}
		events = append(events, ev)
	}
	return events, rows.Err()
}

// Info is the session summary returned by listing and metadata endpoints.
type Info struct {
	ID       string    `json:"id"`
	Title    string    `json:"title,omitempty"`
	Provider string    `json:"provider,omitempty"`
	Model    string    `json:"model,omitempty"`
	Updated  time.Time `json:"updated"`
	// Running is filled by the server from process state, not stored.
	Running bool `json:"running"`
}

// Meta is the stored per-session metadata.
type Meta struct {
	Title    string `json:"title,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
}

// Meta returns the stored metadata; a missing session is an empty Meta.
func (s *Store) Meta(id string) (Meta, error) {
	if !validID.MatchString(id) {
		return Meta{}, fmt.Errorf("invalid session id %q", id)
	}
	var meta Meta
	err := s.db.QueryRow(`SELECT title, provider, model FROM sessions WHERE id = ?`, id).
		Scan(&meta.Title, &meta.Provider, &meta.Model)
	if errors.Is(err, sql.ErrNoRows) {
		return Meta{}, nil
	}
	return meta, err
}

// List returns every session with durable events, sorted by ID.
func (s *Store) List() ([]Info, error) {
	rows, err := s.db.Query(
		`SELECT s.id, s.title, s.provider, s.model, s.updated FROM sessions s
		 WHERE EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id)
		 ORDER BY s.id`,
	)
	if err != nil {
		return nil, err
	}
	infos := make([]Info, 0)
	for rows.Next() {
		var (
			info    Info
			updated int64
		)
		if err := rows.Scan(&info.ID, &info.Title, &info.Provider, &info.Model, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		info.Updated = time.UnixMilli(updated).UTC()
		infos = append(infos, info)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	for i := range infos {
		if infos[i].Title == "" {
			infos[i].Title = s.firstUserTitle(infos[i].ID)
		}
	}
	return infos, nil
}

// Info returns one session summary. A missing session reports os.ErrNotExist.
func (s *Store) Info(id string) (Info, error) {
	if !validID.MatchString(id) {
		return Info{}, fmt.Errorf("invalid session id %q", id)
	}
	var (
		info    Info
		updated int64
	)
	err := s.db.QueryRow(
		`SELECT s.id, s.title, s.provider, s.model, s.updated FROM sessions s
		 WHERE s.id = ? AND EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id)`,
		id,
	).Scan(&info.ID, &info.Title, &info.Provider, &info.Model, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return Info{}, os.ErrNotExist
	}
	if err != nil {
		return Info{}, err
	}
	info.Updated = time.UnixMilli(updated).UTC()
	if info.Title == "" {
		info.Title = s.firstUserTitle(id)
	}
	return info, nil
}

// SetTitle stores a display title for the session.
func (s *Store) SetTitle(id, title string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("title is empty")
	}
	res, err := s.db.Exec(
		`UPDATE sessions SET title = ? WHERE id = ?
		 AND EXISTS (SELECT 1 FROM events WHERE session_id = ?)`,
		title, id, id,
	)
	if err != nil {
		return err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if changed == 0 {
		return os.ErrNotExist
	}
	return nil
}

// SetModel stores the session-level provider/model override. A prompt can set it
// before the session log exists, so the session row is written on its own.
func (s *Store) SetModel(id, provider, model string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	_, err := s.db.Exec(
		`INSERT INTO sessions(id, provider, model, updated) VALUES(?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET provider = excluded.provider, model = excluded.model`,
		id, strings.TrimSpace(provider), strings.TrimSpace(model), time.Now().UnixMilli(),
	)
	return err
}

// Delete removes a session, its events, and any legacy files.
func (s *Store) Delete(id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM events WHERE session_id = ?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	_ = os.Remove(s.legacyPath(id))
	_ = os.Remove(s.legacyMetaPath(id))
	return nil
}
