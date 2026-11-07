// The planner session and the plan gate: runPlanner resolves and invokes
// the planner engine (round-trip through the note-copy, exactly like a
// coding run), validates whatever plan it writes, and retries up to 3
// rounds before giving up; runPlanGate then decides, via plangate.HardRules
// plus one Jev call, whether a valid plan can seed its units straight away
// or must wait for a human.
package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/jev"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/notify"
	"github.com/madevara24/random-bs-go/internal/pause"
	"github.com/madevara24/random-bs-go/internal/plangate"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// maxPlanRounds is how many times runPlanner will re-run the planner
// engine on an unusable plan (ParsePlanUnits failed, or a unit has an
// empty id/title/verify) before giving up and blocking the task.
const maxPlanRounds = 3

// gateTimeout bounds how long runPlanGate waits on Jev's systemone
// endpoint for the gate's own call -- same bound as triagePath's, just a
// separate constant since the two calls are conceptually distinct.
const gateTimeout = 30 * time.Second

// plannerCompletionContract is the deliverable every planner session is
// held to -- restated in every round's prompt, same discipline as
// runner.go's prCompletionContract for coding sessions.
const plannerCompletionContract = "Read this repository to understand it, then write nothing into `%[1]s` but the `## Plan` section: a short summary, then one ```yaml fenced block containing a YAML list of units, each with `id`, `title`, `paths`, `depends_on`, `accept`, and `verify` (a single runnable command that verifies that unit's own work is correct). Every unit must be small enough for one coding session to finish start-to-finish. If you're genuinely unsure about how to split or scope the work, optionally add a second ```yaml fenced block right after the first, `questions:`, mapping a question id to `{instructions, options: {name: description}}`. Set `status` to `done` in `%[1]s` once you're finished. Do not change any other file, do not commit, do not push, and do not open a pull request -- this is a planning session only, not a coding session."

// buildPlannerPrompt constructs what gets passed to the planner engine for
// round (1-indexed) -- copyName is the note-copy's filename, same role as
// buildPrompt's own copyName for a coding run. For round > 1, it looks up
// the PRECEDING round's own review text (notetask.CurrentRoundReview) --
// non-empty means a human rejected that round with written feedback
// (resolvePlanReview's replan path), which takes priority over the
// generic "missing/invalid unit fields" wording used when runPlanner's own
// retry loop bumped the round instead.
func buildPlannerPrompt(note *notetask.Note, copyName string, round int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "This is a planner session (round %d) for a task tracked in the file `%s` in this repository's root (already git-ignored -- do not commit it).\n\n", round, copyName)
	fmt.Fprintf(&b, plannerCompletionContract, copyName)
	b.WriteString("\n\n")
	if round > 1 {
		if feedback := strings.TrimSpace(notetask.CurrentRoundReview(note.PlanReview, round-1)); feedback != "" {
			fmt.Fprintf(&b, "A previous round's plan was rejected by a human reviewer. Their feedback:\n\n%s\n\nRe-read the task below and produce a better plan that addresses it.\n\n", feedback)
		} else {
			b.WriteString("A previous round's plan was rejected as unusable (missing/invalid unit fields) -- re-read the task below and produce a better plan this time.\n\n")
		}
	}
	fmt.Fprintf(&b, "Task:\n%s\n", note.Prompt)
	return b.String()
}

// validatePlanUnits returns an error naming the first unit with an empty
// id, title, or verify -- the three fields notetask.ParsePlanUnits itself
// doesn't check but a plan gate/unit loop can't run without.
func validatePlanUnits(units []notetask.PlanUnit) error {
	for _, u := range units {
		if strings.TrimSpace(u.ID) == "" || strings.TrimSpace(u.Title) == "" || strings.TrimSpace(u.Verify) == "" {
			return fmt.Errorf("plan unit %q has an empty id, title, or verify", u.ID)
		}
	}
	return nil
}

// runPlanner resolves the planner engine and runs it round-trip through
// the note-copy, exactly like a coding run (same invokeEngine, same
// usage-limit trip), retrying on an unusable plan up to maxPlanRounds
// before blocking the task. A valid plan is merged into the real note and
// handed to runPlanGate. branchName/codingEng/reviewEng are threaded
// through purely to reach runUnitLoop once a plan clears the gate -- the
// planner engine itself is resolved independently, right below.
func runPlanner(deps Deps, reporter ActivityReporter, job worker.Job, note *notetask.Note, repoCfg config.RepoConfig, branchName string, codingEng, reviewEng engine.Engine) error {
	registry := deps.engineRegistry()
	planEngineName := resolvePlanEngineName(note, repoCfg, deps.planEngineDefault())
	planEng, err := lookupEngine(registry, planEngineName)
	if err != nil {
		return blockPlannerTask(deps, job, fmt.Sprintf("resolving planner engine: %v", err))
	}

	copyName := copyFileName(job.Slug)
	copyPath := filepath.Join(repoCfg.Path, copyName)
	defer func() {
		if err := os.Remove(copyPath); err != nil && !os.IsNotExist(err) {
			fmt.Printf("[runner] task %s: failed to delete planner note-copy %s: %v\n", job.Slug, copyPath, err)
		}
	}()

	round := note.Frontmatter.PlanRound
	if round == 0 {
		round = 1
	}

	for {
		if err := writeNoteCopy(note, copyPath); err != nil {
			return fmt.Errorf("runner: writing planner note-copy for %s: %w", job.Slug, err)
		}

		prompt := buildPlannerPrompt(note, copyName, round)
		reporter.SetStage(fmt.Sprintf("plan: planner round %d", round))
		_, stderrTail, exitErr := invokeEngine(planEng, repoCfg.Path, prompt, "", deps.idleTimeout(), reporter)

		var usageLimitErr *engine.UsageLimitError
		if errors.As(exitErr, &usageLimitErr) {
			// Stop the round loop here rather than falling through to the
			// plan-parse check below: that check would find no usable plan
			// (the engine never got a chance to write one), increment
			// plan_round, and immediately re-run the planner while this
			// engine is still paused -- burning all maxPlanRounds attempts
			// near-instantly instead of actually waiting for the limit to
			// clear. Block through the same path a crashed coding
			// invocation would (ScenarioUsageLimit), which also registers
			// this note for main.go's auto-resume once the limit resets.
			if deps.Pause != nil {
				deps.Pause.Trip(planEngineName, pause.ReasonUsageLimit, usageLimitErr.ResetsAt)
			}
			return handleCrashFallback(deps, job, crashInfo{
				scenario:        ScenarioUsageLimit,
				stage:           "planner",
				exitErr:         exitErr,
				stderrTail:      stderrTail,
				repoPath:        repoCfg.Path,
				branchName:      branchName,
				engineName:      planEngineName,
				discordThreadID: threadIDOf(note),
				usageLimit:      usageLimitInfoOf(usageLimitErr),
			})
		}

		copyAfter, copyErr := readNoteCopy(copyPath)

		var units []notetask.PlanUnit
		var planErr error
		switch {
		case copyErr != nil:
			planErr = fmt.Errorf("reading planner note-copy: %w", copyErr)
		default:
			units, planErr = notetask.ParsePlanUnits(copyAfter.Plan)
			if planErr == nil {
				planErr = validatePlanUnits(units)
			}
		}

		if planErr != nil {
			round++
			if round > maxPlanRounds {
				return blockPlannerTask(deps, job, fmt.Sprintf("planner produced an unusable plan after %d rounds: %v", maxPlanRounds, planErr))
			}
			werr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: plan_round %d %s", round, job.Slug), func(n *notetask.Note) error {
				n.Frontmatter.PlanRound = round
				return nil
			})
			if werr != nil {
				return fmt.Errorf("runner: writing plan_round for %s: %w", job.Slug, werr)
			}
			note, err = deps.Vault.ReadNote(job.NotePath)
			if err != nil {
				return fmt.Errorf("runner: re-reading note after plan_round write for %s: %w", job.NotePath, err)
			}
			continue
		}

		questions, _ := notetask.ParsePlanQuestions(copyAfter.Plan)

		mergeErr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: plan_written %s", job.Slug), func(n *notetask.Note) error {
			n.HasPlan = true
			n.Plan = copyAfter.Plan
			return nil
		})
		if mergeErr != nil {
			return fmt.Errorf("runner: merging plan for %s: %w", job.Slug, mergeErr)
		}
		note, err = deps.Vault.ReadNote(job.NotePath)
		if err != nil {
			return fmt.Errorf("runner: re-reading note after plan write for %s: %w", job.NotePath, err)
		}

		return runPlanGate(deps, reporter, job, note, repoCfg, units, questions, branchName, codingEng, reviewEng)
	}
}

// plannerQuestionsToJev converts the Plan section's own optional
// `questions:` block into the shape plangate.GateQuestions expects --
// notetask deliberately doesn't import jev (see that package's own doc
// comment), so this conversion happens here instead, in the one package
// that already imports both.
func plannerQuestionsToJev(qs map[string]notetask.PlanQuestion) map[string]jev.Question {
	out := make(map[string]jev.Question, len(qs))
	for id, q := range qs {
		out[id] = jev.Question{
			Type:         "choice",
			Instructions: q.Instructions,
			Criteria:     map[string]any{"options": q.Options},
		}
	}
	return out
}

// gateLogLine renders the plan_gate Runner Log line in the format every
// gate decision writes exactly one of: "plan_gate risky=R ambiguous=A
// q1=opt(conf) ... stop=bool" on a successful Jev call (every answer, in
// deterministic order: risky, ambiguous, then the planner's own question
// ids sorted), or "plan_gate error: <err> stop=true" when Jev was never
// reached at all.
func gateLogLine(askErr error, answers map[string]jev.Answer, plannerQuestionIDs []string, stop bool) string {
	if askErr != nil {
		return fmt.Sprintf("plan_gate error: %v stop=%t", askErr, stop)
	}
	var b strings.Builder
	b.WriteString("plan_gate")
	for _, id := range []string{"risky", "ambiguous"} {
		if a, ok := answers[id]; ok && a.Noul != nil {
			fmt.Fprintf(&b, " %s=%.2f", id, *a.Noul)
		} else {
			fmt.Fprintf(&b, " %s=missing", id)
		}
	}
	sortedIDs := append([]string(nil), plannerQuestionIDs...)
	sort.Strings(sortedIDs)
	for _, id := range sortedIDs {
		if a, ok := answers[id]; ok {
			fmt.Fprintf(&b, " %s=%s(%.2f)", id, a.Choice, a.Confidence)
		} else {
			fmt.Fprintf(&b, " %s=missing", id)
		}
	}
	fmt.Fprintf(&b, " stop=%t", stop)
	return b.String()
}

// runPlanGate decides, for a plan whose units already parsed and
// validated cleanly, whether it can run straight on or must wait for a
// human: plangate.HardRules plus one Jev call against
// plangate.GateQuestions. No stop seeds `units` (one pending entry per
// plan unit, same order), sets current_unit to the first id, and hands
// straight into runUnitLoop -- there's no reason to wait for a separate
// ProcessTask run once the gate has already cleared. A stop posts the
// full Plan for human review via notify.PostToThread, records the
// returned message id, appends a fresh "### Round <n>" heading to Plan
// Review, and flips status to plan_review -- the task's slot frees as
// ProcessTask returns right after.
func runPlanGate(deps Deps, reporter ActivityReporter, job worker.Job, note *notetask.Note, repoCfg config.RepoConfig, units []notetask.PlanUnit, questions map[string]notetask.PlanQuestion, branchName string, codingEng, reviewEng engine.Engine) error {
	hard := plangate.HardRules(units)

	plannerQuestions := plannerQuestionsToJev(questions)
	plannerQuestionIDs := make([]string, 0, len(questions))
	for id := range questions {
		plannerQuestionIDs = append(plannerQuestionIDs, id)
	}

	autoMerge := resolveAutoMerge(note.Frontmatter.AutoMerge, repoCfg.AutoMergeDefault)
	state := map[string]any{
		"task":       map[string]any{"prompt": note.Prompt},
		"plan":       map[string]any{"units": units},
		"repo":       job.Repo,
		"auto_merge": autoMerge,
	}

	reporter.SetStage("plan: gate")
	answers, askErr := askJev(deps.Jev, state, plangate.GateQuestions(plannerQuestions), gateTimeout)
	result := plangate.DecideGate(hard, answers, plannerQuestionIDs, askErr)
	logLine := oneLine(gateLogLine(askErr, answers, plannerQuestionIDs, result.Stop), oneLineMaxRunes)

	if !result.Stop {
		if err := seedUnits(deps, job, units, logLine); err != nil {
			return err
		}
		return runUnitLoopAndFinish(deps, reporter, job, repoCfg, branchName, codingEng, reviewEng)
	}
	return stopForPlanReview(deps, job, note, repoCfg, result, logLine)
}

// seedUnits writes the gate's clean-pass outcome: one pending UnitState per
// plan unit (same order as the plan), current_unit set to the first one,
// and the plan_gate Runner Log line -- all in one commit.
func seedUnits(deps Deps, job worker.Job, units []notetask.PlanUnit, logLine string) error {
	unitStates := make([]notetask.UnitState, 0, len(units))
	for _, u := range units {
		unitStates = append(unitStates, notetask.UnitState{ID: u.ID, Status: notetask.UnitStatusPending})
	}
	firstID := units[0].ID

	werr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: plan_gate_pass %s", job.Slug), func(n *notetask.Note) error {
		n.Frontmatter.Units = unitStates
		n.Frontmatter.CurrentUnit = &firstID
		notetask.AppendRunnerLog(n, logLine, time.Now())
		return nil
	})
	if werr != nil {
		return fmt.Errorf("runner: seeding units for %s: %w", job.Slug, werr)
	}
	return nil
}

// stopForPlanReview writes the gate's stop outcome: posts the full Plan
// into the task's Discord thread for human review (best-effort -- a post
// failure is logged, not fatal, same as postTaskClaim's own rule), then
// records plan_message_id (if the post succeeded), a fresh Plan Review
// round heading, status: plan_review, and the plan_gate Runner Log line,
// all in one commit.
func stopForPlanReview(deps Deps, job worker.Job, note *notetask.Note, repoCfg config.RepoConfig, result plangate.Result, logLine string) error {
	round := note.Frontmatter.PlanRound
	if round == 0 {
		round = 1
	}

	attachmentName := fmt.Sprintf("plan-round-%d.md", round)
	message := fmt.Sprintf("Task `%s` (%s)'s plan needs review: %s. React ✅ to approve, or ❌ and write your feedback under `### Round %d` in `## Plan Review`.",
		job.Slug, job.Repo, strings.Join(result.Reasons, "; "), round)

	var messageID string
	if deps.ForumWebhookURL != "" {
		var postErr error
		messageID, postErr = notify.PostToThread(deps.ForumWebhookURL, threadIDOf(note), message, attachmentName, note.Plan)
		if postErr != nil {
			fmt.Printf("[runner] task %s: posting plan for review failed: %v\n", job.Slug, postErr)
			messageID = ""
		}
	}

	werr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: plan_gate_stop %s", job.Slug), func(n *notetask.Note) error {
		if messageID != "" {
			n.Frontmatter.PlanMessageID = &messageID
		}
		n.Frontmatter.Status = "plan_review"
		notetask.AppendPlanReviewRound(n, round)
		notetask.AppendRunnerLog(n, logLine, time.Now())
		return nil
	})
	if werr != nil {
		return fmt.Errorf("runner: writing plan_review for %s: %w", job.Slug, werr)
	}
	return nil
}

// resolvePlanReview is processMultiUnit's branch for a task that cleared
// the planner (HasPlan) but has no units seeded yet -- the plan gate
// stopped it for human review (stopForPlanReview above), and this is the
// resulting plan_approved/plan_rejected reclaim: dispatch claims that note
// like any ready task (RBG-47), and by the time this runs,
// frontmatter.Status has already gone through dispatch's claim write --
// the same generic "queued" every claimable status gets, same as
// ready/blocker_resolved. So the actual decision comes from this round's
// own review text instead (notetask.CurrentRoundReview), the identical
// rule internal/planpoll.Decide applies on the reaction side: empty text
// is an approval (seed units, run the loop) -- a human setting
// plan_approved by hand with nothing written behaves the same way. A human
// setting plan_rejected by hand must write their feedback under this
// round's own heading first, exactly like reacting ❌ does -- without it
// there's nothing to feed the next planner round, so it's treated as an
// approval instead.
func resolvePlanReview(deps Deps, reporter ActivityReporter, job worker.Job, note *notetask.Note, repoCfg config.RepoConfig, branchName string, codingEng, reviewEng engine.Engine) error {
	round := note.Frontmatter.PlanRound
	if round == 0 {
		round = 1
	}

	if strings.TrimSpace(notetask.CurrentRoundReview(note.PlanReview, round)) == "" {
		units, err := notetask.ParsePlanUnits(note.Plan)
		if err != nil {
			return blockPlannerTask(deps, job, fmt.Sprintf("approved plan no longer parses as units: %v", err))
		}
		if err := seedUnits(deps, job, units, fmt.Sprintf("plan_review_approved round %d", round)); err != nil {
			return err
		}
		return runUnitLoopAndFinish(deps, reporter, job, repoCfg, branchName, codingEng, reviewEng)
	}

	if round >= maxPlanRounds {
		return blockPlannerTask(deps, job, fmt.Sprintf("plan rejected after %d rounds", round))
	}

	nextRound := round + 1
	werr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: plan_round %d %s", nextRound, job.Slug), func(n *notetask.Note) error {
		n.Frontmatter.PlanRound = nextRound
		notetask.AppendRunnerLog(n, fmt.Sprintf("plan_review_rejected round %d", round), time.Now())
		return nil
	})
	if werr != nil {
		return fmt.Errorf("runner: writing plan_round for %s: %w", job.Slug, werr)
	}
	refreshed, err := deps.Vault.ReadNote(job.NotePath)
	if err != nil {
		return fmt.Errorf("runner: re-reading note after plan_round write for %s: %w", job.NotePath, err)
	}
	return runPlanner(deps, reporter, job, refreshed, repoCfg, branchName, codingEng, reviewEng)
}

// blockPlannerTask marks job's task blocked after the planner can't
// produce a usable plan (or its engine can't even be resolved), alerting
// through deps.OnBlocked -- same "always write blocked, always alert"
// contract as setupfallback.go's handleSetupFailure, just for this
// package's own planner failure mode.
func blockPlannerTask(deps Deps, job worker.Job, reason string) error {
	entry := fmt.Sprintf("%s: %s", time.Now().UTC().Format(time.RFC3339), reason)
	var discordThreadID string
	writeErr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: blocked %s (planner)", job.Slug), func(n *notetask.Note) error {
		if n.Frontmatter.DiscordThreadID != nil {
			discordThreadID = *n.Frontmatter.DiscordThreadID
		}
		n.Frontmatter.Status = "blocked"
		if strings.TrimSpace(n.WorkLog) == "" {
			n.WorkLog = entry
		} else {
			n.WorkLog = strings.TrimSpace(n.WorkLog) + "\n" + entry
		}
		n.HasWorkLog = true
		notetask.AppendRunnerLog(n, "blocked", time.Now())
		return nil
	})
	if writeErr != nil {
		fmt.Printf("[runner] task %s: failed to write blocked status after planner failure: %v\n", job.Slug, writeErr)
	}

	payload := AlertPayload{
		Slug:            job.Slug,
		Repo:            job.Repo,
		Scenario:        ScenarioPlannerFailure,
		Stage:           "planner",
		DiscordThreadID: discordThreadID,
		ExitCode:        -1,
		ExitErrText:     reason,
		PRState:         "unknown (planner never opens a PR)",
		LogPointer:      "(per-task transcript logging not yet built -- see Design - Runner.md's Open section)",
	}
	if deps.OnBlocked != nil {
		deps.OnBlocked(job, payload)
	}

	fmt.Printf("[runner] task %s: blocked (planner): %s\n", job.Slug, reason)
	return fmt.Errorf("runner: task %s blocked (planner): %s", job.Slug, reason)
}
