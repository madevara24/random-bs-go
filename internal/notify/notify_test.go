package notify

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
)

func seedNote(t *testing.T, relPath string) {
	t.Helper()
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "in_progress", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Notify test task.")
}

// TestSendRetriesExactlyOnceThenNotifyFailed points at a webhook that
// always fails and confirms exactly one retry happens (not zero, not
// unbounded), landing "notify_failed" in the Runner Log -- Phase 10's
// first test gate.
func TestSendRetriesExactlyOnceThenNotifyFailed(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	var hitCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/phase10-fail-%s.md", run)
	seedNote(t, relPath)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	n := &Notifier{Vault: v, WebhookURL: srv.URL}
	n.sendSync(relPath, "test message", "test.md", "test attachment body")

	if got := hitCount.Load(); got != 2 {
		t.Errorf("webhook hit %d times, want exactly 2 (1 attempt + 1 retry)", got)
	}

	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	found := false
	for _, e := range note.RunnerLog {
		if e.Event == "notify_failed" {
			found = true
		}
		if e.Event == "notified" {
			t.Error("Runner Log has \"notified\" but every attempt failed -- should be \"notify_failed\"")
		}
	}
	if !found {
		t.Errorf("Runner Log missing \"notify_failed\": %+v", note.RunnerLog)
	}
}

// TestSendSucceedsFirstTryNotified points at a webhook that succeeds
// immediately and confirms exactly one attempt (no retry needed) and
// "notified" lands in the Runner Log.
func TestSendSucceedsFirstTryNotified(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	var hitCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/phase10-ok-%s.md", run)
	seedNote(t, relPath)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	n := &Notifier{Vault: v, WebhookURL: srv.URL}
	n.sendSync(relPath, "test message", "test.md", "test attachment body")

	if got := hitCount.Load(); got != 1 {
		t.Errorf("webhook hit %d times, want exactly 1 (no retry needed)", got)
	}

	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	found := false
	for _, e := range note.RunnerLog {
		if e.Event == "notified" {
			found = true
		}
	}
	if !found {
		t.Errorf("Runner Log missing \"notified\": %+v", note.RunnerLog)
	}
}

// TestSendAgainstRealWebhook is Phase 10's second test gate: point it at
// the real webhook, confirm "notified" lands. Gated behind an env var
// since it fires a real (clearly-marked test) message into #dev.
func TestSendAgainstRealWebhook(t *testing.T) {
	if os.Getenv("PMRUNNER_SEND_REAL_DISCORD_TEST") == "" {
		t.Skip("set PMRUNNER_SEND_REAL_DISCORD_TEST=1 to actually fire this against the real Discord webhook")
	}
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	webhookURL, err := ReadWebhookURLFromEnvFile("/home/obsidian/pm-runner-go/.env")
	if err != nil {
		t.Fatalf("reading real webhook URL: %v", err)
	}

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/phase10-real-%s.md", run)
	seedNote(t, relPath)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	n := &Notifier{Vault: v, WebhookURL: webhookURL}
	n.sendSync(relPath,
		"[PM RUNNER GO REWRITE -- TEST FIRE, NOT A REAL TASK ALERT] Phase 10 notify.Send test.",
		"phase10-test.md", "# Phase 10 test\n\nThis is a disposable test fire from the Go rewrite's test suite, not a real task alert.\n")

	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	found := false
	for _, e := range note.RunnerLog {
		if e.Event == "notified" {
			found = true
		}
	}
	if !found {
		t.Errorf("Runner Log missing \"notified\" after a real send: %+v", note.RunnerLog)
	}
}

// TestSendReturnsImmediately confirms Send is genuinely fire-and-forget --
// even against a slow/hanging webhook, the call itself returns fast.
func TestSendReturnsImmediately(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-block
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := &Notifier{WebhookURL: srv.URL}
	start := time.Now()
	n.Send("", "msg", "a.md", "body")
	if elapsed := time.Since(start); elapsed > 100*time.Millisecond {
		t.Errorf("Send blocked for %v, want it to return almost immediately", elapsed)
	}
}

// TestInvokeHermesFiresWithSlugAndReason confirms a blocked task's hermes
// -z call actually fires, with the right slug/reason text -- a stubbed
// hermes binary is fine per Phase 10's test gate, this just confirms it
// gets invoked correctly.
func TestInvokeHermesFiresWithSlugAndReason(t *testing.T) {
	tmpDir := t.TempDir()
	stubPath := filepath.Join(tmpDir, "fake-hermes.sh")
	outPath := filepath.Join(tmpDir, "hermes-argv.txt")

	script := fmt.Sprintf("#!/usr/bin/env bash\nprintf '%%s\\n' \"$@\" > %q\n", outPath)
	if err := os.WriteFile(stubPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}

	n := &Notifier{HermesCmd: []string{stubPath, "-z"}}
	n.invokeHermes("phase10-slug-123", "needs a decision about the API design")

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("reading hermes stub output: %v", err)
	}
	got := string(data)
	if !strings.Contains(got, "-z") {
		t.Errorf("hermes argv missing -z flag: %q", got)
	}
	if !strings.Contains(got, "phase10-slug-123") {
		t.Errorf("hermes argv missing task slug: %q", got)
	}
	if !strings.Contains(got, "needs a decision about the API design") {
		t.Errorf("hermes argv missing reason text: %q", got)
	}
}
