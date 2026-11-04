// Package session persists session events and rebuilds messages from them.
package session

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
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

// Messages rebuilds the conversation from the event log for the next request.
func (s *Store) Messages(id string) ([]model.Message, error) {
	events, err := s.Events(id)
	if err != nil {
		return nil, err
	}
	return BuildMessages(events), nil
}

func BuildMessages(events []event.Event) []model.Message {
	return build(events, nil)
}

// build reconstructs messages and reports, after each event, how many messages
// exist. Compaction events replace everything before them with one checkpoint.
func build(events []event.Event, observe func(messages int, ev event.Event)) []model.Message {
	messages := make([]model.Message, 0, len(events))
	for _, ev := range events {
		switch ev.Type {
		case event.Compaction:
			messages = []model.Message{{Role: model.RoleUser, Content: str(ev, "checkpoint")}}
		case event.UserMessage:
			messages = append(messages, model.Message{Role: model.RoleUser, Content: text(ev)})
		case event.AssistantMessage:
			messages = append(messages, model.Message{Role: model.RoleAssistant, Content: text(ev)})
		case event.ToolCall:
			if len(messages) == 0 || messages[len(messages)-1].Role != model.RoleAssistant {
				messages = append(messages, model.Message{Role: model.RoleAssistant})
			}
			last := &messages[len(messages)-1]
			last.ToolCalls = append(last.ToolCalls, model.ToolCall{
				ID:        str(ev, "id"),
				Name:      str(ev, "name"),
				Arguments: str(ev, "arguments"),
			})
		case event.ToolResult:
			messages = append(messages, model.Message{
				Role:       model.RoleTool,
				Content:    str(ev, "output"),
				ToolCallID: str(ev, "id"),
				Name:       str(ev, "name"),
				Diff:       str(ev, "diff"),
			})
		}
		if observe != nil {
			observe(len(messages), ev)
		}
	}
	return messages
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

// QueuedInput is one persisted steering item.
type QueuedInput struct {
	ID   string
	Text string
}

// Recovery is durable work a previous process left behind. ClaimID is empty
// when only inputs are pending; Inputs are already ordered by arrival.
type Recovery struct {
	SessionID string
	ClaimID   string
	Inputs    []QueuedInput
}

// QueueInput persists an input before it can be delivered. The id is written
// into the delivered user message, so replay never delivers it twice.
func (s *Store) QueueInput(id, inputID, text string) (int64, error) {
	return s.Append(id, event.InputQueuedEvent(inputID, text))
}

// FinishTurn releases a completed or failed turn claim.
func (s *Store) FinishTurn(id, turnID string) error {
	_, err := s.Append(id, event.TurnFinishedEvent(turnID))
	return err
}

// CancelTurn releases a claim and cancels every input queued before it.
func (s *Store) CancelTurn(id, turnID string) error {
	_, err := s.Append(id, event.TurnCancelledEvent(turnID))
	return err
}

// Recovery returns the durable work left in one session.
func (s *Store) Recovery(id string) (Recovery, error) {
	events, err := s.Events(id)
	if err != nil {
		return Recovery{}, err
	}
	recovery := analyze(events)
	recovery.SessionID = id
	return recovery, nil
}

// Recoveries lists sessions with an open turn claim or undelivered input.
func (s *Store) Recoveries() ([]Recovery, error) {
	rows, err := s.db.Query(`SELECT DISTINCT session_id FROM events ORDER BY session_id`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	out := make([]Recovery, 0, len(ids))
	for _, id := range ids {
		events, err := s.Events(id)
		if err != nil {
			return nil, err
		}
		recovery := analyze(events)
		if recovery.ClaimID == "" && len(recovery.Inputs) == 0 {
			continue
		}
		recovery.SessionID = id
		out = append(out, recovery)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SessionID < out[j].SessionID })
	return out, nil
}

// analyze folds turn claims and queued input into the work a restart must do.
func analyze(events []event.Event) Recovery {
	var recovery Recovery
	pending := map[string]QueuedInput{}
	var order []string
	for _, ev := range events {
		switch ev.Type {
		case event.InputQueued:
			id := str(ev, "id")
			if id == "" {
				continue
			}
			if _, exists := pending[id]; !exists {
				order = append(order, id)
			}
			pending[id] = QueuedInput{ID: id, Text: str(ev, "text")}
		case event.UserMessage:
			if id := str(ev, "inputId"); id != "" {
				delete(pending, id)
			}
		case event.TurnStarted:
			if id := str(ev, "id"); id != "" {
				recovery.ClaimID = id
			}
		case event.TurnFinished:
			if id := str(ev, "id"); id != "" && recovery.ClaimID == id {
				recovery.ClaimID = ""
			}
		case event.TurnCancelled:
			if id := str(ev, "id"); id == "" || recovery.ClaimID == id {
				recovery.ClaimID = ""
				pending = map[string]QueuedInput{}
				order = nil
			}
		}
	}
	for _, id := range order {
		if input, ok := pending[id]; ok {
			recovery.Inputs = append(recovery.Inputs, input)
		}
	}
	return recovery
}

// SettleTools appends a failed result for every tool call after the latest
// compaction that never received one, so a resumed request is valid.
func (s *Store) SettleTools(id string) error {
	events, err := s.Events(id)
	if err != nil {
		return err
	}
	type call struct {
		id   string
		name string
	}
	var calls []call
	settled := map[string]bool{}
	for _, ev := range events {
		switch ev.Type {
		case event.Compaction:
			calls = nil
			settled = map[string]bool{}
		case event.ToolCall:
			calls = append(calls, call{id: str(ev, "id"), name: str(ev, "name")})
		case event.ToolResult:
			settled[str(ev, "id")] = true
		}
	}
	for _, call := range calls {
		if call.id == "" || settled[call.id] {
			continue
		}
		if _, err := s.Append(id, event.ToolResultEvent(call.id, call.name, "Tool execution interrupted", true)); err != nil {
			return err
		}
	}
	return nil
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

func (s *Store) legacyPath(id string) string {
	return filepath.Join(s.dir, id+".jsonl")
}

func (s *Store) legacyMetaPath(id string) string {
	return filepath.Join(s.dir, id+".meta.json")
}

// firstUserTitle reads only the first user message. It backs sessions migrated
// without a stored title; normal sessions get their title when the message is
// appended.
func (s *Store) firstUserTitle(id string) string {
	var data string
	err := s.db.QueryRow(
		`SELECT data FROM events WHERE session_id = ? AND type = ? ORDER BY seq LIMIT 1`,
		id, string(event.UserMessage),
	).Scan(&data)
	if err != nil {
		return ""
	}
	var payload map[string]any
	if json.Unmarshal([]byte(data), &payload) != nil {
		return ""
	}
	text, _ := payload["text"].(string)
	return titleFromText(text)
}

func titleFromEvents(events []event.Event) string {
	for _, ev := range events {
		if ev.Type == event.UserMessage {
			if title := titleFromText(text(ev)); title != "" {
				return title
			}
		}
	}
	return ""
}

// titleFromText collapses whitespace and keeps titles short in the sidebar.
func titleFromText(value string) string {
	text := strings.Join(strings.Fields(value), " ")
	if text == "" {
		return ""
	}
	runes := []rune(text)
	if len(runes) > 60 {
		return string(runes[:60]) + "…"
	}
	return text
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

func str(ev event.Event, key string) string {
	if v, ok := ev.Data[key].(string); ok {
		return v
	}
	return ""
}

func text(ev event.Event) string { return str(ev, "text") }
