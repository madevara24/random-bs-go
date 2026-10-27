package runner

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/engine"
)

// fakeStartupTimeoutEngine wraps a real exec.Cmd (built by the test, same
// pattern claude/pi use for real) but declares a StartupTimeout -- the
// only thing invokeEngine needs beyond engine.Engine's other methods to
// exercise the startup-timeout path, without needing a real pi binary.
type fakeStartupTimeoutEngine struct {
	bin     string
	timeout time.Duration
}

func (f *fakeStartupTimeoutEngine) Name() string { return "fake-pi" }

func (f *fakeStartupTimeoutEngine) BuildCommand(dir, prompt, resumeSessionID string) (*exec.Cmd, string) {
	cmd := exec.Command(f.bin)
	cmd.Dir = dir
	return cmd, ""
}

func (f *fakeStartupTimeoutEngine) ParseLine(line string, state *engine.RunState) {}

func (f *fakeStartupTimeoutEngine) ClassifyExit(waitErr error, stderrTail string, state engine.RunState) error {
	return waitErr
}

func (f *fakeStartupTimeoutEngine) BuildReviewCommand(dir, prompt string) *exec.Cmd {
	return exec.Command("true")
}

func (f *fakeStartupTimeoutEngine) ParseReviewAnswer(stdout []byte) (string, error) {
	return "", nil
}

func (f *fakeStartupTimeoutEngine) StartupTimeout() time.Duration { return f.timeout }

// TestStartupTimeoutKillsProcessGroupBeforeAnyOutput is RBG-36's startup-
// timeout test gate: a fake pi-shaped stub that never writes a single
// byte to stdout (the silent-hang-on-bad-credentials behavior verified
// live against the real pi binary, see the task body), having also
// spawned a background child of its own. Confirms invokeEngine kills the
// whole process group (not just the top-level PID) within the declared
// StartupTimeout, well before the much longer idle timeout would ever
// fire, and returns a distinct error wrapping ErrStartupTimeout.
func TestStartupTimeoutKillsProcessGroupBeforeAnyOutput(t *testing.T) {
	tmpDir := t.TempDir()
	stubPidFile := filepath.Join(tmpDir, "stub.pid")
	childPidFile := filepath.Join(tmpDir, "child.pid")
	scriptPath := filepath.Join(tmpDir, "fake-pi-silent-hang.sh")

	script := fmt.Sprintf(`#!/usr/bin/env bash
echo $$ > %q
sleep 600 &
echo $! > %q
sleep 999999
`, stubPidFile, childPidFile)

	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake pi stub: %v", err)
	}

	const startupTimeout = 2 * time.Second
	eng := &fakeStartupTimeoutEngine{bin: scriptPath, timeout: startupTimeout}

	start := time.Now()
	// idleTimeout deliberately much longer than startupTimeout: proves the
	// startup timer is the one that actually fires, not the idle watchdog.
	sessionID, _, err := invokeEngine(eng, tmpDir, "irrelevant prompt", "", time.Hour, nil)
	elapsed := time.Since(start)

	if !errors.Is(err, ErrStartupTimeout) {
		t.Fatalf("invokeEngine error = %v, want it to wrap ErrStartupTimeout", err)
	}
	if sessionID != "" {
		t.Errorf("sessionID = %q, want empty (no output ever arrived to parse one from)", sessionID)
	}

	const buffer = 5 * time.Second
	if elapsed > startupTimeout+buffer {
		t.Errorf("startup timeout took %v to fire, want within startupTimeout(%v)+buffer(%v)", elapsed, startupTimeout, buffer)
	}
	if elapsed < startupTimeout {
		t.Errorf("startup timeout fired after only %v, before startupTimeout(%v) even elapsed", elapsed, startupTimeout)
	}

	stubPID := readPIDFile(t, stubPidFile)
	childPID := readPIDFile(t, childPidFile)

	deadline := time.Now().Add(3 * time.Second)
	for {
		stubDead := !processAlive(stubPID)
		childDead := !processAlive(childPID)
		if stubDead && childDead {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("processes still alive after startup timeout fired: stub(pid=%d) alive=%v, child(pid=%d) alive=%v",
				stubPID, processAlive(stubPID), childPID, processAlive(childPID))
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestStartupTimeoutDoesNotFireOnceFirstLineArrives confirms a slow-but-
// eventually-chatty process (first line arrives just before
// startupTimeout, then the process exits cleanly) is never killed --
// StartupTimeout only guards the silence before the very first line, not
// overall runtime.
func TestStartupTimeoutDoesNotFireOnceFirstLineArrives(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-pi-eventually-chatty.sh")
	script := `#!/usr/bin/env bash
sleep 0.3
echo '{"type":"session","id":"rbg-chatty-session"}'
exit 0
`
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake pi stub: %v", err)
	}

	eng := &fakePiLikeEngine{bin: scriptPath, timeout: 2 * time.Second}
	sessionID, _, err := invokeEngine(eng, tmpDir, "irrelevant prompt", "", time.Hour, nil)
	if err != nil {
		t.Fatalf("invokeEngine: %v, want no error (the process exited cleanly after its first line)", err)
	}
	if sessionID != "rbg-chatty-session" {
		t.Errorf("sessionID = %q, want %q", sessionID, "rbg-chatty-session")
	}
}

// fakePiLikeEngine is like fakeStartupTimeoutEngine but actually parses
// the session header via ParseLine -- TestStartupTimeoutDoesNotFireOnceFirstLineArrives
// needs real session-ID capture to confirm the run was treated as
// perfectly normal, not just error-free.
type fakePiLikeEngine struct {
	bin     string
	timeout time.Duration
}

func (f *fakePiLikeEngine) Name() string { return "fake-pi" }

func (f *fakePiLikeEngine) BuildCommand(dir, prompt, resumeSessionID string) (*exec.Cmd, string) {
	cmd := exec.Command(f.bin)
	cmd.Dir = dir
	return cmd, ""
}

func (f *fakePiLikeEngine) ParseLine(line string, state *engine.RunState) {
	engine.NewPi("", "", "", "", 0).ParseLine(line, state)
}

func (f *fakePiLikeEngine) ClassifyExit(waitErr error, stderrTail string, state engine.RunState) error {
	return engine.NewPi("", "", "", "", 0).ClassifyExit(waitErr, stderrTail, state)
}

func (f *fakePiLikeEngine) BuildReviewCommand(dir, prompt string) *exec.Cmd {
	return exec.Command("true")
}

func (f *fakePiLikeEngine) ParseReviewAnswer(stdout []byte) (string, error) {
	return "", nil
}

func (f *fakePiLikeEngine) StartupTimeout() time.Duration { return f.timeout }
