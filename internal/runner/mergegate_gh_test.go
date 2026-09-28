package runner

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestGitDiffMissingLocalBranch confirms gitDiff still produces a diff when
// the branch it's asked about has been pushed to origin but no longer
// exists as a local ref -- the exact scenario from the reported bug (merge
// gate review round runs against a local clone whose branch ref is gone),
// reproduced here against the local-only testTargetRepoPath fixture (a real
// origin remote, no GitHub/gh dependency needed since gitDiff itself never
// shells out to gh).
func TestGitDiffMissingLocalBranch(t *testing.T) {
	branch := fmt.Sprintf("mergegate-missing-branch-test-%d", time.Now().UnixNano())
	runGit(t, testTargetRepoPath, "checkout", "main")
	runGit(t, testTargetRepoPath, "checkout", "-b", branch)

	readme := testTargetRepoPath + "/README.md"
	marker := fmt.Sprintf("mergegate missing-branch test line %d\n", time.Now().UnixNano())
	f, err := os.OpenFile(readme, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("opening README: %v", err)
	}
	if _, err := f.WriteString(marker); err != nil {
		f.Close()
		t.Fatalf("writing README: %v", err)
	}
	f.Close()

	runGit(t, testTargetRepoPath, "add", "README.md")
	runGit(t, testTargetRepoPath, "commit", "-m", "mergegate missing-branch test commit")
	runGit(t, testTargetRepoPath, "push", "-u", "origin", branch)

	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branch).Run()
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
	})

	// The reported bug: by review time the local branch ref is gone (a
	// prior round moved on, or the clone never checked it out at all) even
	// though the branch is very much still alive on origin.
	runGit(t, testTargetRepoPath, "checkout", "main")
	runGit(t, testTargetRepoPath, "branch", "-D", branch)

	diff, err := gitDiff(testTargetRepoPath, "main", branch)
	if err != nil {
		t.Fatalf("gitDiff with no local branch ref: %v", err)
	}
	if !strings.Contains(diff, strings.TrimSpace(marker)) {
		t.Errorf("diff = %q, want it to contain marker %q", diff, strings.TrimSpace(marker))
	}
}

// TestWaitForCINoWorkflows checks that a repo with no
// .github/workflows returns success immediately, without ever shelling
// out to `gh run list` -- the RepoPath here is a bare temp dir with no
// gh/git setup at all, so any attempt to actually run gh would fail the
// test outright.
func TestWaitForCINoWorkflows(t *testing.T) {
	g := &GhMergeGateOps{RepoPath: t.TempDir(), Branch: "some-branch"}

	conclusion, log, err := g.WaitForCI()
	if err != nil {
		t.Fatalf("WaitForCI: %v", err)
	}
	if conclusion != "success" {
		t.Errorf("conclusion = %q, want %q", conclusion, "success")
	}
	if log != "" {
		t.Errorf("log = %q, want empty", log)
	}
}

// TestRealReviewInvocation exercises reviewOnce (the claude -p review
// invocation + verdict parsing that GhMergeGateOps.RunReview delegates to,
// after fetching prBody/diff for real) directly, with hand-built diffs --
// a genuine claude -p review call, checked for a sane verdict on an
// obviously-fine change and an obviously-bad one. This doesn't need a real
// GitHub repo at all (no gh pr view/comment involved at this level); the
// full RunReview (which does need gh pr view/comment against a real PR)
// and the gh run list / gh pr merge halves of Phase 11 are exercised
// separately against the real madevara24/sandbox repo -- see the PM
// Runner Go Rewrite project notes.
func TestRealReviewInvocation(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH, skipping real invocation test")
	}

	goodDiff := "diff --git a/README.md b/README.md\n" +
		"index e69de29..8b13789 100644\n" +
		"--- a/README.md\n" +
		"+++ b/README.md\n" +
		"@@ -1 +1,2 @@\n" +
		" # phase6-test-repo\n" +
		"+A harmless one-line documentation clarification.\n"

	badDiff := "diff --git a/main.go b/main.go\n" +
		"index e69de29..8b13789 100644\n" +
		"--- a/main.go\n" +
		"+++ b/main.go\n" +
		"@@ -1,3 +1,6 @@\n" +
		" package main\n" +
		"+import \"os/exec\"\n" +
		"+func nuke() {\n" +
		"+\texec.Command(\"rm\", \"-rf\", \"/\").Run() // deliberately malicious, unreviewed, no tests\n" +
		"+}\n"

	t.Run("obviously fine change", func(t *testing.T) {
		verdict, feedback, err := reviewOnce("claude", testTargetRepoPath, 90*time.Second, 0, "Adds a harmless doc clarification to the README.", goodDiff, "")
		if err != nil {
			t.Fatalf("reviewOnce: %v", err)
		}
		t.Logf("verdict=%s feedback=%s", verdict, feedback)
		if verdict != VerdictApprove {
			t.Errorf("verdict = %v for an obviously fine change, want APPROVE (feedback: %s)", verdict, feedback)
		}
	})

	t.Run("obviously bad change", func(t *testing.T) {
		verdict, feedback, err := reviewOnce("claude", testTargetRepoPath, 90*time.Second, 0, "Adds a small utility function.", badDiff, "")
		if err != nil {
			t.Fatalf("reviewOnce: %v", err)
		}
		t.Logf("verdict=%s feedback=%s", verdict, feedback)
		if verdict != VerdictConcerns {
			t.Errorf("verdict = %v for a deliberately destructive/unreviewed change, want CONCERNS (feedback: %s)", verdict, feedback)
		}
	})
}
