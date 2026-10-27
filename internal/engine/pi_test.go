package engine

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPiBuildCommandFreshArgvGetsNewSessionIDEachCall(t *testing.T) {
	eng := NewPi("", "", "", "/tmp/pi-sessions", 0)

	cmd1, preAssigned1 := eng.BuildCommand("/tmp/repo", "do the thing", "")
	cmd2, preAssigned2 := eng.BuildCommand("/tmp/repo", "do the thing", "")

	if preAssigned1 == "" || preAssigned2 == "" {
		t.Fatalf("preAssignedSessionID = %q, %q, want both non-empty (pi always pre-assigns)", preAssigned1, preAssigned2)
	}
	if preAssigned1 == preAssigned2 {
		t.Errorf("preAssignedSessionID = %q twice, want a fresh ID on every fresh BuildCommand call", preAssigned1)
	}
	if !strings.HasPrefix(preAssigned1, "rbg-") || !strings.HasPrefix(preAssigned2, "rbg-") {
		t.Errorf("preAssignedSessionID = %q, %q, want both prefixed %q", preAssigned1, preAssigned2, "rbg-")
	}
	if cmd1.Dir != "/tmp/repo" || cmd2.Dir != "/tmp/repo" {
		t.Errorf("cmd.Dir = %q, %q, want both %q", cmd1.Dir, cmd2.Dir, "/tmp/repo")
	}
	want1 := []string{"pi", "--mode", "json", "-p", "--session-id", preAssigned1, "--session-dir", "/tmp/pi-sessions", "--no-approve", "--", "do the thing"}
	if !equalArgs(cmd1.Args, want1) {
		t.Errorf("cmd.Args = %v, want %v", cmd1.Args, want1)
	}
}

func TestPiBuildCommandResumeArgvReusesGivenID(t *testing.T) {
	eng := NewPi("/usr/local/bin/pi", "", "", "/tmp/pi-sessions", 0)
	cmd, preAssigned := eng.BuildCommand("/tmp/repo", "continue", "rbg-deadbeefdeadbeef")

	if preAssigned != "rbg-deadbeefdeadbeef" {
		t.Errorf("preAssignedSessionID = %q, want the given resume ID unchanged", preAssigned)
	}
	want := []string{"/usr/local/bin/pi", "--mode", "json", "-p", "--session-id", "rbg-deadbeefdeadbeef", "--session-dir", "/tmp/pi-sessions", "--no-approve", "--", "continue"}
	if !equalArgs(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
}

func TestPiBuildCommandOmitsModelWhenUnset(t *testing.T) {
	eng := NewPi("", "", "", "/tmp/pi-sessions", 0)
	cmd, _ := eng.BuildCommand("/tmp/repo", "do the thing", "")
	for _, a := range cmd.Args {
		if a == "--model" {
			t.Fatalf("cmd.Args = %v, want no --model when PI_MODEL is empty", cmd.Args)
		}
	}
}

func TestPiBuildCommandIncludesModelWhenSet(t *testing.T) {
	eng := NewPi("", "deepseek/deepseek-v4-pro", "", "/tmp/pi-sessions", 0)
	cmd, preAssigned := eng.BuildCommand("/tmp/repo", "do the thing", "")
	want := []string{"pi", "--mode", "json", "-p", "--session-id", preAssigned, "--session-dir", "/tmp/pi-sessions", "--no-approve", "--model", "deepseek/deepseek-v4-pro", "--", "do the thing"}
	if !equalArgs(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
}

func TestPiBuildReviewCommandArgv(t *testing.T) {
	eng := NewPi("", "", "", "/tmp/pi-sessions", 0)
	cmd := eng.BuildReviewCommand("/tmp/repo", "review this")
	want := []string{"pi", "-p", "--no-session", "--tools", "read,grep,find,ls", "--no-approve", "--", "review this"}
	if !equalArgs(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
	if cmd.Dir != "/tmp/repo" {
		t.Errorf("cmd.Dir = %q, want %q", cmd.Dir, "/tmp/repo")
	}
}

func TestPiBuildReviewCommandIncludesReviewModelWhenSet(t *testing.T) {
	eng := NewPi("", "deepseek/deepseek-flash", "deepseek/deepseek-v4-pro", "/tmp/pi-sessions", 0)
	cmd := eng.BuildReviewCommand("/tmp/repo", "review this")
	want := []string{"pi", "-p", "--no-session", "--tools", "read,grep,find,ls", "--no-approve", "--model", "deepseek/deepseek-v4-pro", "--", "review this"}
	if !equalArgs(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
}

// TestPiArgvNeverCarriesAPIKey pins the hard requirement that this engine
// never passes or handles any credential -- DeepSeek auth lives entirely
// in pi's own credential store for the runner's OS user.
func TestPiArgvNeverCarriesAPIKey(t *testing.T) {
	eng := NewPi("", "a-model", "a-review-model", "/tmp/pi-sessions", 0)
	codeCmd, _ := eng.BuildCommand("/tmp/repo", "do the thing", "")
	reviewCmd := eng.BuildReviewCommand("/tmp/repo", "review this")
	for _, cmd := range []struct {
		name string
		args []string
	}{{"coding", codeCmd.Args}, {"review", reviewCmd.Args}} {
		for _, a := range cmd.args {
			if a == "--api-key" {
				t.Errorf("%s argv = %v, want no --api-key", cmd.name, cmd.args)
			}
		}
	}
}

func TestPiParseLineCapturesSessionIDFromHeader(t *testing.T) {
	var state RunState
	NewPi("", "", "", "/tmp", 0).ParseLine(`{"type":"session","version":3,"id":"rbg-abc123","timestamp":"2026-10-27T00:00:00Z","cwd":"/tmp/repo"}`, &state)
	if state.SessionID != "rbg-abc123" {
		t.Errorf("SessionID = %q, want %q", state.SessionID, "rbg-abc123")
	}
}

func TestPiParseLineDoesNotOverwritePreAssignedSessionID(t *testing.T) {
	state := RunState{SessionID: "rbg-preassigned"}
	NewPi("", "", "", "/tmp", 0).ParseLine(`{"type":"session","id":"rbg-different"}`, &state)
	if state.SessionID != "rbg-preassigned" {
		t.Errorf("SessionID = %q, want the pre-assigned value left untouched", state.SessionID)
	}
}

func TestPiParseLineCapturesAssistantErrorStopReason(t *testing.T) {
	var state RunState
	NewPi("", "", "", "/tmp", 0).ParseLine(`{"type":"message_end","message":{"role":"assistant","stopReason":"error","errorMessage":"DeepSeek API returned a 500"}}`, &state)
	if state.FailureText != "DeepSeek API returned a 500" {
		t.Errorf("FailureText = %q, want the errorMessage text", state.FailureText)
	}
}

func TestPiParseLineCapturesAssistantAbortedStopReason(t *testing.T) {
	var state RunState
	NewPi("", "", "", "/tmp", 0).ParseLine(`{"type":"message_end","message":{"role":"assistant","stopReason":"aborted","errorMessage":"context canceled"}}`, &state)
	if state.FailureText != "context canceled" {
		t.Errorf("FailureText = %q, want the errorMessage text", state.FailureText)
	}
}

func TestPiParseLineIgnoresNormalStopReasons(t *testing.T) {
	for _, stopReason := range []string{"toolUse", "stop"} {
		var state RunState
		NewPi("", "", "", "/tmp", 0).ParseLine(`{"type":"message_end","message":{"role":"assistant","stopReason":"`+stopReason+`"}}`, &state)
		if state.FailureText != "" {
			t.Errorf("stopReason %q: FailureText = %q, want empty", stopReason, state.FailureText)
		}
	}
}

func TestPiParseLineCapturesAutoRetryEndFailure(t *testing.T) {
	var state RunState
	NewPi("", "", "", "/tmp", 0).ParseLine(`{"type":"auto_retry_end","success":false,"finalError":"rate limited after 3 attempts"}`, &state)
	if state.FailureText != "rate limited after 3 attempts" {
		t.Errorf("FailureText = %q, want the finalError text", state.FailureText)
	}
}

func TestPiParseLineAutoRetryEndSuccessDoesNotTrip(t *testing.T) {
	var state RunState
	NewPi("", "", "", "/tmp", 0).ParseLine(`{"type":"auto_retry_end","success":true}`, &state)
	if state.FailureText != "" {
		t.Errorf("FailureText = %q, want empty for a successful auto_retry_end", state.FailureText)
	}
}

func TestPiClassifyExitFailureTextWinsEvenWithNilWaitErr(t *testing.T) {
	state := RunState{FailureText: "DeepSeek API returned a 500"}
	err := NewPi("", "", "", "/tmp", 0).ClassifyExit(nil, "", state)
	if err == nil {
		t.Fatal("ClassifyExit = nil, want an error carrying the failure text (pi exits 0 even on a failed response)")
	}
	if !strings.Contains(err.Error(), "DeepSeek API returned a 500") {
		t.Errorf("ClassifyExit error = %v, want it to contain the failure text", err)
	}
	var usageLimitErr *UsageLimitError
	if errors.As(err, &usageLimitErr) {
		t.Errorf("ClassifyExit error = %v, want pi failures never classified as a usage limit", err)
	}
}

func TestPiClassifyExitPlainErrorPassesThrough(t *testing.T) {
	waitErr := errExit{}
	err := NewPi("", "", "", "/tmp", 0).ClassifyExit(waitErr, "", RunState{})
	if err != waitErr {
		t.Errorf("ClassifyExit = %v, want waitErr unchanged", err)
	}
}

func TestPiParseReviewAnswerReturnsStdoutAsIs(t *testing.T) {
	answer, err := NewPi("", "", "", "/tmp", 0).ParseReviewAnswer([]byte("VERDICT: APPROVE\n\nLooks fine."))
	if err != nil {
		t.Fatalf("ParseReviewAnswer: %v", err)
	}
	if answer != "VERDICT: APPROVE\n\nLooks fine." {
		t.Errorf("answer = %q, want stdout unchanged", answer)
	}
}

func TestPiStartupTimeoutReturnsConfiguredValue(t *testing.T) {
	if got := NewPi("", "", "", "/tmp", 90*time.Second).StartupTimeout(); got != 90*time.Second {
		t.Errorf("StartupTimeout() = %v, want 90s", got)
	}
}

func TestPiNameIsPi(t *testing.T) {
	if got := NewPi("", "", "", "/tmp", 0).Name(); got != "pi" {
		t.Errorf("Name() = %q, want %q", got, "pi")
	}
}
