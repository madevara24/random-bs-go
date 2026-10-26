package pause

import (
	"testing"
	"time"
)

func TestZeroValueIsUnpaused(t *testing.T) {
	var s State
	if s.IsPaused("claude") {
		t.Error("zero-value State reports paused, want false")
	}
	if got := s.Snapshot("claude"); got.Paused {
		t.Errorf("zero-value Snapshot = %+v, want Paused=false", got)
	}
}

func TestNilStateIsPausedReportsFalse(t *testing.T) {
	var s *State
	if s.IsPaused("claude") {
		t.Error("nil *State.IsPaused() = true, want false (callers that never wire a pause.State must behave as before)")
	}
}

func TestTripSetsPausedReasonAndResetsAt(t *testing.T) {
	var s State
	resetsAt := time.Unix(1791552000, 0)
	s.Trip("claude", ReasonUsageLimit, &resetsAt)

	if !s.IsPaused("claude") {
		t.Fatal("IsPaused() = false after Trip")
	}
	got := s.Snapshot("claude")
	if got.Reason != ReasonUsageLimit {
		t.Errorf("Reason = %q, want %q", got.Reason, ReasonUsageLimit)
	}
	if got.ResetsAt == nil || !got.ResetsAt.Equal(resetsAt) {
		t.Errorf("ResetsAt = %v, want %v", got.ResetsAt, resetsAt)
	}
}

// TestTripWithNilResetsAtNeverClobbersKnownOne covers the concurrent-trip
// case: one task's invocation reports a real resetsAt, a second task hits
// the same engine's limit moments later with no structured rate_limit_info
// (e.g. it only matched the stderr fallback) and must not erase the
// already-known reset time.
func TestTripWithNilResetsAtNeverClobbersKnownOne(t *testing.T) {
	var s State
	resetsAt := time.Unix(1791552000, 0)
	s.Trip("claude", ReasonUsageLimit, &resetsAt)
	s.Trip("claude", ReasonUsageLimit, nil)

	got := s.Snapshot("claude")
	if got.ResetsAt == nil || !got.ResetsAt.Equal(resetsAt) {
		t.Errorf("ResetsAt after second Trip(nil) = %v, want unchanged %v", got.ResetsAt, resetsAt)
	}
}

func TestAddBlockedNoteIsIdempotent(t *testing.T) {
	var s State
	s.AddBlockedNote("claude", "Tasks/a.md")
	s.AddBlockedNote("claude", "Tasks/b.md")
	s.AddBlockedNote("claude", "Tasks/a.md")

	got := s.Snapshot("claude").BlockedNotes
	if len(got) != 2 {
		t.Fatalf("BlockedNotes = %v, want exactly 2 entries (no duplicate)", got)
	}
}

func TestClearUnpausesAndReturnsBlockedNotes(t *testing.T) {
	var s State
	resetsAt := time.Now()
	s.Trip("claude", ReasonUsageLimit, &resetsAt)
	s.AddBlockedNote("claude", "Tasks/a.md")
	s.AddBlockedNote("claude", "Tasks/b.md")

	notes := s.Clear("claude")
	if len(notes) != 2 {
		t.Fatalf("Clear() returned %v, want 2 notes", notes)
	}
	if s.IsPaused("claude") {
		t.Error("IsPaused() = true after Clear, want false")
	}
	got := s.Snapshot("claude")
	if got.Reason != "" || got.ResetsAt != nil || len(got.BlockedNotes) != 0 {
		t.Errorf("Snapshot after Clear = %+v, want fully reset", got)
	}
}

func TestClearOnFreshStateReturnsEmpty(t *testing.T) {
	var s State
	notes := s.Clear("claude")
	if len(notes) != 0 {
		t.Errorf("Clear() on a fresh State = %v, want empty", notes)
	}
}

// TestTripOneEngineLeavesAnotherUnpaused is this task's core guarantee: a
// usage limit belongs to one engine's account, so tripping "claude" must
// never pause "other", and Snapshots must report each engine
// independently.
func TestTripOneEngineLeavesAnotherUnpaused(t *testing.T) {
	var s State
	resetsAt := time.Now().Add(time.Hour)
	s.Trip("claude", ReasonUsageLimit, &resetsAt)

	if !s.IsPaused("claude") {
		t.Error("IsPaused(\"claude\") = false after tripping claude, want true")
	}
	if s.IsPaused("other") {
		t.Error("IsPaused(\"other\") = true after only tripping claude, want false")
	}

	snaps := s.Snapshots()
	if !snaps["claude"].Paused {
		t.Errorf("Snapshots()[\"claude\"].Paused = false, want true: %+v", snaps)
	}
	if _, ok := snaps["other"]; ok {
		t.Errorf("Snapshots() has an entry for \"other\", which was never touched: %+v", snaps)
	}
}

// TestClearOneEngineReturnsOnlyThatEnginesBlockedNotes confirms Clear is
// scoped to the engine it's called with: clearing "claude" must not touch
// "other"'s pause state or return "other"'s blocked notes.
func TestClearOneEngineReturnsOnlyThatEnginesBlockedNotes(t *testing.T) {
	var s State
	resetsAt := time.Now().Add(time.Hour)
	s.Trip("claude", ReasonUsageLimit, &resetsAt)
	s.AddBlockedNote("claude", "Tasks/claude-a.md")
	s.Trip("other", ReasonUsageLimit, &resetsAt)
	s.AddBlockedNote("other", "Tasks/other-a.md")

	notes := s.Clear("claude")
	if len(notes) != 1 || notes[0] != "Tasks/claude-a.md" {
		t.Errorf("Clear(\"claude\") = %v, want exactly [Tasks/claude-a.md]", notes)
	}
	if s.IsPaused("claude") {
		t.Error("IsPaused(\"claude\") = true after Clear, want false")
	}
	if !s.IsPaused("other") {
		t.Error("IsPaused(\"other\") = false after clearing only claude, want still true")
	}
	otherNotes := s.Snapshot("other").BlockedNotes
	if len(otherNotes) != 1 || otherNotes[0] != "Tasks/other-a.md" {
		t.Errorf("other's BlockedNotes = %v, want unchanged [Tasks/other-a.md]", otherNotes)
	}
}
