// Package schedule persists prompts that wake a session later. A background
// loop polls the store and delivers due prompts as synthetic user turns.
package schedule

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	StatusActive = "active"
	StatusPaused = "paused"
	StatusDone   = "done"

	// pollInterval is how often the loop looks for due schedules.
	pollInterval = time.Second
	// retryDelay delays a failed delivery so a broken session does not spin.
	retryDelay = 30 * time.Second
	// minInterval bounds repeating schedules created by the model.
	minInterval = time.Second
)

// Config is the schedule section of the configuration file.
type Config struct {
	Enabled *bool `json:"enabled,omitempty"`
}

// When describes one trigger. Type is in, at or every.
type When struct {
	Type  string `json:"type"`
	In    string `json:"in,omitempty"`
	At    string `json:"at,omitempty"`
	Every string `json:"every,omitempty"`
}

// Item is one stored schedule.
type Item struct {
	ID          string    `json:"id"`
	Name        string    `json:"name,omitempty"`
	SessionID   string    `json:"sessionId"`
	Prompt      string    `json:"prompt"`
	When        When      `json:"when"`
	Status      string    `json:"status"`
	Created     time.Time `json:"created"`
	Updated     time.Time `json:"updated"`
	NextDueAt   time.Time `json:"nextDueAt,omitzero"`
	LastFiredAt time.Time `json:"lastFiredAt,omitzero"`
	RunCount    int       `json:"runCount"`
	LastError   string    `json:"lastError,omitempty"`
}

// CreateRequest is the validated input for a new schedule.
type CreateRequest struct {
	Name   string
	Prompt string
	When   When
}

// UpdateRequest changes an existing schedule; nil fields keep their value.
type UpdateRequest struct {
	Name   *string
	Prompt *string
	When   *When
}

// Manager owns the persisted schedule file and its firing loop.
type Manager struct {
	file string
	mu   sync.Mutex

	items map[string]Item
	fire  func(Item) error
	logf  func(string, ...any)
	now   func() time.Time

	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// New loads previously stored schedules. A nil or false enabled flag disables
// scheduling entirely.
func New(dataDir string, cfg Config, logf func(string, ...any)) (*Manager, error) {
	if cfg.Enabled != nil && !*cfg.Enabled {
		return nil, nil
	}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("schedule store needs a data directory")
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	m := &Manager{
		file:  filepath.Join(dataDir, "schedules.json"),
		items: map[string]Item{},
		logf:  logf,
		now:   time.Now,
		done:  make(chan struct{}),
	}
	if err := m.load(); err != nil {
		return nil, err
	}
	return m, nil
}

// SetFire installs the delivery callback.
func (m *Manager) SetFire(fn func(Item) error) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.fire = fn
	m.mu.Unlock()
}

// Start runs the polling loop until ctx is cancelled or Close is called.
func (m *Manager) Start(ctx context.Context) {
	if m == nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	m.mu.Lock()
	if m.cancel != nil {
		m.mu.Unlock()
		cancel()
		return
	}
	m.cancel = cancel
	m.mu.Unlock()

	go func() {
		defer close(m.done)
		ticker := time.NewTicker(pollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				m.tick()
			}
		}
	}()
}

// Close stops the polling loop.
func (m *Manager) Close() {
	if m == nil {
		return
	}
	m.once.Do(func() {
		m.mu.Lock()
		cancel := m.cancel
		m.cancel = nil
		m.mu.Unlock()
		if cancel != nil {
			cancel()
			<-m.done
		}
	})
}

// Create stores a new active schedule.
func (m *Manager) Create(sessionID string, req CreateRequest) (Item, error) {
	if m == nil {
		return Item{}, errors.New("schedules are disabled")
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return Item{}, errors.New("schedule needs a session")
	}
	prompt := strings.TrimSpace(req.Prompt)
	if prompt == "" {
		return Item{}, errors.New("schedule prompt is empty")
	}
	when, next, err := resolveWhen(req.When, m.now())
	if err != nil {
		return Item{}, err
	}
	id, err := newID()
	if err != nil {
		return Item{}, err
	}
	now := m.now().UTC()
	item := Item{
		ID:        id,
		Name:      strings.TrimSpace(req.Name),
		SessionID: sessionID,
		Prompt:    prompt,
		When:      when,
		Status:    StatusActive,
		Created:   now,
		Updated:   now,
		NextDueAt: next,
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.items[id] = item
	if err := m.saveLocked(); err != nil {
		delete(m.items, id)
		return Item{}, err
	}
	return item, nil
}

// Update changes an existing schedule owned by sessionID.
func (m *Manager) Update(id, sessionID string, req UpdateRequest) (Item, error) {
	if m == nil {
		return Item{}, errors.New("schedules are disabled")
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	item, err := m.ownedLocked(id, sessionID)
	if err != nil {
		return Item{}, err
	}
	if req.Name != nil {
		item.Name = strings.TrimSpace(*req.Name)
	}
	if req.Prompt != nil {
		prompt := strings.TrimSpace(*req.Prompt)
		if prompt == "" {
			return Item{}, errors.New("schedule prompt is empty")
		}
		item.Prompt = prompt
	}
	if req.When != nil {
		when, next, err := resolveWhen(*req.When, now)
		if err != nil {
			return Item{}, err
		}
		item.When = when
		item.NextDueAt = next
		item.Status = StatusActive
	}
	item.Updated = now.UTC()
	m.items[item.ID] = item
	if err := m.saveLocked(); err != nil {
		return Item{}, err
	}
	return item, nil
}

// Pause stops a schedule from firing until it is resumed.
func (m *Manager) Pause(id, sessionID string) (Item, error) {
	if m == nil {
		return Item{}, errors.New("schedules are disabled")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item, err := m.ownedLocked(id, sessionID)
	if err != nil {
		return Item{}, err
	}
	if item.Status == StatusDone {
		return Item{}, errors.New("schedule is already done")
	}
	item.Status = StatusPaused
	item.Updated = m.now().UTC()
	m.items[item.ID] = item
	if err := m.saveLocked(); err != nil {
		return Item{}, err
	}
	return item, nil
}

// Resume re-activates a paused schedule. A one-shot that fell due while paused
// fires once on the next tick; a repeating schedule skips past missed slots.
func (m *Manager) Resume(id, sessionID string) (Item, error) {
	if m == nil {
		return Item{}, errors.New("schedules are disabled")
	}
	now := m.now()
	m.mu.Lock()
	defer m.mu.Unlock()
	item, err := m.ownedLocked(id, sessionID)
	if err != nil {
		return Item{}, err
	}
	if item.Status == StatusDone {
		return Item{}, errors.New("schedule is already done")
	}
	if item.Status == StatusActive {
		return item, nil
	}
	item.Status = StatusActive
	if item.When.Type == "every" {
		item.NextDueAt = nextInterval(item.NextDueAt, now, intervalOf(item.When))
	} else if item.NextDueAt.Before(now) {
		item.NextDueAt = now.UTC()
	}
	item.Updated = now.UTC()
	m.items[item.ID] = item
	if err := m.saveLocked(); err != nil {
		return Item{}, err
	}
	return item, nil
}

// Delete removes a schedule for good.
func (m *Manager) Delete(id, sessionID string) error {
	if m == nil {
		return errors.New("schedules are disabled")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	item, err := m.ownedLocked(id, sessionID)
	if err != nil {
		return err
	}
	delete(m.items, item.ID)
	return m.saveLocked()
}

// List returns one session's schedules ordered by next due time.
func (m *Manager) List(sessionID string) []Item {
	if m == nil {
		return []Item{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Item, 0, len(m.items))
	for _, item := range m.items {
		if item.SessionID == sessionID {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].NextDueAt.Equal(out[j].NextDueAt) {
			return out[i].NextDueAt.Before(out[j].NextDueAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// All returns every stored schedule.
func (m *Manager) All() []Item {
	if m == nil {
		return []Item{}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Item, 0, len(m.items))
	for _, item := range m.items {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].NextDueAt.Equal(out[j].NextDueAt) {
			return out[i].NextDueAt.Before(out[j].NextDueAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// Get returns one schedule owned by sessionID.
func (m *Manager) Get(id, sessionID string) (Item, error) {
	if m == nil {
		return Item{}, errors.New("schedules are disabled")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.ownedLocked(id, sessionID)
}

// Tick fires due schedules once. It is exported for tests.
func (m *Manager) Tick() {
	if m == nil {
		return
	}
	m.tick()
}

type dueItem struct {
	item Item
	due  time.Time
	fire func(Item) error
}

func (m *Manager) tick() {
	now := m.now()
	m.mu.Lock()
	var due []dueItem
	for id, item := range m.items {
		if item.Status != StatusActive || item.NextDueAt.IsZero() || item.NextDueAt.After(now) {
			continue
		}
		// Claim the slot before releasing the lock so a slow delivery cannot
		// be started twice by overlapping ticks.
		original := item.NextDueAt
		item.NextDueAt = now.Add(retryDelay)
		m.items[id] = item
		due = append(due, dueItem{item: item, due: original, fire: m.fire})
	}
	if len(due) > 0 {
		if err := m.saveLocked(); err != nil {
			m.logf("schedule: save: %v", err)
		}
	}
	m.mu.Unlock()

	for _, entry := range due {
		var fireErr error
		if entry.fire != nil {
			fireErr = entry.fire(entry.item)
		}
		m.mu.Lock()
		item, ok := m.items[entry.item.ID]
		if !ok || !item.NextDueAt.Equal(entry.item.NextDueAt) {
			// Deleted or rescheduled while the delivery was running.
			m.mu.Unlock()
			continue
		}
		item.LastFiredAt = now.UTC()
		item.Updated = now.UTC()
		if fireErr != nil {
			item.LastError = fireErr.Error()
			item.NextDueAt = now.Add(retryDelay)
			m.logf("schedule: %s delivery failed: %v", item.ID, fireErr)
		} else {
			item.LastError = ""
			item.RunCount++
			if item.When.Type == "every" {
				item.NextDueAt = nextInterval(entry.due, now, intervalOf(item.When))
			} else {
				item.Status = StatusDone
				item.NextDueAt = time.Time{}
			}
		}
		m.items[item.ID] = item
		if err := m.saveLocked(); err != nil {
			m.logf("schedule: save: %v", err)
		}
		m.mu.Unlock()
	}
}

func (m *Manager) ownedLocked(id, sessionID string) (Item, error) {
	item, ok := m.items[strings.TrimSpace(id)]
	if !ok || (sessionID != "" && item.SessionID != sessionID) {
		return Item{}, fmt.Errorf("schedule %s not found", id)
	}
	return item, nil
}

func (m *Manager) load() error {
	data, err := os.ReadFile(m.file)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return nil
	}
	var items map[string]Item
	if err := json.Unmarshal(data, &items); err != nil {
		return err
	}
	if items != nil {
		m.items = items
	}
	return nil
}

func (m *Manager) saveLocked() error {
	data, err := json.MarshalIndent(m.items, "", "  ")
	if err != nil {
		return err
	}
	temp := m.file + ".tmp"
	if err := os.WriteFile(temp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, m.file)
}

func resolveWhen(when When, now time.Time) (When, time.Time, error) {
	switch strings.ToLower(strings.TrimSpace(when.Type)) {
	case "in":
		delay, err := parseDuration(when.In)
		if err != nil {
			return When{}, time.Time{}, err
		}
		if delay < minInterval {
			return When{}, time.Time{}, errors.New("schedule delay must be at least one second")
		}
		return When{Type: "in", In: strings.TrimSpace(when.In)}, now.Add(delay).UTC(), nil
	case "at":
		whenAt, err := parseAt(when.At, now)
		if err != nil {
			return When{}, time.Time{}, err
		}
		if !whenAt.After(now) {
			return When{}, time.Time{}, errors.New("schedule time is in the past")
		}
		return When{Type: "at", At: strings.TrimSpace(when.At)}, whenAt.UTC(), nil
	case "every":
		interval, err := parseDuration(when.Every)
		if err != nil {
			return When{}, time.Time{}, err
		}
		if interval < minInterval {
			return When{}, time.Time{}, errors.New("schedule interval must be at least one second")
		}
		return When{Type: "every", Every: strings.TrimSpace(when.Every)}, now.Add(interval).UTC(), nil
	case "cron":
		return When{}, time.Time{}, errors.New("cron schedules are not supported; use every")
	}
	return When{}, time.Time{}, fmt.Errorf("schedule when.type must be in, at or every")
}

func intervalOf(when When) time.Duration {
	if when.Type != "every" {
		return 0
	}
	interval, err := parseDuration(when.Every)
	if err != nil || interval < minInterval {
		return minInterval
	}
	return interval
}

// nextInterval advances from the previous slot until it is after now, so a
// repeating schedule keeps its cadence instead of drifting on every tick.
func nextInterval(previous, now time.Time, interval time.Duration) time.Time {
	if interval <= 0 {
		interval = minInterval
	}
	next := previous
	if next.IsZero() {
		next = now
	}
	for !next.After(now) {
		next = next.Add(interval)
	}
	return next.UTC()
}

func parseDuration(value string) (time.Duration, error) {
	value = strings.ToLower(strings.TrimSpace(value))
	if value == "" {
		return 0, errors.New("schedule duration is empty")
	}
	if duration, err := time.ParseDuration(value); err == nil {
		return duration, nil
	}
	fields := strings.Fields(value)
	if len(fields) != 2 {
		return 0, fmt.Errorf("invalid duration %q", value)
	}
	count, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || count <= 0 {
		return 0, fmt.Errorf("invalid duration %q", value)
	}
	unit := strings.TrimSuffix(fields[1], "s")
	var scale time.Duration
	switch unit {
	case "second", "sec":
		scale = time.Second
	case "minute", "min":
		scale = time.Minute
	case "hour", "hr":
		scale = time.Hour
	case "day":
		scale = 24 * time.Hour
	case "week":
		scale = 7 * 24 * time.Hour
	default:
		return 0, fmt.Errorf("invalid duration unit %q", fields[1])
	}
	return time.Duration(count * float64(scale)), nil
}

func parseAt(value string, now time.Time) (time.Time, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, errors.New("schedule time is empty")
	}
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04", "2006-01-02 15:04", "15:04"} {
		if parsed, err := time.ParseInLocation(layout, value, time.Local); err == nil {
			if layout == "15:04" {
				candidate := time.Date(now.Year(), now.Month(), now.Day(), parsed.Hour(), parsed.Minute(), 0, 0, time.Local)
				if !candidate.After(now) {
					candidate = candidate.Add(24 * time.Hour)
				}
				return candidate, nil
			}
			return parsed, nil
		}
	}
	return time.Time{}, fmt.Errorf("invalid schedule time %q", value)
}

func newID() (string, error) {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "sch_" + hex.EncodeToString(buf), nil
}

// Summary renders one schedule for tool output.
func Summary(item Item) string {
	trigger := item.When.Type
	switch item.When.Type {
	case "in":
		trigger = "in " + item.When.In
	case "at":
		trigger = "at " + item.When.At
	case "every":
		trigger = "every " + item.When.Every
	}
	name := ""
	if strings.TrimSpace(item.Name) != "" {
		name = " \"" + item.Name + "\""
	}
	line := fmt.Sprintf("%s%s: %s, %s", item.ID, name, trigger, item.Status)
	if !item.NextDueAt.IsZero() {
		line += ", next " + item.NextDueAt.Local().Format(time.RFC3339)
	}
	if item.RunCount > 0 {
		line += fmt.Sprintf(", runs %d", item.RunCount)
	}
	if item.LastError != "" {
		line += ", error " + item.LastError
	}
	return line
}
