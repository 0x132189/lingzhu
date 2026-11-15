package session

import (
	"testing"

	"lingzhu/internal/event"
)

// TestRecoveryCountsResumeAttempts proves the recovery analysis tracks how
// often one claim was resumed, which bounds a crash loop.
func TestRecoveryCountsResumeAttempts(t *testing.T) {
	events := []event.Event{
		event.TurnStartedEvent("turn_1"),
		event.TurnResumedEvent("turn_1"),
		event.TurnResumedEvent("turn_1"),
	}
	recovery := analyze(events)
	if recovery.ClaimID != "turn_1" {
		t.Fatalf("claim should stay open, got %q", recovery.ClaimID)
	}
	if recovery.Attempts != 2 {
		t.Fatalf("want two attempts, got %d", recovery.Attempts)
	}
}

// TestRecoveryIgnoresAttemptsOfAnEarlierClaim keeps a finished turn from
// counting against the next one.
func TestRecoveryIgnoresAttemptsOfAnEarlierClaim(t *testing.T) {
	events := []event.Event{
		event.TurnStartedEvent("turn_1"),
		event.TurnResumedEvent("turn_1"),
		event.TurnFinishedEvent("turn_1"),
		event.TurnStartedEvent("turn_2"),
	}
	recovery := analyze(events)
	if recovery.ClaimID != "turn_2" {
		t.Fatalf("want turn_2, got %q", recovery.ClaimID)
	}
	if recovery.Attempts != 0 {
		t.Fatalf("a new claim starts at zero attempts, got %d", recovery.Attempts)
	}
}

// TestRecoveryCancelledTurnResetsAttempts mirrors an explicit stop: the claim
// is released, so a later start must not inherit the budget.
func TestRecoveryCancelledTurnResetsAttempts(t *testing.T) {
	events := []event.Event{
		event.TurnStartedEvent("turn_1"),
		event.TurnResumedEvent("turn_1"),
		event.TurnCancelledEvent("turn_1"),
	}
	recovery := analyze(events)
	if recovery.ClaimID != "" || recovery.Attempts != 0 {
		t.Fatalf("a cancelled turn leaves no work: %+v", recovery)
	}
}
