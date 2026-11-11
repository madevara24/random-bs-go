package dispatch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/pause"
	"github.com/madevara24/random-bs-go/internal/taskmeta"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
)

// Same throwaway test vault used by internal/vaultgit's integration tests --
// never the real PM vault. See that package's comment for why.
var testVaultPath = testvault.Path

// TestSlugFromPathMatchesRealBranchConvention pins slugify against the
// exact shape of an already-existing real MDC task branch name, so the
// regression this fixes (an unsanitized filename passed straight through
// as a git branch name) can't silently come back.
func TestSlugFromPathMatchesRealBranchConvention(t *testing.T) {
	cases := []struct{ path, want string }{
		{"Tasks/(MDC) Add Project Zomboid Punchline.md", "mdc-add-project-zomboid-punchline"},
		{"Tasks/(MDC) PR35 Review Follow-up R2 (Fold ask TestMain, delete main_test.go).md",
			"mdc-pr35-review-follow-up-r2-fold-ask-testmain-delete-main_test-go"},
		{"Tasks/plain-slug-already.md", "plain-slug-already"},
	}
	for _, c := range cases {
		if got := taskmeta.SlugFromPath(c.path); got != c.want {
			t.Errorf("taskmeta.SlugFromPath(%q) = %q, want %q", c.path, got, c.want)
		}
	}
}

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
	if err := RunDispatchPass(v, enq, nil, nil, ResumeNotifyOptions{}); err != nil {
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

// TestRunDispatchPassAcceptsResumeNotifyOptions confirms a populated
// ResumeNotifyOptions is accepted and, in this unit, changes nothing: the
// note is still claimed and enqueued exactly as with the zero value (no
// Discord call is made from dispatch here -- there is no server to call).
func TestRunDispatchPassAcceptsResumeNotifyOptions(t *testing.T) {
	skipIfNoTestVault(t)
	t.Cleanup(testvault.Lock(t))
	v := vaultgit.New(testVaultPath, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/phase2-%s-resume-notify.md", run)
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "blocker_resolved", Repo: "repo-resume", Created: "2026-09-15",
	}, "Blocker resolved, resuming.")

	enq := &recordingEnqueuer{}
	opts := ResumeNotifyOptions{
		ForumWebhookURL: "https://discord.example/api/webhooks/forum",
		DiscordUserID:   "123456789",
	}
	if err := RunDispatchPass(v, enq, nil, nil, opts); err != nil {
		t.Fatalf("RunDispatchPass: %v", err)
	}

	if len(enq.calls) != 1 {
		t.Fatalf("enqueued %d jobs, want 1: %+v", len(enq.calls), enq.calls)
	}
	if got := enq.calls[0].repoKey; got != "repo-resume" {
		t.Errorf("enqueued repo = %q, want repo-resume", got)
	}
	if n := readNote(t, v.Path, relPath); n.Frontmatter.Status != "queued" {
		t.Errorf("status = %q, want queued", n.Frontmatter.Status)
	}
}

// TestRunDispatchPassSkipsClaimingOnlyThatNotesEngineWhilePaused covers
// RBG-35's per-engine pause: with "claude" paused, a ready note that
// resolves to "claude" must stay ready, not queued, while a ready note
// that resolves to a different (fake, test-only) engine "other" is
// claimed and enqueued as usual. This is what stops a usage-limit event on
// one engine from burning through the rest of the queue without also
// stopping every other engine's tasks dead.
func TestRunDispatchPassSkipsClaimingOnlyThatNotesEngineWhilePaused(t *testing.T) {
	skipIfNoTestVault(t)
	t.Cleanup(testvault.Lock(t))
	v := vaultgit.New(testVaultPath, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	claudePath := fmt.Sprintf("Tasks/phase2-paused-%s-claude.md", run)
	otherPath := fmt.Sprintf("Tasks/phase2-paused-%s-other.md", run)
	testvault.Seed(t, claudePath, notetask.Frontmatter{
		Status: "ready", Repo: "repo-paused", Created: "2026-09-10",
	}, "Ready while claude is paused.")
	testvault.Seed(t, otherPath, notetask.Frontmatter{
		Status: "ready", Repo: "repo-paused", Created: "2026-09-10", Engine: strp("other"),
	}, "Ready on a different engine while claude is paused.")

	// resolveEngine stands in for the real repo/DEFAULT_ENGINE precedence:
	// a note with no engine: pin resolves to "claude", exercised directly
	// in internal/engineresolve.
	resolveEngine := func(repoKey, noteEngine string) string {
		if noteEngine != "" {
			return noteEngine
		}
		return "claude"
	}

	var p pause.State
	resetsAt := time.Now().Add(time.Hour)
	p.Trip("claude", pause.ReasonUsageLimit, &resetsAt)

	enq := &recordingEnqueuer{}
	if err := RunDispatchPass(v, enq, &p, resolveEngine, ResumeNotifyOptions{}); err != nil {
		t.Fatalf("RunDispatchPass while claude paused: %v", err)
	}

	if len(enq.calls) != 1 {
		t.Fatalf("enqueued %d jobs while claude paused, want 1 (the \"other\"-engine note): %+v", len(enq.calls), enq.calls)
	}
	if enq.calls[0].job.Engine != "other" {
		t.Errorf("enqueued job engine = %q, want %q", enq.calls[0].job.Engine, "other")
	}

	n := readNote(t, v.Path, claudePath)
	if n.Frontmatter.Status != "ready" {
		t.Errorf("claude-engine note status = %q while claude paused, want unchanged %q", n.Frontmatter.Status, "ready")
	}
	n = readNote(t, v.Path, otherPath)
	if n.Frontmatter.Status != "queued" {
		t.Errorf("other-engine note status = %q, want %q (claimed despite claude being paused)", n.Frontmatter.Status, "queued")
	}

	// Once claude clears, the same note is claimed and enqueued normally --
	// proof this is a guard scoped to that engine, not a hidden rejection
	// of this particular note.
	p.Clear("claude")
	if err := RunDispatchPass(v, enq, &p, resolveEngine, ResumeNotifyOptions{}); err != nil {
		t.Fatalf("RunDispatchPass after resume: %v", err)
	}
	if len(enq.calls) != 2 {
		t.Fatalf("enqueued %d jobs after resume, want 2: %+v", len(enq.calls), enq.calls)
	}
	n = readNote(t, v.Path, claudePath)
	if n.Frontmatter.Status != "queued" {
		t.Errorf("claude-engine note status = %q after resume, want %q", n.Frontmatter.Status, "queued")
	}
}

// TestRunDispatchPassResolvesEngine confirms a claimed note gets
// `status: queued` with no `engine:` key written to the vault (dispatch
// never writes that key -- see Job's doc comment), while Job.Engine is set
// from the injected resolver: the note's own `engine:` pin when present,
// else whatever the resolver falls back to (standing in for the real
// repo/DEFAULT_ENGINE precedence, exercised directly in
// internal/engineresolve).
func TestRunDispatchPassResolvesEngine(t *testing.T) {
	skipIfNoTestVault(t)
	t.Cleanup(testvault.Lock(t))
	v := vaultgit.New(testVaultPath, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	pinnedPath := fmt.Sprintf("Tasks/phase34-%s-pinned.md", run)
	fallbackPath := fmt.Sprintf("Tasks/phase34-%s-fallback.md", run)

	testvault.Seed(t, pinnedPath, notetask.Frontmatter{
		Status: "ready", Repo: "repo-engine-pinned", Created: "2026-09-10", Engine: strp("pinned-engine"),
	}, "Pinned engine task.")
	testvault.Seed(t, fallbackPath, notetask.Frontmatter{
		Status: "ready", Repo: "repo-engine-fallback", Created: "2026-09-10",
	}, "Fallback engine task.")

	resolveEngine := func(repoKey, noteEngine string) string {
		if noteEngine != "" {
			return noteEngine
		}
		return "resolver-default-engine"
	}

	enq := &recordingEnqueuer{}
	if err := RunDispatchPass(v, enq, nil, resolveEngine, ResumeNotifyOptions{}); err != nil {
		t.Fatalf("RunDispatchPass: %v", err)
	}

	// The note itself never gets an `engine:` key written by dispatch.
	if n := readNote(t, v.Path, pinnedPath); n.Frontmatter.Status != "queued" {
		t.Errorf("pinned note status = %q, want queued", n.Frontmatter.Status)
	}
	if n := readNote(t, v.Path, fallbackPath); n.Frontmatter.Engine != nil {
		t.Errorf("fallback note Engine = %v, want nil (dispatch must never write engine:)", *n.Frontmatter.Engine)
	}

	var gotPinned, gotFallback string
	for _, c := range enq.calls {
		switch c.repoKey {
		case "repo-engine-pinned":
			gotPinned = c.job.Engine
		case "repo-engine-fallback":
			gotFallback = c.job.Engine
		}
	}
	if gotPinned != "pinned-engine" {
		t.Errorf("pinned job Engine = %q, want %q", gotPinned, "pinned-engine")
	}
	if gotFallback != "resolver-default-engine" {
		t.Errorf("fallback job Engine = %q, want %q", gotFallback, "resolver-default-engine")
	}
}

// TestDispatchClaimsPlanStatuses confirms plan_approved and plan_rejected
// are claimed (status flips to queued, enqueued) exactly like ready/
// blocker_resolved, while plan_review -- still waiting on a human
// reaction -- is left untouched.
func TestDispatchClaimsPlanStatuses(t *testing.T) {
	skipIfNoTestVault(t)
	t.Cleanup(testvault.Lock(t))
	v := vaultgit.New(testVaultPath, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	path := func(name string) string { return fmt.Sprintf("Tasks/planstatus-%s-%s.md", run, name) }

	testvault.Seed(t, path("approved"), notetask.Frontmatter{
		Status: "plan_approved", Repo: "repo-a", Created: "2026-11-01",
	}, "Approved plan.")
	testvault.Seed(t, path("rejected"), notetask.Frontmatter{
		Status: "plan_rejected", Repo: "repo-b", Created: "2026-11-01",
	}, "Rejected plan.")
	testvault.Seed(t, path("review"), notetask.Frontmatter{
		Status: "plan_review", Repo: "repo-c", Created: "2026-11-01",
	}, "Still waiting on a reaction.")

	enq := &recordingEnqueuer{}
	if err := RunDispatchPass(v, enq, nil, nil, ResumeNotifyOptions{}); err != nil {
		t.Fatalf("RunDispatchPass: %v", err)
	}

	if n := readNote(t, v.Path, path("approved")); n.Frontmatter.Status != "queued" {
		t.Errorf("approved note status = %q, want queued", n.Frontmatter.Status)
	}
	if n := readNote(t, v.Path, path("rejected")); n.Frontmatter.Status != "queued" {
		t.Errorf("rejected note status = %q, want queued", n.Frontmatter.Status)
	}
	if n := readNote(t, v.Path, path("review")); n.Frontmatter.Status != "plan_review" {
		t.Errorf("plan_review note status = %q, want it untouched", n.Frontmatter.Status)
	}

	var gotRepos []string
	for _, c := range enq.calls {
		gotRepos = append(gotRepos, c.repoKey)
	}
	if len(gotRepos) != 2 {
		t.Fatalf("enqueued %d jobs, want 2 (approved+rejected, not plan_review): %+v", len(gotRepos), gotRepos)
	}
}

func enqOrder(e *recordingEnqueuer) []string {
	var out []string
	for _, c := range e.calls {
		out = append(out, c.repoKey)
	}
	return out
}

// --- resume check ---------------------------------------------------------

const testForumThreadID = "thread-987654321"

// discordRecorder is a stand-in forum webhook: it records the content and
// thread_id of every PostToThread call and answers with a message id, which
// is all notify.PostToThread needs back.
type discordRecorder struct {
	mu        sync.Mutex
	contents  []string
	threadIDs []string
}

func newDiscordRecorder(t *testing.T) (*httptest.Server, *discordRecorder) {
	t.Helper()
	rec := &discordRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Errorf("parsing discord request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(r.FormValue("payload_json")), &payload); err != nil {
			t.Errorf("decoding payload_json: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rec.mu.Lock()
		rec.contents = append(rec.contents, payload.Content)
		rec.threadIDs = append(rec.threadIDs, r.URL.Query().Get("thread_id"))
		rec.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg-1"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

func (r *discordRecorder) snapshot() (contents, threadIDs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.contents...), append([]string(nil), r.threadIDs...)
}

func newResumeTestVault(t *testing.T) *vaultgit.Vault {
	t.Helper()
	skipIfNoTestVault(t)
	t.Cleanup(testvault.Lock(t))
	v := vaultgit.New(testVaultPath, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	return v
}

// seedBlockerNote seeds a note, then in one vault write gives it blocker
// round 1 and the requested status -- optionally filling the resolution.
// Going through the real WriteNote keeps the seeding identical to how the
// runner opens a blocker in production.
func seedBlockerNote(t *testing.T, v *vaultgit.Vault, relPath, status, resolution string) {
	t.Helper()
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "blocked", Repo: "repo-resume", Created: "2026-09-15",
		DiscordThreadID: strp(testForumThreadID),
	}, "Resume check fixture.")
	err := v.WriteNote(relPath, "test: open blocker for "+relPath, func(n *notetask.Note) error {
		n.Frontmatter.Status = status
		notetask.AppendBlocker(n, "test: blocked", time.Now())
		if resolution != "" && !notetask.FillCurrentBlockerResolution(n, resolution) {
			return fmt.Errorf("resolution did not fill")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("opening blocker for %s: %v", relPath, err)
	}
}

func hasRunnerLogEvent(n *notetask.Note, event string) bool {
	for _, e := range n.RunnerLog {
		if e.Event == event {
			return true
		}
	}
	return false
}

// TestRunDispatchPassRefusesEmptyResolution covers the core refusal: a
// blocker_resolved note whose current blocker has no resolution goes back
// to blocked (blocker_round untouched), records the resume_refused Runner
// Log line, posts once into the task's thread, and never enqueues a job.
func TestRunDispatchPassRefusesEmptyResolution(t *testing.T) {
	v := newResumeTestVault(t)
	srv, rec := newDiscordRecorder(t)

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/resume-%s-empty.md", run)
	seedBlockerNote(t, v, relPath, "blocker_resolved", "")

	enq := &recordingEnqueuer{}
	opts := ResumeNotifyOptions{ForumWebhookURL: srv.URL, DiscordUserID: "111222333"}
	if err := RunDispatchPass(v, enq, nil, nil, opts); err != nil {
		t.Fatalf("RunDispatchPass: %v", err)
	}

	n := readNote(t, v.Path, relPath)
	if n.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want blocked (refused resume)", n.Frontmatter.Status)
	}
	if n.Frontmatter.BlockerRound != 1 {
		t.Errorf("blocker_round = %d, want unchanged 1", n.Frontmatter.BlockerRound)
	}
	if !hasRunnerLogEvent(n, "resume_refused (Blocker 1: empty resolution)") {
		t.Errorf("missing resume_refused Runner Log line: %+v", n.RunnerLog)
	}
	if len(enq.calls) != 0 {
		t.Errorf("enqueued %d jobs, want 0: %+v", len(enq.calls), enq.calls)
	}

	contents, threadIDs := rec.snapshot()
	if len(contents) != 1 {
		t.Fatalf("posted %d messages, want 1: %v", len(contents), contents)
	}
	want := fmt.Sprintf("<@111222333> `blocker_resolved` on `%s` ignored -- `#### Resolution` under `### Blocker 1` in `## Blockers` is empty. Write the resolution there, then flip it to `blocker_resolved` again.",
		taskmeta.SlugFromPath(relPath))
	if contents[0] != want {
		t.Errorf("refusal post = %q, want %q", contents[0], want)
	}
	if threadIDs[0] != testForumThreadID {
		t.Errorf("refusal post thread_id = %q, want %q", threadIDs[0], testForumThreadID)
	}
}

// TestRunDispatchPassResumesFilledResolution covers the happy path: a
// blocker_resolved note with a filled resolution is claimed exactly as
// today and gets one post-claim resume message carrying the blocker number
// and the resolution's first line.
func TestRunDispatchPassResumesFilledResolution(t *testing.T) {
	v := newResumeTestVault(t)
	srv, rec := newDiscordRecorder(t)

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/resume-%s-filled.md", run)
	seedBlockerNote(t, v, relPath, "blocker_resolved", "Use the pooled client instead.\nA second line that must not appear.")

	enq := &recordingEnqueuer{}
	opts := ResumeNotifyOptions{ForumWebhookURL: srv.URL, DiscordUserID: "111222333"}
	if err := RunDispatchPass(v, enq, nil, nil, opts); err != nil {
		t.Fatalf("RunDispatchPass: %v", err)
	}

	n := readNote(t, v.Path, relPath)
	if n.Frontmatter.Status != "queued" {
		t.Errorf("status = %q, want queued", n.Frontmatter.Status)
	}
	if n.Frontmatter.BlockerRound != 1 {
		t.Errorf("blocker_round = %d, want unchanged 1", n.Frontmatter.BlockerRound)
	}
	if hasRunnerLogEvent(n, "resume_refused (Blocker 1: empty resolution)") {
		t.Errorf("filled resolution was refused: %+v", n.RunnerLog)
	}
	if len(enq.calls) != 1 {
		t.Fatalf("enqueued %d jobs, want 1: %+v", len(enq.calls), enq.calls)
	}

	contents, threadIDs := rec.snapshot()
	if len(contents) != 1 {
		t.Fatalf("posted %d messages, want 1: %v", len(contents), contents)
	}
	want := fmt.Sprintf("Task `%s` resumed -- Blocker 1 resolved: Use the pooled client instead.",
		taskmeta.SlugFromPath(relPath))
	if contents[0] != want {
		t.Errorf("resume post = %q, want %q", contents[0], want)
	}
	if strings.Contains(contents[0], "<@") {
		t.Errorf("resume post = %q, want no mention", contents[0])
	}
	if threadIDs[0] != testForumThreadID {
		t.Errorf("resume post thread_id = %q, want %q", threadIDs[0], testForumThreadID)
	}
}

// TestRunDispatchPassClaimsBlockerResolvedWithoutRound covers a note blocked
// before numbered blockers existed: no round means no check and no post, and
// the claim proceeds exactly as before this change.
func TestRunDispatchPassClaimsBlockerResolvedWithoutRound(t *testing.T) {
	v := newResumeTestVault(t)
	srv, rec := newDiscordRecorder(t)

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/resume-%s-legacy.md", run)
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "blocker_resolved", Repo: "repo-resume", Created: "2026-09-15",
		DiscordThreadID: strp(testForumThreadID),
	}, "Blocked before numbered blockers existed.")

	enq := &recordingEnqueuer{}
	opts := ResumeNotifyOptions{ForumWebhookURL: srv.URL, DiscordUserID: "111222333"}
	if err := RunDispatchPass(v, enq, nil, nil, opts); err != nil {
		t.Fatalf("RunDispatchPass: %v", err)
	}

	n := readNote(t, v.Path, relPath)
	if n.Frontmatter.Status != "queued" {
		t.Errorf("status = %q, want queued", n.Frontmatter.Status)
	}
	if n.Frontmatter.BlockerRound != 0 {
		t.Errorf("blocker_round = %d, want 0 (never written)", n.Frontmatter.BlockerRound)
	}
	if len(enq.calls) != 1 {
		t.Fatalf("enqueued %d jobs, want 1: %+v", len(enq.calls), enq.calls)
	}
	if contents, _ := rec.snapshot(); len(contents) != 0 {
		t.Errorf("posted %d messages, want 0: %v", len(contents), contents)
	}
}

// TestRunDispatchPassRefusesEmptyResolutionWhilePaused confirms the resume
// check runs before the paused-engine skip: the refusal does not wait for
// the engine's pause to clear, and still does not enqueue.
func TestRunDispatchPassRefusesEmptyResolutionWhilePaused(t *testing.T) {
	v := newResumeTestVault(t)
	srv, rec := newDiscordRecorder(t)

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/resume-%s-paused.md", run)
	seedBlockerNote(t, v, relPath, "blocker_resolved", "")

	resolveEngine := func(repoKey, noteEngine string) string { return "claude" }
	var p pause.State
	resetsAt := time.Now().Add(time.Hour)
	p.Trip("claude", pause.ReasonUsageLimit, &resetsAt)

	enq := &recordingEnqueuer{}
	opts := ResumeNotifyOptions{ForumWebhookURL: srv.URL, DiscordUserID: "111222333"}
	if err := RunDispatchPass(v, enq, &p, resolveEngine, opts); err != nil {
		t.Fatalf("RunDispatchPass while paused: %v", err)
	}

	n := readNote(t, v.Path, relPath)
	if n.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q while paused, want blocked (refused before the pause skip)", n.Frontmatter.Status)
	}
	if !hasRunnerLogEvent(n, "resume_refused (Blocker 1: empty resolution)") {
		t.Errorf("missing resume_refused Runner Log line while paused: %+v", n.RunnerLog)
	}
	if len(enq.calls) != 0 {
		t.Errorf("enqueued %d jobs while paused, want 0: %+v", len(enq.calls), enq.calls)
	}
	if contents, _ := rec.snapshot(); len(contents) != 1 {
		t.Errorf("posted %d messages, want 1 refusal post: %v", len(contents), contents)
	}
}

// TestRunDispatchPassRefusesEmptyResolutionWithoutWebhook confirms the
// refusal is a status write plus Runner Log line even when no Discord
// routing is configured: no HTTP post is attempted at all.
func TestRunDispatchPassRefusesEmptyResolutionWithoutWebhook(t *testing.T) {
	v := newResumeTestVault(t)
	_, rec := newDiscordRecorder(t)

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/resume-%s-nowebhook.md", run)
	seedBlockerNote(t, v, relPath, "blocker_resolved", "")

	enq := &recordingEnqueuer{}
	// srv.URL is deliberately not passed: an empty webhook URL means no post.
	opts := ResumeNotifyOptions{DiscordUserID: "111222333"}
	if err := RunDispatchPass(v, enq, nil, nil, opts); err != nil {
		t.Fatalf("RunDispatchPass: %v", err)
	}

	n := readNote(t, v.Path, relPath)
	if n.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want blocked", n.Frontmatter.Status)
	}
	if !hasRunnerLogEvent(n, "resume_refused (Blocker 1: empty resolution)") {
		t.Errorf("missing resume_refused Runner Log line: %+v", n.RunnerLog)
	}
	if len(enq.calls) != 0 {
		t.Errorf("enqueued %d jobs, want 0: %+v", len(enq.calls), enq.calls)
	}
	if contents, _ := rec.snapshot(); len(contents) != 0 {
		t.Errorf("posted %d messages with an empty webhook URL, want 0: %v", len(contents), contents)
	}
}

// TestRunDispatchPassReadyNotePostsNothing confirms the resume check never
// touches a ready note: claimed exactly as today, no resume/refusal post.
func TestRunDispatchPassReadyNotePostsNothing(t *testing.T) {
	v := newResumeTestVault(t)
	srv, rec := newDiscordRecorder(t)

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/resume-%s-ready.md", run)
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "ready", Repo: "repo-resume", Created: "2026-09-15",
		DiscordThreadID: strp(testForumThreadID),
	}, "Ready, not a resume.")

	enq := &recordingEnqueuer{}
	opts := ResumeNotifyOptions{ForumWebhookURL: srv.URL, DiscordUserID: "111222333"}
	if err := RunDispatchPass(v, enq, nil, nil, opts); err != nil {
		t.Fatalf("RunDispatchPass: %v", err)
	}

	n := readNote(t, v.Path, relPath)
	if n.Frontmatter.Status != "queued" {
		t.Errorf("status = %q, want queued", n.Frontmatter.Status)
	}
	if len(enq.calls) != 1 {
		t.Fatalf("enqueued %d jobs, want 1: %+v", len(enq.calls), enq.calls)
	}
	if contents, _ := rec.snapshot(); len(contents) != 0 {
		t.Errorf("posted %d messages for a ready note, want 0: %v", len(contents), contents)
	}
}

func TestFirstLineCut(t *testing.T) {
	cases := []struct {
		name  string
		in    string
		limit int
		want  string
	}{
		{"first line only", "one\ntwo", 200, "one"},
		{"trimmed", "  spaced  ", 200, "spaced"},
		{"cut to limit", strings.Repeat("a", 250), 200, strings.Repeat("a", 200)},
		{"multibyte not split", strings.Repeat("é", 250) + "\nignored", 200, strings.Repeat("é", 200)},
		{"empty", "", 200, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := firstLineCut(c.in, c.limit); got != c.want {
				t.Errorf("firstLineCut(%q, %d) = %q, want %q", c.in, c.limit, got, c.want)
			}
		})
	}
}
