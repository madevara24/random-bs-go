package pause

import (
	"testing"
	"time"
)

func TestZeroValueIsUnpaused(t *testing.T) {
	var s State
	if s.IsPaused() {
		t.Error("zero-value State reports paused, want false")
	}
	if got := s.Snapshot(); got.Paused {
		t.Errorf("zero-value Snapshot = %+v, want Paused=false", got)
	}
}

func TestNilStateIsPausedReportsFalse(t *testing.T) {
	var s *State
	if s.IsPaused() {
		t.Error("nil *State.IsPaused() = true, want false (callers that never wire a pause.State must behave as before)")
	}
}

func TestTripSetsPausedReasonAndResetsAt(t *testing.T) {
	var s State
	resetsAt := time.Unix(1791552000, 0)
	s.Trip(ReasonUsageLimit, &resetsAt)

	if !s.IsPaused() {
		t.Fatal("IsPaused() = false after Trip")
	}
	got := s.Snapshot()
	if got.Reason != ReasonUsageLimit {
		t.Errorf("Reason = %q, want %q", got.Reason, ReasonUsageLimit)
	}
	if got.ResetsAt == nil || !got.ResetsAt.Equal(resetsAt) {
		t.Errorf("ResetsAt = %v, want %v", got.ResetsAt, resetsAt)
	}
}

// TestTripWithNilResetsAtNeverClobbersKnownOne covers the concurrent-trip
// case: one task's invocation reports a real resetsAt, a second task hits
// the same global limit moments later with no structured rate_limit_info
// (e.g. it only matched the stderr fallback) and must not erase the
// already-known reset time.
func TestTripWithNilResetsAtNeverClobbersKnownOne(t *testing.T) {
	var s State
	resetsAt := time.Unix(1791552000, 0)
	s.Trip(ReasonUsageLimit, &resetsAt)
	s.Trip(ReasonUsageLimit, nil)

	got := s.Snapshot()
	if got.ResetsAt == nil || !got.ResetsAt.Equal(resetsAt) {
		t.Errorf("ResetsAt after second Trip(nil) = %v, want unchanged %v", got.ResetsAt, resetsAt)
	}
}

func TestAddBlockedNoteIsIdempotent(t *testing.T) {
	var s State
	s.AddBlockedNote("Tasks/a.md")
	s.AddBlockedNote("Tasks/b.md")
	s.AddBlockedNote("Tasks/a.md")

	got := s.Snapshot().BlockedNotes
	if len(got) != 2 {
		t.Fatalf("BlockedNotes = %v, want exactly 2 entries (no duplicate)", got)
	}
}

func TestClearUnpausesAndReturnsBlockedNotes(t *testing.T) {
	var s State
	resetsAt := time.Now()
	s.Trip(ReasonUsageLimit, &resetsAt)
	s.AddBlockedNote("Tasks/a.md")
	s.AddBlockedNote("Tasks/b.md")

	notes := s.Clear()
	if len(notes) != 2 {
		t.Fatalf("Clear() returned %v, want 2 notes", notes)
	}
	if s.IsPaused() {
		t.Error("IsPaused() = true after Clear, want false")
	}
	got := s.Snapshot()
	if got.Reason != "" || got.ResetsAt != nil || len(got.BlockedNotes) != 0 {
		t.Errorf("Snapshot after Clear = %+v, want fully reset", got)
	}
}

func TestClearOnFreshStateReturnsEmpty(t *testing.T) {
	var s State
	notes := s.Clear()
	if len(notes) != 0 {
		t.Errorf("Clear() on a fresh State = %v, want empty", notes)
	}
}
