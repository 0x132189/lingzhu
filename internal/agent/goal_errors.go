package agent

import (
	"errors"
	"time"
)

var (
	errGoalEmpty     = errors.New("goal objective is empty")
	errGoalTooLong   = errors.New("goal objective is too long")
	errGoalNotActive = errors.New("no active goal to pause")
	errGoalNotPaused = errors.New("no paused goal to resume")
	errGoalMissing   = errors.New("no goal is set")
)

// nowUTC returns the current time in UTC.
func nowUTC() time.Time { return time.Now().UTC() }
