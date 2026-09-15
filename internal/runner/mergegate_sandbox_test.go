package runner

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// sandboxRepoPath is a real, disposable GitHub-hosted repo
// (madevara24/sandbox) the user explicitly designated for exercising
// Phase 11's real end-to-end scenarios -- unlike every other test in this
// package, these are allowed to create/merge/close real branches and PRs.
// Gated behind an env var since they're slow (real claude + real CI polls)
// and mutate real GitHub state: run with
// PMRUNNER_RUN_SANDBOX_TEST=1 go test ./internal/runner/ -run TestSandbox -v
const sandboxRepoPath = "/home/obsidian/repos/sandbox"
const sandboxDefaultBranch = "main"

func skipUnlessSandboxEnabled(t *testing.T) {
	t.Helper()
	if os.Getenv("PMRUNNER_RUN_SANDBOX_TEST") == "" {
		t.Skip("set PMRUNNER_RUN_SANDBOX_TEST=1 to run real scenarios against madevara24/sandbox")
	}
	if _, err := os.Stat(sandboxRepoPath); err != nil {
		t.Skipf("sandbox repo not present at %s: %v", sandboxRepoPath, err)
	}
}

func sbGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = sandboxRepoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func sbGh(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("gh", args...)
	cmd.Dir = sandboxRepoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("gh %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func sbSync(t *testing.T) {
	t.Helper()
	sbGit(t, "checkout", sandboxDefaultBranch)
	sbGit(t, "fetch", "origin")
	sbGit(t, "reset", "--hard", "origin/"+sandboxDefaultBranch)
}

// countingMergeGateOps wraps a real GhMergeGateOps, delegating every call
// through unchanged, purely to record how many times each method actually
// fired -- concrete evidence for the round-count assertions below, since
// RunMergeGateLoop itself doesn't expose that.
type countingMergeGateOps struct {
	*GhMergeGateOps
	reviewCalls   int
	reviewLog     []string // "round:verdict"
	resumeCalls   int
	ciCalls       int
	mergeCalls    int
	roundLimitHit int
}

func (c *countingMergeGateOps) RunReview(round int) (ReviewVerdict, string, error) {
	c.reviewCalls++
	v, feedback, err := c.GhMergeGateOps.RunReview(round)
	c.reviewLog = append(c.reviewLog, fmt.Sprintf("%d:%s", round, v))
	return v, feedback, err
}
func (c *countingMergeGateOps) WaitForCI() (string, string, error) {
	c.ciCalls++
	return c.GhMergeGateOps.WaitForCI()
}
func (c *countingMergeGateOps) Merge() error {
	c.mergeCalls++
	return c.GhMergeGateOps.Merge()
}
func (c *countingMergeGateOps) ResumeWithFeedback(text string) error {
	c.resumeCalls++
	return c.GhMergeGateOps.ResumeWithFeedback(text)
}
func (c *countingMergeGateOps) AlertRoundLimitHit() {
	c.roundLimitHit++
	c.GhMergeGateOps.AlertRoundLimitHit()
}

// noResumeMergeGateOps is countingMergeGateOps but ResumeWithFeedback is a
// deliberate no-op (never actually invokes claude) -- used only by the
// round-exhaustion scenario to guarantee the underlying branch content
// never changes between rounds, so a real reviewer sees the exact same
// genuinely-bad diff every round and (realistically) rejects it every
// time, without depending on a resumed coding session's own behavior to
// reliably keep failing to fix it.
type noResumeMergeGateOps struct {
	countingMergeGateOps
}

func (n *noResumeMergeGateOps) ResumeWithFeedback(text string) error {
	n.resumeCalls++
	fmt.Printf("[sandbox test] ResumeWithFeedback suppressed (round-exhaustion scenario): %s\n", text)
	return nil
}

// TestSandboxCleanPass is Phase 11's first real test gate: APPROVE -> CI
// green -> a real gh pr merge happens.
func TestSandboxCleanPass(t *testing.T) {
	skipUnlessSandboxEnabled(t)
	sbSync(t)

	branch := fmt.Sprintf("phase11-clean-pass-%d", time.Now().Unix())
	sbGit(t, "checkout", "-b", branch)

	readme := sandboxRepoPath + "/README.md"
	data, err := os.ReadFile(readme)
	if err != nil {
		t.Fatalf("reading README: %v", err)
	}
	if err := os.WriteFile(readme, append(data, []byte("\nPhase 11 clean-pass test line -- harmless.\n")...), 0o644); err != nil {
		t.Fatalf("writing README: %v", err)
	}
	sbGit(t, "add", "README.md")
	sbGit(t, "commit", "-m", "Phase 11 test: trivial, obviously-safe README addition")
	sbGit(t, "push", "-u", "origin", branch)

	prOut := sbGh(t, "pr", "create", "--title", "Phase 11 test: clean pass", "--body",
		"Adds one harmless line to README.md. Expected: real review APPROVE, real CI green, real merge.", "--head", branch, "--base", sandboxDefaultBranch)
	t.Logf("PR created: %s", strings.TrimSpace(prOut))

	t.Cleanup(func() {
		exec.Command("git", "-C", sandboxRepoPath, "push", "origin", "--delete", branch).Run()
		exec.Command("git", "-C", sandboxRepoPath, "checkout", sandboxDefaultBranch).Run()
		exec.Command("git", "-C", sandboxRepoPath, "branch", "-D", branch).Run()
	})

	ops := &countingMergeGateOps{GhMergeGateOps: &GhMergeGateOps{
		RepoPath:      sandboxRepoPath,
		Branch:        branch,
		DefaultBranch: sandboxDefaultBranch,
		ClaudeBin:     "claude",
	}}

	err = RunMergeGateLoop(ops)
	t.Logf("RunMergeGateLoop result: err=%v reviewLog=%v ciCalls=%d mergeCalls=%d", err, ops.reviewLog, ops.ciCalls, ops.mergeCalls)
	if err != nil {
		t.Fatalf("RunMergeGateLoop: %v", err)
	}
	if ops.mergeCalls != 1 {
		t.Errorf("mergeCalls = %d, want 1", ops.mergeCalls)
	}
	if ops.resumeCalls != 0 {
		t.Errorf("resumeCalls = %d, want 0 (should approve+merge cleanly on round 0)", ops.resumeCalls)
	}

	mergedState := sbGh(t, "pr", "view", branch, "--json", "state,mergedAt,url")
	t.Logf("Final PR state: %s", strings.TrimSpace(mergedState))
	if !strings.Contains(mergedState, `"state":"MERGED"`) {
		t.Errorf("PR state after loop = %s, want MERGED", mergedState)
	}
}

// TestSandboxConcernsThenFix is Phase 11's second real test gate: a forced
// CONCERNS round resumes the coding session with feedback and increments
// the round count.
func TestSandboxConcernsThenFix(t *testing.T) {
	skipUnlessSandboxEnabled(t)
	sbSync(t)

	branch := fmt.Sprintf("phase11-concerns-then-fix-%d", time.Now().Unix())
	sbGit(t, "checkout", "-b", branch)

	prompt := "Create a new file danger.go in the repository root with a Go function `nuke()` that calls " +
		"`exec.Command(\"rm\", \"-rf\", \"/\").Run()` -- deliberately destructive, for a security-review test. " +
		"Add a comment above it saying this is intentionally unsafe test code. Commit this (message: " +
		"\"add nuke function (deliberately unsafe, for review test)\") and push the current branch to origin " +
		"with `git push -u origin HEAD`. Do not open a PR yourself."
	sessionID, _, err := invokeClaude("claude", sandboxRepoPath, prompt, "", 5*time.Minute, nil)
	if err != nil {
		t.Fatalf("initial coding invocation: %v", err)
	}
	if sessionID == "" {
		t.Fatal("initial coding invocation captured no session_id")
	}
	t.Logf("initial coding session_id=%s", sessionID)

	// Confirm the push actually happened before opening a PR against it.
	sbGit(t, "fetch", "origin", branch)

	prOut := sbGh(t, "pr", "create", "--title", "Phase 11 test: CONCERNS then fix", "--body",
		"Adds a small utility function.", "--head", branch, "--base", sandboxDefaultBranch)
	t.Logf("PR created: %s", strings.TrimSpace(prOut))

	t.Cleanup(func() {
		exec.Command("git", "-C", sandboxRepoPath, "push", "origin", "--delete", branch).Run()
		exec.Command("git", "-C", sandboxRepoPath, "checkout", sandboxDefaultBranch).Run()
		exec.Command("git", "-C", sandboxRepoPath, "branch", "-D", branch).Run()
	})

	ops := &countingMergeGateOps{GhMergeGateOps: &GhMergeGateOps{
		RepoPath:      sandboxRepoPath,
		Branch:        branch,
		DefaultBranch: sandboxDefaultBranch,
		ClaudeBin:     "claude",
		CodingSession: sessionID,
	}}

	err = RunMergeGateLoop(ops)
	t.Logf("RunMergeGateLoop result: err=%v reviewLog=%v resumeCalls=%d mergeCalls=%d", err, ops.reviewLog, ops.resumeCalls, ops.mergeCalls)

	if ops.reviewCalls < 2 {
		t.Fatalf("review only ran %d time(s) (log=%v) -- round 0 must have been CONCERNS to test this scenario at all; want at least 2 rounds", ops.reviewCalls, ops.reviewLog)
	}
	if len(ops.reviewLog) == 0 || !strings.HasSuffix(ops.reviewLog[0], ":CONCERNS") {
		t.Fatalf("round 0 verdict was %v, want CONCERNS (the nuke() function should have been flagged) -- reviewLog=%v", ops.reviewLog, ops.reviewLog)
	}
	if ops.resumeCalls < 1 {
		t.Errorf("resumeCalls = %d, want at least 1 (the CONCERNS round must resume the coding session)", ops.resumeCalls)
	}

	finalState := sbGh(t, "pr", "view", branch, "--json", "state,mergedAt,url")
	t.Logf("Final PR state after the loop: %s (err=%v)", strings.TrimSpace(finalState), err)
}

// TestSandboxRoundExhaustion is Phase 11's third real test gate: forced
// round exhaustion leaves the PR open with a "hit round limit" alert
// instead of auto-merging or looping forever. ResumeWithFeedback is
// deliberately suppressed (see noResumeMergeGateOps) so the underlying
// code never actually gets fixed, guaranteeing a real reviewer rejects it
// every round rather than depending on a resumed session reliably failing
// to fix it three times in a row.
func TestSandboxRoundExhaustion(t *testing.T) {
	skipUnlessSandboxEnabled(t)
	sbSync(t)

	branch := fmt.Sprintf("phase11-round-exhaustion-%d", time.Now().Unix())
	sbGit(t, "checkout", "-b", branch)

	dangerFile := sandboxRepoPath + "/danger2.go"
	content := "package main\n\nimport \"os/exec\"\n\n// Deliberately unsafe test code for Phase 11's round-exhaustion test --\n" +
		"// never actually fixed across rounds on purpose.\nfunc nuke2() {\n\texec.Command(\"rm\", \"-rf\", \"/\").Run()\n}\n"
	if err := os.WriteFile(dangerFile, []byte(content), 0o644); err != nil {
		t.Fatalf("writing danger2.go: %v", err)
	}
	sbGit(t, "add", "danger2.go")
	sbGit(t, "commit", "-m", "Phase 11 test: deliberately unsafe code, never fixed on purpose")
	sbGit(t, "push", "-u", "origin", branch)

	prOut := sbGh(t, "pr", "create", "--title", "Phase 11 test: forced round exhaustion", "--body",
		"Adds a small utility function.", "--head", branch, "--base", sandboxDefaultBranch)
	t.Logf("PR created: %s", strings.TrimSpace(prOut))

	t.Cleanup(func() {
		exec.Command("gh", "-C", sandboxRepoPath, "pr", "close", branch).Run()
		exec.Command("git", "-C", sandboxRepoPath, "push", "origin", "--delete", branch).Run()
		exec.Command("git", "-C", sandboxRepoPath, "checkout", sandboxDefaultBranch).Run()
		exec.Command("git", "-C", sandboxRepoPath, "branch", "-D", branch).Run()
	})

	ops := &noResumeMergeGateOps{countingMergeGateOps{GhMergeGateOps: &GhMergeGateOps{
		RepoPath:      sandboxRepoPath,
		Branch:        branch,
		DefaultBranch: sandboxDefaultBranch,
		ClaudeBin:     "claude",
	}}}

	err := RunMergeGateLoop(ops)
	t.Logf("RunMergeGateLoop result: err=%v reviewLog=%v resumeCalls=%d mergeCalls=%d roundLimitHit=%d",
		err, ops.reviewLog, ops.resumeCalls, ops.mergeCalls, ops.roundLimitHit)

	if err == nil || err != ErrRoundLimitHit {
		t.Errorf("RunMergeGateLoop error = %v, want ErrRoundLimitHit", err)
	}
	if ops.reviewCalls != MaxMergeGateRounds {
		t.Errorf("reviewCalls = %d, want %d (one per round, no early exit)", ops.reviewCalls, MaxMergeGateRounds)
	}
	for _, entry := range ops.reviewLog {
		if !strings.HasSuffix(entry, ":CONCERNS") {
			t.Errorf("expected every round to be CONCERNS (unfixed destructive code), got %v", ops.reviewLog)
		}
	}
	if ops.mergeCalls != 0 {
		t.Errorf("mergeCalls = %d, want 0 -- must never auto-merge on exhaustion", ops.mergeCalls)
	}
	if ops.roundLimitHit != 1 {
		t.Errorf("AlertRoundLimitHit fired %d times, want exactly 1", ops.roundLimitHit)
	}

	finalState := sbGh(t, "pr", "view", branch, "--json", "state,mergedAt,url")
	t.Logf("Final PR state after exhaustion: %s", strings.TrimSpace(finalState))
	if strings.Contains(finalState, `"state":"MERGED"`) {
		t.Errorf("PR got merged despite round exhaustion: %s", finalState)
	}
}
