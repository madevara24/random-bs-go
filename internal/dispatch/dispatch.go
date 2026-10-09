// Package dispatch implements the dispatch pass: sync the vault, scan
// Tasks/ for ready work, claim it atomically, and enqueue it for
// processing.
package dispatch

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/pause"
	"github.com/madevara24/random-bs-go/internal/taskmeta"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
)

// Job is what a claimed task hands off for processing. NotePath is
// vault-relative (e.g. "Tasks/foo.md").
type Job struct {
	NotePath string
	Repo     string
	Slug     string
}

// Enqueuer is how a claimed job reaches its repo's queue. Defined here
// (the caller side) rather than in the worker package, so this package
// doesn't need to import worker at all -- worker.RepoWorker satisfies this
// interface via its own Enqueue method, wired together in main.go.
type Enqueuer interface {
	Enqueue(repoKey string, job Job)
}

func claimableStatus(s string) bool {
	return s == "ready" || s == "blocker_resolved"
}

type scannedNote struct {
	relPath string
	note    *notetask.Note
}

func scanTasks(vaultPath string) ([]scannedNote, error) {
	pattern := filepath.Join(vaultPath, taskmeta.TasksDir, "*.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("dispatch: scanning %s: %w", pattern, err)
	}
	var out []scannedNote
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			return nil, fmt.Errorf("dispatch: reading %s: %w", m, err)
		}
		note, err := notetask.Parse(data)
		if err != nil {
			// A single malformed note shouldn't take down the whole pass --
			// skip it, loudly, and keep going.
			fmt.Fprintf(os.Stderr, "[dispatch] skipping unparseable note %s: %v\n", m, err)
			continue
		}
		relPath, err := filepath.Rel(vaultPath, m)
		if err != nil {
			return nil, fmt.Errorf("dispatch: relativizing %s: %w", m, err)
		}
		out = append(out, scannedNote{relPath: relPath, note: note})
	}
	return out, nil
}

// RunDispatchPass syncs the vault, scans Tasks/ for ready/blocker_resolved
// notes, stamps ready_at where empty, claims in oldest-ready_at-first
// (tie-break oldest-created) order, and enqueues each claimed job. This
// function has no internal concurrency guard -- it must run inside one
// dedicated goroutine at a time, with wake-channel coalescing handled one
// layer up in the daemon boot code, so that only one pass ever runs at a
// time.
//
// p is the shared pipeline-pause flag -- nil is valid (treated as never
// paused) for any caller that doesn't wire one in. While paused, this
// returns immediately before syncing or claiming anything: every
// ready/blocker_resolved note stays exactly as it is (ready stays ready,
// not queued) until a human or the auto-resume timer clears the pause.
func RunDispatchPass(v *vaultgit.Vault, enq Enqueuer, p *pause.State) error {
	if p.IsPaused() {
		return nil
	}
	if err := v.Sync(); err != nil {
		return fmt.Errorf("dispatch: sync: %w", err)
	}

	scanned, err := scanTasks(v.Path)
	if err != nil {
		return err
	}

	var candidates []scannedNote
	for _, s := range scanned {
		if claimableStatus(s.note.Frontmatter.Status) {
			candidates = append(candidates, s)
		}
	}
	if len(candidates) == 0 {
		return nil
	}

	// Stamp ready_at where empty -- one "now" for the whole pass, so ties
	// within a single pass fall through to the created tie-break rather
	// than being ordered by microsecond luck.
	now := time.Now().UTC().Format(time.RFC3339)
	for i := range candidates {
		if candidates[i].note.Frontmatter.ReadyAt != nil && *candidates[i].note.Frontmatter.ReadyAt != "" {
			continue
		}
		relPath := candidates[i].relPath
		stampedNow := now
		err := v.WriteNote(relPath, fmt.Sprintf("dispatch: stamp ready_at for %s", taskmeta.SlugFromPath(relPath)), func(n *notetask.Note) error {
			if n.Frontmatter.ReadyAt != nil && *n.Frontmatter.ReadyAt != "" {
				return nil // someone else stamped it between scan and write; leave it
			}
			n.Frontmatter.ReadyAt = &stampedNow
			return nil
		})
		if err != nil {
			return fmt.Errorf("dispatch: stamping ready_at for %s: %w", relPath, err)
		}
		candidates[i].note.Frontmatter.ReadyAt = &stampedNow
	}

	notesOnly := make([]*notetask.Note, len(candidates))
	for i := range candidates {
		notesOnly[i] = candidates[i].note
	}
	notetask.SortByReadyThenCreated(notesOnly)
	// Re-derive the ordered relPath list from the sorted note pointers,
	// since SortByReadyThenCreated only reorders the *Note slice.
	relPathOf := map[*notetask.Note]string{}
	for _, c := range candidates {
		relPathOf[c.note] = c.relPath
	}

	for _, note := range notesOnly {
		relPath := relPathOf[note]
		slug := taskmeta.SlugFromPath(relPath)
		repoKey := note.Frontmatter.Repo

		claimErr := v.WriteNote(relPath, fmt.Sprintf("dispatch: claim %s", slug), func(n *notetask.Note) error {
			if !claimableStatus(n.Frontmatter.Status) {
				return fmt.Errorf("no longer claimable (status is now %q)", n.Frontmatter.Status)
			}
			n.Frontmatter.Status = "queued"
			return nil
		})
		if claimErr != nil {
			fmt.Fprintf(os.Stderr, "[dispatch] failed to claim %s: %v\n", relPath, claimErr)
			continue
		}

		enq.Enqueue(repoKey, Job{NotePath: relPath, Repo: repoKey, Slug: slug})
	}

	return nil
}

// ReconcileQueued scans Tasks/ for notes already at status "queued" and
// re-populates each repo's queue by enqueuing them -- the startup-only
// counterpart to a restart emptying every in-memory queue. The note's own
// frontmatter is the only record of a queued job, so this must run once at
// boot, after a fresh Sync() and before the daemon starts accepting
// dispatch triggers. Does not write anything to the vault -- these notes
// are already claimed.
func ReconcileQueued(v *vaultgit.Vault, enq Enqueuer) error {
	scanned, err := scanTasks(v.Path)
	if err != nil {
		return err
	}

	var queuedNotes []scannedNote
	for _, s := range scanned {
		if s.note.Frontmatter.Status == "queued" {
			queuedNotes = append(queuedNotes, s)
		}
	}

	notesOnly := make([]*notetask.Note, len(queuedNotes))
	for i := range queuedNotes {
		notesOnly[i] = queuedNotes[i].note
	}
	notetask.SortByReadyThenCreated(notesOnly)
	relPathOf := map[*notetask.Note]string{}
	for _, q := range queuedNotes {
		relPathOf[q.note] = q.relPath
	}

	for _, note := range notesOnly {
		relPath := relPathOf[note]
		slug := taskmeta.SlugFromPath(relPath)
		enq.Enqueue(note.Frontmatter.Repo, Job{NotePath: relPath, Repo: note.Frontmatter.Repo, Slug: slug})
	}
	return nil
}
