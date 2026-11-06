// Review & CI merge-gate loop (auto_merge: true only): a fresh (no
// --resume, no vault access) review session plus real CI, both gating the
// runner's own gh pr merge -- never CC approving/merging its own work.
package runner

import "fmt"

// MaxMergeGateRounds is the shared budget between review CONCERNS and CI
// failure -- 3 rounds total, exhaustion leaves the PR open with an alert
// instead of looping forever.
const MaxMergeGateRounds = 3

// ReviewVerdict is what a review session concludes.
type ReviewVerdict string

const (
	VerdictApprove  ReviewVerdict = "APPROVE"
	VerdictConcerns ReviewVerdict = "CONCERNS"
)

// MergeGateOps is everything one round of the loop needs, abstracted so
// the round-budget bookkeeping itself (this file's real subject matter) is
// testable without a real GitHub repo -- GhMergeGateOps (mergegate_gh.go)
// is the real implementation; tests substitute a fake that scripts
// verdicts/CI outcomes directly.
type MergeGateOps interface {
	// RunReview invokes a fresh review session for the given round
	// (0-indexed, for logging/comment context) and returns its verdict
	// plus feedback text. Deliberately takes no prBody/diff parameters --
	// the real implementation fetches both fresh on every call, not once
	// outside the loop, so a round-2 review actually sees whatever the
	// round-0/1 resume sessions pushed, not a stale pre-fix snapshot: on
	// round 2+ it also reads the PR's existing comment thread first, to
	// check whether its own prior feedback was actually addressed, which
	// only means anything if the context itself is re-fetched each round.
	// Also responsible for posting the verdict as a real `gh pr comment`
	// as a side effect.
	RunReview(round int) (ReviewVerdict, string, error)
	// WaitForCI blocks until the branch's latest CI run completes (Actions
	// API, `gh run list` -- never the Checks API). conclusion is e.g.
	// "success"/"failure"; failureLog is populated only on non-success.
	WaitForCI() (conclusion string, failureLog string, err error)
	// Merge executes the actual merge -- kept a separate action from
	// RunReview's APPROVE, so "decide" and "execute" never collapse into
	// one actor's call.
	Merge() error
	// ResumeWithFeedback resumes the original coding session with review
	// CONCERNS or CI failure text, ahead of the loop's next round.
	ResumeWithFeedback(text string) error
	// AlertRoundLimitHit fires once, only if the loop exhausts its budget
	// without a clean merge.
	AlertRoundLimitHit()
	// PRState reports the PR's current GitHub state (OPEN, MERGED, CLOSED)
	// -- checked before the loop ever reviews, and again right before each
	// Merge() call, since a resumed coding session could merge the PR
	// itself (see prCompletionContract's no-self-merge instruction, and
	// TFO-10's incident) at any point while the loop is running.
	PRState() (string, error)
}

// ErrRoundLimitHit is returned when the loop exhausts MaxMergeGateRounds
// without a clean merge -- the loop never touches status itself, so it
// stays whatever the last round left it at.
var ErrRoundLimitHit = fmt.Errorf("runner: hit round limit (%d) without a clean merge -- needs judgment", MaxMergeGateRounds)

// ErrMergedOutsideGate is wrapped by RunMergeGateLoop's returned error
// whenever PRState() reports anything other than OPEN -- a coding session
// (or a human) merged or closed the PR outside the gate's own review+CI
// check, same as TFO-10's incident (the pi builder ran `gh pr merge
// --auto` itself, so GitHub merged before CI or review ever ran). The
// loop never reviews, waits for CI, or merges once this fires.
var ErrMergedOutsideGate = fmt.Errorf("runner: PR was merged or closed outside the merge gate")

// RunMergeGateLoop is the loop itself: both triggers (CONCERNS, CI
// failure) always re-enter through review before re-checking CI -- a fix
// for one thing could plausibly break something the reviewer would catch.
func RunMergeGateLoop(ops MergeGateOps) error {
	if err := checkPROpen(ops); err != nil {
		return err
	}

	for round := 0; round < MaxMergeGateRounds; round++ {
		verdict, feedback, err := ops.RunReview(round)
		if err != nil {
			return fmt.Errorf("runner: review round %d: %w", round, err)
		}

		if verdict == VerdictConcerns {
			if err := ops.ResumeWithFeedback(feedback); err != nil {
				return fmt.Errorf("runner: resuming after CONCERNS (round %d): %w", round, err)
			}
			continue
		}

		conclusion, failureLog, err := ops.WaitForCI()
		if err != nil {
			return fmt.Errorf("runner: waiting for CI (round %d): %w", round, err)
		}
		if conclusion == "success" {
			if err := checkPROpen(ops); err != nil {
				return err
			}
			return ops.Merge()
		}
		if err := ops.ResumeWithFeedback(failureLog); err != nil {
			return fmt.Errorf("runner: resuming after CI failure (round %d): %w", round, err)
		}
	}

	ops.AlertRoundLimitHit()
	return ErrRoundLimitHit
}

// checkPROpen calls PRState() and turns anything other than OPEN into an
// error wrapping ErrMergedOutsideGate, naming the actual state -- called
// before the loop's first review and again right before every Merge()
// call (see RunMergeGateLoop).
func checkPROpen(ops MergeGateOps) error {
	state, err := ops.PRState()
	if err != nil {
		return fmt.Errorf("runner: checking PR state: %w", err)
	}
	if state != "OPEN" {
		return fmt.Errorf("runner: PR state is %s, not OPEN: %w", state, ErrMergedOutsideGate)
	}
	return nil
}
