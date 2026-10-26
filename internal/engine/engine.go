// Package engine abstracts over coding-agent CLIs (claude today, pi/
// DeepSeek later) behind one interface, so internal/runner's process
// lifecycle -- Setpgid, the idle watchdog, the group SIGKILL, the review
// timeout -- stays generic over whichever CLI a given Engine wraps.
//
// This package must never import internal/runner: runner depends on
// engine, not the other way around.
package engine

import (
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// RunState is the shared, CLI-agnostic state accumulated while scanning a
// coding invocation's stdout one line at a time -- SessionID may start
// pre-populated (see Engine.BuildCommand's preAssignedSessionID) rather
// than only ever being discovered via ParseLine.
type RunState struct {
	SessionID          string
	UsageLimitHit      bool
	UsageLimitResetsAt *time.Time
}

// Engine covers exactly what differs between coding-agent CLIs -- argv
// construction, output parsing, and exit classification. Everything else
// (process group, idle watchdog, stderr capture, timeouts) lives in
// internal/runner, generic over any Engine.
type Engine interface {
	// Name identifies this engine, e.g. "claude". Later tooling checks this
	// exact string, so implementations must not change it casually.
	Name() string

	// BuildCommand constructs the coding invocation for dir/prompt,
	// resuming resumeSessionID if non-empty (empty means a fresh session).
	// preAssignedSessionID is the session ID this very invocation will use,
	// if this engine's CLI lets the caller choose one before launch --
	// empty means the caller must instead discover the session ID from
	// stdout via ParseLine, same as claude today.
	BuildCommand(dir, prompt, resumeSessionID string) (cmd *exec.Cmd, preAssignedSessionID string)

	// ParseLine updates state from one line of the coding invocation's
	// stdout. A line this engine doesn't recognize (not JSON, a JSON shape
	// it doesn't care about, ...) is simply ignored.
	ParseLine(line string, state *RunState)

	// ClassifyExit turns a finished invocation's wait error, stderr tail,
	// and final RunState into the error ProcessTask should see: a
	// *UsageLimitError if state or the stderr tail says the usage limit was
	// hit, or waitErr unchanged otherwise.
	ClassifyExit(waitErr error, stderrTail string, state RunState) error

	// BuildReviewCommand constructs a fresh (never resuming) review
	// invocation for prompt, run in dir.
	BuildReviewCommand(dir, prompt string) *exec.Cmd

	// ParseReviewAnswer extracts the review's final answer text from the
	// review invocation's raw stdout.
	ParseReviewAnswer(stdout []byte) (string, error)
}

// ErrUsageLimit is the sentinel every UsageLimitError wraps (via Unwrap,
// so errors.Is(err, ErrUsageLimit) works), returned by an Engine's
// ClassifyExit when it detects the underlying CLI hit its usage limit.
var ErrUsageLimit = errors.New("engine: usage limit hit")

// UsageLimitError is the concrete error ClassifyExit returns on a detected
// usage limit -- a type (not a bare sentinel) so a reset-time epoch can
// ride along with it. Use errors.As to recover one from a wrapped error.
type UsageLimitError struct {
	// ResetsAt is when the limit resets, if the engine's CLI reported one --
	// nil if it didn't (e.g. a stderr-substring fallback match, which
	// carries no structured reset time).
	ResetsAt *time.Time
}

func (e *UsageLimitError) Error() string {
	if e.ResetsAt != nil {
		return fmt.Sprintf("%s (resets at %s)", ErrUsageLimit, e.ResetsAt.UTC().Format(time.RFC3339))
	}
	return fmt.Sprintf("%s (reset time unknown)", ErrUsageLimit)
}

func (e *UsageLimitError) Unwrap() error { return ErrUsageLimit }
