// Package session persists session events and rebuilds messages from them.
package session

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"lingzhu/internal/event"
	"lingzhu/internal/model"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type Store struct {
	dir  string
	mu   sync.Mutex
	seqs map[string]int64
}

func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: path, seqs: map[string]int64{}}, nil
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
	s.mu.Lock()
	defer s.mu.Unlock()
	ev.Seq = s.nextSeqLocked(id) + 1
	line, err := json.Marshal(ev)
	if err != nil {
		return 0, err
	}
	f, err := os.OpenFile(s.path(id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return 0, err
	}
	if err := f.Sync(); err != nil {
		return 0, err
	}
	s.seqs[id] = ev.Seq
	return ev.Seq, nil
}

// nextSeqLocked returns the last sequence of a session, loading it from the log
// on first use. Logs written before sequence numbers existed count their
// durable lines instead.
func (s *Store) nextSeqLocked(id string) int64 {
	if n, ok := s.seqs[id]; ok {
		return n
	}
	events, err := s.Events(id)
	if err != nil {
		return 0
	}
	var last int64
	var count int64
	for _, ev := range events {
		if !ev.Durable() {
			continue
		}
		count++
		if ev.Seq > last {
			last = ev.Seq
		}
	}
	if last == 0 {
		last = count
	}
	s.seqs[id] = last
	return last
}

func (s *Store) Events(id string) ([]event.Event, error) {
	if !validID.MatchString(id) {
		return nil, fmt.Errorf("invalid session id %q", id)
	}
	f, err := os.Open(s.path(id))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var events []event.Event
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for scan.Scan() {
		line := strings.TrimSpace(scan.Text())
		if line == "" {
			continue
		}
		var ev event.Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return nil, fmt.Errorf("session %s: %w", id, err)
		}
		events = append(events, ev)
	}
	return events, scan.Err()
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
	ID      string    `json:"id"`
	Title   string    `json:"title,omitempty"`
	Updated time.Time `json:"updated"`
}

// List returns every session, sorted by ID.
func (s *Store) List() ([]Info, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	infos := make([]Info, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(entry.Name(), ".jsonl")
		info, err := s.Info(id)
		if err != nil {
			continue
		}
		infos = append(infos, info)
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].ID < infos[j].ID })
	return infos, nil
}

// Info returns one session summary. A missing session reports os.ErrNotExist.
func (s *Store) Info(id string) (Info, error) {
	if !validID.MatchString(id) {
		return Info{}, fmt.Errorf("invalid session id %q", id)
	}
	stat, err := os.Stat(s.path(id))
	if err != nil {
		return Info{}, err
	}
	info := Info{ID: id, Updated: stat.ModTime()}
	if data, err := os.ReadFile(s.metaPath(id)); err == nil {
		var meta struct {
			Title string `json:"title"`
		}
		if json.Unmarshal(data, &meta) == nil {
			info.Title = meta.Title
		}
	}
	if info.Title == "" {
		info.Title = s.deriveTitle(id)
	}
	return info, nil
}

// SetTitle stores a display title for the session.
func (s *Store) SetTitle(id, title string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	if _, err := os.Stat(s.path(id)); err != nil {
		return err
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return errors.New("title is empty")
	}
	data, err := json.Marshal(map[string]string{"title": title})
	if err != nil {
		return err
	}
	return os.WriteFile(s.metaPath(id), data, 0o644)
}

// Delete removes a session log and its metadata.
func (s *Store) Delete(id string) error {
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	if err := os.Remove(s.path(id)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	s.mu.Lock()
	delete(s.seqs, id)
	s.mu.Unlock()
	_ = os.Remove(s.metaPath(id))
	return nil
}

// EventsAfter returns durable events with a sequence greater than after.
func (s *Store) EventsAfter(id string, after int64) ([]event.Event, error) {
	events, err := s.Events(id)
	if err != nil {
		return nil, err
	}
	out := make([]event.Event, 0, len(events))
	for _, ev := range events {
		if ev.Seq > after {
			out = append(out, ev)
		}
	}
	return out, nil
}

// deriveTitle falls back to the first user message when no title is stored.
func (s *Store) deriveTitle(id string) string {
	events, err := s.Events(id)
	if err != nil {
		return ""
	}
	for _, ev := range events {
		if ev.Type != event.UserMessage {
			continue
		}
		text := strings.Join(strings.Fields(str(ev, "text")), " ")
		runes := []rune(text)
		if len(runes) > 60 {
			return string(runes[:60]) + "…"
		}
		return text
	}
	return ""
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+".jsonl")
}

func (s *Store) metaPath(id string) string {
	return filepath.Join(s.dir, id+".meta.json")
}

func str(ev event.Event, key string) string {
	if v, ok := ev.Data[key].(string); ok {
		return v
	}
	return ""
}

func text(ev event.Event) string { return str(ev, "text") }
