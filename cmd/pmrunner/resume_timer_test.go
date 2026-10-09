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
// message, and calls doResume exactly once.
func TestResumeTimerFiresAndResumesAfterResetsAt(t *testing.T) {
	srv, alerts := capturingDiscordServer(t)
	defer srv.Close()

	cfg := &config.Config{DiscordUserID: "12345", UsageLimitResumeGraceSeconds: 0}
	notifier := &notify.Notifier{WebhookURL: srv.URL}

	var doResumeCalls int
	done := make(chan struct{}, 1)
	mgr := newResumeTimerMgr(cfg, notifier, func() ([]string, error) {
		doResumeCalls++
		done <- struct{}{}
		return nil, nil
	})

	resetsAt := time.Now().Add(100 * time.Millisecond)
	mgr.handleTrip(&resetsAt)

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for the auto-resume timer to fire")
	}

	if doResumeCalls != 1 {
		t.Errorf("doResume called %d times, want 1", doResumeCalls)
	}

	got := waitForAlert(t, alerts)
	if !strings.Contains(got.content, "<@12345>") {
		t.Errorf("message %q missing Devara mention", got.content)
	}
	if !strings.Contains(got.content, "reset") {
		t.Errorf("message %q missing reset wording", got.content)
	}
}

// TestResumeTimerHandleTripIsIdempotentWhileArmed confirms concurrent/
// repeated handleTrip calls (several tasks hitting the same global limit
// around the same time) don't arm a second timer.
func TestResumeTimerHandleTripIsIdempotentWhileArmed(t *testing.T) {
	srv, _ := capturingDiscordServer(t)
	defer srv.Close()

	cfg := &config.Config{DiscordUserID: "12345", UsageLimitResumeGraceSeconds: 0}
	notifier := &notify.Notifier{WebhookURL: srv.URL}

	mgr := newResumeTimerMgr(cfg, notifier, func() ([]string, error) { return nil, nil })

	resetsAt := time.Now().Add(time.Hour)
	mgr.handleTrip(&resetsAt)
	firstTimer := mgr.timer
	mgr.handleTrip(&resetsAt)
	if mgr.timer != firstTimer {
		t.Error("handleTrip armed a second timer while the first was still outstanding")
	}
	mgr.cancel()
}

// TestResumeTimerUnknownResetsAtSendsMessageNotTimer confirms a nil/past
// resetsAt skips the timer entirely and sends the "reset time unknown,
// manual resume needed" message instead.
func TestResumeTimerUnknownResetsAtSendsMessageNotTimer(t *testing.T) {
	srv, alerts := capturingDiscordServer(t)
	defer srv.Close()

	cfg := &config.Config{DiscordUserID: "12345"}
	notifier := &notify.Notifier{WebhookURL: srv.URL}
	var doResumeCalls int
	mgr := newResumeTimerMgr(cfg, notifier, func() ([]string, error) {
		doResumeCalls++
		return nil, nil
	})

	mgr.handleTrip(nil)

	got := waitForAlert(t, alerts)
	if !strings.Contains(got.content, "unknown") {
		t.Errorf("message %q missing \"unknown\" wording", got.content)
	}
	if !strings.Contains(got.content, "<@12345>") {
		t.Errorf("message %q missing Devara mention", got.content)
	}
	if mgr.timer != nil {
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
	mgr := newResumeTimerMgr(cfg, notifier, func() ([]string, error) {
		doResumeCalls++
		return nil, nil
	})

	resetsAt := time.Now().Add(200 * time.Millisecond)
	mgr.handleTrip(&resetsAt)
	if !mgr.cancel() {
		t.Fatal("cancel() = false, want true (an armed timer was stopped)")
	}

	time.Sleep(500 * time.Millisecond)
	if doResumeCalls != 0 {
		t.Errorf("doResume called %d times after cancel, want 0", doResumeCalls)
	}

	// A fresh trip after cancel can arm/notify again -- cancel must reset
	// unknownSent too, not just stop the timer.
	mgr.handleTrip(nil)
	if !mgr.unknownSent {
		t.Error("handleTrip(nil) after cancel didn't set unknownSent -- cancel should reset it")
	}
}
