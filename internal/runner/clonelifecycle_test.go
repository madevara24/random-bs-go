// Tests for RBG-22's clone/branch lifecycle hardening: pullAndBranch
// starting from a known-clean tree, preserveBlockedWork committing/pushing
// a blocked task's work, and checkoutTaskBranch being a resume's entire
// (deterministic, no-reconciliation) git surface.
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

// TestPullAndBranchCleansDirtyTree reproduces the MLT-27/MLT-28 incident at
// the pullAndBranch level directly: a prior task's uncommitted edit to a
// tracked file, plus an untracked leftover file, both still sitting in the
// clone -- neither must survive into the newly branched tree.
func TestPullAndBranchCleansDirtyTree(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	runGit(t, testTargetRepoPath, "checkout", "-f", "main")
	runGit(t, testTargetRepoPath, "clean", "-fd")

	readmePath := filepath.Join(testTargetRepoPath, "README.md")
	orig, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("reading README.md: %v", err)
	}
	t.Cleanup(func() {
		os.WriteFile(readmePath, orig, 0o644)
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "--", "README.md").Run()
	})
	dirty := append(append([]byte{}, orig...), []byte("\nleftover dirty edit from a prior task\n")...)
	if err := os.WriteFile(readmePath, dirty, 0o644); err != nil {
		t.Fatalf("writing dirty edit: %v", err)
	}

	leftoverPath := filepath.Join(testTargetRepoPath, "leftover-untracked-from-prior-task.txt")
	if err := os.WriteFile(leftoverPath, []byte("leftover"), 0o644); err != nil {
		t.Fatalf("writing leftover untracked file: %v", err)
	}
	t.Cleanup(func() { os.Remove(leftoverPath) })

	repoCfg := config.RepoConfig{Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"}
	branchName := fmt.Sprintf("task/clonelifecycle-clean-%d", time.Now().UnixNano())
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
	})

	if err := pullAndBranch(repoCfg, branchName); err != nil {
		t.Fatalf("pullAndBranch: %v", err)
	}

	current := strings.TrimSpace(runGit(t, testTargetRepoPath, "branch", "--show-current"))
	if current != branchName {
		t.Fatalf("current branch = %q, want %q", current, branchName)
	}
	if _, err := os.Stat(leftoverPath); !os.IsNotExist(err) {
		t.Errorf("leftover untracked file from a prior task survived pullAndBranch's clean step")
	}
	readmeAfter, err := os.ReadFile(readmePath)
	if err != nil {
		t.Fatalf("reading README.md after pullAndBranch: %v", err)
	}
	if strings.Contains(string(readmeAfter), "leftover dirty edit") {
		t.Errorf("dirty README edit from a prior task survived pullAndBranch's reset step")
	}
	status := strings.TrimSpace(runGit(t, testTargetRepoPath, "status", "--porcelain"))
	if status != "" {
		t.Errorf("tree not clean on the new branch after pullAndBranch: %q", status)
	}
}

// TestPreserveBlockedWorkCommitsAndPushesDirtyTree is preserveBlockedWork's
// direct test gate: real, uncommitted work sitting on the task branch must
// end up committed and pushed to origin.
func TestPreserveBlockedWorkCommitsAndPushesDirtyTree(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	branchName := fmt.Sprintf("task/clonelifecycle-preserve-%d", time.Now().UnixNano())
	runGit(t, testTargetRepoPath, "checkout", "main")
	runGit(t, testTargetRepoPath, "checkout", "-b", branchName)
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
	})

	workFile := filepath.Join(testTargetRepoPath, "blocked-work.txt")
	if err := os.WriteFile(workFile, []byte("real task work, left uncommitted when the task went blocked\n"), 0o644); err != nil {
		t.Fatalf("writing work file: %v", err)
	}

	if err := preserveBlockedWork(testTargetRepoPath, branchName); err != nil {
		t.Fatalf("preserveBlockedWork: %v", err)
	}

	status := strings.TrimSpace(runGit(t, testTargetRepoPath, "status", "--porcelain"))
	if status != "" {
		t.Errorf("tree not clean after preserveBlockedWork: %q", status)
	}

	lsTree := runGit(t, testTargetRepoPath, "ls-tree", "-r", "--name-only", "origin/"+branchName)
	if !strings.Contains(lsTree, "blocked-work.txt") {
		t.Errorf("origin/%s missing blocked-work.txt; ls-tree:\n%s", branchName, lsTree)
	}
}

// TestPreserveBlockedWorkExcludesToolSideEffectPaths confirms the
// known-noisy .serena/.claude paths never ride along into the preserve
// commit, even though they're real changes sitting in the same tree --
// confirmed live in monorepo-lek-tumbas: .serena/project.yml is a tracked
// file the Serena MCP tool rewrites merely by being used, unrelated to any
// task's actual work.
func TestPreserveBlockedWorkExcludesToolSideEffectPaths(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	branchName := fmt.Sprintf("task/clonelifecycle-excl-%d", time.Now().UnixNano())
	runGit(t, testTargetRepoPath, "checkout", "main")
	runGit(t, testTargetRepoPath, "checkout", "-b", branchName)
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
	})

	serenaDir := filepath.Join(testTargetRepoPath, ".serena")
	if err := os.MkdirAll(serenaDir, 0o755); err != nil {
		t.Fatalf("mkdir .serena: %v", err)
	}
	serenaFile := filepath.Join(serenaDir, "project.yml")
	if err := os.WriteFile(serenaFile, []byte("serena tool side effect, not real task work\n"), 0o644); err != nil {
		t.Fatalf("writing .serena/project.yml: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(serenaDir) })

	workFile := filepath.Join(testTargetRepoPath, "real-work.txt")
	if err := os.WriteFile(workFile, []byte("real task work\n"), 0o644); err != nil {
		t.Fatalf("writing real-work.txt: %v", err)
	}

	if err := preserveBlockedWork(testTargetRepoPath, branchName); err != nil {
		t.Fatalf("preserveBlockedWork: %v", err)
	}

	lsTree := runGit(t, testTargetRepoPath, "ls-tree", "-r", "--name-only", "origin/"+branchName)
	if strings.Contains(lsTree, ".serena") {
		t.Errorf("origin/%s committed .serena/ tool side effects:\n%s", branchName, lsTree)
	}
	if !strings.Contains(lsTree, "real-work.txt") {
		t.Errorf("origin/%s missing real-work.txt; ls-tree:\n%s", branchName, lsTree)
	}
	// .serena/'s own dirty state is left untouched on disk -- not
	// committed, not deleted, not reset.
	if _, err := os.Stat(serenaFile); err != nil {
		t.Errorf(".serena/project.yml should still exist, untouched, on disk: %v", err)
	}
}

// TestPreserveBlockedWorkNoopWhenOffTaskBranch confirms preserveBlockedWork
// only ever acts on the task's own branch -- called while the clone sits on
// some other branch (e.g. a setup failure that never got as far as
// creating the branch), it must not commit or push anything on that
// branch's behalf.
func TestPreserveBlockedWorkNoopWhenOffTaskBranch(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	runGit(t, testTargetRepoPath, "checkout", "-f", "main")
	runGit(t, testTargetRepoPath, "clean", "-fd")
	mainTipBefore := strings.TrimSpace(runGit(t, testTargetRepoPath, "rev-parse", "main"))

	stray := filepath.Join(testTargetRepoPath, "stray-while-on-main.txt")
	if err := os.WriteFile(stray, []byte("not this task's branch\n"), 0o644); err != nil {
		t.Fatalf("writing stray file: %v", err)
	}
	t.Cleanup(func() { os.Remove(stray) })

	neverCreatedBranch := fmt.Sprintf("task/clonelifecycle-noop-%d", time.Now().UnixNano())
	if err := preserveBlockedWork(testTargetRepoPath, neverCreatedBranch); err != nil {
		t.Fatalf("preserveBlockedWork: %v", err)
	}

	mainTipAfter := strings.TrimSpace(runGit(t, testTargetRepoPath, "rev-parse", "main"))
	if mainTipAfter != mainTipBefore {
		t.Errorf("main advanced from %s to %s -- preserveBlockedWork must not act while off the task branch", mainTipBefore, mainTipAfter)
	}
	if _, err := os.Stat(stray); err != nil {
		t.Errorf("stray file should still be sitting there, untouched: %v", err)
	}
}

// TestCheckoutTaskBranchTracksOriginWhenNotLocal is resume's fresh-clone
// case: the task branch exists on origin (pushed by an earlier
// preserveBlockedWork) but not locally -- checkoutTaskBranch must fetch and
// check it out, tracking origin/<branch>.
func TestCheckoutTaskBranchTracksOriginWhenNotLocal(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	branchName := fmt.Sprintf("task/clonelifecycle-track-%d", time.Now().UnixNano())
	runGit(t, testTargetRepoPath, "checkout", "main")
	runGit(t, testTargetRepoPath, "checkout", "-b", branchName)
	markerPath := filepath.Join(testTargetRepoPath, "resume-marker.txt")
	if err := os.WriteFile(markerPath, []byte("preserved work from a prior blocked run\n"), 0o644); err != nil {
		t.Fatalf("writing marker file: %v", err)
	}
	runGit(t, testTargetRepoPath, "add", "resume-marker.txt")
	runGit(t, testTargetRepoPath, "commit", "-m", "simulate preserved blocked work")
	runGit(t, testTargetRepoPath, "push", "-u", "origin", branchName)
	// Delete the local branch -- as if a fresh/different clone is resuming
	// and only origin has it.
	runGit(t, testTargetRepoPath, "checkout", "main")
	runGit(t, testTargetRepoPath, "branch", "-D", branchName)
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
		os.Remove(markerPath)
	})

	repoCfg := config.RepoConfig{Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"}
	if err := checkoutTaskBranch(repoCfg, branchName); err != nil {
		t.Fatalf("checkoutTaskBranch: %v", err)
	}

	current := strings.TrimSpace(runGit(t, testTargetRepoPath, "branch", "--show-current"))
	if current != branchName {
		t.Fatalf("current branch = %q, want %q", current, branchName)
	}
	if _, err := os.Stat(markerPath); err != nil {
		t.Errorf("checked-out branch missing the preserved marker file: %v", err)
	}
	upstream := strings.TrimSpace(runGit(t, testTargetRepoPath, "rev-parse", "--abbrev-ref", branchName+"@{upstream}"))
	if upstream != "origin/"+branchName {
		t.Errorf("upstream = %q, want %q", upstream, "origin/"+branchName)
	}
}

// TestProcessTaskSelfReportedBlockedPreservesDirtyWork is the end-to-end
// version of the preserve-on-block requirement: a fake CC session leaves
// real, uncommitted work in the tree and self-reports status: blocked
// without ever running git itself -- ProcessTask must commit and push that
// work before returning, the exact gap behind the MLT-27/MLT-28 incident
// (there it was done_without_pr rather than blocked, but either way nothing
// was committed/pushed before the session ended).
func TestProcessTaskSelfReportedBlockedPreservesDirtyWork(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "clonelifecycle-selfblocked-" + run
	relPath := fmt.Sprintf("Tasks/(phase6-test-repo) Clone Lifecycle Self-Blocked Test %s.md", run)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Self-blocked preserve test -- the fake claude stub decides what happens, this prompt is never actually read.")

	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")
	copyName := copyFileName(slug)
	workFileName := "selfblocked-work-" + run + ".txt"
	writeStubScript(t, scriptPath, fmt.Sprintf(
		"echo 'real but uncommitted task work' > %q\n"+
			"printf -- '---\\nstatus: blocked\\nrepo: phase6-test-repo\\ncreated: \"2026-09-15\"\\n---\\nSelf-blocked preserve test.\\n\\n## Work Log\\n\\nGot partway through, blocked on missing credentials.\\n' > %q\nexit 0",
		filepath.Join(testTargetRepoPath, workFileName),
		filepath.Join(testTargetRepoPath, copyName)))

	deps := Deps{
		Vault:     v,
		ClaudeBin: scriptPath,
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	branchName := "task/" + slug
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
		exec.Command("rm", "-f", filepath.Join(testTargetRepoPath, copyName)).Run()
	})

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "blocked" {
		t.Fatalf("status = %q, want %q", noteAfter.Frontmatter.Status, "blocked")
	}

	// The tree ends up clean -- the uncommitted work was committed by the
	// runner, not left dirty for the next task's clone to inherit.
	status := strings.TrimSpace(runGit(t, testTargetRepoPath, "status", "--porcelain"))
	if status != "" {
		t.Errorf("tree not clean after a self-reported blocked task: %q", status)
	}

	// And it really landed on origin, not just committed locally.
	lsTree := runGit(t, testTargetRepoPath, "ls-tree", "-r", "--name-only", "origin/"+branchName)
	if !strings.Contains(lsTree, workFileName) {
		t.Errorf("origin/%s missing the blocked task's uncommitted work; ls-tree:\n%s", branchName, lsTree)
	}
}

// TestProcessTaskResumeDoesNotReconcileDefaultBranchItself is the
// integration-level guard for the "runner never merges/rebases, that's
// CC's job" rule: main advances while a task sits blocked, then the task
// resumes. Resume's checkout of the preserved branch must leave it exactly
// as it was pushed -- no merge commit, no conflict markers, no trace of
// main's advancing commit in the working tree at all.
func TestProcessTaskResumeDoesNotReconcileDefaultBranchItself(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "clonelifecycle-noreconcile-" + run
	relPath := fmt.Sprintf("Tasks/(phase6-test-repo) Clone Lifecycle No-Reconcile Test %s.md", run)

	branchName := "task/" + slug
	runGit(t, testTargetRepoPath, "checkout", "main")
	runGit(t, testTargetRepoPath, "checkout", "-b", branchName)
	preservedMarker := filepath.Join(testTargetRepoPath, "preserved-work-"+run+".txt")
	if err := os.WriteFile(preservedMarker, []byte("preserved work from before the block\n"), 0o644); err != nil {
		t.Fatalf("writing preserved marker: %v", err)
	}
	runGit(t, testTargetRepoPath, "add", filepath.Base(preservedMarker))
	runGit(t, testTargetRepoPath, "commit", "-m", "simulate preserved blocked work")
	runGit(t, testTargetRepoPath, "push", "-u", "origin", branchName)
	branchTipBefore := strings.TrimSpace(runGit(t, testTargetRepoPath, "rev-parse", branchName))

	// main advances while the task sits blocked.
	runGit(t, testTargetRepoPath, "checkout", "main")
	mainTipBefore := strings.TrimSpace(runGit(t, testTargetRepoPath, "rev-parse", "main"))
	advanceMarker := filepath.Join(testTargetRepoPath, "main-advanced-"+run+".txt")
	if err := os.WriteFile(advanceMarker, []byte("main moved on while blocked\n"), 0o644); err != nil {
		t.Fatalf("writing advance marker: %v", err)
	}
	runGit(t, testTargetRepoPath, "add", filepath.Base(advanceMarker))
	runGit(t, testTargetRepoPath, "commit", "-m", "main advances while the task is blocked")
	runGit(t, testTargetRepoPath, "push", "origin", "main")

	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "reset", "--hard", mainTipBefore).Run()
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "main", "--force").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
		os.Remove(advanceMarker)
		os.Remove(preservedMarker)
	})

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	priorSessionID := "fake-prior-session"
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "in_progress", Repo: "phase6-test-repo", Created: "2026-09-15",
		SessionID: &priorSessionID,
	}, "No-reconcile resume test -- the fake claude stub decides what happens, this prompt is never actually read.")

	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")
	writeStubScript(t, scriptPath, "exit 0") // leaves the copy at in_progress -> blocked

	deps := Deps{
		Vault:     v,
		ClaudeBin: scriptPath,
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	copyName := copyFileName(slug)
	t.Cleanup(func() {
		exec.Command("rm", "-f", filepath.Join(testTargetRepoPath, copyName)).Run()
	})

	if err := ProcessTask(deps, noopReporter{}, job); err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}

	current := strings.TrimSpace(runGit(t, testTargetRepoPath, "branch", "--show-current"))
	if current != branchName {
		t.Fatalf("current branch = %q, want %q (resume must check out the task branch)", current, branchName)
	}
	headAfter := strings.TrimSpace(runGit(t, testTargetRepoPath, "rev-parse", "HEAD"))
	if headAfter != branchTipBefore {
		t.Errorf("HEAD = %s, want it unchanged at %s -- the runner must never merge/rebase origin/main into the task branch itself", headAfter, branchTipBefore)
	}
	if _, err := os.Stat(advanceMarker); !os.IsNotExist(err) {
		t.Errorf("main's advancing commit leaked into the task branch's working tree -- reconciliation is CC's job, not the runner's")
	}
	status := strings.TrimSpace(runGit(t, testTargetRepoPath, "status", "--porcelain"))
	if status != "" {
		t.Errorf("tree not clean after resume checkout (no conflict markers expected): %q", status)
	}
}
