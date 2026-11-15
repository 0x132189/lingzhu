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
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"lingzhu/internal/event"
	"lingzhu/internal/permission"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Store keeps durable events in SQLite. A transaction commits a whole event, so
// readers never observe a half-written record; WAL lets a read run while a
// write is in progress.
type Store struct {
	dir string
	db  *sql.DB

	// cache keeps each session's event log in memory. The agent rebuilds the
	// conversation every step, and rereading plus JSON-decoding the whole log
	// each time turned long sessions into O(n²) work. It is bounded by both a
	// session count and a byte budget so a long-lived desktop client does not
	// grow without limit; an evicted session reloads from the database.
	mu      sync.Mutex
	cache   map[string][]event.Event
	order   []string
	bytes   int
	maxSize int
	maxByte int
}

const sessionSchema = `
CREATE TABLE IF NOT EXISTS sessions (
	id        TEXT PRIMARY KEY,
	title     TEXT NOT NULL DEFAULT '',
	provider  TEXT NOT NULL DEFAULT '',
	model     TEXT NOT NULL DEFAULT '',
	agent     TEXT NOT NULL DEFAULT '',
	parent    TEXT NOT NULL DEFAULT '',
	goal      TEXT NOT NULL DEFAULT '',
	directory TEXT NOT NULL DEFAULT '',
	updated   INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS events (
	session_id TEXT NOT NULL,
	seq        INTEGER NOT NULL,
	ord        INTEGER NOT NULL DEFAULT 0,
	type       TEXT NOT NULL,
	at         INTEGER NOT NULL,
	data       TEXT NOT NULL,
	PRIMARY KEY (session_id, seq),
	FOREIGN KEY (session_id) REFERENCES sessions(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS counters (
	name  TEXT PRIMARY KEY,
	value INTEGER NOT NULL
);`

// cacheLimits bound the in-memory event cache. Sessions is how many logs stay
// resident; Bytes is the total payload they may hold. Both are generous enough
// that a normal desktop session never reloads, while a long-running client that
// opens hundreds of sessions stays bounded.
const (
	cacheSessions = 24
	cacheBytes    = 64 << 20
)

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

	store := &Store{
		dir:     path,
		db:      db,
		cache:   map[string][]event.Event{},
		maxSize: cacheSessions,
		maxByte: cacheBytes,
	}
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
	if err := s.ensureColumn("sessions", "agent", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("sessions", "parent", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("sessions", "goal", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("sessions", "directory", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := s.ensureColumn("events", "ord", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if err := s.backfillOrder(); err != nil {
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

// ensureColumn adds a column to a table created by an older release. New
// databases already have it from sessionSchema.
func (s *Store) ensureColumn(table, column, definition string) error {
	rows, err := s.db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var (
			cid     int
			name    string
			kind    string
			notnull int
			dflt    sql.NullString
			pk      int
		)
		if err := rows.Scan(&cid, &name, &kind, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	if found {
		return nil
	}
	_, err = s.db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition)
	return err
}

// Append writes a durable event and returns its per-session sequence number.
// Non-durable events are ignored and return zero.
func (s *Store) Append(id string, ev event.Event) (int64, error) {
	stored, err := s.AppendEvent(id, ev)
	return stored.Seq, err
}

// AppendEvent writes a durable event and returns it with both its per-session
// sequence and its cross-session position filled in. A caller that publishes
// the event to live subscribers needs both, so the all-session stream can be
// resumed from the position it carries.
func (s *Store) AppendEvent(id string, ev event.Event) (event.Event, error) {
	if !ev.Durable() {
		return ev, nil
	}
	if !validID.MatchString(id) {
		return ev, fmt.Errorf("invalid session id %q", id)
	}
	data, err := json.Marshal(ev.Data)
	if err != nil {
		return ev, err
	}
	at := int64(0)
	if !ev.At.IsZero() {
		at = ev.At.UnixMilli()
	}

	tx, err := s.db.Begin()
	if err != nil {
		return ev, err
	}
	defer func() { _ = tx.Rollback() }()

	now := time.Now().UnixMilli()
	if _, err := tx.Exec(
		`INSERT INTO sessions(id, updated) VALUES(?, ?)
		 ON CONFLICT(id) DO UPDATE SET updated = excluded.updated`,
		id, now,
	); err != nil {
		return ev, err
	}

	// ord is the cross-session sequence the all-session stream resumes from. It
	// comes from one counter row, incremented in the same transaction as the
	// event, so no two durable events share a position and none is skipped.
	var ord int64
	if err := tx.QueryRow(
		`INSERT INTO counters(name, value) VALUES('event.ord', 1)
		 ON CONFLICT(name) DO UPDATE SET value = value + 1
		 RETURNING value`,
	).Scan(&ord); err != nil {
		return ev, err
	}

	var seq int64
	if err := tx.QueryRow(
		`INSERT INTO events(session_id, seq, ord, type, at, data)
		 VALUES(?, (SELECT COALESCE(MAX(seq), 0) + 1 FROM events WHERE session_id = ?), ?, ?, ?, ?)
		 RETURNING seq`,
		id, id, ord, string(ev.Type), at, string(data),
	).Scan(&seq); err != nil {
		return ev, err
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
				return ev, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return ev, err
	}
	ev.Seq = seq
	ev.Global = ord
	s.mu.Lock()
	if cached, ok := s.cache[id]; ok {
		// An append keeps the log resident and marks it most recently used, so
		// the session the user is working in is never the one evicted.
		s.bytes -= cachedBytes(cached)
		s.cache[id] = append(cached, ev)
		s.bytes += cachedBytes([]event.Event{ev})
		s.touchLocked(id)
		s.trimLocked()
	} else {
		// A reader may be loading this session right now; drop any partial
		// state so the next read starts from the database.
		s.forgetLocked(id)
	}
	s.mu.Unlock()
	return ev, nil
}

// GlobalAfter returns durable events from every session with a cross-session
// position greater than after, oldest first. A client following the
// all-session stream uses it to fill the gap after a dropped connection.
func (s *Store) GlobalAfter(after int64, limit int) ([]GlobalEvent, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.Query(
		`SELECT session_id, seq, ord, type, at, data FROM events
		  WHERE ord > ? ORDER BY ord LIMIT ?`,
		after, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]GlobalEvent, 0, limit)
	for rows.Next() {
		var (
			sessionID string
			ev        event.Event
			at        int64
			data      string
		)
		if err := rows.Scan(&sessionID, &ev.Seq, &ev.Global, &ev.Type, &at, &data); err != nil {
			return nil, err
		}
		if at != 0 {
			ev.At = time.UnixMilli(at).UTC()
		}
		if data != "" && data != "null" {
			if err := json.Unmarshal([]byte(data), &ev.Data); err != nil {
				return nil, fmt.Errorf("session %s event %d: %w", sessionID, ev.Seq, err)
			}
		}
		out = append(out, GlobalEvent{SessionID: sessionID, Event: ev})
	}
	return out, rows.Err()
}

// GlobalEvent is one durable event tagged with the session it belongs to.
type GlobalEvent struct {
	SessionID string
	Event     event.Event
}

// backfillOrder assigns a cross-session position to events written before the
// ord column existed, then moves the counter past them. The database keeps its
// original per-session order because ord is derived from that order.
func (s *Store) backfillOrder() error {
	var missing int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM events WHERE ord = 0`).Scan(&missing); err != nil {
		return err
	}
	if missing == 0 {
		var highest int64
		if err := s.db.QueryRow(`SELECT COALESCE(MAX(ord), 0) FROM events`).Scan(&highest); err != nil {
			return err
		}
		_, err := s.db.Exec(
			`INSERT INTO counters(name, value) VALUES('event.ord', ?)
			 ON CONFLICT(name) DO UPDATE SET value = MAX(value, excluded.value)`,
			highest,
		)
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.Query(`SELECT session_id, seq FROM events ORDER BY at, session_id, seq`)
	if err != nil {
		return err
	}
	type position struct {
		sessionID string
		seq       int64
	}
	var order []position
	for rows.Next() {
		var item position
		if err := rows.Scan(&item.sessionID, &item.seq); err != nil {
			rows.Close()
			return err
		}
		order = append(order, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for index, item := range order {
		if _, err := tx.Exec(
			`UPDATE events SET ord = ? WHERE session_id = ? AND seq = ?`,
			int64(index+1), item.sessionID, item.seq,
		); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(
		`INSERT INTO counters(name, value) VALUES('event.ord', ?)
		 ON CONFLICT(name) DO UPDATE SET value = excluded.value`,
		int64(len(order)),
	); err != nil {
		return err
	}
	return tx.Commit()
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
	s.mu.Lock()
	defer s.mu.Unlock()
	cached, ok := s.cache[id]
	if !ok {
		loaded, err := s.loadEvents(id)
		if err != nil {
			return nil, err
		}
		s.rememberLocked(id, loaded)
		cached = loaded
	} else {
		s.touchLocked(id)
	}
	if after <= 0 {
		return append([]event.Event(nil), cached...), nil
	}
	out := make([]event.Event, 0, len(cached))
	for _, ev := range cached {
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return out, nil
}

// loadEvents reads one session's whole log. Callers must hold s.mu so a
// concurrent append cannot slip between the read and the cache store.
func (s *Store) loadEvents(id string) ([]event.Event, error) {
	rows, err := s.db.Query(
		`SELECT seq, ord, type, at, data FROM events WHERE session_id = ? ORDER BY seq`,
		id,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []event.Event
	for rows.Next() {
		var (
			seq  int64
			ord  int64
			typ  string
			at   int64
			data string
		)
		if err := rows.Scan(&seq, &ord, &typ, &at, &data); err != nil {
			return nil, err
		}
		ev := event.Event{Type: event.Type(typ), Seq: seq, Global: ord}
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

// HasEvents reports whether a session already has a durable event.
func (s *Store) HasEvents(id string) (bool, error) {
	if !validID.MatchString(id) {
		return false, fmt.Errorf("invalid session id %q", id)
	}
	var exists int
	err := s.db.QueryRow(`SELECT EXISTS(SELECT 1 FROM events WHERE session_id = ?)`, id).Scan(&exists)
	return exists == 1, err
}

// rememberLocked caches one session's log and evicts the least recently used
// entries past the count or byte budget. Callers must hold s.mu.
func (s *Store) rememberLocked(id string, events []event.Event) {
	if cached, ok := s.cache[id]; ok {
		s.bytes -= cachedBytes(cached)
	}
	s.cache[id] = events
	s.bytes += cachedBytes(events)
	s.touchLocked(id)
	s.trimLocked()
}

// trimLocked evicts least recently used sessions until the cache fits its
// budgets. The session touched last is always kept. Callers must hold s.mu.
func (s *Store) trimLocked() {
	for len(s.order) > s.maxSize || (s.maxByte > 0 && s.bytes > s.maxByte && len(s.order) > 1) {
		oldest := s.order[0]
		s.order = s.order[1:]
		if cached, ok := s.cache[oldest]; ok {
			s.bytes -= cachedBytes(cached)
			delete(s.cache, oldest)
		}
	}
}

// touchLocked marks one session as most recently used. Callers must hold s.mu.
func (s *Store) touchLocked(id string) {
	for index, existing := range s.order {
		if existing == id {
			s.order = append(s.order[:index], s.order[index+1:]...)
			break
		}
	}
	s.order = append(s.order, id)
}

// forgetLocked drops one session from the cache. Callers must hold s.mu.
func (s *Store) forgetLocked(id string) {
	if cached, ok := s.cache[id]; ok {
		s.bytes -= cachedBytes(cached)
		delete(s.cache, id)
	}
	for index, existing := range s.order {
		if existing == id {
			s.order = append(s.order[:index], s.order[index+1:]...)
			break
		}
	}
}

// cachedBytes approximates the memory one cached log holds. It counts the text
// the events carry, which dominates the footprint; map and slice overhead is
// ignored because the budget only needs to stay in the right order of
// magnitude.
func cachedBytes(events []event.Event) int {
	total := 0
	for _, ev := range events {
		total += len(ev.Data) * 8
		if text, ok := ev.Data["text"].(string); ok {
			total += len(text)
		}
		if output, ok := ev.Data["output"].(string); ok {
			total += len(output)
		}
		if checkpoint, ok := ev.Data["checkpoint"].(string); ok {
			total += len(checkpoint)
		}
	}
	return total
}

// Info is the session summary returned by listing and metadata endpoints.
type Info struct {
	ID       string `json:"id"`
	Title    string `json:"title,omitempty"`
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Agent    string `json:"agent,omitempty"`
	// Parent is the session that created this one as a subagent.
	Parent string `json:"parent,omitempty"`
	// Directory is the workspace this session runs in. An empty value means the
	// session predates per-location placement and belongs to the core's default
	// directory.
	Directory string    `json:"directory,omitempty"`
	Updated   time.Time `json:"updated"`
	// Running is filled by the server from process state, not stored.
	Running bool `json:"running"`
}

// Meta is the stored per-session metadata.
type Meta struct {
	Title     string `json:"title,omitempty"`
	Provider  string `json:"provider,omitempty"`
	Model     string `json:"model,omitempty"`
	Agent     string `json:"agent,omitempty"`
	Parent    string `json:"parent,omitempty"`
	Directory string `json:"directory,omitempty"`
}

// Meta returns the stored metadata; a missing session is an empty Meta.
func (s *Store) Meta(id string) (Meta, error) {
	if !validID.MatchString(id) {
		return Meta{}, fmt.Errorf("invalid session id %q", id)
	}
	var meta Meta
	err := s.db.QueryRow(`SELECT title, provider, model, agent, parent, directory FROM sessions WHERE id = ?`, id).
		Scan(&meta.Title, &meta.Provider, &meta.Model, &meta.Agent, &meta.Parent, &meta.Directory)
	if errors.Is(err, sql.ErrNoRows) {
		return Meta{}, nil
	}
	return meta, err
}

// List returns every session with durable events, sorted by ID.
func (s *Store) List() ([]Info, error) {
	// The first user message supplies a title for migrated rows in one query;
	// resolving it per session turned the list into an N+1 read.
	rows, err := s.db.Query(
		`SELECT s.id, s.title, s.provider, s.model, s.agent, s.parent, s.directory, s.updated,
		        COALESCE((SELECT e.data FROM events e
		                  WHERE e.session_id = s.id AND e.type = ?
		                  ORDER BY e.seq LIMIT 1), '')
		   FROM sessions s
		  WHERE EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id)
		  ORDER BY s.id`,
		string(event.UserMessage),
	)
	if err != nil {
		return nil, err
	}
	infos := make([]Info, 0)
	for rows.Next() {
		var (
			info      Info
			updated   int64
			firstUser string
		)
		if err := rows.Scan(&info.ID, &info.Title, &info.Provider, &info.Model, &info.Agent, &info.Parent, &info.Directory, &updated, &firstUser); err != nil {
			rows.Close()
			return nil, err
		}
		info.Updated = time.UnixMilli(updated).UTC()
		if info.Title == "" {
			info.Title = titleFromUserData(firstUser)
		}
		infos = append(infos, info)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
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
		`SELECT s.id, s.title, s.provider, s.model, s.agent, s.parent, s.directory, s.updated FROM sessions s
		 WHERE s.id = ? AND EXISTS (SELECT 1 FROM events e WHERE e.session_id = s.id)`,
		id,
	).Scan(&info.ID, &info.Title, &info.Provider, &info.Model, &info.Agent, &info.Parent, &info.Directory, &updated)
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

// SetGoal stores the serialized goal for a session; an empty string clears it.
func (s *Store) SetGoal(id, goal string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	_, err := s.db.Exec(`UPDATE sessions SET goal = ? WHERE id = ?`, goal, id)
	return err
}

// Goal returns the serialized goal stored for a session.
func (s *Store) Goal(id string) (string, error) {
	if !validID.MatchString(id) {
		return "", fmt.Errorf("invalid session id %q", id)
	}
	var goal string
	err := s.db.QueryRow(`SELECT goal FROM sessions WHERE id = ?`, id).Scan(&goal)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return goal, err
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

// SetAgent stores the session's selected agent profile.
func (s *Store) SetAgent(id, agent string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	_, err := s.db.Exec(
		`INSERT INTO sessions(id, agent, updated) VALUES(?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET agent = excluded.agent`,
		id, strings.TrimSpace(agent), time.Now().UnixMilli(),
	)
	return err
}

// SetDirectory stores the workspace a session runs in. An empty value clears
// the placement, which puts the session back on the core's default directory.
func (s *Store) SetDirectory(id, directory string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	_, err := s.db.Exec(
		`INSERT INTO sessions(id, directory, updated) VALUES(?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET directory = excluded.directory`,
		id, strings.TrimSpace(directory), time.Now().UnixMilli(),
	)
	return err
}

// SetPermissions appends a session-level permission overlay. Agent rules are
// merged before it, so this overlay can only narrow the agent profile.
func (s *Store) SetPermissions(id string, rules permission.Rules) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	_, err := s.Append(id, event.PermissionsEvent(rules))
	return err
}

// Permissions returns the latest session permission overlay.
func (s *Store) Permissions(id string) (permission.Rules, error) {
	events, err := s.Events(id)
	if err != nil {
		return nil, err
	}
	var rules permission.Rules
	for _, ev := range events {
		if ev.Type != event.SessionPermissions {
			continue
		}
		data, err := json.Marshal(ev.Data["permissions"])
		if err != nil {
			continue
		}
		var next permission.Rules
		if json.Unmarshal(data, &next) == nil {
			rules = next
		}
	}
	return rules, nil
}

// SetParent records the session that created a subagent session. An empty
// parent clears the link.
func (s *Store) SetParent(id, parent string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	_, err := s.db.Exec(
		`INSERT INTO sessions(id, parent, updated) VALUES(?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET parent = excluded.parent`,
		id, strings.TrimSpace(parent), time.Now().UnixMilli(),
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
	s.mu.Lock()
	s.forgetLocked(id)
	s.mu.Unlock()
	_ = os.Remove(s.legacyPath(id))
	_ = os.Remove(s.legacyMetaPath(id))
	return nil
}
