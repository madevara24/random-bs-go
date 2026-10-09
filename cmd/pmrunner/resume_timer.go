package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notify"
)

// resumeTimerMgr owns the auto-resume timer policy for a usage-limit pause.
// pause.State itself stays a dumb flag -- no timers, no Discord -- so this
// is where main.go holds that policy, per the task's own framing ("Own the
// timer goroutine in cmd/pmrunner/runner_mode.go, so pause stays a dumb
// flag and main holds the policy"). One instance per runRunner call, shared
// between the OnBlocked/OnTerminal callbacks that detect a usage-limit trip
// (both call handleTrip) and doResume -- httpapi.Server.Resume, injected
// rather than imported directly so this file doesn't need to know how
// resume's mechanics (clearing the pause, flipping notes, waking
// dispatch/workers) actually happen, only that calling it resumes exactly
// once.
type resumeTimerMgr struct {
	cfg      *config.Config
	notifier *notify.Notifier
	doResume func() ([]string, error)

	mu          sync.Mutex
	timer       *time.Timer
	unknownSent bool
}

func newResumeTimerMgr(cfg *config.Config, notifier *notify.Notifier, doResume func() ([]string, error)) *resumeTimerMgr {
	return &resumeTimerMgr{cfg: cfg, notifier: notifier, doResume: doResume}
}

// handleTrip is called every time a usage-limit block fires -- OnBlocked's
// ScenarioUsageLimit case, or OnTerminal's self-reported-blocked-while-
// tripped case. Idempotent by design: several concurrent tasks can all hit
// the same global limit around the same time, and only the first such call
// should either arm the timer or send the "reset time unknown" message --
// every later call while either of those is still outstanding is a silent
// no-op, re-armable again only once cancel() (a resume, manual or
// automatic) has run.
func (m *resumeTimerMgr) handleTrip(resetsAt *time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.timer != nil || m.unknownSent {
		return
	}
	if resetsAt == nil || !resetsAt.After(time.Now()) {
		m.unknownSent = true
		m.notifier.Send("", "", fmt.Sprintf(
			"<@%s> The Claude usage limit's reset time is unknown -- the pipeline is **paused**; a manual `POST /resume` is needed once it clears.",
			m.cfg.DiscordUserID), "", "")
		return
	}
	grace := time.Duration(m.cfg.UsageLimitResumeGraceSeconds) * time.Second
	d := time.Until(*resetsAt) + grace
	m.timer = time.AfterFunc(d, func() {
		m.notifier.Send("", "", fmt.Sprintf(
			"<@%s> The Claude usage limit reset has passed -- resuming the pipeline.",
			m.cfg.DiscordUserID), "", "")
		if _, err := m.doResume(); err != nil {
			fmt.Printf("[runner] auto-resume after usage limit reset failed: %v\n", err)
		}
		m.mu.Lock()
		m.timer = nil
		m.unknownSent = false
		m.mu.Unlock()
	})
}

// cancel stops the armed timer, if any, and resets unknownSent so a later
// trip can arm/notify again -- called by httpapi's CancelResumeTimer hook
// before a manual POST /resume runs its own resume mechanics, so the timer
// can never also fire afterward and resume a second time.
func (m *resumeTimerMgr) cancel() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	stopped := false
	if m.timer != nil {
		stopped = m.timer.Stop()
		m.timer = nil
	}
	m.unknownSent = false
	return stopped
}
