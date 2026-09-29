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
// runOneWatchCycle can fire needs it, not just task-blocked ones.
func TestRunOneWatchCycleHealthCheckFailedMentionsBoth(t *testing.T) {
	badRunner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer badRunner.Close()

	discord, alerts := capturingDiscordServer(t)
	defer discord.Close()

	w := &watcher.Watcher{BaseURL: badRunner.URL, Vault: emptyVault(t), HealthHTTPTimeout: 2 * time.Second}
	notifier := &notify.Notifier{WebhookURL: discord.URL}

	runOneWatchCycle(w, notifier, watcherTestCfg())

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

	runOneWatchCycle(w, notifier, watcherTestCfg())

	got := waitForAlert(t, alerts)
	assertBothMentions(t, got.content)
	if !strings.Contains(got.content, "wedged") {
		t.Errorf("message %q missing wedged-task wording", got.content)
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

	runOneWatchCycle(w, notifier, watcherTestCfg())

	got := waitForAlert(t, alerts)
	assertBothMentions(t, got.content)
}
