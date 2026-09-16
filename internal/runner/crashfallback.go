// Crash-fallback handling: the three post-watchdog scenarios from
// Design - Runner.md's "Crash fallback" section, and the alert payload
// content built from whichever one fired. Distinct from worker's own
// recover() -- this is runner's expected-failure handling for a CLI that
// ran but didn't finish cleanly, not a panic in the Go code itself.
package runner

import (
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// Scenario names one of the three crash-fallback paths (a fourth, hang,
// resolves into scenario 1 or 3 once the idle watchdog kills the process --
// see Design - Runner.md).
type Scenario int

const (
	// ScenarioNoCopy: process killed/exited before writing the copy at all
	// -- OOM, SIGKILL, host crash, or (post-watchdog) a hang that never got
	// as far as a first write.
	ScenarioNoCopy Scenario = iota
	// ScenarioParseFailure: copy exists but fails to parse (broken/
	// truncated YAML) -- treated identically to ScenarioNoCopy per the
	// two-branch trust rule: a parse failure means nothing in the copy can
	// be trusted, no partial credit.
	ScenarioParseFailure
	// ScenarioNonTerminal: copy exists and parses fine, but its status
	// field is missing or still in_progress -- unlike the two above, the
	// runner *can* trust whatever fields did parse (e.g. partial Work Log
	// content) and folds them in before marking blocked.
	ScenarioNonTerminal
)

func (s Scenario) String() string {
	switch s {
	case ScenarioNoCopy:
		return "no_copy"
	case ScenarioParseFailure:
		return "parse_failure"
	case ScenarioNonTerminal:
		return "non_terminal"
	case ScenarioSetupFailure:
		return "setup_failure"
	default:
		return "unknown"
	}
}

// crashInfo is everything handleCrashFallback needs to build the alert and
// write the vault note -- assembled by ProcessTask's read-back switch.
type crashInfo struct {
	scenario   Scenario
	stage      string
	exitErr    error
	stderrTail string
	repoPath   string
	branchName string
	sessionID  string

	// Set only for ScenarioNonTerminal: whatever did parse from the copy,
	// to fold into the vault note before marking blocked.
	haveCopyToUse bool
	partialCopy   *notetask.Note
}

// AlertPayload is the crash-fallback alert's content -- a short Discord
// message plus a fuller .md attachment, per Design - Runner.md's "write
// blocked, then alert alone isn't diagnosable" requirement. Phase 9 builds
// this content; Phase 10's notify package is what actually sends it
// (async, with retry).
type AlertPayload struct {
	Slug     string
	Repo     string
	Scenario Scenario
	Stage    string

	WatchdogKilled bool
	ExitCode       int // -1 if unknown/signal-terminated
	ExitErrText    string
	StderrTail     string

	BranchExists          bool
	HasUncommittedChanges bool
	PRState               string // best-effort; "unknown" if unchecked/unavailable

	LogPointer string // placeholder until the per-task transcript log (Open item) exists
}

// DiscordMessage is the short, one-line summary -- the attachment carries
// the rest. mentionID is the Discord user ID to @-mention (Devara), per
// the bash design's "always @-mentions" behavior.
func (p AlertPayload) DiscordMessage(mentionID string) string {
	return fmt.Sprintf("<@%s> Task `%s` (%s) is **blocked** -- %s during %s. See attached for details.",
		mentionID, p.Slug, p.Repo, p.Scenario, p.Stage)
}

// AttachmentMarkdown is the fuller diagnosable content: task slug/repo,
// scenario name, stage, exit code/stderr tail, target-repo state, and a
// log pointer.
func (p AlertPayload) AttachmentMarkdown() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# Task blocked: %s\n\n", p.Slug)
	fmt.Fprintf(&b, "- **Repo**: %s\n", p.Repo)
	fmt.Fprintf(&b, "- **Scenario**: %s\n", p.Scenario)
	fmt.Fprintf(&b, "- **Stage**: %s\n", p.Stage)
	if p.WatchdogKilled {
		b.WriteString("- **Cause**: the runner's idle watchdog killed the process group after it went silent past the configured idle timeout -- not a bare crash.\n")
	}
	fmt.Fprintf(&b, "- **Exit code**: %d\n", p.ExitCode)
	if p.ExitErrText != "" {
		fmt.Fprintf(&b, "- **Exit error**: %s\n", p.ExitErrText)
	}
	fmt.Fprintf(&b, "- **Branch exists**: %v\n", p.BranchExists)
	fmt.Fprintf(&b, "- **Uncommitted changes in target repo**: %v\n", p.HasUncommittedChanges)
	fmt.Fprintf(&b, "- **PR state**: %s\n", p.PRState)
	fmt.Fprintf(&b, "- **Log**: %s\n", p.LogPointer)
	b.WriteString("\n## stderr tail\n\n```\n")
	if strings.TrimSpace(p.StderrTail) == "" {
		b.WriteString("(empty)")
	} else {
		b.WriteString(p.StderrTail)
	}
	b.WriteString("\n```\n")
	return b.String()
}

// inspectRepoState checks whether branchName exists and whether the repo
// has uncommitted changes -- part of what the alert needs to say "does a
// human resume this or clean up manually."
func inspectRepoState(repoPath, branchName string) (branchExists, hasUncommitted bool) {
	if err := exec.Command("git", "-C", repoPath, "show-ref", "--verify", "--quiet", "refs/heads/"+branchName).Run(); err == nil {
		branchExists = true
	}
	out, err := exec.Command("git", "-C", repoPath, "status", "--porcelain").Output()
	if err == nil && strings.TrimSpace(string(out)) != "" {
		hasUncommitted = true
	}
	return branchExists, hasUncommitted
}

// checkPRState is a best-effort `gh pr view` -- returns "unknown" rather
// than erroring the whole alert if gh isn't available/functional (e.g. a
// local-only test repo with no GitHub remote at all).
func checkPRState(repoPath, branchName string) string {
	out, err := exec.Command("gh", "pr", "view", branchName, "--json", "state,url").CombinedOutput()
	if err != nil {
		return "unknown (gh check failed or unavailable: " + strings.TrimSpace(string(out)) + ")"
	}
	return strings.TrimSpace(string(out))
}

func exitCodeOf(err error) int {
	var exitErr *exec.ExitError
	if err != nil && errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return -1
}

// handleCrashFallback writes status: blocked to the vault note (folding in
// whatever the copy did parse for ScenarioNonTerminal), appends the
// "blocked" Runner Log line, builds the alert payload, and invokes
// Deps.OnBlocked with it.
func handleCrashFallback(deps Deps, job worker.Job, info crashInfo) error {
	writeErr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: blocked %s (%s)", job.Slug, info.scenario), func(n *notetask.Note) error {
		n.Frontmatter.Status = "blocked"
		if info.sessionID != "" {
			n.Frontmatter.SessionID = &info.sessionID
		}
		if info.haveCopyToUse && info.partialCopy != nil {
			// Fold in whatever did parse -- real information CC managed to
			// record, per Design - Runner.md's trust-rule distinction.
			if info.partialCopy.HasWorkLog {
				n.HasWorkLog = true
				n.WorkLog = info.partialCopy.WorkLog
			}
			if info.partialCopy.Frontmatter.PRURL != nil {
				n.Frontmatter.PRURL = info.partialCopy.Frontmatter.PRURL
			}
		}
		notetask.AppendRunnerLog(n, "blocked", time.Now())
		return nil
	})
	if writeErr != nil {
		return fmt.Errorf("runner: writing blocked status for %s (%s): %w", job.Slug, info.scenario, writeErr)
	}

	branchExists, hasUncommitted := inspectRepoState(info.repoPath, info.branchName)
	payload := AlertPayload{
		Slug:                  job.Slug,
		Repo:                  job.Repo,
		Scenario:              info.scenario,
		Stage:                 info.stage,
		WatchdogKilled:        errorIsIdleTimeout(info.exitErr),
		ExitCode:              exitCodeOf(info.exitErr),
		StderrTail:            info.stderrTail,
		BranchExists:          branchExists,
		HasUncommittedChanges: hasUncommitted,
		PRState:               checkPRState(info.repoPath, info.branchName),
		LogPointer:            "(per-task transcript logging not yet built -- see Design - Runner.md's Open section)",
	}
	if info.exitErr != nil {
		payload.ExitErrText = info.exitErr.Error()
	}

	if deps.OnBlocked != nil {
		deps.OnBlocked(payload)
	}

	fmt.Printf("[runner] task %s: blocked (%s) at stage %q\n", job.Slug, info.scenario, info.stage)
	return fmt.Errorf("runner: task %s blocked (%s) at stage %q", job.Slug, info.scenario, info.stage)
}

func errorIsIdleTimeout(err error) bool {
	return err != nil && errors.Is(err, ErrIdleTimeout)
}
