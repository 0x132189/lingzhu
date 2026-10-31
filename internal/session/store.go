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

	"lingzhu/internal/event"
	"lingzhu/internal/model"
)

var validID = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type Store struct {
	dir string
	mu  sync.Mutex
}

func Open(dir string) (*Store, error) {
	path := filepath.Join(dir, "sessions")
	if err := os.MkdirAll(path, 0o755); err != nil {
		return nil, err
	}
	return &Store{dir: path}, nil
}

// Append writes a durable event; non-durable events are ignored.
func (s *Store) Append(id string, ev event.Event) error {
	if !ev.Durable() {
		return nil
	}
	if !validID.MatchString(id) {
		return fmt.Errorf("invalid session id %q", id)
	}
	line, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path(id), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return err
	}
	return f.Sync()
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
	messages := make([]model.Message, 0, len(events))
	for _, ev := range events {
		switch ev.Type {
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
	}
	return messages
}

func (s *Store) List() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		ids = append(ids, strings.TrimSuffix(entry.Name(), ".jsonl"))
	}
	sort.Strings(ids)
	return ids, nil
}

func (s *Store) path(id string) string {
	return filepath.Join(s.dir, id+".jsonl")
}

func str(ev event.Event, key string) string {
	if v, ok := ev.Data[key].(string); ok {
		return v
	}
	return ""
}

func text(ev event.Event) string { return str(ev, "text") }
