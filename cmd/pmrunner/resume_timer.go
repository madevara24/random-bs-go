package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notify"
)

// engineResumeState is one engine's auto-resume timer bookkeeping.
type engineResumeState struct {
	timer       *time.Timer
	unknownSent bool
}

// resumeTimerMgr owns the auto-resume timer policy for a usage-limit
// pause, one timer per engine -- pause.State itself stays a dumb flag, no
// timers, no Discord, no notion of engine names -- so this is where
// main.go holds that policy, per the task's own framing ("Own the timer
// goroutine in cmd/pmrunner/runner_mode.go, so pause stays a dumb flag and
// main holds the policy"). One instance per runRunner call, shared
// between the OnBlocked/OnTerminal callbacks that detect a usage-limit
// trip (both call handleTrip) and doResume -- httpapi.Server.Resume,
// injected rather than imported directly so this file doesn't need to
// know how resume's mechanics (clearing the pause, flipping notes, waking
// dispatch/workers) actually happen, only that calling it resumes exactly
// one engine, exactly once.
type resumeTimerMgr struct {
	cfg      *config.Config
	notifier *notify.Notifier
	doResume func(engine string) ([]string, error)

	mu       sync.Mutex
	byEngine map[string]*engineResumeState
}

func newResumeTimerMgr(cfg *config.Config, notifier *notify.Notifier, doResume func(engine string) ([]string, error)) *resumeTimerMgr {
	return &resumeTimerMgr{cfg: cfg, notifier: notifier, doResume: doResume}
}

func (m *resumeTimerMgr) stateLocked(engine string) *engineResumeState {
	if m.byEngine == nil {
		m.byEngine = map[string]*engineResumeState{}
	}
	st := m.byEngine[engine]
	if st == nil {
		st = &engineResumeState{}
		m.byEngine[engine] = st
	}
	return st
}

// timerFor and unknownSentFor are test-only accessors (this package's own
// tests live alongside this file) -- production code never needs to peek
// at one engine's state directly.
func (m *resumeTimerMgr) timerFor(engine string) *time.Timer {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.byEngine[engine]
	if st == nil {
		return nil
	}
	return st.timer
}

func (m *resumeTimerMgr) unknownSentFor(engine string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.byEngine[engine]
	return st != nil && st.unknownSent
}

// handleTrip is called every time a usage-limit hit trips engine's
// pause -- OnBlocked's ScenarioUsageLimit case, or OnTerminal's usage-limit
// case, the latter regardless of whether the task's own terminal status
// came out done, blocked, or failed. Idempotent by design, scoped to
// engine: several concurrent tasks on the same engine can all hit its
// limit around the same time, and only the first call with no usable
// resetsAt should send that engine's "reset time unknown" message -- every
// later call for the same engine while its timer is already armed is a
// silent no-op, re-armable again only once cancel(engine) (a resume,
// manual or automatic) has run. But a call that does carry a real resetsAt
// always arms the timer, even after an earlier call for the same engine
// already sent the "unknown" message, since that's the gap this would
// otherwise leave: the first trip to land might have no resetsAt (e.g. the
// stderr fallback) while a later one does, and that engine's pipeline must
// not stay stuck waiting on a manual resume once a real reset time is
// known. A trip for one engine never arms or notifies for any other.
func (m *resumeTimerMgr) handleTrip(engine string, resetsAt *time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.stateLocked(engine)
	if st.timer != nil {
		return
	}
	if resetsAt == nil || !resetsAt.After(time.Now()) {
		if st.unknownSent {
			return
		}
		st.unknownSent = true
		m.notifier.Send("", "", fmt.Sprintf(
			"<@%s> The %s usage limit's reset time is unknown -- %s tasks are **paused**; a manual `POST /resume?engine=%s` is needed once it clears.",
			m.cfg.DiscordUserID, engine, engine, engine), "", "")
		return
	}
	st.unknownSent = false
	grace := time.Duration(m.cfg.UsageLimitResumeGraceSeconds) * time.Second
	d := time.Until(*resetsAt) + grace
	st.timer = time.AfterFunc(d, func() {
		m.notifier.Send("", "", fmt.Sprintf(
			"<@%s> The %s usage limit reset has passed -- resuming %s tasks.",
			m.cfg.DiscordUserID, engine, engine), "", "")
		if _, err := m.doResume(engine); err != nil {
			fmt.Printf("[runner] auto-resume after usage limit reset failed for engine %s: %v\n", engine, err)
		}
		m.mu.Lock()
		if s := m.byEngine[engine]; s != nil {
			s.timer = nil
			s.unknownSent = false
		}
		m.mu.Unlock()
	})
}

// cancel stops engine's armed timer, if any, and resets its unknownSent so
// a later trip for that engine can arm/notify again -- called by
// httpapi's CancelResumeTimer hook, once per engine being resumed, before
// a manual POST /resume runs its own resume mechanics for that engine, so
// its timer can never also fire afterward and resume it a second time.
// Never touches any other engine's state.
func (m *resumeTimerMgr) cancel(engine string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.byEngine[engine]
	if st == nil {
		return false
	}
	stopped := false
	if st.timer != nil {
		stopped = st.timer.Stop()
		st.timer = nil
	}
	st.unknownSent = false
	return stopped
}
