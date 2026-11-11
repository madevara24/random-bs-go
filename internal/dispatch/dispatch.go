// Package dispatch implements the dispatch pass: sync the vault, scan
// Tasks/ for ready work, claim it atomically, and enqueue it for
// processing.
package dispatch

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/notify"
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

	// Engine is the coding engine's name, resolved once at claim time (see
	// EngineResolver) -- never written back to the note here; the note's
	// own `engine:` key is only ever written once the run actually starts
	// (internal/runner.ProcessTask), so until then it stays meaning
	// exactly "the task author pinned this."
	Engine string
}

// EngineResolver decides which engine codes repoKey's task, given the
// note's own `engine:` pin (noteEngine, "" if unset). Injected so this
// package never needs to import config (for RepoConfig) or runner (for
// the engine registry) -- the caller (cmd/rbg) closes over whatever
// it needs to apply the real repo/default precedence (see
// internal/engineresolve.Coding).
type EngineResolver func(repoKey, noteEngine string) string

// ResumeNotifyOptions carries the Discord routing values a dispatch pass
// needs when it has to report on a resumed task: the forum-channel webhook
// that owns the task's thread and the human to mention. Both empty is the
// valid "nothing to post to" default. It is a single struct rather than
// loose parameters so later units can add fields without churning
// RunDispatchPass's signature again.
//
// RunDispatchPass reads both fields in its resume check: ForumWebhookURL is
// where the refusal/resume messages are posted, into the task's own thread,
// and DiscordUserID is the human mentioned when a blocker_resolved note is
// refused for having an empty resolution. Empty ForumWebhookURL skips both
// posts entirely (the status writes still happen).
type ResumeNotifyOptions struct {
	ForumWebhookURL string
	DiscordUserID   string
}

// Enqueuer is how a claimed job reaches its repo's queue. Defined here
// (the caller side) rather than in the worker package, so this package
// doesn't need to import worker at all -- worker.RepoWorker satisfies this
// interface via its own Enqueue method, wired together in main.go.
type Enqueuer interface {
	Enqueue(repoKey string, job Job)
}

func claimableStatus(s string) bool {
	return s == "ready" || s == "blocker_resolved" || s == taskmeta.StatusPlanApproved || s == taskmeta.StatusPlanRejected
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
// paused for any engine) for any caller that doesn't wire one in. A usage
// limit belongs to one engine's account, so this never returns early on a
// global pause: it always syncs and claims. Instead, each candidate note's
// own resolved engine (via resolveEngine, same resolution a claimed Job's
// Engine field gets) is checked against p individually -- a note whose
// resolved engine is currently paused is skipped, left exactly as it is
// (ready stays ready, not queued), while a note resolved to any other,
// unpaused engine is claimed as usual. A skipped note is reconsidered on
// every later pass until its engine's pause clears.
//
// resolveEngine resolves the coding engine for each claimed job's Engine
// field -- nil is valid (every Job.Engine comes out "", same as before
// this field existed) for any caller that doesn't care. Resolution never
// writes `engine:` to the note itself -- see Job's doc comment.
//
// resumeNotify carries the Discord routing values for reporting on a
// refused or resumed task -- see ResumeNotifyOptions. Both posts are best
// effort: a failed post is only logged, never retried, and never undoes the
// status write that preceded it.
func RunDispatchPass(v *vaultgit.Vault, enq Enqueuer, p *pause.State, resolveEngine EngineResolver, resumeNotify ResumeNotifyOptions) error {
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
		engineName := resolveEngineFor(resolveEngine, repoKey, note)

		// The resume check runs before the pause check on purpose: a
		// blocker_resolved note with an empty resolution is refused
		// even while its engine is paused, so the human finds out now
		// rather than waiting for the pause to clear. Dispatch is the
		// last place that still sees blocker_resolved -- by the time the
		// runner starts, the claim write has already rewritten it to
		// queued.
		var resumed *resumeOutcome
		if note.Frontmatter.Status == "blocker_resolved" {
			outcome, checkErr := checkResume(v, relPath, slug)
			if checkErr != nil {
				fmt.Fprintf(os.Stderr, "[dispatch] resume check failed for %s: %v\n", relPath, checkErr)
				continue
			}
			if outcome.refused {
				postResumeRefusal(resumeNotify, slug, outcome.blockerN, outcome.threadID)
				continue
			}
			if outcome.resuming {
				resumed = &outcome
			}
		}

		if p.IsPaused(engineName) {
			// This note's engine is paused -- leave it exactly as it is
			// (ready/blocker_resolved, not queued) for a later pass to
			// pick up once that engine's pause clears. A different note
			// resolved to any other engine is unaffected.
			continue
		}

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

		enq.Enqueue(repoKey, Job{NotePath: relPath, Repo: repoKey, Slug: slug, Engine: engineName})

		if resumed != nil {
			postResumedNotice(resumeNotify, slug, resumed.blockerN, resumed.firstLine, resumed.threadID)
		}
	}

	return nil
}

// errResumeCheckSkip aborts the resume-check WriteNote without touching the
// note. vaultgit.WriteNote commits on every call (and a git commit with
// nothing staged is an error), so the "nothing to do" branches have to
// abort the write instead of returning nil.
var errResumeCheckSkip = errors.New("dispatch: resume check: no write needed")

// resumeOutcome is what the resume check learned from the fresh note.
// resuming is true only when the note is a blocker_resolved note with a
// non-zero blocker_round and a non-empty current resolution -- i.e. the
// claim is a genuine resume and gets a post-claim message.
type resumeOutcome struct {
	resuming  bool
	refused   bool
	blockerN  int
	firstLine string
	threadID  string
}

// checkResume re-reads relPath inside a dedicated WriteNote and applies the
// resume rule: a blocker_resolved note whose current blocker has no
// resolution is flipped back to blocked (blocker_round untouched) and
// logged as resume_refused, so it is not claimed. A blocker_resolved note
// with no blocker_round (blocked before numbered blockers existed) or with
// a filled resolution falls through to the normal claim. Every branch that
// needs no write aborts the WriteNote via errResumeCheckSkip.
func checkResume(v *vaultgit.Vault, relPath, slug string) (resumeOutcome, error) {
	var out resumeOutcome
	err := v.WriteNote(relPath, fmt.Sprintf("dispatch: resume check %s", slug), func(n *notetask.Note) error {
		// Re-read the fresh note: between scan and here someone may have
		// claimed or re-blocked it.
		if n.Frontmatter.Status != "blocker_resolved" {
			return errResumeCheckSkip
		}
		if n.Frontmatter.BlockerRound == 0 {
			// Blocked before numbered blockers existed -- no check to do.
			return errResumeCheckSkip
		}

		if resolution := notetask.CurrentBlockerResolution(n); resolution != "" {
			out.resuming = true
			out.blockerN = n.Frontmatter.BlockerRound
			out.firstLine = firstLineCut(resolution, 200)
			out.threadID = threadIDOf(n)
			return errResumeCheckSkip
		}

		n.Frontmatter.Status = "blocked"
		notetask.AppendRunnerLog(n,
			fmt.Sprintf("resume_refused (Blocker %d: empty resolution)", n.Frontmatter.BlockerRound),
			time.Now())
		out.refused = true
		out.blockerN = n.Frontmatter.BlockerRound
		out.threadID = threadIDOf(n)
		return nil
	})
	if errors.Is(err, errResumeCheckSkip) {
		return out, nil
	}
	if err != nil {
		return out, err
	}
	return out, nil
}

// postResumeRefusal tells the task's thread why a blocker_resolved flip was
// ignored. Best effort: skipped when there is no forum webhook or no thread
// to post into, and a failure is only logged, never retried.
func postResumeRefusal(opts ResumeNotifyOptions, slug string, blockerN int, threadID string) {
	if opts.ForumWebhookURL == "" || threadID == "" {
		return
	}
	msg := fmt.Sprintf("<@%s> `blocker_resolved` on `%s` ignored -- `#### Resolution` under `### Blocker %d` in `## Blockers` is empty. Write the resolution there, then flip it to `blocker_resolved` again.",
		opts.DiscordUserID, slug, blockerN)
	if _, err := notify.PostToThread(opts.ForumWebhookURL, threadID, msg, "", ""); err != nil {
		fmt.Fprintf(os.Stderr, "[dispatch] posting resume refusal for %s: %v\n", slug, err)
	}
}

// postResumedNotice tells the task's thread that its blocker is resolved
// and the task is running again. Same best-effort/skip rules as
// postResumeRefusal; no @-mention.
func postResumedNotice(opts ResumeNotifyOptions, slug string, blockerN int, firstLine, threadID string) {
	if opts.ForumWebhookURL == "" || threadID == "" {
		return
	}
	msg := fmt.Sprintf("Task `%s` resumed -- Blocker %d resolved: %s", slug, blockerN, firstLine)
	if _, err := notify.PostToThread(opts.ForumWebhookURL, threadID, msg, "", ""); err != nil {
		fmt.Fprintf(os.Stderr, "[dispatch] posting resume notice for %s: %v\n", slug, err)
	}
}

func threadIDOf(n *notetask.Note) string {
	if n.Frontmatter.DiscordThreadID == nil {
		return ""
	}
	return *n.Frontmatter.DiscordThreadID
}

// firstLineCut returns text's first line, trimmed, cut to at most limit
// runes (so a multi-byte character is never split).
func firstLineCut(text string, limit int) string {
	line := text
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = line[:i]
	}
	line = strings.TrimSpace(line)
	runes := []rune(line)
	if len(runes) > limit {
		return string(runes[:limit])
	}
	return line
}

// resolveEngineFor calls resolveEngine (if non-nil) with repoKey and
// note's own `engine:` pin, deref'd to "" if unset.
func resolveEngineFor(resolveEngine EngineResolver, repoKey string, note *notetask.Note) string {
	if resolveEngine == nil {
		return ""
	}
	var noteEngine string
	if note.Frontmatter.Engine != nil {
		noteEngine = *note.Frontmatter.Engine
	}
	return resolveEngine(repoKey, noteEngine)
}

// ReconcileQueued scans Tasks/ for notes already at status "queued" and
// re-populates each repo's queue by enqueuing them -- the startup-only
// counterpart to a restart emptying every in-memory queue. The note's own
// frontmatter is the only record of a queued job, so this must run once at
// boot, after a fresh Sync() and before the daemon starts accepting
// dispatch triggers. Does not write anything to the vault -- these notes
// are already claimed. resolveEngine resolves each enqueued Job's Engine
// field the same way RunDispatchPass does -- nil is valid, same meaning.
func ReconcileQueued(v *vaultgit.Vault, enq Enqueuer, resolveEngine EngineResolver) error {
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
		repoKey := note.Frontmatter.Repo
		enq.Enqueue(repoKey, Job{NotePath: relPath, Repo: repoKey, Slug: slug, Engine: resolveEngineFor(resolveEngine, repoKey, note)})
	}
	return nil
}
