// Package goal keeps the per-session objective the agent works toward across
// turns, together with the reviewer verdicts that drive continuation.
package goal

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
	"time"
)

// Status is the lifecycle state of a goal.
type Status string

const (
	// Active means the loop keeps continuing the session.
	Active Status = "active"
	// Paused means the goal stopped and waits for the user to resume it.
	Paused Status = "paused"
	// Complete means the reviewer decided the objective is met.
	Complete Status = "complete"
)

// MaxObjective bounds one objective.
const MaxObjective = 4000

// MaxRounds is the backstop for loops that stall detection misses.
const MaxRounds = 50

// MaxStalls pauses a goal that repeats itself.
const MaxStalls = 3

// State is one session's goal.
type State struct {
	ID        string `json:"id"`
	Objective string `json:"objective"`
	Status    Status `json:"status"`
	// Reason is the reviewer's reason or what paused the goal.
	Reason string `json:"reason,omitempty"`
	// Rounds counts continuations sent since the goal was set or resumed.
	Rounds int `json:"rounds"`
	// MaxRounds is the per-goal round budget.
	MaxRounds int `json:"maxRounds"`
	// Stalls counts rounds in a row without progress.
	Stalls int `json:"stalls,omitempty"`
	// Boot identifies the process that armed the goal.
	Boot      string    `json:"boot"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// New returns an active goal for an objective.
func New(objective, boot string) State {
	return State{
		ID:        newID(),
		Objective: strings.TrimSpace(objective),
		Status:    Active,
		Rounds:    0,
		MaxRounds: MaxRounds,
		Boot:      boot,
		UpdatedAt: time.Now().UTC(),
	}
}

// newID returns a short random identifier.
func newID() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format("20060102150405")))
	}
	return hex.EncodeToString(buffer)
}

// Verdict is the reviewer's decision for one round.
type Verdict struct {
	Verdict  string `json:"verdict"`
	Reason   string `json:"reason"`
	NextStep string `json:"next_step,omitempty"`
}

// IsDone reports whether the objective is met.
func (v Verdict) IsDone() bool { return v.Verdict == "done" }

// IsContinue reports whether the loop should take another round.
func (v Verdict) IsContinue() bool { return v.Verdict == "continue" }

// IsBlocked reports whether only the user can unblock the goal.
func (v Verdict) IsBlocked() bool { return v.Verdict == "blocked" }

// IsPaused reports whether the user asked to stop.
func (v Verdict) IsPaused() bool { return v.Verdict == "paused" }
