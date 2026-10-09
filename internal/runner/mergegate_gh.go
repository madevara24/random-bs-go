package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GhMergeGateOps is the real MergeGateOps: a fresh claude -p review
// session (no --resume, no vault access), gh run list for CI (Actions
// API, never gh pr checks --watch -- confirmed structurally broken, 403,
// per the bash design's own hard-won finding), and gh pr merge executed
// by the runner itself.
type GhMergeGateOps struct {
	RepoPath string

	// PRURL is the PR's own URL (the task note's real pr_url) -- every
	// `gh pr` call (view/comment/merge) is keyed off this, never a branch
	// name: the runner's own constructed branch name (see ProcessTask's
	// branchName in runner.go) is stable per task, but still only tracks
	// what the runner itself last fetched/checked out -- the PR's actual
	// head could have moved past that if a resumed session rebased or
	// reconciled it against the default branch (see
	// resumeReconciliationNotice) before this merge-gate loop ever ran.
	PRURL         string
	DefaultBranch string
	ClaudeBin     string
	CodingSession string // the original coding session's session_id, to resume with feedback

	// ReviewTimeout bounds one review invocation -- a review session isn't
	// covered by the coding session's own idle watchdog, so it needs its
	// own bound against a hang. Defaults to 5 minutes if zero.
	ReviewTimeout time.Duration

	// headRef caches the PR's actual head branch name, resolved once from
	// PRURL via `gh pr view --json headRefName` -- needed anywhere a real
	// git ref is required (gh run list --branch, gitDiff), since PRURL
	// itself isn't a ref git or the Actions API understands.
	headRef string
}

func (g *GhMergeGateOps) reviewTimeout() time.Duration {
	if g.ReviewTimeout > 0 {
		return g.ReviewTimeout
	}
	return 5 * time.Minute
}

func (g *GhMergeGateOps) runGh(args ...string) (string, error) {
	cmd := exec.Command("gh", args...)
	cmd.Dir = g.RepoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("gh %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// headBranch resolves and caches the PR's actual head branch name --
// resolved once per GhMergeGateOps (it's reused across every round of the
// same loop), so repeated calls across a round's RunReview+WaitForCI don't
// each pay for their own `gh pr view`.
func (g *GhMergeGateOps) headBranch() (string, error) {
	if g.headRef != "" {
		return g.headRef, nil
	}
	out, err := g.runGh("pr", "view", g.PRURL, "--json", "headRefName", "--jq", ".headRefName")
	if err != nil {
		return "", fmt.Errorf("resolving head branch for %s: %w", g.PRURL, err)
	}
	g.headRef = strings.TrimSpace(out)
	return g.headRef, nil
}

// RunReview implements MergeGateOps -- fetches the PR body, current diff,
// and (round 2+) the existing comment thread fresh on every call, so a
// later round genuinely sees whatever an earlier round's resumed coding
// session pushed, not a stale pre-fix snapshot.
func (g *GhMergeGateOps) RunReview(round int) (ReviewVerdict, string, error) {
	prBody, err := fetchPRBody(g.RepoPath, g.PRURL)
	if err != nil {
		return "", "", fmt.Errorf("fetching PR body: %w", err)
	}
	branch, err := g.headBranch()
	if err != nil {
		return "", "", fmt.Errorf("resolving head branch: %w", err)
	}
	diff, err := gitDiff(g.RepoPath, g.DefaultBranch, branch)
	if err != nil {
		return "", "", fmt.Errorf("fetching diff: %w", err)
	}

	var priorComments string
	if round > 0 {
		// Best-effort -- reading the thread failing shouldn't block the
		// review itself, it just loses this round's "was my feedback
		// addressed" framing.
		priorComments, _ = fetchPRComments(g.RepoPath, g.PRURL)
	}

	verdict, feedback, err := reviewOnce(g.ClaudeBin, g.RepoPath, g.reviewTimeout(), round, prBody, diff, priorComments)
	if err != nil {
		return "", "", err
	}

	// Post the verdict as a real PR comment -- best-effort: a comment-post
	// failure shouldn't fail the review itself (the verdict was still
	// genuinely reached), but is worth surfacing.
	if _, err := g.runGh("pr", "comment", g.PRURL, "--body", fmt.Sprintf("**Round %d review: %s**\n\n%s", round, verdict, feedback)); err != nil {
		fmt.Printf("[runner] mergegate: posting review comment failed (round %d): %v\n", round, err)
	}

	return verdict, feedback, nil
}

// reviewOnce is the actual claude -p review invocation + verdict parsing,
// pulled out of RunReview so it's directly testable with hand-built
// prBody/diff content (see TestRealReviewInvocation) without needing a
// real PR/gh state to fetch from. It's this package's other claude
// invocation, separate from invokeClaude -- it runs --output-format json,
// not stream-json, so it never streams a rate_limit_event line and isn't
// covered by this task's usage-limit detection/pause (RBG-24, out of
// scope). A review session hitting the limit mid-loop would surface as a
// plain invocation error here, handled (or not) by whatever already calls
// RunReview, not by pause.State.
func reviewOnce(claudeBin, repoPath string, timeout time.Duration, round int, prBody, diff, priorComments string) (ReviewVerdict, string, error) {
	prompt := buildReviewPrompt(round, prBody, diff, priorComments)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{"-p", prompt, "--output-format", "json", "--dangerously-skip-permissions"}
	cmd := exec.CommandContext(ctx, claudeBinOrDefault(claudeBin), args...)
	cmd.Dir = repoPath
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("review invocation: %w", err)
	}

	var result struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return "", "", fmt.Errorf("parsing review invocation output: %w\n%s", err, out)
	}

	verdict, feedback := parseReviewVerdict(result.Result)
	return verdict, feedback, nil
}

// buildReviewPrompt asks for a specific, parseable format: a VERDICT line
// followed by feedback -- review sessions get the task's PR description
// and diff, never the task note or its copy (no vault access, no shared
// context with the coding session).
func buildReviewPrompt(round int, prBody, diff, priorComments string) string {
	var b strings.Builder
	if round > 0 {
		b.WriteString("This is a follow-up review round -- the coding session already attempted a fix based on your (or CI's) previous feedback. Check whether your own prior feedback below was actually addressed before deciding.\n\n")
		if priorComments != "" {
			b.WriteString("## Prior review comment thread\n\n")
			b.WriteString(priorComments)
			b.WriteString("\n\n")
		}
	}
	b.WriteString("You are reviewing a pull request as an independent reviewer. You have zero shared context with the session that wrote this code -- judge it purely on the PR description and diff below. Do not run any git/gh commands yourself; you are given everything you need.\n\n")
	b.WriteString("Respond with your verdict as the exact first line `VERDICT: APPROVE` or `VERDICT: CONCERNS`, followed by a blank line, then your reasoning (and, if CONCERNS, exactly what needs to change).\n\n")
	b.WriteString("## PR description\n\n")
	b.WriteString(prBody)
	b.WriteString("\n\n## Diff\n\n```diff\n")
	b.WriteString(diff)
	b.WriteString("\n```\n")
	return b.String()
}

func parseReviewVerdict(text string) (ReviewVerdict, string) {
	trimmed := strings.TrimSpace(text)
	lines := strings.SplitN(trimmed, "\n", 2)
	first := strings.ToUpper(strings.TrimSpace(lines[0]))
	feedback := trimmed
	if len(lines) > 1 {
		feedback = strings.TrimSpace(lines[1])
	}
	if strings.Contains(first, "APPROVE") {
		return VerdictApprove, feedback
	}
	// Anything else (including a malformed/missing VERDICT line) is
	// treated as CONCERNS -- conservative default, never silently approve
	// on an unparseable response.
	return VerdictConcerns, feedback
}

// ciRun is the subset of `gh run list`'s JSON this package needs.
type ciRun struct {
	DatabaseID int64  `json:"databaseId"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

// hasCIWorkflows reports whether the local clone at repoPath has any
// GitHub Actions workflow files at all. Checked against the repo itself
// so this can never drift from what's actually configured.
func hasCIWorkflows(repoPath string) bool {
	for _, ext := range []string{"yml", "yaml"} {
		matches, _ := filepath.Glob(filepath.Join(repoPath, ".github", "workflows", "*."+ext))
		if len(matches) > 0 {
			return true
		}
	}
	return false
}

// WaitForCI implements MergeGateOps -- polls `gh run list` (Actions API)
// for the branch's latest run until it completes. Never uses
// `gh pr checks --watch` (the Checks API): confirmed structurally broken
// (403 Resource not accessible), not flaky, per the bash design's own
// hard-won finding.
//
// If the repo has no workflow files at all, there's nothing CI will ever
// report -- return success immediately instead of polling for 30 minutes.
func (g *GhMergeGateOps) WaitForCI() (string, string, error) {
	if !hasCIWorkflows(g.RepoPath) {
		return "success", "", nil
	}
	branch, err := g.headBranch()
	if err != nil {
		return "", "", fmt.Errorf("resolving head branch: %w", err)
	}
	deadline := time.Now().Add(30 * time.Minute)
	for {
		out, err := g.runGh("run", "list", "--branch", branch, "--json", "databaseId,status,conclusion", "--limit", "1")
		if err != nil {
			return "", "", fmt.Errorf("gh run list: %w", err)
		}
		var runs []ciRun
		if err := json.Unmarshal([]byte(out), &runs); err != nil {
			return "", "", fmt.Errorf("parsing gh run list output: %w\n%s", err, out)
		}
		if len(runs) == 0 {
			if time.Now().After(deadline) {
				return "", "", fmt.Errorf("no CI run ever appeared for branch %s within the wait window", branch)
			}
			time.Sleep(10 * time.Second)
			continue
		}
		run := runs[0]
		if run.Status != "completed" {
			if time.Now().After(deadline) {
				return "", "", fmt.Errorf("CI run %d for branch %s did not complete within the wait window", run.DatabaseID, branch)
			}
			time.Sleep(10 * time.Second)
			continue
		}
		if run.Conclusion == "success" {
			return run.Conclusion, "", nil
		}
		logOut, _ := g.runGh("run", "view", fmt.Sprintf("%d", run.DatabaseID), "--log-failed")
		return run.Conclusion, logOut, nil
	}
}

// Merge implements MergeGateOps.
func (g *GhMergeGateOps) Merge() error {
	_, err := g.runGh("pr", "merge", g.PRURL, "--squash")
	return err
}

// ResumeWithFeedback implements MergeGateOps -- resumes the *coding*
// session (not the review session, which is always fresh) with the
// review's CONCERNS or CI's failure text.
func (g *GhMergeGateOps) ResumeWithFeedback(text string) error {
	_, _, err := invokeClaude(claudeBinOrDefault(g.ClaudeBin), g.RepoPath, text, g.CodingSession, 15*time.Minute, nil)
	return err
}

// AlertRoundLimitHit implements MergeGateOps.
func (g *GhMergeGateOps) AlertRoundLimitHit() {
	fmt.Printf("[runner] mergegate: hit round limit (%d) on PR %s without a clean merge\n", MaxMergeGateRounds, g.PRURL)
}

func claudeBinOrDefault(bin string) string {
	if bin == "" {
		return "claude"
	}
	return bin
}

// fetchPRBody reads the PR description via `gh pr view` -- the review
// session's task summary: review sessions read the PR's own description,
// never the task note or its copy. Returns an error (caller falls back to
// the Work Log) if gh isn't functional here at all, e.g. a local-only
// disposable repo with no GitHub remote.
func fetchPRBody(repoPath, prURL string) (string, error) {
	cmd := exec.Command("gh", "pr", "view", prURL, "--json", "body", "--jq", ".body")
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr view %s: %w\n%s", prURL, err, out)
	}
	return string(out), nil
}

// fetchPRComments reads the PR's existing comment thread -- used from
// round 2+ so the reviewer can check whether its own prior feedback was
// actually addressed.
func fetchPRComments(repoPath, prURL string) (string, error) {
	cmd := exec.Command("gh", "pr", "view", prURL, "--json", "comments", "--jq", ".comments[].body")
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr view %s (comments): %w\n%s", prURL, err, out)
	}
	return string(out), nil
}

// gitDiff is the review session's other input -- the actual code change.
// Diffs against a freshly fetched origin/<branch>, never the local branch
// ref directly: by review time the runner's own local clone may never have
// checked the branch out, or may have since deleted/moved past it (e.g. a
// prior round's coding session left it behind), while the branch always
// still exists on origin as long as the PR is open.
func gitDiff(repoPath, base, branch string) (string, error) {
	if out, err := exec.Command("git", "-C", repoPath, "fetch", "origin", branch).CombinedOutput(); err != nil {
		return string(out), fmt.Errorf("git fetch origin %s: %w", branch, err)
	}
	out, err := exec.Command("git", "-C", repoPath, "diff", base+"...FETCH_HEAD").CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git diff %s...FETCH_HEAD (branch %s): %w", base, branch, err)
	}
	return string(out), nil
}
