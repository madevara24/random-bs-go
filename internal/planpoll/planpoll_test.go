package planpoll

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/plangate"
	"github.com/madevara24/random-bs-go/internal/taskmeta"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
)

// TestDecide pins Decide against the Verify section's own table: ❌ always
// needs this round's own feedback already written before it counts as a
// rejection, ✅ alone approves, and anything else (including ✅+❌ with no
// feedback yet) is no change.
func TestDecide(t *testing.T) {
	cases := []struct {
		name       string
		reactions  map[string]int
		review     string
		wantStatus string
		wantChange bool
	}{
		{"approve", map[string]int{"✅": 1}, "", taskmeta.StatusPlanApproved, true},
		{"reject no feedback yet", map[string]int{"❌": 1}, "", "", false},
		{"reject with feedback", map[string]int{"❌": 1}, "use X", taskmeta.StatusPlanRejected, true},
		{"both no feedback", map[string]int{"✅": 1, "❌": 1}, "", "", false},
		{"both with feedback", map[string]int{"✅": 1, "❌": 1}, "use X", taskmeta.StatusPlanRejected, true},
		{"unrelated emoji", map[string]int{"👍": 1}, "", "", false},
		{"no reactions", map[string]int{}, "", "", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStatus, gotChanged := Decide(c.reactions, c.review)
			if gotStatus != c.wantStatus || gotChanged != c.wantChange {
				t.Errorf("Decide(%+v, %q) = (%q, %v), want (%q, %v)", c.reactions, c.review, gotStatus, gotChanged, c.wantStatus, c.wantChange)
			}
		})
	}
}

// waitForStatus polls relPath until its status is want or the timeout
// elapses, returning the last read note either way.
func waitForStatus(t *testing.T, v *vaultgit.Vault, relPath, want string, timeout time.Duration) *notetask.Note {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var note *notetask.Note
	for time.Now().Before(deadline) {
		n, err := v.ReadNote(relPath)
		if err == nil {
			note = n
			if n.Frontmatter.Status == want {
				return n
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return note
}

// planRepostServer stands in for PostToThread's target and records each
// posted message's content on the returned buffered channel.
func planRepostServer(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	messages := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(r.FormValue("payload_json")), &payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		messages <- payload.Content
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"repost-1"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, messages
}

// TestRetryMessageMatchesStopForPlanReviewForPersistedReasons confirms the
// retry path renders a note's plan_gate_reasons exactly as stopForPlanReview
// does -- same mention, same category headings, same bullets -- rather than
// falling back to the Runner Log when the field is present. The stop path's
// own exact text for these reasons is pinned by internal/runner's
// TestPlanReviewStopMessageAndGroupedReasons; this compares the retry against
// the same renderer call that test exercises.
func TestRetryMessageMatchesStopForPlanReviewForPersistedReasons(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "planpoll-reasons-" + run
	repo := "phase6-test-repo"
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	threadID := "thread-reasons"
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "plan_review", Repo: repo, Created: "2026-11-06",
		DiscordThreadID: &threadID, PlanRound: 2,
		PlanGateReasons: map[string][]string{
			"Hard rules":        {`unit "U1" touches "db/migrations/0003.sql", which matches hard-stop pattern "migrations/"`},
			"Jev classifier":    {"risky=0.90 is a clear yes"},
			"Planner questions": {"approach=none"},
			"Errors":            {`missing answer for "ambiguous"`},
		},
	}, "Do the thing.")

	if err := v.WriteNote(relPath, "test: seed stale gate line "+slug, func(n *notetask.Note) error {
		n.HasPlan = true
		n.Plan = "Two units.\n\n```yaml\n- id: U1\n  title: Do it\n  verify: \"true\"\n```\n"
		notetask.AppendRunnerLog(n, "plan_gate stale=should-not-render stop=true", time.Now())
		return nil
	}); err != nil {
		t.Fatalf("seeding stale gate line: %v", err)
	}
	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading seeded note: %v", err)
	}

	discordSrv, messages := planRepostServer(t)
	retryMissingPlanMessage(v, discordSrv.URL, "12345", relPath, note, threadID, newMissingMessageTracker())

	var got string
	select {
	case got = <-messages:
	case <-time.After(5 * time.Second):
		t.Fatal("no plan-review message was reposted")
	}

	stopReasons := []plangate.Reason{
		{Category: plangate.CategoryHardRules, Text: `unit "U1" touches "db/migrations/0003.sql", which matches hard-stop pattern "migrations/"`},
		{Category: plangate.CategoryJevClassifier, Text: "risky=0.90 is a clear yes"},
		{Category: plangate.CategoryPlannerQuestions, Text: "approach=none"},
		{Category: plangate.CategoryErrors, Text: `missing answer for "ambiguous"`},
	}
	want := plangate.RenderPlanReviewMessage("<@12345>", slug, repo, 2, stopReasons)
	if got != want {
		t.Errorf("retry message =\n%s\n\nstopForPlanReview message =\n%s", got, want)
	}
	if strings.Contains(got, "stale=should-not-render") {
		t.Errorf("retry fell back to the Runner Log despite plan_gate_reasons being present:\n%s", got)
	}
}

// TestRetryFallsBackToRunnerLogUnderErrors confirms a note that reached
// plan_review before plan_gate_reasons existed still reposts its latest
// plan_gate Runner Log line, rendered as one bullet under **Errors**.
func TestRetryFallsBackToRunnerLogUnderErrors(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "planpoll-fallback-" + run
	repo := "phase6-test-repo"
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	threadID := "thread-fallback"
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "plan_review", Repo: repo, Created: "2026-11-06",
		DiscordThreadID: &threadID, PlanRound: 1,
	}, "Do the thing.")

	line := "plan_gate risky=0.90 ambiguous=0.10 stop=true"
	if err := v.WriteNote(relPath, "test: seed gate line "+slug, func(n *notetask.Note) error {
		n.HasPlan = true
		n.Plan = "Two units.\n\n```yaml\n- id: U1\n  title: Do it\n  verify: \"true\"\n```\n"
		notetask.AppendRunnerLog(n, line, time.Now())
		return nil
	}); err != nil {
		t.Fatalf("seeding gate line: %v", err)
	}
	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading seeded note: %v", err)
	}
	if note.Frontmatter.PlanGateReasons != nil {
		t.Fatalf("PlanGateReasons = %#v, want nil for the fallback case", note.Frontmatter.PlanGateReasons)
	}

	discordSrv, messages := planRepostServer(t)
	retryMissingPlanMessage(v, discordSrv.URL, "12345", relPath, note, threadID, newMissingMessageTracker())

	var got string
	select {
	case got = <-messages:
	case <-time.After(5 * time.Second):
		t.Fatal("no plan-review message was reposted")
	}

	want := plangate.RenderPlanReviewMessage("<@12345>", slug, repo, 1, []plangate.Reason{
		{Category: plangate.CategoryErrors, Text: line},
	})
	if got != want {
		t.Errorf("retry message =\n%s\n\nwant Runner-Log fallback =\n%s", got, want)
	}
	if !strings.Contains(got, "**Errors**\n- "+line+"\n") {
		t.Errorf("Runner Log line not rendered as an Errors bullet:\n%s", got)
	}
	if strings.Contains(got, "**Hard rules**") || strings.Contains(got, "**Jev classifier**") || strings.Contains(got, "**Planner questions**") {
		t.Errorf("fallback rendered categories beyond Errors:\n%s", got)
	}
}

// decisionReplyRecorder stands in for both FetchReactions (GET, returning
// emoji as the message's only reaction) and PostToThread (POST) on one
// httptest server, recording every posted content string.
type decisionReplyRecorder struct {
	mu       sync.Mutex
	contents []string
}

func (r *decisionReplyRecorder) posted() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.contents...)
}

func decisionReplyServer(t *testing.T, emoji string, postStatus int) (*httptest.Server, *decisionReplyRecorder) {
	return reactionReplyServer(t, postStatus, emoji)
}

// reactionReplyServer is decisionReplyServer generalized to more than one
// reaction, so a test can pin the ✅+❌-with-no-feedback case.
func reactionReplyServer(t *testing.T, postStatus int, emojis ...string) (*httptest.Server, *decisionReplyRecorder) {
	t.Helper()
	rec := &decisionReplyRecorder{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			quoted := make([]string, 0, len(emojis))
			for _, emoji := range emojis {
				quoted = append(quoted, fmt.Sprintf(`{"emoji":{"name":%q},"count":1}`, emoji))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `{"reactions":[%s]}`, strings.Join(quoted, ","))
			return
		}
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(r.FormValue("payload_json")), &payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		rec.mu.Lock()
		rec.contents = append(rec.contents, payload.Content)
		rec.mu.Unlock()
		if postStatus < 200 || postStatus >= 300 {
			http.Error(w, "webhook down", postStatus)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"reply-1"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, rec
}

// seedPlanReviewNote seeds a plan_review note with a plan message id (so
// processNote polls reactions rather than reposting), optionally writing a
// round's review feedback, and returns the vault, relative path and parsed
// note processNote expects.
func seedPlanReviewNote(t *testing.T, slug, threadID, messageID string, round int, review string) (*vaultgit.Vault, string, *notetask.Note) {
	t.Helper()
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "plan_review", Repo: "phase6-test-repo", Created: "2026-11-06",
		DiscordThreadID: &threadID, PlanMessageID: &messageID, PlanRound: round,
	}, "Do the thing.")

	if review != "" {
		if err := v.WriteNote(relPath, "test: seed review "+slug, func(n *notetask.Note) error {
			n.HasPlanReview = true
			n.PlanReview = fmt.Sprintf("### Round %d\n%s\n", round, review)
			return nil
		}); err != nil {
			t.Fatalf("seeding review: %v", err)
		}
	}

	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading seeded note: %v", err)
	}
	return v, relPath, note
}

// TestProcessNoteApprovedPostsOneReply confirms a recorded ✅ writes
// plan_approved and posts exactly one acknowledgement into the task's
// thread, mentioning the user and naming the round.
func TestProcessNoteApprovedPostsOneReply(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	slug := fmt.Sprintf("planpoll-approved-reply-%d", time.Now().UnixNano())
	v, relPath, note := seedPlanReviewNote(t, slug, "thread-approved-reply", "msg-approved-reply", 2, "")

	srv, rec := decisionReplyServer(t, "✅", http.StatusOK)
	var wakes atomic.Int32
	processNote(v, srv.URL, "user-approved", func() { wakes.Add(1) }, newMissingMessageTracker(), relPath, note)

	posts := rec.posted()
	if len(posts) != 1 {
		t.Fatalf("posted %d messages, want exactly 1: %#v", len(posts), posts)
	}
	want := fmt.Sprintf("<@user-approved> Plan for `%s` approved -- round 2 recorded, the unit loop starts on the next claim.", slug)
	if posts[0] != want {
		t.Errorf("approved reply =\n%s\n\nwant =\n%s", posts[0], want)
	}

	updated, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note after processNote: %v", err)
	}
	if updated.Frontmatter.Status != taskmeta.StatusPlanApproved {
		t.Errorf("status = %q, want %q", updated.Frontmatter.Status, taskmeta.StatusPlanApproved)
	}
	if got := wakes.Load(); got != 1 {
		t.Errorf("wake called %d times, want 1", got)
	}
}

// TestProcessNoteRejectedPostsOneReply confirms a recorded ❌ (with this
// round's feedback written) writes plan_rejected and posts exactly one
// acknowledgement, mentioning the user and naming the round.
func TestProcessNoteRejectedPostsOneReply(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	slug := fmt.Sprintf("planpoll-rejected-reply-%d", time.Now().UnixNano())
	v, relPath, note := seedPlanReviewNote(t, slug, "thread-rejected-reply", "msg-rejected-reply", 1, "Use X instead.")

	srv, rec := decisionReplyServer(t, "❌", http.StatusOK)
	var wakes atomic.Int32
	processNote(v, srv.URL, "user-rejected", func() { wakes.Add(1) }, newMissingMessageTracker(), relPath, note)

	posts := rec.posted()
	if len(posts) != 1 {
		t.Fatalf("posted %d messages, want exactly 1: %#v", len(posts), posts)
	}
	want := fmt.Sprintf("<@user-rejected> Plan for `%s` rejected -- round 1 feedback recorded, the planner will replan on the next claim.", slug)
	if posts[0] != want {
		t.Errorf("rejected reply =\n%s\n\nwant =\n%s", posts[0], want)
	}

	updated, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note after processNote: %v", err)
	}
	if updated.Frontmatter.Status != taskmeta.StatusPlanRejected {
		t.Errorf("status = %q, want %q", updated.Frontmatter.Status, taskmeta.StatusPlanRejected)
	}
	if got := wakes.Load(); got != 1 {
		t.Errorf("wake called %d times, want 1", got)
	}
}

// TestProcessNoteReplyFailureKeepsStatusWrite confirms a webhook failure on
// the acknowledgement is only logged: the status write stands, the attempt
// happens once (no retry loop), and the task is still woken.
func TestProcessNoteReplyFailureKeepsStatusWrite(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	slug := fmt.Sprintf("planpoll-reply-fail-%d", time.Now().UnixNano())
	v, relPath, note := seedPlanReviewNote(t, slug, "thread-reply-fail", "msg-reply-fail", 1, "Use X instead.")

	srv, rec := decisionReplyServer(t, "❌", http.StatusInternalServerError)
	var wakes atomic.Int32
	processNote(v, srv.URL, "user-reply-fail", func() { wakes.Add(1) }, newMissingMessageTracker(), relPath, note)

	if posts := rec.posted(); len(posts) != 1 {
		t.Errorf("posted %d attempts, want exactly 1 (never retried)", len(posts))
	}

	updated, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note after processNote: %v", err)
	}
	if updated.Frontmatter.Status != taskmeta.StatusPlanRejected {
		t.Errorf("status = %q, want %q even though the reply failed", updated.Frontmatter.Status, taskmeta.StatusPlanRejected)
	}
	if got := wakes.Load(); got != 1 {
		t.Errorf("wake called %d times, want 1", got)
	}
}

// TestProcessNoteEmptyRejectWarnsOnce covers the Verify table's first two
// planpoll cases: the first tick with a ❌ and an empty round review posts
// the warning and appends the persisted log line, and a second tick -- with
// a fresh tracker, standing in for a runner restart -- posts nothing.
func TestProcessNoteEmptyRejectWarnsOnce(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	slug := fmt.Sprintf("planpoll-empty-reject-%d", time.Now().UnixNano())
	v, relPath, note := seedPlanReviewNote(t, slug, "thread-empty-reject", "msg-empty-reject", 3, "")

	srv, rec := decisionReplyServer(t, "❌", http.StatusOK)
	var wakes atomic.Int32

	processNote(v, srv.URL, "user-empty-reject", func() { wakes.Add(1) }, newMissingMessageTracker(), relPath, note)

	posts := rec.posted()
	if len(posts) != 1 {
		t.Fatalf("posted %d messages on the first tick, want exactly 1: %#v", len(posts), posts)
	}
	want := fmt.Sprintf("<@user-empty-reject> ❌ on `%s` round 3 ignored -- `### Round 3` under `## Plan Review` is empty. Write your feedback there; the poller picks it up within a minute. The plan will not be redone on empty feedback.", slug)
	if posts[0] != want {
		t.Errorf("empty-feedback warning =\n%s\n\nwant =\n%s", posts[0], want)
	}

	updated, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note after first tick: %v", err)
	}
	if updated.Frontmatter.Status != taskmeta.StatusPlanReview {
		t.Errorf("status = %q, want %q (empty feedback is a no-op)", updated.Frontmatter.Status, taskmeta.StatusPlanReview)
	}
	if got := countRunnerLogEvent(updated, "plan_reject_empty (round 3)"); got != 1 {
		t.Errorf("Runner Log has %d %q lines, want 1: %+v", got, "plan_reject_empty (round 3)", updated.RunnerLog)
	}
	if got := wakes.Load(); got != 0 {
		t.Errorf("wake called %d times, want 0 for an empty-feedback ❌", got)
	}

	// A fresh tracker is what a restarted runner has; the persisted Runner
	// Log line must be enough to keep the second tick silent.
	processNote(v, srv.URL, "user-empty-reject", func() { wakes.Add(1) }, newMissingMessageTracker(), relPath, updated)
	if posts := rec.posted(); len(posts) != 1 {
		t.Errorf("posted %d messages after a second tick/restart, want still 1: %#v", len(posts), posts)
	}
	if got := wakes.Load(); got != 0 {
		t.Errorf("wake called %d times after the second tick, want 0", got)
	}
}

// TestProcessNoteEmptyRejectWithApproveStillWarns confirms the ✅+❌ with no
// feedback case keeps Decide's unchanged outcome (no status write, no
// wake) while still getting the one-time warning.
func TestProcessNoteEmptyRejectWithApproveStillWarns(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	slug := fmt.Sprintf("planpoll-empty-both-%d", time.Now().UnixNano())
	v, relPath, note := seedPlanReviewNote(t, slug, "thread-empty-both", "msg-empty-both", 2, "")

	srv, rec := reactionReplyServer(t, http.StatusOK, "✅", "❌")
	var wakes atomic.Int32

	processNote(v, srv.URL, "user-empty-both", func() { wakes.Add(1) }, newMissingMessageTracker(), relPath, note)

	posts := rec.posted()
	if len(posts) != 1 {
		t.Fatalf("posted %d messages, want exactly 1: %#v", len(posts), posts)
	}
	want := fmt.Sprintf("<@user-empty-both> ❌ on `%s` round 2 ignored -- `### Round 2` under `## Plan Review` is empty. Write your feedback there; the poller picks it up within a minute. The plan will not be redone on empty feedback.", slug)
	if posts[0] != want {
		t.Errorf("empty-feedback warning =\n%s\n\nwant =\n%s", posts[0], want)
	}

	updated, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note after processNote: %v", err)
	}
	if updated.Frontmatter.Status != taskmeta.StatusPlanReview {
		t.Errorf("status = %q, want unchanged %q when ✅ and ❌ both present without feedback", updated.Frontmatter.Status, taskmeta.StatusPlanReview)
	}
	if got := countRunnerLogEvent(updated, "plan_reject_empty (round 2)"); got != 1 {
		t.Errorf("Runner Log has %d %q lines, want 1: %+v", got, "plan_reject_empty (round 2)", updated.RunnerLog)
	}
	if got := wakes.Load(); got != 0 {
		t.Errorf("wake called %d times, want 0", got)
	}

	processNote(v, srv.URL, "user-empty-both", func() { wakes.Add(1) }, newMissingMessageTracker(), relPath, updated)
	if posts := rec.posted(); len(posts) != 1 {
		t.Errorf("posted %d messages after a second tick, want still 1: %#v", len(posts), posts)
	}
}

// countRunnerLogEvent counts exact event matches, so the once-per-round
// assertion can't be fooled by a substring match.
func countRunnerLogEvent(note *notetask.Note, event string) int {
	count := 0
	for _, entry := range note.RunnerLog {
		if entry.Event == event {
			count++
		}
	}
	return count
}

// TestRunApprovesAndWakes confirms a plan_review note whose Discord
// message has a ✅ reaction becomes plan_approved with a Runner Log line,
// and wake fires exactly once -- against an httptest webhook (never the
// real Discord API) and the fixture vault.
func TestRunApprovesAndWakes(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "planpoll-approve-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	threadID := "thread-approve"
	messageID := "msg-approve"
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "plan_review", Repo: "phase6-test-repo", Created: "2026-11-06",
		DiscordThreadID: &threadID, PlanMessageID: &messageID, PlanRound: 1,
	}, "Trivial multi-unit task.")

	discordSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"reactions":[{"emoji":{"name":"✅"},"count":1}]}`))
	}))
	defer discordSrv.Close()

	var wakes atomic.Int32
	wake := func() { wakes.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		Run(ctx, v, discordSrv.URL, "user-approve", wake, 20*time.Millisecond)
		close(done)
	}()

	note := waitForStatus(t, v, relPath, "plan_approved", 5*time.Second)
	cancel()
	<-done // Run must have fully stopped touching the shared vault before this test's own cleanup (a raw, unsynchronized git rm/commit/push) runs

	if note == nil || note.Frontmatter.Status != "plan_approved" {
		t.Fatalf("status = %+v, want plan_approved", note)
	}
	found := false
	for _, e := range note.RunnerLog {
		if e.Event == "plan_approved (reaction)" {
			found = true
		}
	}
	if !found {
		t.Errorf("Runner Log missing %q: %+v", "plan_approved (reaction)", note.RunnerLog)
	}
	if got := wakes.Load(); got != 1 {
		t.Errorf("wake called %d times, want 1", got)
	}
}

// TestRunRepostsMissingPlanMessage confirms a plan_review note whose
// plan_message_id is empty (the original post failed) gets reposted, with
// the returned message id saved and a "plan_posted (retry)" Runner Log
// line -- stopForPlanReview's own "no message to poll" gap.
func TestRunRepostsMissingPlanMessage(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "planpoll-repost-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	threadID := "thread-repost"
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "plan_review", Repo: "phase6-test-repo", Created: "2026-11-06",
		DiscordThreadID: &threadID, PlanRound: 1,
	}, "Trivial multi-unit task.")

	if err := v.WriteNote(relPath, "test: seed plan+gate line "+slug, func(n *notetask.Note) error {
		n.HasPlan = true
		n.Plan = "Two units.\n\n```yaml\n- id: U1\n  title: Do it\n  verify: \"true\"\n```\n"
		notetask.AppendRunnerLog(n, "plan_gate risky=0.90 ambiguous=0.10 stop=true", time.Now())
		return nil
	}); err != nil {
		t.Fatalf("seeding plan+gate line: %v", err)
	}

	var posts atomic.Int32
	discordSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		posts.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"999"}`))
	}))
	defer discordSrv.Close()

	wake := func() {}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		Run(ctx, v, discordSrv.URL, "user-repost", wake, 20*time.Millisecond)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	var note *notetask.Note
	for time.Now().Before(deadline) {
		n, err := v.ReadNote(relPath)
		if err == nil && n.Frontmatter.PlanMessageID != nil && *n.Frontmatter.PlanMessageID != "" {
			note = n
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done // see TestRunApprovesAndWakes: must fully join before this test's cleanup touches the shared vault

	if note == nil {
		t.Fatal("plan_message_id was never set")
	}
	if *note.Frontmatter.PlanMessageID != "999" {
		t.Errorf("plan_message_id = %q, want %q", *note.Frontmatter.PlanMessageID, "999")
	}
	found := false
	for _, e := range note.RunnerLog {
		if e.Event == "plan_posted (retry)" {
			found = true
		}
	}
	if !found {
		t.Errorf("Runner Log missing %q: %+v", "plan_posted (retry)", note.RunnerLog)
	}
	if posts.Load() == 0 {
		t.Error("the forum webhook was never hit")
	}
}
