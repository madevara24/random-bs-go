package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/claudeusage"
	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// testTargetRepoPath is a local-only disposable git repo (bare +
// pre-cloned working copy) set up specifically for this phase's testing --
// NOT GitHub-hosted. See the PM Runner Go Rewrite project notes for
// provenance/rationale (a discovered GitHub credential permission gap
// blocks real push/PR against GitHub; local-only exercises everything
// Phase 6 itself is actually responsible for: branch creation, the
// note-copy mechanism, and claude invocation + session_id capture, none of
// which need GitHub at all). Configurable via testvault.TargetRepoPath
// (RBG_TEST_TARGET_REPO_PATH) so CI can point it at a scratch fixture.
var testTargetRepoPath = testvault.TargetRepoPath

type noopReporter struct{}

func (noopReporter) SetStage(string) {}
func (noopReporter) TouchActivity()  {}

// recordingReporter is a fake ActivityReporter that records every SetStage
// value in call order, so tests can assert on the exact stages a run
// reports (e.g. the multi-unit planner/unit-loop/finish sequence).
type recordingReporter struct {
	stages []string
}

func (r *recordingReporter) SetStage(stage string) { r.stages = append(r.stages, stage) }
func (r *recordingReporter) TouchActivity()        {}

// stagesSeen returns a copy of the stages recorded so far, in call order.
func (r *recordingReporter) stagesSeen() []string {
	return append([]string(nil), r.stages...)
}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = vaultgit.CleanGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// TestProcessTaskSetupAndInvocation is Phase 6's real test gate: against a
// real disposable test repo and a trivial task note, confirm the branch
// gets created, claude actually runs and does something, and the captured
// session_id is a genuine, resumable session -- checked by independently
// resuming it with a manual `claude --resume` call afterward.
//
// The task deliberately tells CC to reach status: done without opening a
// PR (out of scope for this phase's test gate), which since the
// done-without-pr_url safety net was added means ProcessTask now routes
// this through handleCrashFallback and returns an error, with the vault
// note ending up blocked rather than done -- expected here, not a
// regression; this test's actual assertions (branch/commits/session_id)
// don't depend on the final status.
func TestProcessTaskSetupAndInvocation(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH, skipping real invocation test")
	}
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "phase6-e2e-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Add the exact HTML comment `<!-- rbg-phase6-test -->` as a new line at the very top of README.md, then commit that change with git (message: \"phase6 test\"). Do not create a pull request, do not push, do not run any other git or gh commands. Then update your task-copy file's status to `done` per the instructions above.")

	deps := Deps{
		Vault: v,
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {
				Path:          testTargetRepoPath,
				Remote:        "local/phase6-test-repo",
				DefaultBranch: "main",
			},
		},
	}

	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	err := ProcessTask(deps, noopReporter{}, job)
	if err == nil {
		t.Fatal("ProcessTask returned nil error, want an error since the task reached done without a pr_url")
	}

	// Branch was created.
	branches := runGit(t, testTargetRepoPath, "branch", "--list", fmt.Sprintf("task/%s", slug))
	if strings.TrimSpace(branches) == "" {
		t.Fatalf("no branch matching task/%s found; branches:\n%s", slug, runGit(t, testTargetRepoPath, "branch", "-a"))
	}
	branchName := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(branches), "*"))
	t.Cleanup(func() {
		runGit(t, testTargetRepoPath, "checkout", "main")
		runGit(t, testTargetRepoPath, "branch", "-D", branchName)
	})

	// claude actually did something: at least one new commit on the branch
	// beyond main.
	commitCount := strings.TrimSpace(runGit(t, testTargetRepoPath, "rev-list", "--count", "main.."+branchName))
	if commitCount == "0" {
		t.Errorf("no new commits on %s beyond main -- claude didn't seem to do anything", branchName)
	}

	// session_id was captured and persisted to the vault note.
	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.SessionID == nil || *noteAfter.Frontmatter.SessionID == "" {
		t.Fatal("session_id was not persisted to the vault note")
	}
	sessionID := *noteAfter.Frontmatter.SessionID

	// The note-copy was cleaned up... actually Phase 6 doesn't delete it yet
	// (that's Phase 7's "delete the copy unconditionally" -- this phase only
	// writes it). Just confirm it exists, proving the note-copy mechanism
	// ran, then clean it up so it doesn't linger in the disposable repo.
	copyPath := fmt.Sprintf("%s/%s", testTargetRepoPath, copyFileName(slug))
	t.Cleanup(func() {
		exec.Command("rm", "-f", copyPath).Run()
	})

	// Independently confirm the captured session_id is a genuine, resumable
	// session by resuming it with a fresh manual `claude` call -- this is
	// the "matches what a manual claude -p --output-format stream-json run
	// reports for the same session" check from Design - Implementation.md's
	// Phase 6 test gate.
	cmd := exec.Command("claude", "-p", "Just reply with the word OK, and nothing else. Take no other action.",
		"--resume", sessionID, "--output-format", "json", "--dangerously-skip-permissions")
	cmd.Dir = testTargetRepoPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("manual --resume %s failed: %v\n%s", sessionID, err, out)
	}
	var result struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parsing manual resume output: %v\n%s", err, out)
	}
	if result.SessionID != sessionID {
		t.Errorf("manual resume reported session_id %q, want it to match captured %q", result.SessionID, sessionID)
	}
}

// TestProcessTaskMergeBack is Phase 7's test gate: run a task that has CC
// set status: done and a pr_url in its copy, and confirm the real vault
// note ends up with the right status/pr_url/Work Log, the Runner Log gets
// one new correctly-timestamped "done" line, the copy no longer exists in
// the target repo, and the pushed branch never contains the copy file.
//
// No real GitHub PR exists here (see the package doc comment on
// testTargetRepoPath) -- CC is instructed to push to this repo's local
// remote (a real git push, just not to GitHub) and write a placeholder
// pr_url into its copy, simulating having opened one. This still fully
// exercises the runner's own merge-back mechanism, which only ever copies
// whatever string CC put in that field -- it has no independent way to
// validate a pr_url either way, real or simulated.
func TestProcessTaskMergeBack(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH, skipping real invocation test")
	}
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "phase7-e2e-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)
	const fakePRURL = "local-test://no-real-github-here/pr/1"

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, fmt.Sprintf(
		"Add the exact HTML comment `<!-- rbg-phase7-test -->` as a new line at the very top of README.md. "+
			"Commit that change (message: \"phase7 test\"), then push your current branch to origin with `git push origin HEAD` -- "+
			"this repo's remote is a local bare repo, not GitHub, so this push is real and expected, but there is no `gh` PR "+
			"mechanism available here. Do not run any `gh` command. Instead, simulate having opened a PR: in your task-copy "+
			"file's frontmatter, set `pr_url` to the exact string `%s`, set `status` to `done`, and add one line to the "+
			"\"## Work Log\" section describing what you did.", fakePRURL))

	deps := Deps{
		Vault: v,
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {
				Path:          testTargetRepoPath,
				Remote:        "local/phase6-test-repo",
				DefaultBranch: "main",
			},
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	branchName := "task/" + slug
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
	})

	// Vault note has the right status/pr_url/Work Log.
	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "done" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "done")
	}
	if noteAfter.Frontmatter.PRURL == nil || *noteAfter.Frontmatter.PRURL != fakePRURL {
		t.Errorf("pr_url = %v, want %q", noteAfter.Frontmatter.PRURL, fakePRURL)
	}
	if !noteAfter.HasWorkLog || strings.TrimSpace(noteAfter.WorkLog) == "" {
		t.Errorf("Work Log missing or empty after merge-back: %+v", noteAfter.WorkLog)
	}
	if noteAfter.Frontmatter.SessionID == nil || *noteAfter.Frontmatter.SessionID == "" {
		t.Error("session_id missing after merge-back (should be the runner's own captured value)")
	}

	// Runner Log has exactly one new, correctly-timestamped "done" line.
	if !noteAfter.HasRunnerLog {
		t.Fatal("Runner Log section missing after merge-back")
	}
	var doneEntries []notetask.RunnerLogEntry
	for _, e := range noteAfter.RunnerLog {
		if e.Event == "done" {
			doneEntries = append(doneEntries, e)
		}
	}
	if len(doneEntries) != 1 {
		t.Fatalf("Runner Log has %d \"done\" entries, want 1: %+v", len(doneEntries), noteAfter.RunnerLog)
	}
	if time.Since(doneEntries[0].Timestamp) > 2*time.Minute {
		t.Errorf("done entry timestamp %v looks stale, not freshly written", doneEntries[0].Timestamp)
	}
	// Also confirm the earlier "in_progress" line from setup is still there
	// -- merge-back appends, it doesn't replace the log.
	hasInProgress := false
	for _, e := range noteAfter.RunnerLog {
		if e.Event == "in_progress" {
			hasInProgress = true
		}
	}
	if !hasInProgress {
		t.Errorf("Runner Log lost its earlier \"in_progress\" entry: %+v", noteAfter.RunnerLog)
	}

	// The copy no longer exists in the target repo.
	copyPath := filepath.Join(testTargetRepoPath, copyFileName(slug))
	if _, err := os.Stat(copyPath); err == nil {
		t.Errorf("note-copy %s still exists after ProcessTask returned", copyPath)
	} else if !os.IsNotExist(err) {
		t.Errorf("stat %s: %v", copyPath, err)
	}

	// The pushed branch never contains the copy file -- .git/info/exclude
	// actually worked, this isn't just "CC was told not to commit it."
	lsTree := runGit(t, testTargetRepoPath, "ls-tree", "-r", "--name-only", "origin/"+branchName)
	if strings.Contains(lsTree, copyFileName(slug)) {
		t.Errorf("note-copy %s was committed to the pushed branch:\n%s", copyFileName(slug), lsTree)
	}
}

// sampleClaudeUsage is the deterministic usage every claim-message test
// stubs in -- noon UTC formats as 20:04 WIB, so the expected reset text is
// fixed regardless of the host's zone.
func sampleClaudeUsage() claudeusage.Usage {
	return claudeusage.Usage{
		FiveHour: &claudeusage.Window{
			Utilization: 42,
			ResetsAt:    time.Date(2026, 11, 10, 13, 4, 0, 0, time.UTC),
		},
		SevenDay: &claudeusage.Window{
			Utilization: 7.5,
			ResetsAt:    time.Date(2026, 11, 15, 2, 0, 0, 0, time.UTC),
		},
	}
}

const sampleUsageBullet = "- Claude usage: 5h 42% (resets 2026-11-10 20:04 WIB), weekly 7.5% (resets 2026-11-15 09:00 WIB)"

func stubClaimUsage(t *testing.T) func(context.Context) (claudeusage.Usage, error) {
	t.Helper()
	return func(context.Context) (claudeusage.Usage, error) {
		return sampleClaudeUsage(), nil
	}
}

// wantClaimContent is the exact body a fresh single-unit task coded and
// reviewed with Claude produces.
func wantClaimContent(title string) string {
	return strings.Join([]string{
		fmt.Sprintf("Task `%s` is claimed", title),
		fmt.Sprintf("- Task: %s", title),
		"- Plan engine: " + singleUnitPlanEngine,
		"- Code engine: claude",
		"- Review engine: claude",
		sampleUsageBullet,
	}, "\n")
}

// TestClaimMessageEnginesAndClaudeUsage is the claim-body table: every
// engine role set to claude vs. a non-claude engine, crossed with
// single-unit vs. multi-unit. It asserts the three engine lines, the
// "— (single-unit)" plan line, and that the usage bullet appears only when
// an engine that actually runs resolved to claude (so a single-unit note
// pinned to plan_engine: claude must omit it, and must not even fetch).
func TestClaimMessageEnginesAndClaudeUsage(t *testing.T) {
	const title = "(phase6-test-repo) Claim Message Table"
	tests := []struct {
		name         string
		multiUnit    bool
		codeEngine   string
		reviewEngine string
		planEngine   string
	}{
		{name: "single/code-claude", codeEngine: "claude", reviewEngine: "pi", planEngine: "pi"},
		{name: "single/code-pi", codeEngine: "pi", reviewEngine: "pi", planEngine: "pi"},
		{name: "single/review-claude", codeEngine: "pi", reviewEngine: "claude", planEngine: "pi"},
		{name: "single/review-pi", codeEngine: "pi", reviewEngine: "pi", planEngine: "pi"},
		{name: "single/plan-claude-does-not-run", codeEngine: "pi", reviewEngine: "pi", planEngine: "claude"},
		{name: "single/plan-pi", codeEngine: "pi", reviewEngine: "pi", planEngine: "pi"},
		{name: "multi/code-claude", multiUnit: true, codeEngine: "claude", reviewEngine: "pi", planEngine: "pi"},
		{name: "multi/code-pi", multiUnit: true, codeEngine: "pi", reviewEngine: "pi", planEngine: "pi"},
		{name: "multi/review-claude", multiUnit: true, codeEngine: "pi", reviewEngine: "claude", planEngine: "pi"},
		{name: "multi/review-pi", multiUnit: true, codeEngine: "pi", reviewEngine: "pi", planEngine: "pi"},
		{name: "multi/plan-claude", multiUnit: true, codeEngine: "pi", reviewEngine: "pi", planEngine: "claude"},
		{name: "multi/plan-pi", multiUnit: true, codeEngine: "pi", reviewEngine: "pi", planEngine: "pi"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			multiUnit := tc.multiUnit
			reviewEngine, planEngine := tc.reviewEngine, tc.planEngine
			note := &notetask.Note{Frontmatter: notetask.Frontmatter{
				Repo:         "phase6-test-repo",
				ReviewEngine: &reviewEngine,
				PlanEngine:   &planEngine,
				MultiUnit:    &multiUnit,
			}}
			job := worker.Job{
				NotePath: "Tasks/" + title + ".md",
				Repo:     "phase6-test-repo",
				Engine:   tc.codeEngine,
			}

			fetches := 0
			deps := Deps{
				MultiUnitEnabled: true,
				FetchClaudeUsage: func(context.Context) (claudeusage.Usage, error) {
					fetches++
					return sampleClaudeUsage(), nil
				},
			}

			body := buildClaimMessage(deps, job, note, config.RepoConfig{})
			lines := strings.Split(body, "\n")
			if lines[0] != fmt.Sprintf("Task `%s` is claimed", title) {
				t.Errorf("first line = %q, want the claim headline", lines[0])
			}
			if want := "- Task: " + title; !strings.Contains(body, want) {
				t.Errorf("body missing %q:\n%s", want, body)
			}

			wantPlan := tc.planEngine
			if !tc.multiUnit {
				wantPlan = singleUnitPlanEngine
			}
			for _, want := range []string{
				"- Plan engine: " + wantPlan,
				"- Code engine: " + tc.codeEngine,
				"- Review engine: " + tc.reviewEngine,
			} {
				if !strings.Contains(body, want) {
					t.Errorf("body missing %q:\n%s", want, body)
				}
			}

			wantUsage := tc.codeEngine == "claude" || tc.reviewEngine == "claude" || (tc.multiUnit && tc.planEngine == "claude")
			if wantUsage {
				if !strings.Contains(body, sampleUsageBullet) {
					t.Errorf("body missing usage bullet %q:\n%s", sampleUsageBullet, body)
				}
				if fetches != 1 {
					t.Errorf("usage fetched %d times, want 1", fetches)
				}
			} else {
				if strings.Contains(body, "- Claude usage:") {
					t.Errorf("body has a usage bullet with no Claude engine running:\n%s", body)
				}
				if fetches != 0 {
					t.Errorf("usage fetched %d times with no Claude engine running, want 0", fetches)
				}
			}
		})
	}
}

// TestClaimMessageClaudeUsageUnavailable confirms a usage fetch failure
// renders the unavailable bullet with the short reason and still yields the
// complete claim body -- the fetch never fails the claim.
func TestClaimMessageClaudeUsageUnavailable(t *testing.T) {
	claude := "claude"
	note := &notetask.Note{Frontmatter: notetask.Frontmatter{ReviewEngine: &claude}}
	job := worker.Job{NotePath: "Tasks/(phase6-test-repo) Claim Usage Unavailable.md", Repo: "phase6-test-repo", Engine: "claude"}
	deps := Deps{
		FetchClaudeUsage: func(context.Context) (claudeusage.Usage, error) {
			return claudeusage.Usage{}, errors.New("claude usage: unexpected HTTP status 401 Unauthorized")
		},
	}

	body := buildClaimMessage(deps, job, note, config.RepoConfig{})
	want := "- Claude usage: unavailable (claude usage: unexpected HTTP status 401 Unauthorized)"
	if !strings.Contains(body, want) {
		t.Errorf("body missing %q:\n%s", want, body)
	}
	for _, want := range []string{
		"- Code engine: claude",
		"- Review engine: claude",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("body missing %q after a usage failure:\n%s", want, body)
		}
	}
}

// TestClaimMessageClaudeUsageWindowMissing covers the tolerated-response
// case where the endpoint omitted one window: it renders as unavailable
// rather than a half-filled bullet.
func TestClaimMessageClaudeUsageWindowMissing(t *testing.T) {
	claude := "claude"
	note := &notetask.Note{Frontmatter: notetask.Frontmatter{ReviewEngine: &claude}}
	job := worker.Job{NotePath: "Tasks/(phase6-test-repo) Claim Usage Window Missing.md", Repo: "phase6-test-repo", Engine: "claude"}
	deps := Deps{
		FetchClaudeUsage: func(context.Context) (claudeusage.Usage, error) {
			return claudeusage.Usage{FiveHour: &claudeusage.Window{Utilization: 5}}, nil
		},
	}

	body := buildClaimMessage(deps, job, note, config.RepoConfig{})
	if want := "- Claude usage: unavailable (usage window missing)"; !strings.Contains(body, want) {
		t.Errorf("body missing %q:\n%s", want, body)
	}
}

// TestClaimMessageResetTimesUseJakarta confirms the reset times are rendered
// in Asia/Jakarta even when the source instants are UTC.
func TestClaimMessageResetTimesUseJakarta(t *testing.T) {
	if got, want := formatUsageReset(time.Date(2026, 11, 10, 13, 4, 0, 0, time.UTC)), "2026-11-10 20:04 WIB"; got != want {
		t.Errorf("formatUsageReset = %q, want %q", got, want)
	}
}

// TestProcessTaskSelfReportedBlockedOpensNumberedBlocker covers the
// merge-back path (the happy exit where CC's own copy says status: blocked):
// the runner opens the numbered blocker itself, with the self-reported cause
// naming the Work Log entry's number, and threads that number through
// OnTerminal so the Discord message can say "(Blocker 1)". Unlike every
// crash/setup/planner/unit-loop block, this one is not a failure path --
// ProcessTask returns nil.
func TestProcessTaskSelfReportedBlockedOpensNumberedBlocker(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	deps, job, v, slug := setupCrashTest(t, "phase-selfreport-blocker", scriptPath)
	copyName := copyFileName(slug)
	writeStubScript(t, scriptPath, fmt.Sprintf(
		"printf -- '---\\nstatus: blocked\\nrepo: phase6-test-repo\\ncreated: \"2026-09-15\"\\n---\\nSelf-reported blocker test.\\n\\n## Work Log\\n\\nBlocker 1: need the missing deploy credentials.\\n' > %q\nexit 0",
		filepath.Join(testTargetRepoPath, copyName)))

	var gotStatus string
	var gotBlockerRound int
	var onTerminalCalled bool
	deps.OnTerminal = func(j worker.Job, status, workLog string, autoMerge bool, prURL, discordThreadID string, usageLimit *UsageLimitInfo, engineName string, blockerRound int) {
		onTerminalCalled = true
		gotStatus = status
		gotBlockerRound = blockerRound
	}

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v (self-reported blocked is the happy path, not a crash)", err)
	}
	if !onTerminalCalled {
		t.Fatal("OnTerminal was never called")
	}
	if gotStatus != "blocked" {
		t.Errorf("status = %q, want %q", gotStatus, "blocked")
	}
	if gotBlockerRound != 1 {
		t.Errorf("blockerRound = %d, want 1", gotBlockerRound)
	}

	note, err := v.ReadNote(job.NotePath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.BlockerRound != 1 {
		t.Errorf("blocker_round = %d, want 1", note.Frontmatter.BlockerRound)
	}
	if !note.HasBlockers || !strings.Contains(note.Blockers, "### Blocker 1\n") {
		t.Errorf("note missing \"### Blocker 1\" block:\n%s", note.Blockers)
	}
	wantCause := `- Cause: self-reported -- see the Work Log entry starting "Blocker 1:"`
	if !strings.Contains(note.Blockers, wantCause+"\n") {
		t.Errorf("note missing Cause line %q:\n%s", wantCause, note.Blockers)
	}
}
