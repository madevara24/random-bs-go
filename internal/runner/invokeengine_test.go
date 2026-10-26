package runner

import (
	"os/exec"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/engine"
)

// fakePreAssignEngine is a minimal engine.Engine whose BuildCommand hands
// back a pre-assigned session ID and a command that never emits a
// session_id line of its own -- confirming invokeEngine trusts the
// pre-assigned ID rather than requiring ParseLine to discover one.
type fakePreAssignEngine struct {
	sessionID string
}

func (f *fakePreAssignEngine) Name() string { return "fake" }

func (f *fakePreAssignEngine) BuildCommand(dir, prompt, resumeSessionID string) (*exec.Cmd, string) {
	cmd := exec.Command("echo", "no session_id line here")
	cmd.Dir = dir
	return cmd, f.sessionID
}

func (f *fakePreAssignEngine) ParseLine(line string, state *engine.RunState) {
	// Deliberately never sets state.SessionID -- this engine's whole point
	// is that the caller shouldn't need to scrape one from output.
}

func (f *fakePreAssignEngine) ClassifyExit(waitErr error, stderrTail string, state engine.RunState) error {
	return waitErr
}

func (f *fakePreAssignEngine) BuildReviewCommand(dir, prompt string) *exec.Cmd {
	return exec.Command("true")
}

func (f *fakePreAssignEngine) ParseReviewAnswer(stdout []byte) (string, error) {
	return "", nil
}

func TestInvokeEngineUsesPreAssignedSessionID(t *testing.T) {
	tmpDir := t.TempDir()
	eng := &fakePreAssignEngine{sessionID: "preassigned-session-xyz"}

	sessionID, _, err := invokeEngine(eng, tmpDir, "irrelevant prompt", "", 5*time.Second, nil)
	if err != nil {
		t.Fatalf("invokeEngine: %v", err)
	}
	if sessionID != "preassigned-session-xyz" {
		t.Errorf("sessionID = %q, want the pre-assigned %q (no session_id line was ever emitted)", sessionID, "preassigned-session-xyz")
	}
}
