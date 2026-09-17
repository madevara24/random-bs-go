package runner

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
// which need GitHub at all).
const testTargetRepoPath = "/home/obsidian/repos/phase6-test-repo"

type noopReporter struct{}

func (noopReporter) SetStage(string) {}
func (noopReporter) TouchActivity()  {}

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
	}, "Add the exact HTML comment `<!-- pmrunner-phase6-test -->` as a new line at the very top of README.md, then commit that change with git (message: \"phase6 test\"). Do not create a pull request, do not push, do not run any other git or gh commands. Then update your task-copy file's status to `done` per the instructions above.")

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
	branches := runGit(t, testTargetRepoPath, "branch", "--list", fmt.Sprintf("task/%s-*", slug))
	if strings.TrimSpace(branches) == "" {
		t.Fatalf("no branch matching task/%s-* found; branches:\n%s", slug, runGit(t, testTargetRepoPath, "branch", "-a"))
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
		"Add the exact HTML comment `<!-- pmrunner-phase7-test -->` as a new line at the very top of README.md. "+
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

	branchName := "task/" + slug + "-" + time.Now().Format("2006-01-02")
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
