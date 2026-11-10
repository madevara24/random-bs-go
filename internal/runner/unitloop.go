// The unit loop: once a multi-unit task's plan has cleared the plan gate
// (seedUnits, planner.go) or a prior run left it mid-way through, this file
// runs its units one at a time -- each unit gets a fresh coding session
// scoped to only its own brief, then a verify command, then an independent
// review of just that unit's own diff, before the next unit ever starts.
// Every step writes the vault note's frontmatter before moving on, so a
// task that blocks (a round cap, or a usage limit) resumes at exactly the
// unit and stage it left off at.
package runner

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/pause"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// unitPerUnitRoundCap is how many review_rounds (verify failures and
// review CONCERNS both count) a single unit may spend before the task
// blocks on it.
const unitPerUnitRoundCap = 3

// unitCompletionContract is the deliverable every unit's coding session is
// held to -- restated in buildUnitPrompt, same discipline as runner.go's
// prCompletionContract for the single-session path.
const unitCompletionContract = "The session must commit its work on the current branch, append a `### %s` entry of at most 10 lines to `## Handoff` in `%s` (what changed, interfaces later units rely on, anything left undone), and set `status` to `done` if this unit is finished or `blocked` if it genuinely can't be. Never push, open a pull request, merge, or touch any unit but this one -- that happens once every unit is done, or is this runner's own job between units."

// allUnitsDone reports whether every entry in units has reached
// notetask.UnitStatusDone.
func allUnitsDone(units []notetask.UnitState) bool {
	for _, u := range units {
		if u.Status != notetask.UnitStatusDone {
			return false
		}
	}
	return true
}

// findPlanUnit returns the plan unit with the given id, if present.
func findPlanUnit(units []notetask.PlanUnit, id string) (notetask.PlanUnit, bool) {
	for _, u := range units {
		if u.ID == id {
			return u, true
		}
	}
	return notetask.PlanUnit{}, false
}

// planUnitPosition returns id's 1-based position in plan order (the order
// seedUnits writes the frontmatter units in), or 0 if id isn't in units.
// Only used to label the reported stage ("unit U2 (2/5): ...").
func planUnitPosition(units []notetask.PlanUnit, id string) int {
	for i, u := range units {
		if u.ID == id {
			return i + 1
		}
	}
	return 0
}

// findUnitState returns the frontmatter unit state with the given id, if
// present.
func findUnitState(units []notetask.UnitState, id string) (notetask.UnitState, bool) {
	for _, u := range units {
		if u.ID == id {
			return u, true
		}
	}
	return notetask.UnitState{}, false
}

// mutateUnit finds the unit state with the given id in n's frontmatter and
// applies fn to it in place, reporting whether it was found.
func mutateUnit(n *notetask.Note, id string, fn func(*notetask.UnitState)) bool {
	for i := range n.Frontmatter.Units {
		if n.Frontmatter.Units[i].ID == id {
			fn(&n.Frontmatter.Units[i])
			return true
		}
	}
	return false
}

// nextPendingUnitID returns the id of the unit positionally after
// currentID in units, or nil if currentID is the last one -- unit order is
// always plan order (seedUnits), so "next" is just "next index."
func nextPendingUnitID(units []notetask.UnitState, currentID string) *string {
	for i, u := range units {
		if u.ID == currentID {
			if i+1 < len(units) {
				next := units[i+1].ID
				return &next
			}
			return nil
		}
	}
	return nil
}

// checkUnitRoundCap decides whether spending one more review round on
// unitID would hit either cap: this unit's own (unitPerUnitRoundCap) or the
// task-wide budget (2 * len(units)) summed across every unit. Checked
// before a round is actually spent, so the round that would exceed a cap
// never happens -- the caller blocks instead of resuming.
func checkUnitRoundCap(units []notetask.UnitState, unitID string) (blocked bool, reason string) {
	var unitRounds, totalRounds int
	for _, u := range units {
		totalRounds += u.ReviewRounds
		if u.ID == unitID {
			unitRounds = u.ReviewRounds
		}
	}
	taskBudget := 2 * len(units)
	if unitRounds+1 >= unitPerUnitRoundCap {
		return true, fmt.Sprintf("unit %s hit its per-unit review-round cap (%d)", unitID, unitPerUnitRoundCap)
	}
	if totalRounds+1 > taskBudget {
		return true, fmt.Sprintf("task hit its total review-round budget (%d) needing another round for unit %s", taskBudget, unitID)
	}
	return false, ""
}

// buildUnitPrompt constructs what gets passed to the coding engine for one
// unit -- its own title/paths/acceptance/verify command only, plus whatever
// `## Handoff` prior units have written so far. Used both for a fresh unit
// and for resuming one's session_id (invokeEngine's resumeSessionID
// handles which); the prompt text itself never differs between the two.
func buildUnitPrompt(unit notetask.PlanUnit, handoff, copyName string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "You are working on one unit (`%s`: %s) of a larger multi-unit task, tracked in the file `%s` in this repository's root (already git-ignored -- do not commit it).\n\n", unit.ID, unit.Title, copyName)
	if len(unit.Paths) > 0 {
		fmt.Fprintf(&b, "Paths: %s\n\n", strings.Join(unit.Paths, ", "))
	}
	if strings.TrimSpace(unit.Accept) != "" {
		fmt.Fprintf(&b, "Acceptance: %s\n\n", unit.Accept)
	}
	fmt.Fprintf(&b, "Verify command (the runner runs this itself once you're done -- you don't need to run it): `%s`\n\n", unit.Verify)
	fmt.Fprintf(&b, unitCompletionContract, unit.ID, copyName)
	b.WriteString("\n\n")
	if strings.TrimSpace(handoff) != "" {
		b.WriteString("## Handoff so far\n\n")
		b.WriteString(strings.TrimSpace(handoff))
		b.WriteString("\n\n")
	}
	return b.String()
}

// gitRevParse resolves ref to its full SHA in repoPath.
func gitRevParse(repoPath, ref string) (string, error) {
	out, err := exec.Command("git", "-C", repoPath, "rev-parse", ref).Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse %s (in %s): %w", ref, repoPath, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// errVerifyStartFailed marks a verify command that never got to run at all
// (e.g. the shell itself couldn't start) -- distinct from one that ran and
// failed verification, which the unit loop retries as a normal round:
// retrying a command that can't even start will never succeed, so
// runUnitLoop blocks on this immediately instead of burning a review round
// (and feeding the coding session an empty, uninformative "failure").
var errVerifyStartFailed = errors.New("verify command failed to start")

// runUnitVerify runs unit's verify command in repoPath under a hard
// timeout, returning its combined output. A nil error means it exited
// zero; output is returned either way, since a failure's output is exactly
// what gets fed back to the coding session as feedback -- unless the error
// wraps errVerifyStartFailed, which the caller blocks on instead.
//
// Started in its own process group (same SysProcAttr as invokeEngine) so a
// timeout kill reaches every descendant the verify command spawned (e.g.
// `go test` under `sh -c`), not just the `sh` wrapper itself -- plain
// exec.CommandContext only ever kills the top-level PID, which on timeout
// left the real child running on the production host indefinitely.
func runUnitVerify(repoPath, verifyCmd string, timeout time.Duration) (string, error) {
	cmd := exec.Command("sh", "-c", verifyCmd)
	cmd.Dir = repoPath
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("%w: starting verify command `%s`: %v", errVerifyStartFailed, verifyCmd, err)
	}

	timer := time.AfterFunc(timeout, func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	})
	err := cmd.Wait()
	timer.Stop()

	if err != nil {
		return out.String(), fmt.Errorf("verify command `%s` failed: %w", verifyCmd, err)
	}
	return out.String(), nil
}

// blockUnitTask writes status: blocked to the vault note (and, if unitID
// is non-empty, that one unit's own status too), preserves the branch's
// current work, registers the note for auto-resume when scenario is
// ScenarioUsageLimit, and alerts via Deps.OnBlocked -- the unit loop's one
// shared exit path for every way it can't continue: a round cap, a crashed
// coding session, a self-reported blocked unit, or a review engine's usage
// limit tripping mid-loop. Always returns a non-nil error.
func blockUnitTask(deps Deps, job worker.Job, repoCfg config.RepoConfig, branchName, unitID string, scenario Scenario, reason string, usageLimit *UsageLimitInfo, engineName string) error {
	var discordThreadID string
	writeErr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: blocked %s (unit loop)", job.Slug), func(n *notetask.Note) error {
		discordThreadID = threadIDOf(n)
		n.Frontmatter.Status = "blocked"
		if unitID != "" {
			mutateUnit(n, unitID, func(us *notetask.UnitState) { us.Status = notetask.UnitStatusBlocked })
		}
		notetask.AppendRunnerLog(n, "blocked: "+reason, time.Now())
		return nil
	})
	if writeErr != nil {
		fmt.Printf("[runner] task %s: failed to write blocked status after unit loop failure: %v\n", job.Slug, writeErr)
	}

	var preserveErrText string
	if err := preserveBlockedWork(repoCfg.Path, branchName); err != nil {
		preserveErrText = err.Error()
		fmt.Printf("[runner] task %s: failed to preserve blocked work: %v\n", job.Slug, err)
	}

	if scenario == ScenarioUsageLimit && deps.Pause != nil {
		deps.Pause.AddBlockedNote(engineName, job.NotePath)
	}

	payload := AlertPayload{
		Slug:            job.Slug,
		Repo:            job.Repo,
		Scenario:        scenario,
		Stage:           "unit loop",
		DiscordThreadID: discordThreadID,
		EngineName:      engineName,
		UsageLimit:      usageLimit,
		ExitCode:        -1,
		ExitErrText:     reason,
		PRState:         "unknown (units run before a PR exists)",
		LogPointer:      "(per-task transcript logging not yet built -- see Design - Runner.md's Open section)",
		PreserveErr:     preserveErrText,
		RepoPath:        repoCfg.Path,
	}
	if deps.OnBlocked != nil {
		deps.OnBlocked(job, payload)
	}

	fmt.Printf("[runner] task %s: blocked (unit loop): %s\n", job.Slug, reason)
	return fmt.Errorf("runner: task %s blocked (unit loop): %s", job.Slug, reason)
}

// blockUnitTaskOnErr is runUnitLoop's shared conversion for a plain
// infrastructure error (a vault read/write, a note-copy write, a git
// command, a verify command that couldn't even start) that would
// otherwise bubble a bare error out of processMultiUnit and strand the
// task at in_progress. Routes through blockUnitTask, forcing this unit's
// own status to blocked -- unlike a coding or review engine's own failure
// (see runCodingRound and runUnitLoop's review step), there's no specific
// resumable state worth preserving for an infrastructure error, so the
// unit-level status is just informational here. unitID may be "" when the
// failure happened before a unit was even identified (e.g. the unit
// loop's own top-of-iteration note read).
func blockUnitTaskOnErr(deps Deps, job worker.Job, repoCfg config.RepoConfig, branchName, unitID, codingEngineName, label string, err error) error {
	reason := fmt.Sprintf("%s: %v", label, err)
	if unitID != "" {
		reason = fmt.Sprintf("unit %s: %s", unitID, reason)
	}
	return blockUnitTask(deps, job, repoCfg, branchName, unitID, ScenarioUnitLoopBlocked, reason, nil, codingEngineName)
}

// runCodingRound runs one coding-engine invocation (fresh or resumed,
// prompt supplied by the caller) for the unit named unitID, then records
// whatever happened: the new session_id/head_sha always, and -- once a
// note-copy shows up and parses -- the merged `## Handoff`/Work Log, in one
// vault write. Blocks the whole task itself (via blockUnitTask) and
// returns a non-nil error if the engine itself fails (usage limit, or any
// other invocation failure), the copy never shows up or fails to parse,
// the copy self-reports status: blocked, or the vault write/re-read after
// a successful copy read itself fails; the caller only needs to propagate
// that error up, never build its own alert for those cases.
func runCodingRound(deps Deps, reporter ActivityReporter, job worker.Job, repoCfg config.RepoConfig, branchName, copyPath, codingEngineName string, codingEng engine.Engine, unitID, prompt, resumeSessionID string) (*notetask.Note, error) {
	sessionID, _, exitErr := invokeEngine(codingEng, repoCfg.Path, prompt, resumeSessionID, deps.idleTimeout(), reporter)
	headSHA, _ := gitRevParse(repoCfg.Path, branchName)

	// Always appends a Runner Log line -- not just bookkeeping, but what
	// guarantees this write actually has something to commit even on a
	// round where the coding session changed neither its session_id nor
	// the branch tip (e.g. it made no new commit at all): a mutateFn that
	// touches nothing else would otherwise serialize byte-identical to
	// what's already on disk, and vaultgit.WriteNote's `git commit` fails
	// outright on an empty diff.
	recordSessionHead := func(n *notetask.Note) error {
		mutateUnit(n, unitID, func(us *notetask.UnitState) {
			if sessionID != "" {
				us.SessionID = sessionID
			}
			if headSHA != "" {
				us.HeadSHA = headSHA
			}
		})
		notetask.AppendRunnerLog(n, fmt.Sprintf("unit %s coding round (session=%s head=%s)", unitID, sessionID, headSHA), time.Now())
		return nil
	}

	var usageLimitErr *engine.UsageLimitError
	if errors.As(exitErr, &usageLimitErr) {
		if deps.Pause != nil {
			deps.Pause.Trip(codingEngineName, pause.ReasonUsageLimit, usageLimitErr.ResetsAt)
		}
		_ = deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: unit %s session %s", unitID, job.Slug), recordSessionHead)
		return nil, blockUnitTask(deps, job, repoCfg, branchName, "", ScenarioUsageLimit,
			fmt.Sprintf("unit %s: coding engine %s hit its usage limit", unitID, codingEngineName),
			usageLimitInfoOf(usageLimitErr), codingEngineName)
	}
	if exitErr != nil {
		_ = deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: unit %s session %s", unitID, job.Slug), recordSessionHead)
		// unitID="" here, not unitID: an invocation that failed outright
		// leaves the unit's own status exactly as it already is
		// (building), per RBG-49's resume rule -- a later
		// blocker_resolved re-enters the building branch and tries a
		// fresh coding round.
		return nil, blockUnitTask(deps, job, repoCfg, branchName, "", ScenarioUnitLoopBlocked,
			fmt.Sprintf("unit %s: coding engine %s invocation failed: %v", unitID, codingEngineName, exitErr),
			nil, codingEngineName)
	}

	copyAfter, copyErr := readNoteCopy(copyPath)
	if copyErr != nil {
		_ = deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: unit %s session %s", unitID, job.Slug), recordSessionHead)
		// unitID="" here, not unitID: a coding invocation that never
		// produced a usable copy leaves the unit's own status exactly as
		// it already is (building), per RBG-49's resume rule -- a later
		// blocker_resolved re-enters the building branch and tries a
		// fresh coding round, same as it would for any other
		// still-building unit.
		return nil, blockUnitTask(deps, job, repoCfg, branchName, "", ScenarioUnitLoopBlocked,
			fmt.Sprintf("unit %s: coding session ended without a usable note-copy: %v", unitID, copyErr),
			nil, codingEngineName)
	}

	writeErr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: unit %s round %s", unitID, job.Slug), func(n *notetask.Note) error {
		if err := recordSessionHead(n); err != nil {
			return err
		}
		n.HasHandoff = copyAfter.HasHandoff
		n.Handoff = copyAfter.Handoff
		n.HasWorkLog = copyAfter.HasWorkLog
		n.WorkLog = copyAfter.WorkLog
		return nil
	})
	if writeErr != nil {
		return nil, blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "writing coding round", writeErr)
	}

	if copyAfter.Frontmatter.Status == "blocked" {
		return nil, blockUnitTask(deps, job, repoCfg, branchName, unitID, ScenarioUnitLoopBlocked,
			fmt.Sprintf("unit %s self-reported blocked", unitID), nil, codingEngineName)
	}

	refreshed, err := deps.Vault.ReadNote(job.NotePath)
	if err != nil {
		return nil, blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "re-reading note after coding round", err)
	}
	return refreshed, nil
}

// runUnitLoop runs job's task's units, one at a time starting from
// current_unit, each through its own build/verify/review cycle, writing
// the vault note's frontmatter after every transition so a blocked task
// resumes exactly where it left off. See the package comment at the top of
// this file for the overall shape.
func runUnitLoop(deps Deps, reporter ActivityReporter, job worker.Job, repoCfg config.RepoConfig, branchName string, codingEng, reviewEng engine.Engine) error {
	codingEngineName := codingEng.Name()
	reviewEngineName := reviewEng.Name()
	copyName := copyFileName(job.Slug)
	copyPath := filepath.Join(repoCfg.Path, copyName)
	defer func() {
		if err := os.Remove(copyPath); err != nil && !os.IsNotExist(err) {
			fmt.Printf("[runner] task %s: failed to delete unit-loop note-copy %s: %v\n", job.Slug, copyPath, err)
		}
	}()

unitLoop:
	for {
		note, err := deps.Vault.ReadNote(job.NotePath)
		if err != nil {
			return blockUnitTaskOnErr(deps, job, repoCfg, branchName, "", codingEngineName, "reading note for unit loop", err)
		}
		if note.Frontmatter.CurrentUnit == nil {
			return nil
		}
		unitID := *note.Frontmatter.CurrentUnit

		planUnits, err := notetask.ParsePlanUnits(note.Plan)
		if err != nil {
			return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "parsing plan units", err)
		}
		unit, ok := findPlanUnit(planUnits, unitID)
		if !ok {
			return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "current_unit", errors.New("has no matching plan unit"))
		}
		unitPos := planUnitPosition(planUnits, unitID)
		totalUnits := len(planUnits)
		unitState, ok := findUnitState(note.Frontmatter.Units, unitID)
		if !ok {
			return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "current_unit", errors.New("has no matching units entry"))
		}

		// Building step: skipped entirely when resuming a unit already at
		// review (no coding call -- just re-review, per RBG-45's resume
		// contract).
		if unitState.Status != notetask.UnitStatusReview {
			if unitState.SessionID == "" {
				headSHA, err := gitRevParse(repoCfg.Path, branchName)
				if err != nil {
					return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "resolving base_sha", err)
				}
				if err := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: unit %s building %s", unitID, job.Slug), func(n *notetask.Note) error {
					mutateUnit(n, unitID, func(us *notetask.UnitState) {
						us.Status = notetask.UnitStatusBuilding
						us.BaseSHA = headSHA
					})
					notetask.AppendRunnerLog(n, fmt.Sprintf("unit %s building", unitID), time.Now())
					return nil
				}); err != nil {
					return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "writing building status", err)
				}
				if note, err = deps.Vault.ReadNote(job.NotePath); err != nil {
					return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "re-reading note after building write", err)
				}
				unitState, _ = findUnitState(note.Frontmatter.Units, unitID)
			}

			if err := writeNoteCopy(note, copyPath); err != nil {
				return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "writing note-copy", err)
			}
			prompt := buildUnitPrompt(unit, note.Handoff, copyName)
			reporter.SetStage(fmt.Sprintf("unit %s (%d/%d): building", unitID, unitPos, totalUnits))
			note, err = runCodingRound(deps, reporter, job, repoCfg, branchName, copyPath, codingEngineName, codingEng, unitID, prompt, unitState.SessionID)
			if err != nil {
				return err
			}
		}

		// Verify + review cycle for this unit: loops on a verify failure or
		// a review CONCERNS (each resumes the coding session and counts
		// one review round), ends on a review APPROVE (advance to the next
		// unit) or a cap/usage-limit block.
		for {
			unitState, ok = findUnitState(note.Frontmatter.Units, unitID)
			if !ok {
				return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "unit disappeared from units", errors.New("not found"))
			}

			if unitState.Status != notetask.UnitStatusReview {
				reporter.SetStage(fmt.Sprintf("unit %s (%d/%d): verify", unitID, unitPos, totalUnits))
				output, verifyErr := runUnitVerify(repoCfg.Path, unit.Verify, deps.idleTimeout())
				if verifyErr == nil {
					if err := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: unit %s review %s", unitID, job.Slug), func(n *notetask.Note) error {
						mutateUnit(n, unitID, func(us *notetask.UnitState) { us.Status = notetask.UnitStatusReview })
						notetask.AppendRunnerLog(n, fmt.Sprintf("unit %s review", unitID), time.Now())
						return nil
					}); err != nil {
						return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "writing review status", err)
					}
					if note, err = deps.Vault.ReadNote(job.NotePath); err != nil {
						return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "re-reading note after review-status write", err)
					}
					continue
				}

				// A verify command that couldn't even start would never
				// succeed on retry -- block immediately instead of
				// counting it as a review round and feeding the coding
				// session empty, uninformative "failure" output.
				if errors.Is(verifyErr, errVerifyStartFailed) {
					return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "verify", verifyErr)
				}

				if blocked, reason := checkUnitRoundCap(note.Frontmatter.Units, unitID); blocked {
					return blockUnitTask(deps, job, repoCfg, branchName, unitID, ScenarioUnitLoopBlocked, reason, nil, codingEngineName)
				}
				if err := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: unit %s verify-round %s", unitID, job.Slug), func(n *notetask.Note) error {
					mutateUnit(n, unitID, func(us *notetask.UnitState) { us.ReviewRounds++ })
					return nil
				}); err != nil {
					return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "writing verify-round", err)
				}
				if note, err = deps.Vault.ReadNote(job.NotePath); err != nil {
					return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "re-reading note after verify-round write", err)
				}
				unitState, _ = findUnitState(note.Frontmatter.Units, unitID)

				reporter.SetStage(fmt.Sprintf("unit %s (%d/%d): building", unitID, unitPos, totalUnits))
				note, err = runCodingRound(deps, reporter, job, repoCfg, branchName, copyPath, codingEngineName, codingEng, unitID, output, unitState.SessionID)
				if err != nil {
					return err
				}
				continue
			}

			// status == review: an independent review of just this unit's
			// own diff.
			reporter.SetStage(fmt.Sprintf("unit %s (%d/%d): review round %d", unitID, unitPos, totalUnits, unitState.ReviewRounds+1))
			verdict, feedback, reviewErr := reviewUnit(reviewEng, repoCfg.Path, unit, unitState.BaseSHA, unitState.HeadSHA, deps.unitReviewTimeout())
			if reviewErr != nil {
				var usageLimitErr *engine.UsageLimitError
				if errors.As(reviewErr, &usageLimitErr) {
					if deps.Pause != nil {
						deps.Pause.Trip(reviewEngineName, pause.ReasonUsageLimit, usageLimitErr.ResetsAt)
					}
					// Leaves the unit at review, exactly as it is -- a
					// later resume re-enters this same case with no
					// coding call, re-reviewing fresh.
					return nil
				}
				// unitID="" here, not unitID: a review invocation that
				// failed outright (crashed, couldn't start, timed out)
				// leaves the unit's own status exactly as it already is
				// (review), per RBG-49's resume rule -- a later
				// blocker_resolved re-enters this same case and runs a
				// fresh review, never a coding round.
				return blockUnitTask(deps, job, repoCfg, branchName, "", ScenarioUnitLoopBlocked,
					fmt.Sprintf("unit %s: reviewing: %v", unitID, reviewErr), nil, codingEngineName)
			}

			if verdict == VerdictApprove {
				nextID := nextPendingUnitID(note.Frontmatter.Units, unitID)
				if err := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: unit %s done %s", unitID, job.Slug), func(n *notetask.Note) error {
					mutateUnit(n, unitID, func(us *notetask.UnitState) { us.Status = notetask.UnitStatusDone })
					n.Frontmatter.CurrentUnit = nextID
					notetask.AppendRunnerLog(n, fmt.Sprintf("unit %s done", unitID), time.Now())
					return nil
				}); err != nil {
					return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "writing done status", err)
				}
				continue unitLoop
			}

			// CONCERNS.
			if blocked, reason := checkUnitRoundCap(note.Frontmatter.Units, unitID); blocked {
				return blockUnitTask(deps, job, repoCfg, branchName, unitID, ScenarioUnitLoopBlocked, reason, nil, codingEngineName)
			}
			if err := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: unit %s concerns %s", unitID, job.Slug), func(n *notetask.Note) error {
				mutateUnit(n, unitID, func(us *notetask.UnitState) {
					us.ReviewRounds++
					us.Status = notetask.UnitStatusBuilding
				})
				return nil
			}); err != nil {
				return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "writing concerns", err)
			}
			if note, err = deps.Vault.ReadNote(job.NotePath); err != nil {
				return blockUnitTaskOnErr(deps, job, repoCfg, branchName, unitID, codingEngineName, "re-reading note after concerns write", err)
			}
			unitState, _ = findUnitState(note.Frontmatter.Units, unitID)

			reporter.SetStage(fmt.Sprintf("unit %s (%d/%d): building", unitID, unitPos, totalUnits))
			note, err = runCodingRound(deps, reporter, job, repoCfg, branchName, copyPath, codingEngineName, codingEng, unitID, feedback, unitState.SessionID)
			if err != nil {
				return err
			}
			continue
		}
	}
}
