package main

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

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/notify"
	"github.com/madevara24/random-bs-go/internal/runner"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// TestBuildWorkersWiresOnPanicAndOnError is this package's wiring test: for
// every configured repo, buildWorkers must hand back a RepoWorker whose
// OnPanic and OnError are both non-nil -- OnError already defaults to a
// non-nil stub in worker.New, but OnPanic didn't, until this task wired
// newPanicHandler in. A production regression here means a panic inside
// ProcessTask goes back to worker.New's log-only default: no blocked
// write, no Discord alert.
func TestBuildWorkersWiresOnPanicAndOnError(t *testing.T) {
	cfg := &config.Config{
		Repos: map[string]config.RepoConfig{
			"repo-a": {Path: "/tmp/repo-a", DefaultBranch: "main"},
			"repo-b": {Path: "/tmp/repo-b", DefaultBranch: "main"},
		},
	}

	globalSlots := worker.NewGlobalSlots(1)
	onPanic := func(worker.Job, any) {}
	workers := buildWorkers(cfg, globalSlots, runner.Deps{}, onPanic)

	if len(workers) != len(cfg.Repos) {
		t.Fatalf("got %d workers, want %d", len(workers), len(cfg.Repos))
	}
	for key, rw := range workers {
		if rw.OnPanic == nil {
			t.Errorf("worker %q: OnPanic is nil", key)
		}
		if rw.OnError == nil {
			t.Errorf("worker %q: OnError is nil", key)
		}
	}
}

// TestPanicHandlerWritesBlockedAndAlerts forces a panic in a stub Process
// function through a real RepoWorker and confirms newPanicHandler's
// callback -- the thing runRunner wires into RepoWorker.OnPanic -- writes
// status: blocked (with the recovered panic value in the Work Log) and
// fires the Discord alert, same as worker.New's previous log-only default
// silently failed to do.
func TestPanicHandlerWritesBlockedAndAlerts(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	var hitCount atomic.Int32
	discord := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer discord.Close()

	tmpDir := t.TempDir()
	stubHermes := filepath.Join(tmpDir, "fake-hermes.sh")
	if err := os.WriteFile(stubHermes, []byte("#!/usr/bin/env bash\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("writing hermes stub: %v", err)
	}

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "onpanic-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "in_progress", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "OnPanic wiring test task -- Process below panics unconditionally.")

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	// Deliberately no Vault on the notifier: SendBlocked's own async
	// follow-up write (appending "notified" to the Runner Log) goes through
	// a raw, unsynchronized git call in testvault's cleanup once this test
	// returns, and racing that fire-and-forget goroutine's git commit/push
	// against cleanup's git rm is exactly the kind of flake this test
	// doesn't need -- the blocked write below (the thing this test actually
	// verifies) happens synchronously, before SendBlocked is even called.
	notifier := &notify.Notifier{WebhookURL: discord.URL, HermesCmd: []string{stubHermes}}

	globalSlots := worker.NewGlobalSlots(1)
	rw := worker.New("phase6-test-repo", globalSlots, func(job worker.Job) error {
		panic("boom: simulated ProcessTask panic")
	})
	rw.OnPanic = newPanicHandler(v, notifier, "12345")

	go rw.Run()
	rw.Enqueue(worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug})

	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
waitLoop:
	for {
		select {
		case <-tick.C:
			if hitCount.Load() > 0 {
				break waitLoop
			}
		case <-deadline:
			t.Fatal("timed out waiting for the Discord alert to fire")
		}
	}

	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", note.Frontmatter.Status, "blocked")
	}
	if !note.HasWorkLog || !strings.Contains(note.WorkLog, "boom: simulated ProcessTask panic") {
		t.Errorf("Work Log missing the recovered panic value: %q", note.WorkLog)
	}
}
