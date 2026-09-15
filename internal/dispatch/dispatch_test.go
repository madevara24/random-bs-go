package dispatch

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
)

// Same throwaway test vault used by internal/vaultgit's integration tests --
// never the real PM vault. See that package's comment for why.
const testVaultPath = testvault.Path

func skipIfNoTestVault(t *testing.T) {
	t.Helper()
	testvault.SkipIfAbsent(t)
}

func strp(s string) *string { return &s }

type recordingEnqueuer struct {
	mu    sync.Mutex
	calls []struct {
		repoKey string
		job     Job
	}
}

func (r *recordingEnqueuer) Enqueue(repoKey string, job Job) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, struct {
		repoKey string
		job     Job
	}{repoKey, job})
}

func readNote(t *testing.T, vaultPath, relPath string) *notetask.Note {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(vaultPath, relPath))
	if err != nil {
		t.Fatalf("reading %s: %v", relPath, err)
	}
	n, err := notetask.Parse(data)
	if err != nil {
		t.Fatalf("parsing %s: %v", relPath, err)
	}
	return n
}

// TestRunDispatchPass seeds notes in every state and confirms one pass
// claims only ready/blocker_resolved, in the right order, stamps ready_at
// only where it was empty, and leaves everything else untouched -- exactly
// Phase 2's test gate in Design - Implementation.md.
func TestRunDispatchPass(t *testing.T) {
	skipIfNoTestVault(t)
	t.Cleanup(testvault.Lock(t))
	v := vaultgit.New(testVaultPath, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	path := func(name string) string { return fmt.Sprintf("Tasks/phase2-%s-%s.md", run, name) }

	// draft: never touched.
	testvault.Seed(t, path("draft"), notetask.Frontmatter{
		Status: "draft", Repo: "test-repo", Created: "2026-09-15",
	}, "A draft, not ready yet.")

	// queued: already claimed by an earlier pass, must not be re-claimed or re-enqueued.
	testvault.Seed(t, path("queued"), notetask.Frontmatter{
		Status: "queued", Repo: "test-repo", Created: "2026-09-01", ReadyAt: strp("2026-09-02T00:00:00Z"),
	}, "Already queued.")

	// done: terminal, never touched.
	testvault.Seed(t, path("done"), notetask.Frontmatter{
		Status: "done", Repo: "test-repo", Created: "2026-08-01",
	}, "Already done.")

	// ready, no ready_at, created 2026-09-10 -- should be stamped + claimed.
	testvault.Seed(t, path("ready-a"), notetask.Frontmatter{
		Status: "ready", Repo: "repo-a", Created: "2026-09-10",
	}, "Ready A.")

	// ready, no ready_at, created 2026-09-05 (older) -- same pass, ties with A on
	// the stamped ready_at, so created tie-break should put this before A.
	testvault.Seed(t, path("ready-b"), notetask.Frontmatter{
		Status: "ready", Repo: "repo-b", Created: "2026-09-05",
	}, "Ready B.")

	// blocker_resolved, no ready_at, created 2026-09-01 (oldest of the tied
	// group) -- claimable exactly like ready, should sort before B and A.
	testvault.Seed(t, path("resolved-c"), notetask.Frontmatter{
		Status: "blocker_resolved", Repo: "repo-c", Created: "2026-09-01",
	}, "Resolved C.")

	// ready with a pre-existing ready_at earlier than "now" -- must NOT be
	// overwritten, and should sort first (earliest ready_at of the batch).
	testvault.Seed(t, path("ready-f-preexisting"), notetask.Frontmatter{
		Status: "ready", Repo: "repo-f", Created: "2026-09-14", ReadyAt: strp("2026-09-14T00:00:00Z"),
	}, "Ready F, already stamped.")

	enq := &recordingEnqueuer{}
	if err := RunDispatchPass(v, enq); err != nil {
		t.Fatalf("RunDispatchPass: %v", err)
	}

	// draft/queued/done: untouched.
	if n := readNote(t, v.Path, path("draft")); n.Frontmatter.Status != "draft" || n.Frontmatter.ReadyAt != nil {
		t.Errorf("draft note was touched: status=%q ready_at=%v", n.Frontmatter.Status, n.Frontmatter.ReadyAt)
	}
	if n := readNote(t, v.Path, path("queued")); n.Frontmatter.Status != "queued" || *n.Frontmatter.ReadyAt != "2026-09-02T00:00:00Z" {
		t.Errorf("already-queued note was touched: status=%q ready_at=%v", n.Frontmatter.Status, n.Frontmatter.ReadyAt)
	}
	if n := readNote(t, v.Path, path("done")); n.Frontmatter.Status != "done" {
		t.Errorf("done note was touched: status=%q", n.Frontmatter.Status)
	}

	// ready-f: ready_at NOT overwritten.
	if n := readNote(t, v.Path, path("ready-f-preexisting")); n.Frontmatter.Status != "queued" {
		t.Errorf("ready-f status = %q, want queued", n.Frontmatter.Status)
	} else if n.Frontmatter.ReadyAt == nil || *n.Frontmatter.ReadyAt != "2026-09-14T00:00:00Z" {
		t.Errorf("ready-f ready_at = %v, want unchanged 2026-09-14T00:00:00Z", n.Frontmatter.ReadyAt)
	}

	// ready-a/b, resolved-c: claimed to queued, ready_at freshly stamped (non-empty).
	for _, name := range []string{"ready-a", "ready-b", "resolved-c"} {
		n := readNote(t, v.Path, path(name))
		if n.Frontmatter.Status != "queued" {
			t.Errorf("%s status = %q, want queued", name, n.Frontmatter.Status)
		}
		if n.Frontmatter.ReadyAt == nil || *n.Frontmatter.ReadyAt == "" {
			t.Errorf("%s ready_at was not stamped", name)
		}
	}

	// Enqueue order: ready-f (pre-existing earliest ready_at), then
	// resolved-c, ready-b, ready-a (same freshly-stamped ready_at, tie-break
	// oldest created first).
	if len(enq.calls) != 4 {
		t.Fatalf("enqueued %d jobs, want 4: %+v", len(enq.calls), enq.calls)
	}
	wantOrder := []string{"repo-f", "repo-c", "repo-b", "repo-a"}
	for i, want := range wantOrder {
		if enq.calls[i].repoKey != want {
			t.Errorf("enqueue order[%d] = %q, want %q (full order: %v)", i, enq.calls[i].repoKey, want, enqOrder(enq))
		}
	}
}

func enqOrder(e *recordingEnqueuer) []string {
	var out []string
	for _, c := range e.calls {
		out = append(out, c.repoKey)
	}
	return out
}
