package main

import (
	"strings"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notify"
)

// TestResumeTimerFiresAndResumesAfterResetsAt confirms the auto-resume
// timer, once armed via handleTrip, waits for resetsAt plus the configured
// grace buffer, sends exactly one "reset passed, resuming" Devara-only
// message, and calls doResume exactly once with the tripped engine's name.
func TestResumeTimerFiresAndResumesAfterResetsAt(t *testing.T) {
	srv, alerts := capturingDiscordServer(t)
	defer srv.Close()

	cfg := &config.Config{DiscordUserID: "12345", UsageLimitResumeGraceSeconds: 0}
	notifier := &notify.Notifier{WebhookURL: srv.URL}

	var doResumeCalls int
	var gotEngine string
	done := make(chan struct{}, 1)
	mgr := newResumeTimerMgr(cfg, notifier, func(engine string) ([]string, error) {
		doResumeCalls++
		gotEngine = engine
		done <- struct{}{}
		return nil, nil
	})

	resetsAt := time.Now().Add(100 * time.Millisecond)
	mgr.handleTrip("claude", &resetsAt)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the auto-resume timer to fire")
	}

	if doResumeCalls != 1 {
		t.Errorf("doResume called %d times, want 1", doResumeCalls)
	}
	if gotEngine != "claude" {
		t.Errorf("doResume called with engine %q, want %q", gotEngine, "claude")
	}

	got := waitForAlert(t, alerts)
	if !strings.Contains(got.content, "<@12345>") {
		t.Errorf("message %q missing Devara mention", got.content)
	}
	if !strings.Contains(got.content, "reset") {
		t.Errorf("message %q missing reset wording", got.content)
	}
	if !strings.Contains(got.content, "claude") {
		t.Errorf("message %q missing the engine name", got.content)
	}
}

// TestResumeTimerHandleTripIsIdempotentWhileArmed confirms concurrent/
// repeated handleTrip calls for the same engine (several tasks hitting
// that engine's limit around the same time) don't arm a second timer.
func TestResumeTimerHandleTripIsIdempotentWhileArmed(t *testing.T) {
	srv, _ := capturingDiscordServer(t)
	defer srv.Close()

	cfg := &config.Config{DiscordUserID: "12345", UsageLimitResumeGraceSeconds: 0}
	notifier := &notify.Notifier{WebhookURL: srv.URL}

	mgr := newResumeTimerMgr(cfg, notifier, func(string) ([]string, error) { return nil, nil })

	resetsAt := time.Now().Add(time.Hour)
	mgr.handleTrip("claude", &resetsAt)
	firstTimer := mgr.timerFor("claude")
	mgr.handleTrip("claude", &resetsAt)
	if mgr.timerFor("claude") != firstTimer {
		t.Error("handleTrip armed a second timer while the first was still outstanding")
	}
	mgr.cancel("claude")
}

// TestResumeTimerUnknownResetsAtSendsMessageNotTimer confirms a nil/past
// resetsAt skips the timer entirely and sends that engine's "reset time
// unknown, manual resume needed" message instead.
func TestResumeTimerUnknownResetsAtSendsMessageNotTimer(t *testing.T) {
	srv, alerts := capturingDiscordServer(t)
	defer srv.Close()

	cfg := &config.Config{DiscordUserID: "12345"}
	notifier := &notify.Notifier{WebhookURL: srv.URL}
	var doResumeCalls int
	mgr := newResumeTimerMgr(cfg, notifier, func(string) ([]string, error) {
		doResumeCalls++
		return nil, nil
	})

	mgr.handleTrip("claude", nil)

	got := waitForAlert(t, alerts)
	if !strings.Contains(got.content, "unknown") {
		t.Errorf("message %q missing \"unknown\" wording", got.content)
	}
	if !strings.Contains(got.content, "<@12345>") {
		t.Errorf("message %q missing Devara mention", got.content)
	}
	if !strings.Contains(got.content, "claude") {
		t.Errorf("message %q missing the engine name", got.content)
	}
	if mgr.timerFor("claude") != nil {
		t.Error("timer armed despite an unknown resetsAt")
	}
	if doResumeCalls != 0 {
		t.Errorf("doResume called %d times, want 0 (no timer fired)", doResumeCalls)
	}
}

// TestResumeTimerCancelStopsPendingTimer confirms cancel actually stops an
// armed timer before it fires -- the manual-POST-/resume-before-the-timer
// case, which must never let the pipeline resume twice.
func TestResumeTimerCancelStopsPendingTimer(t *testing.T) {
	srv, _ := capturingDiscordServer(t)
	defer srv.Close()

	cfg := &config.Config{DiscordUserID: "12345", UsageLimitResumeGraceSeconds: 0}
	notifier := &notify.Notifier{WebhookURL: srv.URL}
	var doResumeCalls int
	mgr := newResumeTimerMgr(cfg, notifier, func(string) ([]string, error) {
		doResumeCalls++
		return nil, nil
	})

	resetsAt := time.Now().Add(200 * time.Millisecond)
	mgr.handleTrip("claude", &resetsAt)
	if !mgr.cancel("claude") {
		t.Fatal("cancel() = false, want true (an armed timer was stopped)")
	}

	time.Sleep(500 * time.Millisecond)
	if doResumeCalls != 0 {
		t.Errorf("doResume called %d times after cancel, want 0", doResumeCalls)
	}

	// A fresh trip after cancel can arm/notify again -- cancel must reset
	// unknownSent too, not just stop the timer.
	mgr.handleTrip("claude", nil)
	if !mgr.unknownSentFor("claude") {
		t.Error("handleTrip(nil) after cancel didn't set unknownSent -- cancel should reset it")
	}
}

// TestResumeTimerArmsAfterUnknownTripThenKnownResetsAt is the RBG-25 Gap 1
// regression: the stderr fallback trips first with no resetsAt (sends the
// "unknown" message), and a later trip for the same engine's pause episode
// does carry a real resetsAt. The timer must still arm -- an already-sent
// "unknown" message must not permanently block auto-resume -- and the
// "unknown" message must fire exactly once.
func TestResumeTimerArmsAfterUnknownTripThenKnownResetsAt(t *testing.T) {
	srv, alerts := capturingDiscordServer(t)
	defer srv.Close()

	cfg := &config.Config{DiscordUserID: "12345", UsageLimitResumeGraceSeconds: 0}
	notifier := &notify.Notifier{WebhookURL: srv.URL}

	var doResumeCalls int
	done := make(chan struct{}, 1)
	mgr := newResumeTimerMgr(cfg, notifier, func(string) ([]string, error) {
		doResumeCalls++
		done <- struct{}{}
		return nil, nil
	})

	mgr.handleTrip("claude", nil)
	got := waitForAlert(t, alerts)
	if !strings.Contains(got.content, "unknown") {
		t.Errorf("message %q missing \"unknown\" wording", got.content)
	}
	if !mgr.unknownSentFor("claude") {
		t.Fatal("unknownSent not set after the first (unknown) trip")
	}

	resetsAt := time.Now().Add(100 * time.Millisecond)
	mgr.handleTrip("claude", &resetsAt)

	if mgr.timerFor("claude") == nil {
		t.Fatal("handleTrip with a real resetsAt after an unknown trip didn't arm the timer")
	}
	if mgr.unknownSentFor("claude") {
		t.Error("unknownSent still true after the timer armed, want it cleared")
	}

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the auto-resume timer to fire")
	}
	if doResumeCalls != 1 {
		t.Errorf("doResume called %d times, want 1", doResumeCalls)
	}

	// Exactly one more alert -- the timer's own "reset passed, resuming"
	// message -- should have fired, not a second "unknown" message.
	resumeAlert := waitForAlert(t, alerts)
	if strings.Contains(resumeAlert.content, "unknown") {
		t.Errorf("second alert %q repeats the \"unknown\" message, want it sent at most once per pause episode", resumeAlert.content)
	}
	select {
	case a := <-alerts:
		t.Errorf("unexpected third alert sent: %q", a.content)
	default:
	}
}

// TestResumeTimerTwoEnginesArmIndependently is RBG-35's manager-level test
// gate: trips for two different engines must arm two separate timers, and
// resuming (cancelling) one must leave the other armed.
func TestResumeTimerTwoEnginesArmIndependently(t *testing.T) {
	srv, _ := capturingDiscordServer(t)
	defer srv.Close()

	cfg := &config.Config{DiscordUserID: "12345", UsageLimitResumeGraceSeconds: 0}
	notifier := &notify.Notifier{WebhookURL: srv.URL}
	mgr := newResumeTimerMgr(cfg, notifier, func(string) ([]string, error) { return nil, nil })

	resetsAt := time.Now().Add(time.Hour)
	mgr.handleTrip("claude", &resetsAt)
	mgr.handleTrip("other", &resetsAt)

	if mgr.timerFor("claude") == nil {
		t.Fatal("claude's timer was not armed")
	}
	if mgr.timerFor("other") == nil {
		t.Fatal("other's timer was not armed")
	}

	if !mgr.cancel("claude") {
		t.Fatal("cancel(\"claude\") = false, want true")
	}
	if mgr.timerFor("claude") != nil {
		t.Error("claude's timer still armed after cancel")
	}
	if mgr.timerFor("other") == nil {
		t.Error("other's timer was cancelled by cancelling claude, want it still armed")
	}

	mgr.cancel("other")
}
