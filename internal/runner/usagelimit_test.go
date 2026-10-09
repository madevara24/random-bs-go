package runner

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/pause"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// writeInvokeClaudeStub writes an executable fake-claude script, with no
// testvault/testTargetRepoPath dependency -- these tests exercise
// invokeClaude directly, the same level TestIdleWatchdogKillsProcessGroup
// (watchdog_test.go) operates at.
func writeInvokeClaudeStub(t *testing.T, dir, body string) string {
	t.Helper()
	scriptPath := filepath.Join(dir, "fake-claude.sh")
	script := "#!/usr/bin/env bash\n" + body + "\n"
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake claude stub: %v", err)
	}
	return scriptPath
}

// shQuote wraps s in single quotes for safe embedding in a generated bash
// script, escaping any single quote already inside it (the captured
// "result" text contains "You've", which would otherwise terminate the
// quoted string early and corrupt the whole script -- a bash *syntax
// error* on such a line echoes the broken source line back on stderr,
// which happens to contain the words "session limit" and would otherwise
// make a test pass for the wrong reason, via the stderr fallback, not the
// real event-based detection it's meant to exercise).
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// echoLine is "echo " + shQuote(s) -- shorthand for building the scripts
// below out of one or more JSON lines.
func echoLine(s string) string {
	return "echo " + shQuote(s)
}

// The captured 2026-10-09 limited-run rate_limit_event/result payload, from
// this task's own spec -- pinned verbatim so a regression that starts
// keying on subtype or overageStatus fails loudly.
const limitedRateLimitEventLine = `{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":1791552000,"rateLimitType":"five_hour","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":1,"resetsAt":1791552000},"seven_day":{"utilization":0.09,"resetsAt":1792076400}}}}`
const limitedResultLine = `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"api_error_code":null,"terminal_reason":"api_error","result":"You've hit your session limit · resets 8:20pm (Asia/Bangkok)","num_turns":1}`

// The captured healthy-run rate_limit_event payload.
const healthyRateLimitEventLine = `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","rateLimitType":"five_hour","overageStatus":"rejected","overageDisabledReason":"org_level_disabled","isUsingOverage":false,"unifiedWindows":{"five_hour":{"utilization":0,"resetsAt":1791552000},"seven_day":{"utilization":0.02,"resetsAt":1792076400}}}}`

const initLine = `{"type":"system","subtype":"init","session_id":"fake-usagelimit-session"}`

// TestInvokeClaudeDetectsUsageLimitFromRateLimitEvent pins the discriminator
// this task's spec calls out: rate_limit_info.status == "rejected" (never
// subtype, which stays "success" even on the limit; never overageStatus,
// which reads "rejected" in both the healthy and limited state).
func TestInvokeClaudeDetectsUsageLimitFromRateLimitEvent(t *testing.T) {
	tmpDir := t.TempDir()
	body := strings.Join([]string{echoLine(initLine), echoLine(limitedRateLimitEventLine), echoLine(limitedResultLine), "exit 1"}, "\n")
	script := writeInvokeClaudeStub(t, tmpDir, body)

	sessionID, _, err := invokeClaude(script, tmpDir, "irrelevant prompt", "", 5*time.Second, nil)

	var usageLimitErr *UsageLimitError
	if !errors.As(err, &usageLimitErr) {
		t.Fatalf("invokeClaude error = %v, want a *UsageLimitError", err)
	}
	if usageLimitErr.ResetsAt == nil || !usageLimitErr.ResetsAt.Equal(time.Unix(1791552000, 0)) {
		t.Errorf("ResetsAt = %v, want %v", usageLimitErr.ResetsAt, time.Unix(1791552000, 0))
	}
	if sessionID != "fake-usagelimit-session" {
		t.Errorf("sessionID = %q, want it captured from the init line despite the limit", sessionID)
	}
	if !errors.Is(err, ErrUsageLimit) {
		t.Error("errors.Is(err, ErrUsageLimit) = false, want true")
	}
}

// TestInvokeClaudeResultEventAloneCorroboratesUsageLimit confirms the
// result event's is_error+429 alone (independent of rate_limit_event) is
// enough -- the spec's "corroborating" signal, for a future claude version
// that might omit the dedicated event.
func TestInvokeClaudeResultEventAloneCorroboratesUsageLimit(t *testing.T) {
	tmpDir := t.TempDir()
	body := strings.Join([]string{echoLine(initLine), echoLine(limitedResultLine), "exit 1"}, "\n")
	script := writeInvokeClaudeStub(t, tmpDir, body)

	_, _, err := invokeClaude(script, tmpDir, "irrelevant prompt", "", 5*time.Second, nil)

	var usageLimitErr *UsageLimitError
	if !errors.As(err, &usageLimitErr) {
		t.Fatalf("invokeClaude error = %v, want a *UsageLimitError", err)
	}
	if usageLimitErr.ResetsAt != nil {
		t.Errorf("ResetsAt = %v, want nil (no rate_limit_event arrived to supply one)", usageLimitErr.ResetsAt)
	}
}

// TestInvokeClaudeHealthyRateLimitEventDoesNotTrip is the healthy fixture:
// status "allowed", five_hour.utilization 0, api_error_status null,
// is_error false -- must not be mistaken for a usage limit.
func TestInvokeClaudeHealthyRateLimitEventDoesNotTrip(t *testing.T) {
	tmpDir := t.TempDir()
	healthyResult := `{"type":"result","subtype":"success","is_error":false,"api_error_status":null,"num_turns":1}`
	body := strings.Join([]string{echoLine(initLine), echoLine(healthyRateLimitEventLine), echoLine(healthyResult), "exit 0"}, "\n")
	script := writeInvokeClaudeStub(t, tmpDir, body)

	_, _, err := invokeClaude(script, tmpDir, "irrelevant prompt", "", 5*time.Second, nil)

	var usageLimitErr *UsageLimitError
	if errors.As(err, &usageLimitErr) {
		t.Fatalf("invokeClaude error = %v, want no UsageLimitError for the healthy fixture", err)
	}
	if err != nil {
		t.Errorf("invokeClaude error = %v, want nil", err)
	}
}

// TestInvokeClaudeOverageRejectedButStatusAllowedDoesNotTrip is the spec's
// explicit trap case: overageStatus reads "rejected" even in a perfectly
// healthy run -- only rate_limit_info.status matters.
func TestInvokeClaudeOverageRejectedButStatusAllowedDoesNotTrip(t *testing.T) {
	tmpDir := t.TempDir()
	line := `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","overageStatus":"rejected","unifiedWindows":{"five_hour":{"utilization":0.5}}}}`
	body := strings.Join([]string{echoLine(initLine), echoLine(line), "exit 0"}, "\n")
	script := writeInvokeClaudeStub(t, tmpDir, body)

	_, _, err := invokeClaude(script, tmpDir, "irrelevant prompt", "", 5*time.Second, nil)

	var usageLimitErr *UsageLimitError
	if errors.As(err, &usageLimitErr) {
		t.Fatalf("invokeClaude error = %v, want no UsageLimitError when status is \"allowed\" regardless of overageStatus", err)
	}
}

// TestInvokeClaudeFallbackDetectsStderrMessage covers the "claude exits
// before ever emitting the structured event" fallback -- no
// rate_limit_event/result line at all, just a stderr message and a non-zero
// exit.
func TestInvokeClaudeFallbackDetectsStderrMessage(t *testing.T) {
	tmpDir := t.TempDir()
	body := strings.Join([]string{echoLine(initLine), "echo " + shQuote("Error: you've hit your session limit") + " >&2", "exit 1"}, "\n")
	script := writeInvokeClaudeStub(t, tmpDir, body)

	_, stderrTail, err := invokeClaude(script, tmpDir, "irrelevant prompt", "", 5*time.Second, nil)

	var usageLimitErr *UsageLimitError
	if !errors.As(err, &usageLimitErr) {
		t.Fatalf("invokeClaude error = %v, want a *UsageLimitError from the stderr fallback", err)
	}
	if usageLimitErr.ResetsAt != nil {
		t.Errorf("ResetsAt = %v, want nil (fallback has no structured resetsAt)", usageLimitErr.ResetsAt)
	}
	if stderrTail == "" {
		t.Error("stderrTail is empty, want the captured stderr message")
	}
}

// TestInvokeClaudeUsageLimitBeforeIdleTimeoutWins is the RBG-25 Gap 2
// regression: claude reports the limit via rate_limit_event and then hangs
// (never exits) until the idle watchdog kills it. The real usage-limit
// signal, reported before the process went silent, must win over
// ErrIdleTimeout -- otherwise a mid-session limit hit gets masked as a plain
// idle timeout and the pause never trips.
func TestInvokeClaudeUsageLimitBeforeIdleTimeoutWins(t *testing.T) {
	tmpDir := t.TempDir()
	script := fmt.Sprintf(`#!/usr/bin/env bash
echo %s
echo %s
sleep 999999
`, shQuote(initLine), shQuote(limitedRateLimitEventLine))
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake claude stub: %v", err)
	}

	const idleTimeout = 2 * time.Second
	sessionID, _, err := invokeClaude(scriptPath, tmpDir, "irrelevant prompt", "", idleTimeout, nil)

	var usageLimitErr *UsageLimitError
	if !errors.As(err, &usageLimitErr) {
		t.Fatalf("invokeClaude error = %v, want a *UsageLimitError even though the idle watchdog also fired", err)
	}
	if errors.Is(err, ErrIdleTimeout) {
		t.Error("errors.Is(err, ErrIdleTimeout) = true, want the usage-limit error reported instead")
	}
	if usageLimitErr.ResetsAt == nil || !usageLimitErr.ResetsAt.Equal(time.Unix(1791552000, 0)) {
		t.Errorf("ResetsAt = %v, want %v", usageLimitErr.ResetsAt, time.Unix(1791552000, 0))
	}
	if sessionID != "fake-usagelimit-session" {
		t.Errorf("sessionID = %q, want it captured from the init line", sessionID)
	}
}

// TestNormalBlockedNeverTripsPause confirms an ordinary crash-fallback
// block (no usage-limit error from invokeClaude at all) never calls
// pause.State.Trip/AddBlockedNote -- the pause is exactly as specific as
// the detector, not triggered by "blocked" alone.
func TestNormalBlockedNeverTripsPause(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	deps, job, v, _ := setupCrashTest(t, "phase-normal-no-pause", scriptPath)
	var p pause.State
	deps.Pause = &p
	writeStubScript(t, scriptPath, "exit 0") // touches nothing -> ScenarioNonTerminal, no usage limit

	err := ProcessTask(deps, noopReporter{}, job)
	if err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}
	if p.IsPaused() {
		t.Error("pause was tripped by an ordinary (non-usage-limit) block")
	}
	if len(p.Snapshot().BlockedNotes) != 0 {
		t.Errorf("BlockedNotes = %v, want empty for an ordinary block", p.Snapshot().BlockedNotes)
	}

	note, err := v.ReadNote(job.NotePath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", note.Frontmatter.Status, "blocked")
	}
}

// TestCrashFallbackScenarioUsageLimit is the ProcessTask-level test gate:
// a stub that emits the limited rate_limit_event+result and leaves the
// copy untouched (which, absent the limit, would read as ScenarioNonTerminal)
// must be retagged ScenarioUsageLimit, trip the shared pause, record this
// task's note for later auto-resume, and mention Devara only.
func TestCrashFallbackScenarioUsageLimit(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	deps, job, v, slug := setupCrashTest(t, "phase-usagelimit", scriptPath)
	var p pause.State
	deps.Pause = &p

	writeStubScript(t, scriptPath, strings.Join([]string{echoLine(limitedResultLine), "exit 1"}, "\n"))
	_ = slug

	var gotJob *worker.Job
	var gotAlert *AlertPayload
	deps.OnBlocked = func(j worker.Job, pl AlertPayload) { gotJob = &j; gotAlert = &pl }

	err := ProcessTask(deps, noopReporter{}, job)
	if err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}
	assertBlocked(t, v, job.NotePath, ScenarioUsageLimit, gotJob, gotAlert)

	if !p.IsPaused() {
		t.Error("pause was not tripped by a usage-limit block")
	}
	snap := p.Snapshot()
	found := false
	for _, n := range snap.BlockedNotes {
		if n == job.NotePath {
			found = true
		}
	}
	if !found {
		t.Errorf("BlockedNotes = %v, want it to contain %q", snap.BlockedNotes, job.NotePath)
	}

	msg := gotAlert.DiscordMessage("devara-id", "aradev-id")
	if !strings.Contains(msg, "devara-id") {
		t.Errorf("message %q missing Devara mention", msg)
	}
	if strings.Contains(msg, "aradev-id") {
		t.Errorf("message %q has Ara-Dev mention, want none for a usage-limit block", msg)
	}
}

// TestProcessTaskSelfReportedBlockedWhileUsageLimitTripped covers the
// "self-reported blocked while a limit is tripped" case: CC's copy still
// parsed fine with its own status: blocked, but invokeClaude's exit error
// says the real cause was the usage limit. This must reach OnTerminal (not
// OnBlocked -- no crash-fallback path fires), with a non-nil usageLimit,
// and must still trip the pause and record the note for auto-resume.
func TestProcessTaskSelfReportedBlockedWhileUsageLimitTripped(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	deps, job, v, slug := setupCrashTest(t, "phase-usagelimit-selfreport", scriptPath)
	var p pause.State
	deps.Pause = &p

	copyName := copyFileName(slug)
	writeStubScript(t, scriptPath, strings.Join([]string{
		echoLine(limitedResultLine),
		fmt.Sprintf("printf -- '---\\nstatus: blocked\\nrepo: phase6-test-repo\\ncreated: \"2026-09-15\"\\n---\\nCrash-fallback test task.\\n\\n## Work Log\\n\\nHit the usage limit mid-task.\\n' > %q",
			filepath.Join(testTargetRepoPath, copyName)),
		"exit 1",
	}, "\n"))

	var gotBlockedCalled bool
	deps.OnBlocked = func(j worker.Job, pl AlertPayload) { gotBlockedCalled = true }

	var gotStatus string
	var gotUsageLimit *UsageLimitInfo
	var onTerminalCalled bool
	deps.OnTerminal = func(j worker.Job, status, workLog string, autoMerge bool, prURL, discordThreadID string, usageLimit *UsageLimitInfo) {
		onTerminalCalled = true
		gotStatus = status
		gotUsageLimit = usageLimit
	}

	err := ProcessTask(deps, noopReporter{}, job)
	if err != nil {
		t.Fatalf("ProcessTask: %v (self-reported blocked is the happy path, not a crash)", err)
	}
	if gotBlockedCalled {
		t.Error("OnBlocked was called, want only OnTerminal for a self-reported blocked")
	}
	if !onTerminalCalled {
		t.Fatal("OnTerminal was never called")
	}
	if gotStatus != "blocked" {
		t.Errorf("status = %q, want %q", gotStatus, "blocked")
	}
	if gotUsageLimit == nil {
		t.Fatal("usageLimit = nil, want it set for a self-reported blocked while the limit tripped on this invocation")
	}

	if !p.IsPaused() {
		t.Error("pause was not tripped")
	}
	snap := p.Snapshot()
	found := false
	for _, n := range snap.BlockedNotes {
		if n == job.NotePath {
			found = true
		}
	}
	if !found {
		t.Errorf("BlockedNotes = %v, want it to contain %q", snap.BlockedNotes, job.NotePath)
	}

	note, err := v.ReadNote(job.NotePath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", note.Frontmatter.Status, "blocked")
	}
}
