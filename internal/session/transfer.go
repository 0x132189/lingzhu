package session

import (
	"errors"
	"fmt"
	"strings"

	"lingzhu/internal/event"
)

// Bundle is one portable session, including every durable event.
type Bundle struct {
	ID        string        `json:"id"`
	Title     string        `json:"title,omitempty"`
	Provider  string        `json:"provider,omitempty"`
	Model     string        `json:"model,omitempty"`
	Agent     string        `json:"agent,omitempty"`
	Directory string        `json:"directory,omitempty"`
	Events    []event.Event `json:"events"`
}

// Export reads one session and all of its events.
func (s *Store) Export(id string) (Bundle, error) {
	info, err := s.Info(id)
	if err != nil {
		return Bundle{}, err
	}
	events, err := s.Events(id)
	if err != nil {
		return Bundle{}, err
	}
	meta, err := s.Meta(id)
	if err != nil {
		return Bundle{}, err
	}
	return Bundle{
		ID:        id,
		Title:     info.Title,
		Provider:  meta.Provider,
		Model:     meta.Model,
		Agent:     meta.Agent,
		Directory: meta.Directory,
		Events:    events,
	}, nil
}

// Import writes a bundle into this store. An existing session with the same id
// is rejected so an import can never merge into unrelated history.
func (s *Store) Import(bundle Bundle) (Info, error) {
	if !validID.MatchString(bundle.ID) {
		return Info{}, fmt.Errorf("invalid session id %q", bundle.ID)
	}
	if len(bundle.Events) == 0 {
		return Info{}, errors.New("session has no events")
	}
	if exists, err := s.HasEvents(bundle.ID); err != nil {
		return Info{}, err
	} else if exists {
		return Info{}, fmt.Errorf("session %q already exists", bundle.ID)
	}
	for _, ev := range bundle.Events {
		if _, err := s.Append(bundle.ID, ev); err != nil {
			_ = s.Delete(bundle.ID)
			return Info{}, err
		}
	}
	if strings.TrimSpace(bundle.Title) != "" {
		if err := s.SetTitle(bundle.ID, bundle.Title); err != nil {
			_ = s.Delete(bundle.ID)
			return Info{}, err
		}
	}
	if bundle.Provider != "" || bundle.Model != "" {
		if err := s.SetModel(bundle.ID, bundle.Provider, bundle.Model); err != nil {
			_ = s.Delete(bundle.ID)
			return Info{}, err
		}
	}
	if bundle.Agent != "" {
		if err := s.SetAgent(bundle.ID, bundle.Agent); err != nil {
			_ = s.Delete(bundle.ID)
			return Info{}, err
		}
	}
	if bundle.Directory != "" {
		if err := s.SetDirectory(bundle.ID, bundle.Directory); err != nil {
			_ = s.Delete(bundle.ID)
			return Info{}, err
		}
	}
	return s.Info(bundle.ID)
}

// Move copies a session into target and removes it from the source store.
func (s *Store) Move(id string, target *Store) (Info, error) {
	if target == nil {
		return Info{}, errors.New("target store is nil")
	}
	if target == s {
		return Info{}, errors.New("source and target stores are the same")
	}
	bundle, err := s.Export(id)
	if err != nil {
		return Info{}, err
	}
	info, err := target.Import(bundle)
	if err != nil {
		return Info{}, err
	}
	if err := s.Delete(id); err != nil {
		return Info{}, err
	}
	return info, nil
}

// ForkOptions controls a session copy.
type ForkOptions struct {
	// ID is the new session id; required and must be unused.
	ID string
	// Title overrides the derived "<source> (fork)" title.
	Title string
	// BeforeSeq copies only events with a sequence strictly below it. Zero
	// copies the whole session.
	BeforeSeq int64
}

// Fork copies a session into a new id without touching the source. Events that
// belong to an unfinished turn are dropped so the copy never resumes work.
func (s *Store) Fork(id string, options ForkOptions) (Info, error) {
	if !validID.MatchString(options.ID) {
		return Info{}, fmt.Errorf("invalid session id %q", options.ID)
	}
	if options.ID == id {
		return Info{}, errors.New("fork needs a different session id")
	}
	bundle, err := s.Export(id)
	if err != nil {
		return Info{}, err
	}
	events := bundle.Events
	if options.BeforeSeq > 0 {
		kept := make([]event.Event, 0, len(events))
		for _, ev := range events {
			if ev.Seq < options.BeforeSeq {
				kept = append(kept, ev)
			}
		}
		events = kept
	}
	events = trimOpenTurn(events)
	if len(events) == 0 {
		return Info{}, errors.New("fork point has no events")
	}
	title := strings.TrimSpace(options.Title)
	if title == "" {
		base := strings.TrimSpace(bundle.Title)
		if base == "" {
			base = "Session " + id
		}
		title = base + " (fork)"
	}
	bundle.ID = options.ID
	bundle.Title = title
	bundle.Events = events
	return s.Import(bundle)
}

// trimOpenTurn drops a trailing turn that never reached a durable finish so a
// fork starts idle. The event before TurnStarted is usually TurnSnapshot.
func trimOpenTurn(events []event.Event) []event.Event {
	lastBoundary := -1
	for i, ev := range events {
		switch ev.Type {
		case event.RunEnd, event.TurnFinished, event.TurnCancelled:
			lastBoundary = i
		}
	}
	for i := lastBoundary + 1; i < len(events); i++ {
		if events[i].Type != event.TurnStarted {
			continue
		}
		cut := i
		if cut > 0 && events[cut-1].Type == event.TurnSnapshot {
			cut--
		}
		return events[:cut]
	}
	return events
}
