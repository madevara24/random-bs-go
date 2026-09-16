package runner

import (
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

// TestSetupFailurePullAndBranchRestoresRepoAndAlerts reproduces the real
// production trigger confirmed live 2026-09-17: a branch collision makes
// pullAndBranch's final `checkout -b` fail. Confirms the note reaches
// blocked, the target repo ends up back on its default branch with no
// leftover branch, and the Discord alert fires.
func TestSetupFailurePullAndBranchRestoresRepoAndAlerts(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "setupfail-branch-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Setup-failure test task -- a branch with the same name pullAndBranch would create is pre-created, forcing its final `checkout -b` to fail, same as the real invalid-branch-name production trigger.")

	deps := Deps{
		Vault: v,
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	branchName := "task/" + slug + "-" + time.Now().Format("2006-01-02")

	// Pre-create the colliding branch (without checking it out) -- this is
	// what makes pullAndBranch's `checkout -b branchName` fail.
	runGit(t, testTargetRepoPath, "checkout", "main")
	runGit(t, testTargetRepoPath, "branch", branchName)
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
	})

	var gotAlert *AlertPayload
	deps.OnBlocked = func(p AlertPayload) { gotAlert = &p }

	err := ProcessTask(deps, noopReporter{}, job)
	if err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}

	noteAfter, readErr := v.ReadNote(relPath)
	if readErr != nil {
		t.Fatalf("reading note back: %v", readErr)
	}
	if noteAfter.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "blocked")
	}
	if !noteAfter.HasWorkLog || !strings.Contains(noteAfter.WorkLog, "pulling default branch") {
		t.Errorf("Work Log missing a setup-failure entry naming the failed stage: %q", noteAfter.WorkLog)
	}

	// Target repo ends up back on its default branch...
	currentBranch := strings.TrimSpace(runGit(t, testTargetRepoPath, "branch", "--show-current"))
	if currentBranch != "main" {
		t.Errorf("target repo left on branch %q, want %q", currentBranch, "main")
	}
	// ...with no leftover branch (the pre-created collision is cleaned up
	// too, not just anything pullAndBranch itself might have created).
	branches := runGit(t, testTargetRepoPath, "branch", "--list", branchName)
	if strings.TrimSpace(branches) != "" {
		t.Errorf("branch %s still exists after cleanup, want it deleted:\n%s", branchName, branches)
	}

	if gotAlert == nil {
		t.Fatal("OnBlocked was never called")
	}
	if gotAlert.Scenario != ScenarioSetupFailure {
		t.Errorf("alert scenario = %v, want %v", gotAlert.Scenario, ScenarioSetupFailure)
	}
	if gotAlert.Slug != slug || gotAlert.Repo != job.Repo {
		t.Errorf("alert slug/repo = %q/%q, want %q/%q", gotAlert.Slug, gotAlert.Repo, slug, job.Repo)
	}
}

// TestSetupFailureWriteInProgressStillAlertsDiscord forces a failure at the
// very first WriteNote call (marking status: in_progress) by making the
// note file read-only before ProcessTask runs. Since handleSetupFailure's
// own attempt to write status: blocked reads/writes that exact same file,
// it fails identically -- this confirms the Discord alert still fires even
// when the note's own blocked write can't be verified to succeed, per the
// task's requirement that Discord not depend on the vault being writable.
func TestSetupFailureWriteInProgressStillAlertsDiscord(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "setupfail-writenote-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Setup-failure test task -- the note file is made read-only before ProcessTask runs, forcing the very first WriteNote call (writing status: in_progress) to fail.")

	absPath := filepath.Join(testvault.Path, relPath)
	if err := os.Chmod(absPath, 0o444); err != nil {
		t.Fatalf("chmod %s read-only: %v", absPath, err)
	}
	t.Cleanup(func() { os.Chmod(absPath, 0o644) })

	deps := Deps{
		Vault: v,
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	var gotAlert *AlertPayload
	deps.OnBlocked = func(p AlertPayload) { gotAlert = &p }

	err := ProcessTask(deps, noopReporter{}, job)
	if err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}

	if gotAlert == nil {
		t.Fatal("OnBlocked was never called, even though the note's own blocked write couldn't succeed")
	}
	if gotAlert.Scenario != ScenarioSetupFailure {
		t.Errorf("alert scenario = %v, want %v", gotAlert.Scenario, ScenarioSetupFailure)
	}
	if gotAlert.Slug != slug {
		t.Errorf("alert slug = %q, want %q", gotAlert.Slug, slug)
	}

	// Confirm the blocked write really did fail (not just theoretically
	// could have): the note is still stuck at "queued", proving Discord
	// fired independently of that write's outcome.
	os.Chmod(absPath, 0o644)
	noteAfter, readErr := v.ReadNote(relPath)
	if readErr != nil {
		t.Fatalf("reading note back: %v", readErr)
	}
	if noteAfter.Frontmatter.Status != "queued" {
		t.Errorf("status = %q, want still %q (the blocked write should have failed given the read-only note)", noteAfter.Frontmatter.Status, "queued")
	}
}
