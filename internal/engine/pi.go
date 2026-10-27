package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os/exec"
	"time"
)

// PiEngine drives the pi coding-agent CLI (`pi`), configured to talk to a
// DeepSeek model -- pi's own credential store and settings for the
// runner's OS user already hold the DeepSeek API key and default model,
// so this engine never passes --api-key and only passes --model when the
// operator has configured one explicitly (see NewPi).
//
// Facts this engine is built against (pi 1.4.0, verified on the
// production host, see RBG-36's task body -- not independently
// discoverable anywhere else):
//   - JSON mode (`--mode json`) emits strict JSONL on stdout, one object
//     per line: a `session` header first, then a sequence of typed
//     events, ending in `agent_settled`.
//   - A failed or aborted model response does not change the process's
//     exit code -- JSON mode still exits 0. Failure is detected from two
//     event shapes instead (see ParseLine): an assistant message_end
//     whose stopReason is "error"/"aborted", or an auto_retry_end whose
//     success is false.
//   - A bad credential makes pi hang silently -- no stdout, no stderr,
//     no exit -- which is what PiEngine.StartupTimeout exists to catch
//     (internal/runner's invokeEngine kills the process group if no
//     stdout line arrives in time).
type PiEngine struct {
	// bin is the pi binary's path/name -- "" defaults to "pi" (see bin_()).
	bin string
	// model is passed as --model on the coding invocation when non-empty;
	// empty means pi uses its own configured default model.
	model string
	// reviewModel is passed as --model on the review invocation when
	// non-empty; independent of model (the caller resolves "defaults to
	// model" before constructing this engine, see internal/config).
	reviewModel string
	// sessionDir is passed as --session-dir on every coding invocation.
	// Must already exist -- PiEngine never creates it (see NewPi).
	sessionDir string
	// startupTimeout is StartupTimeout's return value.
	startupTimeout time.Duration
}

// NewPi constructs the pi engine. bin is pi's path/name ("" means "pi",
// resolved via PATH); model/reviewModel are the --model values for the
// coding/review invocations respectively ("" means omit --model, letting
// pi fall back to its own configured default); sessionDir is the
// directory pi's --session-dir points at. NewPi does no I/O -- it never
// creates sessionDir itself, since it's also called with empty arguments
// just to list registered engine names (see cmd/pmrunner/runner_mode.go's
// registeredEngineNames); the caller creates sessionDir once at startup.
func NewPi(bin, model, reviewModel, sessionDir string, startupTimeout time.Duration) *PiEngine {
	return &PiEngine{bin: bin, model: model, reviewModel: reviewModel, sessionDir: sessionDir, startupTimeout: startupTimeout}
}

func (p *PiEngine) bin_() string {
	if p.bin == "" {
		return "pi"
	}
	return p.bin
}

// Name implements Engine.
func (p *PiEngine) Name() string { return "pi" }

// newSessionID generates a fresh, unique pi session ID -- "rbg-" plus 16
// random hex characters from crypto/rand. Deliberately never derived from
// the task slug alone: a task note re-filed under the same slug (e.g.
// after a prior run finished or was abandoned) must get a brand-new pi
// session, not accidentally resume an old one just because the slug
// matches.
func newSessionID() string {
	buf := make([]byte, 8)
	if _, err := rand.Read(buf); err != nil {
		panic(fmt.Sprintf("engine: reading random bytes for a pi session id: %v", err))
	}
	return "rbg-" + hex.EncodeToString(buf)
}

// BuildCommand implements Engine. pi always lets the caller choose its
// own session ID up front (via --session-id), so preAssignedSessionID is
// always non-empty: a fresh run mints one with newSessionID, a resume
// reuses resumeSessionID exactly as given.
func (p *PiEngine) BuildCommand(dir, prompt, resumeSessionID string) (*exec.Cmd, string) {
	sessionID := resumeSessionID
	if sessionID == "" {
		sessionID = newSessionID()
	}

	args := []string{"--mode", "json", "-p", "--session-id", sessionID, "--session-dir", p.sessionDir, "--no-approve"}
	if p.model != "" {
		args = append(args, "--model", p.model)
	}
	args = append(args, "--", prompt)

	cmd := exec.Command(p.bin_(), args...)
	cmd.Dir = dir
	return cmd, sessionID
}

// ParseLine implements Engine -- scans pi's JSONL for the session header
// (session ID, only if the caller hadn't already pre-assigned one -- see
// BuildCommand) and the two failure shapes a JSON-mode run can end on
// without ever changing its exit code: an assistant message_end whose
// stopReason is "error"/"aborted" (text in errorMessage), or an
// auto_retry_end whose success is false (text in finalError). The first
// failure signal seen wins; later lines don't overwrite it.
func (p *PiEngine) ParseLine(line string, state *RunState) {
	var evt map[string]any
	if err := json.Unmarshal([]byte(line), &evt); err != nil {
		return // not every line need be JSON we care about
	}

	switch evt["type"] {
	case "session":
		if state.SessionID == "" {
			if id, ok := evt["id"].(string); ok && id != "" {
				state.SessionID = id
			}
		}
	case "message_end":
		if state.FailureText != "" {
			return
		}
		msg, ok := evt["message"].(map[string]any)
		if !ok {
			return
		}
		role, _ := msg["role"].(string)
		stopReason, _ := msg["stopReason"].(string)
		if role != "assistant" || (stopReason != "error" && stopReason != "aborted") {
			return
		}
		if errMsg, ok := msg["errorMessage"].(string); ok && errMsg != "" {
			state.FailureText = errMsg
		} else {
			state.FailureText = fmt.Sprintf("assistant message ended with stopReason %q", stopReason)
		}
	case "auto_retry_end":
		if state.FailureText != "" {
			return
		}
		success, ok := evt["success"].(bool)
		if !ok || success {
			return
		}
		if finalErr, ok := evt["finalError"].(string); ok && finalErr != "" {
			state.FailureText = finalErr
		} else {
			state.FailureText = "auto_retry_end reported failure with no finalError"
		}
	}
}

// ClassifyExit implements Engine. A FailureText recorded by ParseLine is
// an error even though pi's JSON mode exits 0 in that case -- never
// classified as a usage limit (no DeepSeek balance/quota error sample
// exists yet to build that detection against, see the task's Out of
// scope).
func (p *PiEngine) ClassifyExit(waitErr error, stderrTail string, state RunState) error {
	if state.FailureText != "" {
		return fmt.Errorf("pi reported a failed response: %s", state.FailureText)
	}
	return waitErr
}

// BuildReviewCommand implements Engine -- a fresh (--no-session),
// read-only (--tools read,grep,find,ls) review invocation; print mode, no
// --mode json, since the review path only needs the final answer text.
func (p *PiEngine) BuildReviewCommand(dir, prompt string) *exec.Cmd {
	args := []string{"-p", "--no-session", "--tools", "read,grep,find,ls", "--no-approve"}
	if p.reviewModel != "" {
		args = append(args, "--model", p.reviewModel)
	}
	args = append(args, "--", prompt)

	cmd := exec.Command(p.bin_(), args...)
	cmd.Dir = dir
	return cmd
}

// ParseReviewAnswer implements Engine -- pi's print mode writes only the
// final assistant text to stdout, so the answer is stdout as-is.
func (p *PiEngine) ParseReviewAnswer(stdout []byte) (string, error) {
	return string(stdout), nil
}

// StartupTimeout implements Engine.
func (p *PiEngine) StartupTimeout() time.Duration { return p.startupTimeout }
