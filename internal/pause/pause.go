// Package pause holds the runner pipeline's shared pause flag: the one
// mutex-guarded bit of state that stops dispatch.RunDispatchPass from
// claiming new work and stops every repo worker from popping its queue once
// a task has hit the Claude usage limit, until either the stored resetsAt
// (plus main.go's grace buffer) fires or a human calls POST /resume.
//
// A leaf package on purpose: internal/runner, internal/dispatch, and
// internal/worker all need to read or write this flag, but none of them may
// import each other (runner can't read a field on daemon.Runner, and daemon
// doesn't import runner), so this package exists as the shared seam,
// constructed once in cmd/pmrunner/runner_mode.go and threaded into
// runner.Deps, daemon.NewRunner, and httpapi.New. It holds no policy of its
// own (no timers, no Discord) -- that stays in main.go, per the task's own
// framing: "pause stays a dumb flag and main holds the policy."
package pause

import (
	"sync"
	"time"
)

// ReasonUsageLimit is the only Reason this codebase currently trips the
// pause with -- a named constant anyway, rather than a bare string literal
// scattered across runner/main.go, since the exact string also appears in
// GET /status/tasks's JSON payload.
const ReasonUsageLimit = "usage_limit"

// State is the shared pause flag. The zero value is unpaused and ready to
// use. Safe for concurrent use by any number of goroutines.
type State struct {
	mu           sync.Mutex
	paused       bool
	reason       string
	resetsAt     *time.Time
	blockedNotes []string
}

// Snapshot is a point-in-time, lock-free copy of State for read-only callers
// (GET /status/tasks, main.go's timer-arming decision).
type Snapshot struct {
	Paused       bool
	Reason       string
	ResetsAt     *time.Time
	BlockedNotes []string
}

// Snapshot copies out the current state under the lock. A nil receiver
// reports the unpaused zero value -- same nil-safety as IsPaused, for
// callers (httpapi's /status/tasks handler in particular) that may not
// have a *State wired in.
func (s *State) Snapshot() Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var resetsAt *time.Time
	if s.resetsAt != nil {
		t := *s.resetsAt
		resetsAt = &t
	}
	return Snapshot{
		Paused:       s.paused,
		Reason:       s.reason,
		ResetsAt:     resetsAt,
		BlockedNotes: append([]string(nil), s.blockedNotes...),
	}
}

// IsPaused is the hot-path check dispatch.RunDispatchPass and a repo
// worker's drain loop make before claiming/popping anything. A nil receiver
// reports false -- callers that never wire a *State (tests, call sites that
// predate this feature) behave exactly as before.
func (s *State) IsPaused() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.paused
}

// Trip pauses the pipeline. Idempotent and safe to call from multiple
// concurrent task invocations that all hit the limit around the same time:
// a resetsAt of nil never clobbers an already-known one, since a later
// detection with no structured rate_limit_info shouldn't erase an earlier,
// more informative one. A nil receiver is a no-op -- same nil-safety as
// IsPaused/Snapshot, for callers that never wired a *State in.
func (s *State) Trip(reason string, resetsAt *time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paused = true
	s.reason = reason
	if resetsAt != nil {
		s.resetsAt = resetsAt
	}
}

// AddBlockedNote records notePath as one of the notes blocked by the
// current pause, for main.go's resume path to flip to blocker_resolved once
// the limit clears. Idempotent -- no duplicate entries. A nil receiver is a
// no-op.
func (s *State) AddBlockedNote(notePath string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.blockedNotes {
		if n == notePath {
			return
		}
	}
	s.blockedNotes = append(s.blockedNotes, notePath)
}

// Clear unpauses and returns the notes that were recorded as blocked by this
// pause episode, so the caller (main.go's resume path, via httpapi.Resume)
// can flip each one to blocker_resolved. Safe to call on an already-cleared
// State (returns an empty slice). A nil receiver returns an empty slice.
func (s *State) Clear() []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	notes := s.blockedNotes
	s.paused = false
	s.reason = ""
	s.resetsAt = nil
	s.blockedNotes = nil
	return notes
}
