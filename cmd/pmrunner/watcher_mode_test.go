package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/notify"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/watcher"
)

func watcherTestCfg() *config.Config {
	return &config.Config{DiscordUserID: "12345", DiscordAraDevUserID: "67890"}
}

func assertBothMentions(t *testing.T, content string) {
	t.Helper()
	if !strings.Contains(content, "<@12345>") {
		t.Errorf("message %q missing Devara mention", content)
	}
	if !strings.Contains(content, "<@67890>") {
		t.Errorf("message %q missing Ara-Dev mention", content)
	}
}

// emptyVault points a *vaultgit.Vault at a fresh temp directory with no
// Tasks/ subdirectory -- enough for ScanForUnnotified (which
// runOneWatchCycle always calls, regardless of which branch above it fired)
// to find nothing, without pulling in the real git-backed throwaway test
// vault these mention-wiring tests have no other need for.
func emptyVault(t *testing.T) *vaultgit.Vault {
	t.Helper()
	return vaultgit.New(t.TempDir(), "master")
}

// TestRunOneWatchCycleHealthCheckFailedMentionsBoth confirms the Tier 1
// /health-check-failed alert mentions both Devara and Ara-Dev -- the `dev`
// gateway auto-threads its reply off its own mention, so every alert shape
// runOneWatchCycle can fire needs it, not just task-blocked ones. It has to
// drive the cycle healthAlertThreshold times since a single failed cycle no
// longer alerts on its own.
func TestRunOneWatchCycleHealthCheckFailedMentionsBoth(t *testing.T) {
	badRunner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badRunner.Close()

	discord, alerts := capturingDiscordServer(t)
	defer discord.Close()

	w := &watcher.Watcher{BaseURL: badRunner.URL, Vault: emptyVault(t), HealthHTTPTimeout: 2 * time.Second}
	notifier := &notify.Notifier{WebhookURL: discord.URL}
	healthAlerts := &healthAlertDebouncer{}

	for i := 0; i < healthAlertThreshold; i++ {
		runOneWatchCycle(w, notifier, watcherTestCfg(), healthAlerts)
	}

	got := waitForAlert(t, alerts)
	assertBothMentions(t, got.content)
	if !strings.Contains(got.content, "health check failed") {
		t.Errorf("message %q missing health-check wording", got.content)
	}
}

// TestRunOneWatchCycleStaleTaskMentionsBoth confirms the Tier 2
// wedged-session alert mentions both Devara and Ara-Dev.
func TestRunOneWatchCycleStaleTaskMentionsBoth(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/status/tasks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]watcher.TaskStatus{
			"repo-a": {
				Slug:           "task-1",
				Repo:           "repo-a",
				StartedAt:      time.Now().Add(-1 * time.Hour),
				LastActivityAt: time.Now().Add(-1 * time.Hour),
				Stage:          "invocation",
			},
		})
	})
	runnerSrv := httptest.NewServer(mux)
	defer runnerSrv.Close()

	discord, alerts := capturingDiscordServer(t)
	defer discord.Close()

	w := &watcher.Watcher{
		BaseURL:           runnerSrv.URL,
		Vault:             emptyVault(t),
		IdleTimeout:       time.Millisecond,
		HealthHTTPTimeout: 2 * time.Second,
		StatusHTTPTimeout: 2 * time.Second,
	}
	notifier := &notify.Notifier{WebhookURL: discord.URL}

	runOneWatchCycle(w, notifier, watcherTestCfg(), &healthAlertDebouncer{})

	got := waitForAlert(t, alerts)
	assertBothMentions(t, got.content)
	if !strings.Contains(got.content, "wedged") {
		t.Errorf("message %q missing wedged-task wording", got.content)
	}
}

// TestRunOneWatchCycleStaleTaskRoutesIntoThread confirms the wedged-session
// alert routes into the task's own Discord thread (via ForumWebhookURL,
// ?thread_id=) when /status/tasks reports a discord_thread_id, not the
// plain top-level webhook.
func TestRunOneWatchCycleStaleTaskRoutesIntoThread(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/status/tasks", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]watcher.TaskStatus{
			"repo-a": {
				Slug:            "task-1",
				Repo:            "repo-a",
				StartedAt:       time.Now().Add(-1 * time.Hour),
				LastActivityAt:  time.Now().Add(-1 * time.Hour),
				Stage:           "invocation",
				DiscordThreadID: "wedged-thread-456",
			},
		})
	})
	runnerSrv := httptest.NewServer(mux)
	defer runnerSrv.Close()

	var forumQuery string
	forum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forumQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer forum.Close()

	topLevelHit := false
	topLevel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topLevelHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer topLevel.Close()

	w := &watcher.Watcher{
		BaseURL:           runnerSrv.URL,
		Vault:             emptyVault(t),
		IdleTimeout:       time.Millisecond,
		HealthHTTPTimeout: 2 * time.Second,
		StatusHTTPTimeout: 2 * time.Second,
	}
	notifier := &notify.Notifier{WebhookURL: topLevel.URL, ForumWebhookURL: forum.URL}

	runOneWatchCycle(w, notifier, watcherTestCfg(), &healthAlertDebouncer{})

	deadline := time.Now().Add(2 * time.Second)
	for forumQuery == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(forumQuery, "thread_id=wedged-thread-456") {
		t.Errorf("forum query = %q, want it to contain thread_id=wedged-thread-456", forumQuery)
	}
	if topLevelHit {
		t.Error("stale-task alert hit the top-level webhook, want only the forum webhook")
	}
}

// TestRunOneWatchCycleUnnotifiedNoteMentionsBoth confirms the gap-3
// lost-notification alert mentions both Devara and Ara-Dev.
func TestRunOneWatchCycleUnnotifiedNoteMentionsBoth(t *testing.T) {
	okRunner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer okRunner.Close()

	discord, alerts := capturingDiscordServer(t)
	defer discord.Close()

	vaultDir := t.TempDir()
	tasksDir := filepath.Join(vaultDir, "Tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir Tasks: %v", err)
	}
	note := &notetask.Note{
		Frontmatter: notetask.Frontmatter{Status: "done", Repo: "repo-a", Created: "2026-09-15"},
		Prompt:      "Completed, notify never ran.",
	}
	out, err := note.Bytes()
	if err != nil {
		t.Fatalf("serializing note: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "task-1.md"), out, 0o644); err != nil {
		t.Fatalf("writing note: %v", err)
	}

	w := &watcher.Watcher{
		BaseURL:           okRunner.URL,
		Vault:             vaultgit.New(vaultDir, "master"),
		HealthHTTPTimeout: 2 * time.Second,
		StatusHTTPTimeout: 2 * time.Second,
	}
	notifier := &notify.Notifier{WebhookURL: discord.URL}

	runOneWatchCycle(w, notifier, watcherTestCfg(), &healthAlertDebouncer{})

	got := waitForAlert(t, alerts)
	assertBothMentions(t, got.content)
}

// TestRunOneWatchCycleUnnotifiedNoteRoutesIntoThread confirms the gap-3
// lost-notification alert routes into the task's own Discord thread when
// the note has a discord_thread_id -- not the plain top-level webhook.
func TestRunOneWatchCycleUnnotifiedNoteRoutesIntoThread(t *testing.T) {
	okRunner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer okRunner.Close()

	var forumQuery string
	forum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forumQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer forum.Close()

	topLevelHit := false
	topLevel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topLevelHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer topLevel.Close()

	vaultDir := t.TempDir()
	tasksDir := filepath.Join(vaultDir, "Tasks")
	if err := os.MkdirAll(tasksDir, 0o755); err != nil {
		t.Fatalf("mkdir Tasks: %v", err)
	}
	threadID := "unnotified-thread-321"
	note := &notetask.Note{
		Frontmatter: notetask.Frontmatter{Status: "done", Repo: "repo-a", Created: "2026-09-15", DiscordThreadID: &threadID},
		Prompt:      "Completed, notify never ran, but it does have a thread.",
	}
	out, err := note.Bytes()
	if err != nil {
		t.Fatalf("serializing note: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tasksDir, "task-1.md"), out, 0o644); err != nil {
		t.Fatalf("writing note: %v", err)
	}

	w := &watcher.Watcher{
		BaseURL:           okRunner.URL,
		Vault:             vaultgit.New(vaultDir, "master"),
		HealthHTTPTimeout: 2 * time.Second,
		StatusHTTPTimeout: 2 * time.Second,
	}
	notifier := &notify.Notifier{WebhookURL: topLevel.URL, ForumWebhookURL: forum.URL}

	runOneWatchCycle(w, notifier, watcherTestCfg(), &healthAlertDebouncer{})

	deadline := time.Now().Add(2 * time.Second)
	for forumQuery == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(forumQuery, "thread_id=unnotified-thread-321") {
		t.Errorf("forum query = %q, want it to contain thread_id=unnotified-thread-321", forumQuery)
	}
	if topLevelHit {
		t.Error("unnotified-note alert hit the top-level webhook, want only the forum webhook")
	}
}

// TestHealthAlertDebouncer covers the debounce contract runOneWatchCycle
// relies on: a lone failure stays quiet, healthAlertThreshold consecutive
// failures alert exactly once, and a success in between resets the streak
// so the next alert again needs a full healthAlertThreshold run.
func TestHealthAlertDebouncer(t *testing.T) {
	d := &healthAlertDebouncer{}

	if d.Observe(watcher.HealthConnectionRefused) {
		t.Fatal("a single failure must not alert")
	}
	if d.Observe(watcher.HealthConnectionRefused) {
		t.Fatal("two consecutive failures must not alert (threshold is 3)")
	}
	if !d.Observe(watcher.HealthConnectionRefused) {
		t.Fatal("the 3rd consecutive failure must alert")
	}
	if d.Observe(watcher.HealthConnectionRefused) {
		t.Fatal("a 4th consecutive failure must not alert again on its own")
	}

	if d.Observe(watcher.HealthOK) {
		t.Fatal("HealthOK must never alert")
	}

	if d.Observe(watcher.HealthConnectionRefused) {
		t.Fatal("after a reset, 1 failure must not alert")
	}
	if d.Observe(watcher.HealthConnectionRefused) {
		t.Fatal("after a reset, 2 failures must not alert")
	}
	if !d.Observe(watcher.HealthConnectionRefused) {
		t.Fatal("after a reset, the 3rd consecutive failure must alert again")
	}
}

// TestRunOneWatchCycleHealthCheckFailedDoesNotAlertBelowThreshold confirms
// runOneWatchCycle stays quiet on the webhook through the first
// healthAlertThreshold-1 failed cycles -- this is the deploy-restart race
// the debounce exists for.
func TestRunOneWatchCycleHealthCheckFailedDoesNotAlertBelowThreshold(t *testing.T) {
	badRunner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badRunner.Close()

	discord, alerts := capturingDiscordServer(t)
	defer discord.Close()

	w := &watcher.Watcher{BaseURL: badRunner.URL, Vault: emptyVault(t), HealthHTTPTimeout: 2 * time.Second}
	notifier := &notify.Notifier{WebhookURL: discord.URL}
	healthAlerts := &healthAlertDebouncer{}

	for i := 0; i < healthAlertThreshold-1; i++ {
		runOneWatchCycle(w, notifier, watcherTestCfg(), healthAlerts)
	}

	select {
	case got := <-alerts:
		t.Fatalf("unexpected alert before threshold reached: %q", got.content)
	case <-time.After(200 * time.Millisecond):
	}
}
