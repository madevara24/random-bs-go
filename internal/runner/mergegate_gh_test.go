package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	g := &GhMergeGateOps{RepoPath: t.TempDir(), PRURL: "https://github.com/fake/fake/pull/1"}

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

// writeFakeGh writes an executable fake `gh` script that answers the three
// `gh pr view` shapes RunReview needs (headRefName, body, comments) plus a
// no-op `gh pr comment`, without ever touching a real GitHub repo --
// headBranch and prBody are baked in directly rather than keyed off the PR
// URL, since this test only ever asks about one PR.
func writeFakeGh(t *testing.T, path, headBranch, prBody string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
case "$*" in
  *"headRefName"*)
    echo %q
    ;;
  *"--json body"*)
    echo %q
    ;;
  *"comments"*)
    echo ""
    ;;
  *"pr comment"*)
    exit 0
    ;;
  *)
    echo "fake gh: unexpected invocation: $*" >&2
    exit 1
    ;;
esac
`, headBranch, prBody)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake gh script: %v", err)
	}
}

// writeFakeClaudeApprove writes an executable fake `claude` script that
// always reports VERDICT: APPROVE in the same --output-format json shape
// reviewOnce parses, regardless of the prompt it's given -- this test's
// subject is RunReview's PR-identification plumbing, not the review
// content itself.
func writeFakeClaudeApprove(t *testing.T, path string) {
	t.Helper()
	script := "#!/bin/sh\n" + `echo '{"result":"VERDICT: APPROVE"}'` + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake claude script: %v", err)
	}
}

// TestRunReviewResolvesHeadBranchFromPRURL is RBG-20's real test gate: the
// runner's own constructed branch-name guess (see ProcessTask's branchName
// in runner.go) regularly doesn't match the PR's actual head branch, so
// GhMergeGateOps must identify the PR purely from PRURL -- resolving the
// real head branch once via `gh pr view --json headRefName` for anything
// that needs an actual git ref (gitDiff here) -- rather than ever deriving
// one from the other. PRURL below is a fake, structurally-plausible GitHub
// URL that intentionally has no relationship at all to the real branch
// name, proving RunReview never falls back to string-deriving a branch
// from it.
func TestRunReviewResolvesHeadBranchFromPRURL(t *testing.T) {
	branch := fmt.Sprintf("mergegate-prurl-test-%d", time.Now().UnixNano())
	runGit(t, testTargetRepoPath, "checkout", "main")
	runGit(t, testTargetRepoPath, "checkout", "-b", branch)

	readme := testTargetRepoPath + "/README.md"
	marker := fmt.Sprintf("mergegate prurl test line %d\n", time.Now().UnixNano())
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
	runGit(t, testTargetRepoPath, "commit", "-m", "mergegate prurl test commit")
	runGit(t, testTargetRepoPath, "push", "-u", "origin", branch)

	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branch).Run()
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branch).Run()
	})

	binDir := t.TempDir()
	writeFakeGh(t, filepath.Join(binDir, "gh"), branch, "fake PR body")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	claudeScript := filepath.Join(binDir, "fake-claude-approve.sh")
	writeFakeClaudeApprove(t, claudeScript)

	g := &GhMergeGateOps{
		RepoPath: testTargetRepoPath,
		// Deliberately unrelated to `branch` -- that's the entire point of
		// this test.
		PRURL:         "https://github.com/fake-owner/fake-repo/pull/42",
		DefaultBranch: "main",
		ClaudeBin:     claudeScript,
	}

	verdict, _, err := g.RunReview(0)
	if err != nil {
		t.Fatalf("RunReview: %v", err)
	}
	if verdict != VerdictApprove {
		t.Errorf("verdict = %v, want APPROVE", verdict)
	}
}
