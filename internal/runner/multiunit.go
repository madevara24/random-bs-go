// The multi-unit path's entry points: triage (deciding whether a fresh
// task should run as multi-unit) and processMultiUnit (ProcessTask's
// branch once a task is triaged multi-unit). Both gated behind
// Deps.MultiUnitEnabled -- off by default, see internal/config/config.go's
// MultiUnitEnabled doc comment. Sibling to planner.go, which this file
// hands off to for the "no ## Plan yet" case.
package runner

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/jev"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/plangate"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// triageTimeout bounds how long triagePath waits on Jev's systemone
// endpoint -- a hung triage call must never stall a fresh task's setup
// indefinitely.
const triageTimeout = 30 * time.Second

// oneLineMaxRunes is the rune cap every Jev-derived Runner Log line goes
// through oneLine with.
const oneLineMaxRunes = 200

// oneLine collapses s to one line (newlines/tabs/carriage returns become
// spaces) and cuts it to at most max runes, appending "…" if it was cut.
// jev.Client.Ask's own error text embeds Jev's raw HTTP response body
// verbatim, which can contain newlines -- and notetask.Parse hard-fails on
// a multi-line Runner Log entry (see notetask's parseRunnerLog), which
// would make the whole note unreadable. Every Jev-derived string this
// package writes to the Runner Log goes through this first.
func oneLine(s string, max int) string {
	replacer := strings.NewReplacer("\n", " ", "\t", " ", "\r", " ")
	out := replacer.Replace(s)
	runes := []rune(out)
	if len(runes) <= max {
		return out
	}
	if max <= 0 {
		return ""
	}
	return string(runes[:max-1]) + "…"
}

// triagePath decides whether job's task should run as multi-unit, asking
// Jev only when every one of these holds: the flag is on, this is a fresh
// run (no session_id yet), and the author never pinned multi_unit
// themselves. Every other case returns (false, "") -- nothing happened
// worth recording, and in particular an author override is honored
// exactly, with zero Jev requests.
func triagePath(deps Deps, job worker.Job, note *notetask.Note) (multiUnit bool, logLine string) {
	if !deps.MultiUnitEnabled {
		return false, ""
	}
	if note.Frontmatter.SessionID != nil && *note.Frontmatter.SessionID != "" {
		return false, ""
	}
	if note.Frontmatter.MultiUnit != nil {
		return *note.Frontmatter.MultiUnit, ""
	}

	state := map[string]any{
		"task": map[string]any{
			"title":  taskTitle(job.NotePath),
			"prompt": note.Prompt,
		},
		"repo": job.Repo,
	}

	answers, askErr := askJev(deps.Jev, state, plangate.TriageQuestion(), triageTimeout)
	multiUnit, logLine = plangate.DecideTriage(answers, askErr)
	return multiUnit, oneLine(logLine, oneLineMaxRunes)
}

// askJev is triagePath/runPlanGate's shared call shape: a nil client
// (Deps.Jev never wired in) is treated exactly like jev.ErrNoAPIKey rather
// than panicking, the same fail-safe-to-single-session/always-stop
// behavior an empty TYPESAFE_API_KEY produces via the real client.
func askJev(client *jev.Client, state any, questions map[string]jev.Question, timeout time.Duration) (map[string]jev.Answer, error) {
	if client == nil {
		return nil, jev.ErrNoAPIKey
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return client.Ask(ctx, state, questions)
}

// multiUnitContinuing reports whether note is a multi-unit task already
// past its first ProcessTask call -- the planner already wrote a Plan, so
// any later claim (a plan_approved/plan_rejected reclaim, RBG-47, or a
// blocker_resolved resume mid-unit-loop) is picking up a branch that
// already exists in the clone, never a fresh claim that needs
// pullAndBranch's from-scratch checkout. Gated on deps.MultiUnitEnabled so
// this can never flip runner.go's own resume to true for a flag-off run
// (where processMultiUnit itself never runs) and fall through into the
// single-session path's own SessionID dereference.
func multiUnitContinuing(deps Deps, note *notetask.Note) bool {
	return deps.MultiUnitEnabled && note.Frontmatter.MultiUnit != nil && *note.Frontmatter.MultiUnit && note.HasPlan
}

// isMultiUnit is multiUnitContinuing's own condition minus the HasPlan
// check -- true for a multi-unit task regardless of whether the planner
// has written a Plan yet, used by ProcessTask's branch setup to decide
// whether a pre-existing task/<slug> branch (left behind by a planner round
// that blocked before writing one) should be resumed rather than
// recreated.
func isMultiUnit(deps Deps, note *notetask.Note) bool {
	return deps.MultiUnitEnabled && note.Frontmatter.MultiUnit != nil && *note.Frontmatter.MultiUnit
}

// processMultiUnit is ProcessTask's branch for a task triaged multi-unit:
// "no ## Plan yet" runs the planner through the plan gate (which itself
// hands a clean pass straight into runUnitLoop, see planner.go's
// runPlanGate), "a plan is waiting on human review" resolves that
// decision (resolvePlanReview, planner.go -- a plan_approved/plan_rejected
// reclaim, or blocker_resolved before the gate ever seeded units), and
// "units already seeded, not all done" re-enters runUnitLoop directly --
// the normal resume path, and also how a task whose status flipped
// blocked -> blocker_resolved (main.go's auto-resume, e.g. after a usage
// limit cleared mid-unit) picks back up at current_unit: ProcessTask's own
// resume branch reaches here exactly the same way regardless of which
// status string got it there.
func processMultiUnit(deps Deps, reporter ActivityReporter, job worker.Job, note *notetask.Note, repoCfg config.RepoConfig, branchName string, codingEng, reviewEng engine.Engine) error {
	if !note.HasPlan || strings.TrimSpace(note.Plan) == "" {
		return runPlanner(deps, reporter, job, note, repoCfg, branchName, codingEng, reviewEng)
	}
	if len(note.Frontmatter.Units) == 0 {
		return resolvePlanReview(deps, reporter, job, note, repoCfg, branchName, codingEng, reviewEng)
	}
	if !allUnitsDone(note.Frontmatter.Units) {
		return runUnitLoopAndFinish(deps, reporter, job, repoCfg, branchName, codingEng, reviewEng)
	}
	// Every unit was already done as of this read -- a prior run reached
	// current_unit: nil but never got as far as finishing (the PR-open
	// step, or a crash in between). Finish now rather than silently
	// stopping, unless it already finished (pr_url is already set).
	if derefStr(note.Frontmatter.PRURL) == "" {
		return finishMultiUnitTask(deps, reporter, job, repoCfg, branchName, codingEng, reviewEng)
	}
	return nil
}

// runUnitLoopAndFinish runs the unit loop, then -- only if it returned nil
// because every unit reached done -- finishes the task: opens its one PR
// and, for an auto_merge task, runs the merge gate. Both of
// processMultiUnit's own entry into the unit loop and runPlanGate's
// (planner.go) clean-pass hand-off go through this, never runUnitLoop
// directly, so finishing only ever happens in this one place.
func runUnitLoopAndFinish(deps Deps, reporter ActivityReporter, job worker.Job, repoCfg config.RepoConfig, branchName string, codingEng, reviewEng engine.Engine) error {
	if err := runUnitLoop(deps, reporter, job, repoCfg, branchName, codingEng, reviewEng); err != nil {
		return err
	}
	return finishMultiUnitTask(deps, reporter, job, repoCfg, branchName, codingEng, reviewEng)
}

// openTaskPR pushes branchName to origin and opens the one PR a multi-unit
// task's finish step ever opens for it: `gh pr create`, run inside the
// clone, with title/body supplied by the caller (see
// finishMultiUnitTask's taskTitle/multiUnitPRBody). Returns the PR's URL
// (gh pr create prints it, and nothing else, to stdout on success).
func openTaskPR(repoCfg config.RepoConfig, branchName, title, body string) (prURL string, err error) {
	if err := runGitCommand(repoCfg.Path, "push", "-u", "origin", branchName); err != nil {
		return "", fmt.Errorf("pushing task branch %s: %w", branchName, err)
	}

	cmd := exec.Command("gh", "pr", "create", "--base", repoCfg.DefaultBranch, "--head", branchName, "--title", title, "--body", body)
	cmd.Dir = repoCfg.Path
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("gh pr create: %w\n%s", err, out)
	}
	return strings.TrimSpace(string(out)), nil
}

// planSummary is the prose the planner wrote in `## Plan` before its
// ```yaml units block -- the human-readable half of the plan, as opposed
// to the machine-readable unit list ParsePlanUnits reads. Falls back to
// the whole trimmed Plan section if there's no fenced block to cut before
// (shouldn't happen for a plan that ever cleared the plan gate, but this
// is PR-description text, not something worth hard-failing finish over).
func planSummary(plan string) string {
	if idx := strings.Index(plan, "```yaml"); idx != -1 {
		return strings.TrimSpace(plan[:idx])
	}
	return strings.TrimSpace(plan)
}

// multiUnitPRBody is the PR description finishMultiUnitTask opens the
// task's one PR with: the plan's own summary (what the task set out to
// do), then the full accumulated `## Handoff` (what every unit actually
// did, and anything later units -- or this PR's own reviewer -- need to
// know), so a reviewer who's never seen the task note still has both the
// intent and the result.
func multiUnitPRBody(note *notetask.Note) string {
	var b strings.Builder
	if summary := planSummary(note.Plan); summary != "" {
		b.WriteString(summary)
		b.WriteString("\n\n")
	}
	b.WriteString("## Handoff\n\n")
	b.WriteString(strings.TrimSpace(note.Handoff))
	b.WriteString("\n")
	return b.String()
}

// lastUnitSessionID is the last plan-ordered unit's own session_id --
// what the merge gate's ResumeWithFeedback resumes on a CONCERNS or CI
// failure for a multi-unit task's finish-time integration check, since
// that unit's session is the one still sitting on the finished branch tip.
func lastUnitSessionID(units []notetask.UnitState) string {
	if len(units) == 0 {
		return ""
	}
	return units[len(units)-1].SessionID
}

// finishMultiUnitTask runs once every unit has reached done: opens the
// task's one PR, writes pr_url and status: done with a Runner Log line,
// fires deps.OnTerminal exactly as the single-session path does, and --
// for an auto_merge task -- runs the merge gate with the integration check
// in place of the whole-diff review (see GhMergeGateOps.IntegrationCheck),
// resuming the last unit's session_id on CONCERNS or CI failure.
func finishMultiUnitTask(deps Deps, reporter ActivityReporter, job worker.Job, repoCfg config.RepoConfig, branchName string, codingEng, reviewEng engine.Engine) error {
	note, err := deps.Vault.ReadNote(job.NotePath)
	if err != nil {
		return blockFinishFailure(deps, job, repoCfg, branchName, "reading note to finish",
			fmt.Errorf("reading note to finish multi-unit %s: %w", job.Slug, err))
	}

	reporter.SetStage("finish: opening PR")
	prURL, err := openTaskPR(repoCfg, branchName, taskTitle(job.NotePath), multiUnitPRBody(note))
	if err != nil {
		return blockFinishFailure(deps, job, repoCfg, branchName, "opening pull request", err)
	}

	discordThreadID := threadIDOf(note)
	werr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: done %s (multi-unit PR opened)", job.Slug), func(n *notetask.Note) error {
		n.Frontmatter.Status = "done"
		n.Frontmatter.PRURL = &prURL
		notetask.AppendRunnerLog(n, "done", time.Now())
		return nil
	})
	if werr != nil {
		// The PR is already open at prURL -- name it in the alert so it
		// isn't orphaned: a human resolving this block needs to find it
		// without digging through gh themselves.
		return blockFinishFailure(deps, job, repoCfg, branchName, "writing done+pr_url",
			fmt.Errorf("writing done+pr_url for %s (PR already opened at %s): %w", job.Slug, prURL, werr))
	}

	autoMerge := resolveAutoMerge(note.Frontmatter.AutoMerge, repoCfg.AutoMergeDefault)

	if deps.OnTerminal != nil {
		deps.OnTerminal(job, "done", note.WorkLog, autoMerge, prURL, discordThreadID, nil, codingEng.Name(), 0)
	}

	if autoMerge && deps.MergeGateFactory != nil {
		runMergeGateForTask(deps, reporter, repoCfg, job, branchName, lastUnitSessionID(note.Frontmatter.Units), prURL, discordThreadID, codingEng, reviewEng, true, note.Handoff)
	}

	return nil
}

// blockFinishFailure is finishMultiUnitTask's one failure path, covering
// every way finishing can go wrong once every unit is already done:
// re-reading the note, opening the task's PR, or writing back done/pr_url
// once the PR is open. Blocks with the branch preserved
// (preserveBlockedWork) and the usual Discord alert -- same "always write
// blocked, always alert" contract every other blocked family in this
// package follows. label names the stage for the alert and Runner Log
// line; failErr is the underlying error.
func blockFinishFailure(deps Deps, job worker.Job, repoCfg config.RepoConfig, branchName, label string, failErr error) error {
	var discordThreadID string
	var blockerN int
	writeErr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: blocked %s (%s)", job.Slug, label), func(n *notetask.Note) error {
		discordThreadID = threadIDOf(n)
		n.Frontmatter.Status = "blocked"
		blockerN = notetask.AppendBlocker(n, "open_pr_failure: "+label, time.Now())
		notetask.AppendRunnerLog(n, fmt.Sprintf("blocked: %s failed: %s", label, oneLine(failErr.Error(), oneLineMaxRunes)), time.Now())
		return nil
	})
	if writeErr != nil {
		fmt.Printf("[runner] task %s: failed to write blocked status after %s failure: %v\n", job.Slug, label, writeErr)
	}

	var preserveErrText string
	if err := preserveBlockedWork(repoCfg.Path, branchName); err != nil {
		preserveErrText = err.Error()
		fmt.Printf("[runner] task %s: failed to preserve blocked work: %v\n", job.Slug, err)
	}

	payload := AlertPayload{
		Slug:            job.Slug,
		Repo:            job.Repo,
		Scenario:        ScenarioOpenPRFailure,
		Stage:           label,
		BlockerN:        blockerN,
		DiscordThreadID: discordThreadID,
		ExitCode:        -1,
		ExitErrText:     failErr.Error(),
		PRState:         "unknown (" + label + " is what failed)",
		LogPointer:      "(per-task transcript logging not yet built -- see Design - Runner.md's Open section)",
		PreserveErr:     preserveErrText,
		RepoPath:        repoCfg.Path,
	}
	if deps.OnBlocked != nil {
		deps.OnBlocked(job, payload)
	}

	fmt.Printf("[runner] task %s: blocked (%s failed): %v\n", job.Slug, label, failErr)
	return fmt.Errorf("runner: task %s blocked (%s failed): %w", job.Slug, label, failErr)
}
