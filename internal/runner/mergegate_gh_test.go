package runner

import (
	"os/exec"
	"testing"
	"time"
)

// TestRealReviewInvocation exercises GhMergeGateOps.RunReview for real --
// a genuine claude -p review call against a genuine diff, checked for a
// sane verdict on an obviously-fine change and an obviously-bad one. This
// is the one piece of Phase 11 that doesn't inherently require a real
// GitHub repo (gh pr comment is best-effort inside RunReview and doesn't
// fail the call if it errors, which it will here -- no real PR exists on
// this local-only disposable repo). The gh run list / gh pr merge halves
// of Phase 11 genuinely do need a real GitHub repo with real CI and are
// NOT exercised by this test -- see the PM Runner Go Rewrite project notes
// for why (a still-missing Administration permission on the fine-grained
// PAT blocks `gh repo create`).
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

	ops := &GhMergeGateOps{
		RepoPath:      testTargetRepoPath,
		Branch:        "nonexistent-branch-no-real-pr",
		DefaultBranch: "main",
		ClaudeBin:     "claude",
		ReviewTimeout: 90 * time.Second,
	}

	t.Run("obviously fine change", func(t *testing.T) {
		verdict, feedback, err := ops.RunReview(0, "Adds a harmless doc clarification to the README.", goodDiff)
		if err != nil {
			t.Fatalf("RunReview: %v", err)
		}
		t.Logf("verdict=%s feedback=%s", verdict, feedback)
		if verdict != VerdictApprove {
			t.Errorf("verdict = %v for an obviously fine change, want APPROVE (feedback: %s)", verdict, feedback)
		}
	})

	t.Run("obviously bad change", func(t *testing.T) {
		verdict, feedback, err := ops.RunReview(0, "Adds a small utility function.", badDiff)
		if err != nil {
			t.Fatalf("RunReview: %v", err)
		}
		t.Logf("verdict=%s feedback=%s", verdict, feedback)
		if verdict != VerdictConcerns {
			t.Errorf("verdict = %v for a deliberately destructive/unreviewed change, want CONCERNS (feedback: %s)", verdict, feedback)
		}
	})
}
