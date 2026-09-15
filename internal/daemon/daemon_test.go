package daemon

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

func strp(s string) *string { return &s }

// newRecordingWorkers builds a worker.Workers map for the given repo keys,
// each backed by a stub ProcessFunc that just records which slugs it was
// called with -- standing in for the real runner.ProcessTask, which
// doesn't exist until Phase 6.
func newRecordingWorkers(repoKeys ...string) (worker.Workers, *sync.Mutex, *[]string) {
	var mu sync.Mutex
	var processed []string
	globalSlots := worker.NewGlobalSlots(2)
	ws := worker.Workers{}
	for _, key := range repoKeys {
		process := func(job worker.Job) error {
			mu.Lock()
			processed = append(processed, job.Slug)
			mu.Unlock()
			time.Sleep(10 * time.Millisecond)
			return nil
		}
		ws[key] = worker.New(key, globalSlots, process)
	}
	return ws, &mu, &processed
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.After(timeout)
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if cond() {
				return
			}
		case <-deadline:
			t.Fatal("timed out waiting for condition")
		}
	}
}

// TestBootReconcilesAlreadyQueuedNotes seeds a "queued" note with no
// corresponding in-memory job (simulating a restart), boots the daemon's
// internals directly (no HTTP), and confirms it's picked up and processed
// by the stub task without a fresh dispatch pass -- exactly Phase 4's first
// test gate.
func TestBootReconcilesAlreadyQueuedNotes(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	v := vaultgit.New(testvault.Path, "master")
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	repoKey := "phase4-repo-" + run
	relPath := fmt.Sprintf("Tasks/phase4-%s-restart.md", run)

	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: repoKey, Created: "2026-09-15", ReadyAt: strp("2026-09-15T09:00:00Z"),
	}, "Already queued from a prior run.")

	ws, mu, processedPtr := newRecordingWorkers(repoKey)
	ws.StartAll()

	r := NewRunner(v, ws)
	if err := r.Boot(); err != nil {
		t.Fatalf("Boot: %v", err)
	}

	wantSlug := fmt.Sprintf("phase4-%s-restart", run)

	// Boot alone (sync + reconcile), no RunDispatchPass call, must be
	// enough for the stub task to actually run.
	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, s := range *processedPtr {
			if s == wantSlug {
				return true
			}
		}
		return false
	})
}

// TestDispatchPassFlowsThroughToStubProcessing seeds several ready notes
// and confirms one dispatch pass takes them all the way through to the
// stub processTask actually running -- Phase 4's second test gate.
func TestDispatchPassFlowsThroughToStubProcessing(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	v := vaultgit.New(testvault.Path, "master")
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	repoKey := "phase4-repo-b-" + run

	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	var relPaths []string
	for i := 0; i < 3; i++ {
		rp := fmt.Sprintf("Tasks/phase4-%s-ready-%d.md", run, i)
		relPaths = append(relPaths, rp)
		testvault.Seed(t, rp, notetask.Frontmatter{
			Status: "ready", Repo: repoKey, Created: "2026-09-15",
		}, fmt.Sprintf("Ready task %d.", i))
	}

	ws, mu, processedPtr := newRecordingWorkers(repoKey)
	ws.StartAll()

	r := NewRunner(v, ws)
	if err := r.Boot(); err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if err := r.RunDispatchPass(); err != nil {
		t.Fatalf("RunDispatchPass: %v", err)
	}

	waitFor(t, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(*processedPtr) == 3
	})

	mu.Lock()
	defer mu.Unlock()
	if len(*processedPtr) != 3 {
		t.Errorf("processed %d jobs, want 3: %v", len(*processedPtr), *processedPtr)
	}
}
