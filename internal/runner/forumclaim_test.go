package runner

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/notify"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// forumClaimServer stands in for the real Discord forum webhook: it
// records every request's query string and JSON body, always answers with
// channelID as the starter message's channel_id.
func forumClaimServer(t *testing.T, channelID string) (srv *httptest.Server, hits *atomic.Int32, requests chan capturedForumRequest) {
	t.Helper()
	hits = &atomic.Int32{}
	requests = make(chan capturedForumRequest, 10)
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		body, _ := io.ReadAll(r.Body)
		var payload map[string]string
		_ = json.Unmarshal(body, &payload)
		requests <- capturedForumRequest{query: r.URL.RawQuery, body: payload}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(fmt.Sprintf(`{"channel_id":%q}`, channelID)))
	}))
	return srv, hits, requests
}

type capturedForumRequest struct {
	query string
	body  map[string]string
}

func postClaimVia(forumURL string) func(threadName, content string) (string, error) {
	return func(threadName, content string) (string, error) {
		return notify.PostForumClaim(forumURL, threadName, content)
	}
}

// TestProcessTaskClaimPostsThreadNameAndStoresThreadID is the Verify
// section's first bullet: a fresh claim posts thread_name + content with
// ?wait=true to the forum webhook, and the returned channel_id is stored
// as discord_thread_id -- against a real httptest server, not real
// Discord.
func TestProcessTaskClaimPostsThreadNameAndStoresThreadID(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	deps, job, v, _ := setupCrashTest(t, "forumclaim-basic", scriptPath)
	writeStubScript(t, scriptPath, "exit 0") // leaves the copy at in_progress -> ScenarioNonTerminal blocked

	forum, hits, requests := forumClaimServer(t, "555444333")
	defer forum.Close()
	deps.PostClaim = postClaimVia(forum.URL)

	var gotAlert *AlertPayload
	deps.OnBlocked = func(_ worker.Job, p AlertPayload) { gotAlert = &p }

	if err := ProcessTask(deps, noopReporter{}, job); err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}

	if got := hits.Load(); got != 1 {
		t.Fatalf("forum webhook hit %d times, want exactly 1", got)
	}
	req := <-requests
	if !strings.Contains(req.query, "wait=true") {
		t.Errorf("claim query = %q, want it to contain wait=true", req.query)
	}
	wantTitle := taskTitle(job.NotePath)
	if req.body["thread_name"] != wantTitle {
		t.Errorf("thread_name = %q, want %q", req.body["thread_name"], wantTitle)
	}
	if req.body["content"] != fmt.Sprintf("Task `%s` is claimed", wantTitle) {
		t.Errorf("content = %q, want the claim message", req.body["content"])
	}
	if strings.Contains(req.body["content"], "<@") {
		t.Errorf("content = %q, want no @-mentions on a bare claim", req.body["content"])
	}

	note, err := v.ReadNote(job.NotePath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.DiscordThreadID == nil || *note.Frontmatter.DiscordThreadID != "555444333" {
		t.Errorf("discord_thread_id = %v, want %q", note.Frontmatter.DiscordThreadID, "555444333")
	}

	// The claim itself must not satisfy the watcher's "no notification
	// recorded" check -- only claim_notified, never notified/notify_failed.
	hasClaimNotified, hasNotified := false, false
	for _, e := range note.RunnerLog {
		if e.Event == "claim_notified" {
			hasClaimNotified = true
		}
		if e.Event == "notified" || e.Event == "notify_failed" {
			hasNotified = true
		}
	}
	if !hasClaimNotified {
		t.Errorf("Runner Log missing claim_notified: %+v", note.RunnerLog)
	}
	if hasNotified {
		t.Errorf("Runner Log has notified/notify_failed from the claim alone, want neither: %+v", note.RunnerLog)
	}

	// The later blocked alert's payload carries the same thread ID, ready
	// for main.go's OnBlocked wiring to route it into the thread.
	if gotAlert == nil {
		t.Fatal("OnBlocked was never called")
	}
	if gotAlert.DiscordThreadID != "555444333" {
		t.Errorf("alert DiscordThreadID = %q, want %q", gotAlert.DiscordThreadID, "555444333")
	}
}

// TestProcessTaskLaterBlockedMessageRoutesIntoThread is the Verify
// section's second bullet, wired exactly as main.go wires OnBlocked in
// production (notifier.Send(job.NotePath, payload.DiscordThreadID, ...)):
// confirms the later blocked message actually lands on the forum webhook
// with ?thread_id=<id> set, not the plain top-level webhook.
func TestProcessTaskLaterBlockedMessageRoutesIntoThread(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	deps, job, _, _ := setupCrashTest(t, "forumclaim-routing", scriptPath)
	writeStubScript(t, scriptPath, "exit 0")

	forum, _, claimRequests := forumClaimServer(t, "888777666")
	defer forum.Close()
	deps.PostClaim = postClaimVia(forum.URL)

	topLevelHit := false
	topLevel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topLevelHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer topLevel.Close()

	notifier := &notify.Notifier{WebhookURL: topLevel.URL, ForumWebhookURL: forum.URL}
	deps.OnBlocked = func(job worker.Job, payload AlertPayload) {
		notifier.Send(job.NotePath, payload.DiscordThreadID,
			payload.DiscordMessage("12345", "67890"), payload.Slug+".md", payload.AttachmentMarkdown())
	}

	if err := ProcessTask(deps, noopReporter{}, job); err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}
	<-claimRequests // drain the claim request

	// notifier.Send fires asynchronously (fire-and-forget), so the blocked
	// alert's request arrives on claimRequests some short time after
	// ProcessTask itself returns.
	var blockedReq capturedForumRequest
	select {
	case blockedReq = <-claimRequests:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the blocked alert to reach the forum webhook")
	}
	if !strings.Contains(blockedReq.query, "thread_id=888777666") {
		t.Errorf("blocked alert query = %q, want it to contain thread_id=888777666", blockedReq.query)
	}
	if topLevelHit {
		t.Error("blocked alert hit the top-level webhook, want only the forum webhook")
	}
}

// TestProcessTaskUnsetForumWebhookSkipsClaimAndStaysTopLevel is the Verify
// section's third bullet: with deps.PostClaim nil -- exactly what main.go
// wires when DISCORD_TASK_FORUM_WEBHOOK_URL is unset -- no claim post
// happens and discord_thread_id never gets set.
func TestProcessTaskUnsetForumWebhookSkipsClaimAndStaysTopLevel(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	deps, job, v, _ := setupCrashTest(t, "forumclaim-unset", scriptPath)
	writeStubScript(t, scriptPath, "exit 0")
	// deps.PostClaim left nil -- the unset-forum-URL production wiring.

	var gotAlert *AlertPayload
	deps.OnBlocked = func(_ worker.Job, p AlertPayload) { gotAlert = &p }

	if err := ProcessTask(deps, noopReporter{}, job); err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}

	note, err := v.ReadNote(job.NotePath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.DiscordThreadID != nil {
		t.Errorf("discord_thread_id = %v, want nil (no forum webhook configured)", *note.Frontmatter.DiscordThreadID)
	}
	for _, e := range note.RunnerLog {
		if e.Event == "claim_notified" {
			t.Errorf("Runner Log has claim_notified with no forum webhook configured: %+v", note.RunnerLog)
		}
	}
	if gotAlert == nil {
		t.Fatal("OnBlocked was never called")
	}
	if gotAlert.DiscordThreadID != "" {
		t.Errorf("alert DiscordThreadID = %q, want empty (falls back to the top-level webhook)", gotAlert.DiscordThreadID)
	}
}

// TestProcessTaskClaimPostFailureFallsBackToTopLevel is the Verify
// section's fourth bullet: a claim post that fails must not block the
// task -- it proceeds exactly as if no forum webhook were configured, and
// later messages fall back to the top-level webhook.
func TestProcessTaskClaimPostFailureFallsBackToTopLevel(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	deps, job, v, _ := setupCrashTest(t, "forumclaim-postfail", scriptPath)
	writeStubScript(t, scriptPath, "exit 0")

	var claimAttempts atomic.Int32
	deps.PostClaim = func(threadName, content string) (string, error) {
		claimAttempts.Add(1)
		return "", errors.New("simulated forum webhook outage")
	}

	var gotAlert *AlertPayload
	deps.OnBlocked = func(_ worker.Job, p AlertPayload) { gotAlert = &p }

	if err := ProcessTask(deps, noopReporter{}, job); err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task -- the claim failure must not itself abort the task differently")
	}

	if claimAttempts.Load() == 0 {
		t.Fatal("PostClaim was never called")
	}

	note, err := v.ReadNote(job.NotePath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.DiscordThreadID != nil {
		t.Errorf("discord_thread_id = %v, want nil after a failed claim post", *note.Frontmatter.DiscordThreadID)
	}
	if gotAlert == nil {
		t.Fatal("OnBlocked was never called")
	}
	if gotAlert.DiscordThreadID != "" {
		t.Errorf("alert DiscordThreadID = %q, want empty after a failed claim post", gotAlert.DiscordThreadID)
	}
}

// TestProcessTaskResumeDoesNotRepostClaim is the Verify section's sixth
// bullet: a blocker_resolved resume (session_id already set) must reuse
// the note's existing discord_thread_id, never call PostClaim again.
func TestProcessTaskResumeDoesNotRepostClaim(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "forumclaim-resume-" + run
	relPath := fmt.Sprintf("Tasks/(phase6-test-repo) Forum Claim Resume Test %s.md", run)

	priorSessionID := "fake-prior-session"
	existingThreadID := "existing-thread-id-999"
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "in_progress", Repo: "phase6-test-repo", Created: "2026-09-15",
		SessionID: &priorSessionID, DiscordThreadID: &existingThreadID,
	}, "Resume test task -- session_id and discord_thread_id are both already set, as if claimed and blocked in an earlier run.")

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")
	writeStubScript(t, scriptPath, "exit 0") // leaves the copy at in_progress -> ScenarioNonTerminal blocked

	var claimAttempts atomic.Int32
	deps := Deps{
		Vault:     v,
		ClaudeBin: scriptPath,
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
		},
		PostClaim: func(threadName, content string) (string, error) {
			claimAttempts.Add(1)
			return "should-never-be-used", nil
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	// A resume's entire git surface is checking out this exact branch from
	// origin (checkoutTaskBranch) -- push one for real first, as if a prior
	// run had already created and preserved it, so this exercises the real
	// resume path rather than failing at setup before PostClaim is even a
	// question.
	branchName := "task/" + slug
	runGit(t, testTargetRepoPath, "checkout", "main")
	runGit(t, testTargetRepoPath, "checkout", "-b", branchName)
	runGit(t, testTargetRepoPath, "push", "-u", "origin", branchName)
	runGit(t, testTargetRepoPath, "checkout", "main")
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
	})

	copyName := copyFileName(slug)
	t.Cleanup(func() {
		exec.Command("rm", "-f", filepath.Join(testTargetRepoPath, copyName)).Run()
	})

	var gotAlert *AlertPayload
	deps.OnBlocked = func(_ worker.Job, p AlertPayload) { gotAlert = &p }

	if err := ProcessTask(deps, noopReporter{}, job); err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}

	if got := claimAttempts.Load(); got != 0 {
		t.Errorf("PostClaim called %d times on a resume, want 0", got)
	}

	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.DiscordThreadID == nil || *note.Frontmatter.DiscordThreadID != existingThreadID {
		t.Errorf("discord_thread_id = %v, want it unchanged at %q", note.Frontmatter.DiscordThreadID, existingThreadID)
	}
	if gotAlert == nil {
		t.Fatal("OnBlocked was never called")
	}
	if gotAlert.DiscordThreadID != existingThreadID {
		t.Errorf("alert DiscordThreadID = %q, want the pre-existing thread %q", gotAlert.DiscordThreadID, existingThreadID)
	}
	// Confirms the resumed session actually reached invocation on the
	// preserved branch (checkoutTaskBranch succeeded) and was blocked by the
	// stub leaving the copy non-terminal -- not by some earlier setup
	// failure that would also happen to leave claimAttempts at 0.
	if gotAlert.Scenario != ScenarioNonTerminal {
		t.Errorf("alert scenario = %v, want %v (resume should have reached invocation)", gotAlert.Scenario, ScenarioNonTerminal)
	}
}
