package planpoll

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
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
