package runner

import (
	"errors"
	"testing"
)

// fakeMergeGateOps scripts verdicts/CI outcomes directly, so the round-
// budget bookkeeping in RunMergeGateLoop is testable without any real
// GitHub repo -- Phase 11's loop logic is the actual subject matter here;
// GhMergeGateOps (mergegate_gh.go) is what talks to real claude/gh, and is
// exercised separately by TestRealReviewInvocation and (where a real
// GitHub repo is available) an end-to-end test.
type fakeMergeGateOps struct {
	// verdicts[i] is what RunReview returns on round i; if there are fewer
	// entries than rounds actually requested, the last one repeats.
	verdicts []ReviewVerdict
	// ciConclusion is returned by every WaitForCI call.
	ciConclusion string

	// prStates[i] is what PRState returns on its i-th call (0-indexed); if
	// there are fewer entries than calls actually made, the last one
	// repeats. Defaults to "OPEN" (via prState()) when empty, so every
	// existing test that never sets this keeps behaving exactly as before.
	prStates   []string
	prStateErr error

	reviewCalls   []int
	resumeCalls   []string
	mergeCalls    int
	roundLimitHit int
	prStateCalls  int
}

func (f *fakeMergeGateOps) PRState() (string, error) {
	f.prStateCalls++
	if f.prStateErr != nil {
		return "", f.prStateErr
	}
	if len(f.prStates) == 0 {
		return "OPEN", nil
	}
	i := f.prStateCalls - 1
	if i >= len(f.prStates) {
		i = len(f.prStates) - 1
	}
	return f.prStates[i], nil
}

func (f *fakeMergeGateOps) RunReview(round int) (ReviewVerdict, string, error) {
	f.reviewCalls = append(f.reviewCalls, round)
	v := f.verdicts[len(f.verdicts)-1]
	if round < len(f.verdicts) {
		v = f.verdicts[round]
	}
	return v, "feedback for round " + string(rune('0'+round)), nil
}

func (f *fakeMergeGateOps) WaitForCI() (string, string, error) {
	if f.ciConclusion == "failure" {
		return "failure", "ci failure log", nil
	}
	return "success", "", nil
}

func (f *fakeMergeGateOps) Merge() error {
	f.mergeCalls++
	return nil
}

func (f *fakeMergeGateOps) ResumeWithFeedback(text string) error {
	f.resumeCalls = append(f.resumeCalls, text)
	return nil
}

func (f *fakeMergeGateOps) AlertRoundLimitHit() {
	f.roundLimitHit++
}

// TestMergeGateLoopCleanPass is Phase 11's first test gate's logic half:
// APPROVE -> CI green -> merge happens, no resume needed.
func TestMergeGateLoopCleanPass(t *testing.T) {
	ops := &fakeMergeGateOps{verdicts: []ReviewVerdict{VerdictApprove}, ciConclusion: "success"}
	err := RunMergeGateLoop(ops)
	if err != nil {
		t.Fatalf("RunMergeGateLoop: %v", err)
	}
	if ops.mergeCalls != 1 {
		t.Errorf("mergeCalls = %d, want 1", ops.mergeCalls)
	}
	if len(ops.resumeCalls) != 0 {
		t.Errorf("resumeCalls = %v, want none", ops.resumeCalls)
	}
	if len(ops.reviewCalls) != 1 || ops.reviewCalls[0] != 0 {
		t.Errorf("reviewCalls = %v, want [0]", ops.reviewCalls)
	}
}

// TestMergeGateLoopConcernsThenApprove is Phase 11's second test gate: a
// forced CONCERNS round resumes the coding session with feedback and
// increments the round count, then a clean round merges.
func TestMergeGateLoopConcernsThenApprove(t *testing.T) {
	ops := &fakeMergeGateOps{verdicts: []ReviewVerdict{VerdictConcerns, VerdictApprove}, ciConclusion: "success"}
	err := RunMergeGateLoop(ops)
	if err != nil {
		t.Fatalf("RunMergeGateLoop: %v", err)
	}
	if ops.mergeCalls != 1 {
		t.Errorf("mergeCalls = %d, want 1", ops.mergeCalls)
	}
	if len(ops.resumeCalls) != 1 {
		t.Fatalf("resumeCalls = %v, want exactly 1 (the CONCERNS round)", ops.resumeCalls)
	}
	if len(ops.reviewCalls) != 2 || ops.reviewCalls[0] != 0 || ops.reviewCalls[1] != 1 {
		t.Errorf("reviewCalls = %v, want [0 1] (round count incremented)", ops.reviewCalls)
	}
}

// TestMergeGateLoopCIFailureThenApprove confirms a CI failure round also
// resumes (with the failure log) and re-enters through review on the next
// round, not straight back to CI.
func TestMergeGateLoopCIFailureThenApprove(t *testing.T) {
	f := &flakyCIOps{}
	err := RunMergeGateLoop(f)
	if err != nil {
		t.Fatalf("RunMergeGateLoop: %v", err)
	}
	if f.mergeCalls != 1 {
		t.Errorf("mergeCalls = %d, want 1", f.mergeCalls)
	}
	if len(f.resumeCalls) != 1 || f.resumeCalls[0] != "ci failure log" {
		t.Errorf("resumeCalls = %v, want exactly [\"ci failure log\"]", f.resumeCalls)
	}
	if f.reviewRounds != 2 {
		t.Errorf("review ran %d times, want 2 (both the initial pass and the CI-failure retry re-enter through review)", f.reviewRounds)
	}
}

// flakyCIOps always approves on review, fails CI once, then succeeds.
type flakyCIOps struct {
	ciCallCount  int
	reviewRounds int
	resumeCalls  []string
	mergeCalls   int
}

func (f *flakyCIOps) RunReview(round int) (ReviewVerdict, string, error) {
	f.reviewRounds++
	return VerdictApprove, "", nil
}
func (f *flakyCIOps) WaitForCI() (string, string, error) {
	f.ciCallCount++
	if f.ciCallCount == 1 {
		return "failure", "ci failure log", nil
	}
	return "success", "", nil
}
func (f *flakyCIOps) Merge() error { f.mergeCalls++; return nil }
func (f *flakyCIOps) ResumeWithFeedback(text string) error {
	f.resumeCalls = append(f.resumeCalls, text)
	return nil
}
func (f *flakyCIOps) AlertRoundLimitHit()      {}
func (f *flakyCIOps) PRState() (string, error) { return "OPEN", nil }

// TestMergeGateLoopExhaustsRoundLimit is Phase 11's third test gate:
// forced round exhaustion leaves the PR open (no Merge call) with a "hit
// round limit" alert instead of looping forever.
func TestMergeGateLoopExhaustsRoundLimit(t *testing.T) {
	ops := &fakeMergeGateOps{verdicts: []ReviewVerdict{VerdictConcerns}, ciConclusion: "success"}
	err := RunMergeGateLoop(ops)
	if !errors.Is(err, ErrRoundLimitHit) {
		t.Fatalf("RunMergeGateLoop error = %v, want ErrRoundLimitHit", err)
	}
	if ops.mergeCalls != 0 {
		t.Errorf("mergeCalls = %d, want 0 (never merged)", ops.mergeCalls)
	}
	if len(ops.resumeCalls) != MaxMergeGateRounds {
		t.Errorf("resumeCalls = %d, want %d (one per round, never re-entering after the budget)", len(ops.resumeCalls), MaxMergeGateRounds)
	}
	if ops.roundLimitHit != 1 {
		t.Errorf("AlertRoundLimitHit called %d times, want exactly 1", ops.roundLimitHit)
	}
}

// TestMergeGateLoopDetectsOutsideMergeBeforeFirstReview covers RBG-40 /
// TFO-10: if the PR is already MERGED (or CLOSED) before the loop's first
// review, the loop must stop immediately -- no review, no CI wait, no
// merge -- and return an error matching ErrMergedOutsideGate.
func TestMergeGateLoopDetectsOutsideMergeBeforeFirstReview(t *testing.T) {
	for _, state := range []string{"MERGED", "CLOSED"} {
		t.Run(state, func(t *testing.T) {
			ops := &fakeMergeGateOps{
				verdicts:     []ReviewVerdict{VerdictApprove},
				ciConclusion: "success",
				prStates:     []string{state},
			}
			err := RunMergeGateLoop(ops)
			if !errors.Is(err, ErrMergedOutsideGate) {
				t.Fatalf("RunMergeGateLoop error = %v, want ErrMergedOutsideGate", err)
			}
			if len(ops.reviewCalls) != 0 {
				t.Errorf("reviewCalls = %v, want none", ops.reviewCalls)
			}
			if ops.mergeCalls != 0 {
				t.Errorf("mergeCalls = %d, want 0", ops.mergeCalls)
			}
			if len(ops.resumeCalls) != 0 {
				t.Errorf("resumeCalls = %v, want none", ops.resumeCalls)
			}
		})
	}
}

// TestMergeGateLoopDetectsOutsideMergeBeforeMerge covers the resumed-
// coding-session case: the PR is OPEN when the loop starts (review and CI
// both run normally) but has been merged by the time the loop is about to
// call Merge() itself -- the loop must re-check PRState right before
// Merge() and never call it.
func TestMergeGateLoopDetectsOutsideMergeBeforeMerge(t *testing.T) {
	ops := &fakeMergeGateOps{
		verdicts:     []ReviewVerdict{VerdictApprove},
		ciConclusion: "success",
		prStates:     []string{"OPEN", "MERGED"},
	}
	err := RunMergeGateLoop(ops)
	if !errors.Is(err, ErrMergedOutsideGate) {
		t.Fatalf("RunMergeGateLoop error = %v, want ErrMergedOutsideGate", err)
	}
	if ops.mergeCalls != 0 {
		t.Errorf("mergeCalls = %d, want 0 (never merged)", ops.mergeCalls)
	}
	if len(ops.reviewCalls) != 1 {
		t.Errorf("reviewCalls = %v, want exactly 1 (review did run before the pre-merge check)", ops.reviewCalls)
	}
}

// TestMergeGateLoopPRStateErrorIsOrdinaryLoopError confirms a PRState
// error surfaces as a plain loop error, not one matching
// ErrMergedOutsideGate -- the two must stay distinguishable so the alert
// wiring (cmd/pmrunner/runner_mode.go) doesn't send the outside-merge
// message for a plain gh/network failure.
func TestMergeGateLoopPRStateErrorIsOrdinaryLoopError(t *testing.T) {
	ops := &fakeMergeGateOps{
		verdicts:     []ReviewVerdict{VerdictApprove},
		ciConclusion: "success",
		prStateErr:   errors.New("gh pr view: network error"),
	}
	err := RunMergeGateLoop(ops)
	if err == nil {
		t.Fatal("RunMergeGateLoop error = nil, want a loop error")
	}
	if errors.Is(err, ErrMergedOutsideGate) {
		t.Errorf("RunMergeGateLoop error = %v, want it NOT to match ErrMergedOutsideGate", err)
	}
	if ops.mergeCalls != 0 {
		t.Errorf("mergeCalls = %d, want 0", ops.mergeCalls)
	}
}
