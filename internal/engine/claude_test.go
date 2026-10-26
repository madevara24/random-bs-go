package engine

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestClaudeBuildCommandFreshArgv(t *testing.T) {
	cmd, preAssigned := NewClaude("").BuildCommand("/tmp/repo", "do the thing", "")
	if preAssigned != "" {
		t.Errorf("preAssignedSessionID = %q, want \"\" (claude never pre-assigns one)", preAssigned)
	}
	if cmd.Dir != "/tmp/repo" {
		t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, "/tmp/repo")
	}
	want := []string{"claude", "-p", "do the thing", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
	if !equalArgs(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
}

func TestClaudeBuildCommandResumeArgv(t *testing.T) {
	cmd, preAssigned := NewClaude("/usr/local/bin/claude").BuildCommand("/tmp/repo", "continue", "sess-123")
	if preAssigned != "" {
		t.Errorf("preAssignedSessionID = %q, want \"\"", preAssigned)
	}
	want := []string{"/usr/local/bin/claude", "-p", "continue", "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions", "--resume", "sess-123"}
	if !equalArgs(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
}

func TestClaudeBuildReviewCommandArgv(t *testing.T) {
	cmd := NewClaude("").BuildReviewCommand("/tmp/repo", "review this")
	want := []string{"claude", "-p", "review this", "--output-format", "json", "--dangerously-skip-permissions"}
	if !equalArgs(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
	if cmd.Dir != "/tmp/repo" {
		t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, "/tmp/repo")
	}
}

func TestClaudeParseLineCapturesSessionID(t *testing.T) {
	var state RunState
	NewClaude("").ParseLine(`{"type":"system","subtype":"init","session_id":"abc-123"}`, &state)
	if state.SessionID != "abc-123" {
		t.Errorf("SessionID = %q, want %q", state.SessionID, "abc-123")
	}
	if state.UsageLimitHit {
		t.Error("UsageLimitHit = true, want false for a plain init line")
	}
}

func TestClaudeParseLineRateLimitAllowedDoesNotTrip(t *testing.T) {
	var state RunState
	NewClaude("").ParseLine(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1791552000}}`, &state)
	if state.UsageLimitHit {
		t.Error("UsageLimitHit = true, want false for status \"allowed\"")
	}
}

// TestClaudeParseLineRateLimitAllowedWarningDoesNotTrip pins RBG-33 item
// 8: the production 2026-10-26 incident line -- "allowed_warning" at 90%
// utilization, several minutes ahead of the real "rejected" -- must not
// be counted as a hit, even though it's neither "" nor "allowed".
func TestClaudeParseLineRateLimitAllowedWarningDoesNotTrip(t *testing.T) {
	var state RunState
	NewClaude("").ParseLine(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed_warning","resetsAt":1793020200,"rateLimitType":"five_hour","utilization":0.9,"isUsingOverage":false,"surpassedThreshold":0.9}}`, &state)
	if state.UsageLimitHit {
		t.Error("UsageLimitHit = true, want false for status \"allowed_warning\"")
	}
	if state.UsageLimitResetsAt != nil {
		t.Errorf("UsageLimitResetsAt = %v, want nil (not a hit, so no resetsAt captured)", state.UsageLimitResetsAt)
	}
}

func TestClaudeParseLineRateLimitRejectedTripsWithResetsAt(t *testing.T) {
	var state RunState
	NewClaude("").ParseLine(`{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1791552000}}`, &state)
	if !state.UsageLimitHit {
		t.Fatal("UsageLimitHit = false, want true for status \"rejected\"")
	}
	if state.UsageLimitResetsAt == nil || !state.UsageLimitResetsAt.Equal(time.Unix(1791552000, 0)) {
		t.Errorf("UsageLimitResetsAt = %v, want %v", state.UsageLimitResetsAt, time.Unix(1791552000, 0))
	}
}

func TestClaudeParseLineResultIsErrorAnd429TripsUsageLimit(t *testing.T) {
	var state RunState
	NewClaude("").ParseLine(`{"type":"result","is_error":true,"api_error_status":429}`, &state)
	if !state.UsageLimitHit {
		t.Error("UsageLimitHit = false, want true for is_error+429 result line")
	}
}

func TestClaudeClassifyExitUsageLimitStateWins(t *testing.T) {
	resetsAt := time.Unix(1791552000, 0)
	state := RunState{UsageLimitHit: true, UsageLimitResetsAt: &resetsAt}
	err := NewClaude("").ClassifyExit(nil, "", state)
	var usageLimitErr *UsageLimitError
	if !errors.As(err, &usageLimitErr) {
		t.Fatalf("ClassifyExit = %v, want a *UsageLimitError", err)
	}
	if usageLimitErr.ResetsAt == nil || !usageLimitErr.ResetsAt.Equal(resetsAt) {
		t.Errorf("ResetsAt = %v, want %v", usageLimitErr.ResetsAt, resetsAt)
	}
}

func TestClaudeClassifyExitStderrFallback(t *testing.T) {
	waitErr := errExit{}
	err := NewClaude("").ClassifyExit(waitErr, "Error: you've hit your session limit", RunState{})
	var usageLimitErr *UsageLimitError
	if !errors.As(err, &usageLimitErr) {
		t.Fatalf("ClassifyExit = %v, want a *UsageLimitError from the stderr fallback", err)
	}
	if usageLimitErr.ResetsAt != nil {
		t.Errorf("ResetsAt = %v, want nil (fallback has no structured resetsAt)", usageLimitErr.ResetsAt)
	}
}

func TestClaudeClassifyExitPlainErrorPassesThrough(t *testing.T) {
	waitErr := errExit{}
	err := NewClaude("").ClassifyExit(waitErr, "nothing usage-limit-shaped here", RunState{})
	if err != waitErr {
		t.Errorf("ClassifyExit = %v, want waitErr unchanged", err)
	}
}

func TestClaudeParseReviewAnswerExtractsResult(t *testing.T) {
	answer, err := NewClaude("").ParseReviewAnswer([]byte(`{"result":"VERDICT: APPROVE\n\nLooks fine."}`))
	if err != nil {
		t.Fatalf("ParseReviewAnswer: %v", err)
	}
	if !strings.HasPrefix(answer, "VERDICT: APPROVE") {
		t.Errorf("answer = %q, want it to start with VERDICT: APPROVE", answer)
	}
}

type errExit struct{}

func (errExit) Error() string { return "exit status 1" }

func equalArgs(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}
