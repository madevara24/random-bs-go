// Package pause holds the runner pipeline's shared pause flag, keyed by
// engine name: the one mutex-guarded piece of state that stops
// dispatch.RunDispatchPass from claiming a note whose resolved coding
// engine is paused, and stops a repo worker from popping a queued job for
// that same engine, until either the stored resetsAt (plus main.go's grace
// buffer) fires or a human calls POST /resume for that engine. A usage
// limit belongs to one engine's account, so a Claude limit must never stop
// a task that runs on a different engine, and vice versa -- every method
// here takes the engine name it applies to.
//
// A leaf package on purpose: internal/runner, internal/dispatch, and
// internal/worker all need to read or write this state, but none of them
// may import each other (runner can't read a field on daemon.Runner, and
// daemon doesn't import runner), so this package exists as the shared
// seam, constructed once in cmd/pmrunner/runner_mode.go and threaded into
// runner.Deps, daemon.NewRunner, and httpapi.New. It holds no policy of its
// own (no timers, no Discord, no notion of which engines exist) -- that
// stays in main.go, per the task's own framing: "pause stays a dumb flag
// and main holds the policy."
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

// engineState is one engine's pause state.
type engineState struct {
	paused       bool
	reason       string
	resetsAt     *time.Time
	blockedNotes []string
}

// State is the shared pause flag, keyed by engine name. The zero value has
// every engine unpaused and is ready to use. Safe for concurrent use by
// any number of goroutines.
type State struct {
	mu       sync.Mutex
	byEngine map[string]*engineState
}

// Snapshot is a point-in-time, lock-free copy of one engine's state, for
// read-only callers (GET /status/tasks, main.go's timer-arming decision).
type Snapshot struct {
	Paused       bool
	Reason       string
	ResetsAt     *time.Time
	BlockedNotes []string
}

func copyResetsAt(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	c := *t
	return &c
}

func snapshotOf(es *engineState) Snapshot {
	if es == nil {
		return Snapshot{}
	}
	return Snapshot{
		Paused:       es.paused,
		Reason:       es.reason,
		ResetsAt:     copyResetsAt(es.resetsAt),
		BlockedNotes: append([]string(nil), es.blockedNotes...),
	}
}

// Snapshot copies out engine's current state under the lock. An engine
// that was never tripped reports the unpaused zero value. A nil receiver
// does the same -- same nil-safety as IsPaused, for callers (httpapi's
// /status/tasks handler in particular) that may not have a *State wired
// in.
func (s *State) Snapshot(engine string) Snapshot {
	if s == nil {
		return Snapshot{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return snapshotOf(s.byEngine[engine])
}

// Snapshots copies out every engine's current state under the lock, keyed
// by engine name -- only engines that have been tripped or recorded a
// blocked note at some point appear here; an engine that's never been
// touched is simply absent, same meaning as a zero-value Snapshot. A nil
// receiver returns a nil map.
func (s *State) Snapshots() map[string]Snapshot {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]Snapshot, len(s.byEngine))
	for name, es := range s.byEngine {
		out[name] = snapshotOf(es)
	}
	return out
}

// IsPaused is the hot-path check dispatch.RunDispatchPass and a repo
// worker's drain loop make before claiming/popping a note or job resolved
// to engine. A nil receiver, or an engine that was never tripped, reports
// false -- callers that never wire a *State (tests, call sites that
// predate this feature) behave exactly as before.
func (s *State) IsPaused(engine string) bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	es := s.byEngine[engine]
	return es != nil && es.paused
}

func (s *State) engineStateLocked(engine string) *engineState {
	if s.byEngine == nil {
		s.byEngine = map[string]*engineState{}
	}
	es := s.byEngine[engine]
	if es == nil {
		es = &engineState{}
		s.byEngine[engine] = es
	}
	return es
}

// Trip pauses engine. Idempotent and safe to call from multiple concurrent
// task invocations on the same engine that all hit the limit around the
// same time: a resetsAt of nil never clobbers an already-known one, since
// a later detection with no structured rate_limit_info shouldn't erase an
// earlier, more informative one. Pausing one engine never touches any
// other engine's state. A nil receiver is a no-op -- same nil-safety as
// IsPaused/Snapshot, for callers that never wired a *State in.
func (s *State) Trip(engine, reason string, resetsAt *time.Time) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	es := s.engineStateLocked(engine)
	es.paused = true
	es.reason = reason
	if resetsAt != nil {
		es.resetsAt = resetsAt
	}
}

// AddBlockedNote records notePath as one of the notes blocked by engine's
// current pause episode, for main.go's resume path to flip to
// blocker_resolved once that engine's limit clears. Idempotent -- no
// duplicate entries. A nil receiver is a no-op.
func (s *State) AddBlockedNote(engine, notePath string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	es := s.engineStateLocked(engine)
	for _, n := range es.blockedNotes {
		if n == notePath {
			return
		}
	}
	es.blockedNotes = append(es.blockedNotes, notePath)
}

// Clear unpauses engine and returns the notes that were recorded as
// blocked by that engine's pause episode, so the caller (main.go's resume
// path, via httpapi.Server.Resume) can flip each one to blocker_resolved.
// Every other engine's state is untouched. Safe to call on an engine
// that's already clear, or was never tripped (returns an empty slice). A
// nil receiver returns an empty slice.
func (s *State) Clear(engine string) []string {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	es := s.byEngine[engine]
	if es == nil {
		return nil
	}
	delete(s.byEngine, engine)
	return es.blockedNotes
}
