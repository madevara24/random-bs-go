package engine

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ClaudeEngine drives the Claude Code CLI (`claude`) -- the engine every
// task has run on so far, moved here unchanged from internal/runner's
// former invokeClaude/reviewOnce.
type ClaudeEngine struct {
	// bin is the claude binary's path/name -- "" defaults to "claude" (see
	// bin()), overridable so tests can point it at a fake stub script.
	bin string
}

// NewClaude constructs the claude engine. bin is the claude CLI's
// path/name; "" means "claude" (resolved via PATH).
func NewClaude(bin string) *ClaudeEngine {
	return &ClaudeEngine{bin: bin}
}

func (c *ClaudeEngine) bin_() string {
	if c.bin == "" {
		return "claude"
	}
	return c.bin
}

// Name implements Engine.
func (c *ClaudeEngine) Name() string { return "claude" }

// BuildCommand implements Engine -- the exact argv invokeClaude used to
// build: `-p <prompt> --output-format stream-json --verbose
// --dangerously-skip-permissions`, plus `--resume <id>` when resuming.
// claude never lets the caller pre-assign a session ID, so
// preAssignedSessionID is always "".
func (c *ClaudeEngine) BuildCommand(dir, prompt, resumeSessionID string) (*exec.Cmd, string) {
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
	if resumeSessionID != "" {
		args = append(args, "--resume", resumeSessionID)
	}
	cmd := exec.Command(c.bin_(), args...)
	cmd.Dir = dir
	return cmd, ""
}

// ParseLine implements Engine -- the stream-json line handling moved
// verbatim out of invokeClaude's scan loop: captures session_id from
// whichever line first carries one, and detects a usage-limit hit via
// either a rate_limit_event line with rate_limit_info.status != "allowed"
// and != "allowed_warning" (capturing resetsAt if present), or a result
// line with is_error and a 429 api_error_status -- verified live
// 2026-10-09 against claude 2.1.278. Deliberately never keyed on
// result.subtype (stays "success" even on the limit) or on overageStatus
// (reads "rejected" in both the healthy and limited state).
//
// "allowed_warning" is logged (every status other than "allowed" is) but
// never counted as a hit: production observed it from 90% utilization
// up, several minutes ahead of the real "rejected" -- treating it as a
// hit paused the pipeline on a task that still finished cleanly.
func (c *ClaudeEngine) ParseLine(line string, state *RunState) {
	var evt map[string]any
	if err := json.Unmarshal([]byte(line), &evt); err != nil {
		return // not every line need be JSON we care about
	}
	if state.SessionID == "" {
		if sid, ok := evt["session_id"].(string); ok && sid != "" {
			state.SessionID = sid
		}
	}

	switch evt["type"] {
	case "rate_limit_event":
		if info, ok := evt["rate_limit_info"].(map[string]any); ok {
			if status, _ := info["status"].(string); status != "" && status != "allowed" {
				fmt.Printf("[engine/claude] rate_limit_event (not allowed): %s\n", line)
				if status != "allowed_warning" {
					state.UsageLimitHit = true
					if ra, ok := info["resetsAt"].(float64); ok {
						t := time.Unix(int64(ra), 0)
						state.UsageLimitResetsAt = &t
					}
				}
			}
		}
	case "result":
		// Corroborating signal, independent of rate_limit_event above in
		// case a future version ever omits it: is_error with a 429
		// api_error_status.
		if isErr, _ := evt["is_error"].(bool); isErr {
			if code, ok := evt["api_error_status"].(float64); ok && int(code) == 429 {
				state.UsageLimitHit = true
			}
		}
	}
}

// ClassifyExit implements Engine. state.UsageLimitHit (set by ParseLine)
// is checked first; failing that, a fallback for a future claude version
// that could exit before ever emitting the structured event: if the
// process errored out and its stderr tail still reads like a usage-limit
// message, treat it the same way, just without a known resetsAt.
func (c *ClaudeEngine) ClassifyExit(waitErr error, stderrTail string, state RunState) error {
	if state.UsageLimitHit {
		return &UsageLimitError{ResetsAt: state.UsageLimitResetsAt}
	}
	if waitErr != nil && looksLikeUsageLimitMessage(stderrTail) {
		return &UsageLimitError{}
	}
	return waitErr
}

// looksLikeUsageLimitMessage is ClassifyExit's fallback detector, for a
// future claude version that exits before ever emitting a structured
// rate_limit_event/result line -- a best-effort substring match against
// the wording captured live 2026-10-09 ("You've hit your session limit").
func looksLikeUsageLimitMessage(s string) bool {
	s = strings.ToLower(s)
	return strings.Contains(s, "usage limit") || strings.Contains(s, "session limit") || strings.Contains(s, "rate limit")
}

// BuildReviewCommand implements Engine -- a fresh (no --resume, no vault
// access) review invocation, `--output-format json` rather than
// stream-json since it never streams a rate_limit_event line and isn't
// covered by usage-limit detection (RBG-24, out of scope).
func (c *ClaudeEngine) BuildReviewCommand(dir, prompt string) *exec.Cmd {
	args := []string{"-p", prompt, "--output-format", "json", "--dangerously-skip-permissions"}
	cmd := exec.Command(c.bin_(), args...)
	cmd.Dir = dir
	return cmd
}

// ParseReviewAnswer implements Engine -- claude's `--output-format json`
// wraps the final answer text in a top-level "result" field.
func (c *ClaudeEngine) ParseReviewAnswer(stdout []byte) (string, error) {
	var result struct {
		Result string `json:"result"`
	}
	if err := json.Unmarshal(stdout, &result); err != nil {
		return "", fmt.Errorf("parsing review invocation output: %w\n%s", err, stdout)
	}
	return result.Result, nil
}
