// A single unit's review: a fresh (no --resume, no vault access) review
// session judging only that unit's own diff against its own acceptance
// criteria -- the unit-loop counterpart to mergegate_gh.go's reviewOnce,
// which judges a whole PR.
package runner

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/notetask"
)

// localGitDiff diffs baseSHA..headSHA in repoPath -- both local commits on
// the same clone the coding session just worked in, never fetched from
// anywhere, since a unit's work is never pushed before this review runs.
func localGitDiff(repoPath, baseSHA, headSHA string) (string, error) {
	out, err := exec.Command("git", "-C", repoPath, "diff", baseSHA+".."+headSHA).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git diff %s..%s (in %s): %w\n%s", baseSHA, headSHA, repoPath, err, out)
	}
	return string(out), nil
}

// buildUnitReviewPrompt asks for the same parseable VERDICT format the
// merge gate's buildReviewPrompt does -- the unit's own acceptance and diff
// only, no PR description, no prior comment thread (a unit review is
// always a fresh, single-shot judgment).
func buildUnitReviewPrompt(unit notetask.PlanUnit, diff string) string {
	var b strings.Builder
	b.WriteString("You are reviewing one unit of a larger multi-unit task as an independent reviewer. You have zero shared context with the session that wrote this code -- judge it purely on the acceptance criteria and diff below. The diff below is the complete change and everything needed to judge it: do not read repository files, do not run any command (including git/gh), and do not run tests -- the unit's verify command has already passed on this commit before the review started.\n\n")
	b.WriteString("Respond with your verdict as the exact first line `VERDICT: APPROVE` or `VERDICT: CONCERNS`, followed by a blank line, then your reasoning (and, if CONCERNS, exactly what needs to change).\n\n")
	fmt.Fprintf(&b, "## Unit: %s\n\n%s\n\n", unit.Title, unit.Accept)
	b.WriteString("## Diff\n\n```diff\n")
	b.WriteString(diff)
	b.WriteString("\n```\n")
	return b.String()
}

// reviewUnit runs a fresh review session judging unit's own diff
// (baseSHA..headSHA) against its own acceptance criteria, reusing
// parseReviewVerdict (mergegate_gh.go) for the exact same APPROVE/CONCERNS
// parsing the merge gate's review uses. A usage limit hit during this
// invocation surfaces as a wrapped *engine.UsageLimitError the caller can
// recover with errors.As -- runUnitLoop's review step trips the review
// engine's pause on that and leaves the unit at review, same as any other
// error here leaves it unresolved for a later retry.
func reviewUnit(eng engine.Engine, repoPath string, unit notetask.PlanUnit, baseSHA, headSHA string, timeout time.Duration) (ReviewVerdict, string, error) {
	diff, err := localGitDiff(repoPath, baseSHA, headSHA)
	if err != nil {
		return "", "", fmt.Errorf("runner: diffing unit %s (%s..%s): %w", unit.ID, baseSHA, headSHA, err)
	}

	prompt := buildUnitReviewPrompt(unit, diff)
	cmd := eng.BuildReviewCommand(repoPath, prompt)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return "", "", fmt.Errorf("runner: starting unit %s review invocation: %w", unit.ID, err)
	}

	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	var waitErr error
	select {
	case waitErr = <-waitDone:
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-waitDone
		return "", "", fmt.Errorf("runner: unit %s review invocation: timed out after %s", unit.ID, timeout)
	}

	if classified := eng.ClassifyExit(waitErr, stderr.String(), engine.RunState{}); classified != nil {
		return "", "", fmt.Errorf("runner: unit %s review invocation: %w\n%s", unit.ID, classified, stderr.String())
	}

	answer, err := eng.ParseReviewAnswer(stdout.Bytes())
	if err != nil {
		return "", "", fmt.Errorf("runner: unit %s review: %w", unit.ID, err)
	}

	verdict, feedback := parseReviewVerdict(answer)
	return verdict, feedback, nil
}
