// Review & CI merge-gate loop (auto_merge: true only). See Design -
// Runner.md's "Review & CI merge-gate loop" section: a fresh (no
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
	// round-0/1 resume sessions pushed, not a stale pre-fix snapshot (see
	// Design - Runner.md: "on round 2+, reads the PR's existing comment
	// thread first, so it checks whether its own prior feedback was
	// actually addressed" -- that only means anything if the context
	// itself is re-fetched each round). Also responsible for posting the
	// verdict as a real `gh pr comment` as a side effect.
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
}

// ErrRoundLimitHit is returned when the loop exhausts MaxMergeGateRounds
// without a clean merge -- status stays whatever the last round left it at
// (per Design - Runner.md, the loop doesn't touch status itself).
var ErrRoundLimitHit = fmt.Errorf("runner: hit round limit (%d) without a clean merge -- needs judgment", MaxMergeGateRounds)

// RunMergeGateLoop is the loop itself: both triggers (CONCERNS, CI
// failure) always re-enter through review before re-checking CI -- a fix
// for one thing could plausibly break something the reviewer would catch.
func RunMergeGateLoop(ops MergeGateOps) error {
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
			return ops.Merge()
		}
		if err := ops.ResumeWithFeedback(failureLog); err != nil {
			return fmt.Errorf("runner: resuming after CI failure (round %d): %w", round, err)
		}
	}

	ops.AlertRoundLimitHit()
	return ErrRoundLimitHit
}
