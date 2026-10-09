// Crash-fallback handling: the three post-watchdog scenarios (no copy,
// unparseable copy, non-terminal copy), and the alert payload content
// built from whichever one fired. Distinct from worker's own recover() --
// this is runner's expected-failure handling for a CLI that ran but
// didn't finish cleanly, not a panic in the Go code itself.
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
// resolves into scenario 1 or 3 once the idle watchdog kills the
// process).
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

// ScenarioDoneWithoutPR: copy parses fine and reports status: done, but
// pr_url is empty -- CC's own report of a clean finish can't be trusted
// without the PR it implies exists. Same partial-credit treatment as
// ScenarioNonTerminal: whatever did parse (e.g. Work Log content) is folded
// in before marking blocked. Confirmed live 2026-09-17: task
// random-bs-go-wire-onpanic-to-real-alerting did the real work and set
// status: done, but never ran git commit/push or gh pr create.
//
// Declared as an explicit value outside the iota block above, same pattern
// as setupfallback.go's ScenarioSetupFailure (value 3) -- this package's
// scenarios grew a fourth family across two files, and mixing an iota block
// with a value appended by a different file invites exactly the accidental
// collision a plain sequential iota would hide.
const ScenarioDoneWithoutPR Scenario = 4

// ScenarioUsageLimit: this invocation's claude process hit the Claude usage
// limit (see runner.go's UsageLimitError/ErrUsageLimit) -- takes priority
// over whichever of the scenarios above the copy read-back would otherwise
// have picked, since the real cause is known rather than merely inferred
// from the copy's state. Distinct from every other scenario in one more
// way: handleCrashFallback also records this task's note in the shared
// pause state for main.go's later auto-resume, and the mention set this
// scenario's DiscordMessage uses is Devara-only (see that method) --
// there's nothing for the `dev` Ara-Dev gateway to unblock, the cause
// clears on its own.
const ScenarioUsageLimit Scenario = 5

func (s Scenario) String() string {
	switch s {
	case ScenarioNoCopy:
		return "no_copy"
	case ScenarioParseFailure:
		return "parse_failure"
	case ScenarioNonTerminal:
		return "non_terminal"
	case ScenarioDoneWithoutPR:
		return "done_without_pr"
	case ScenarioSetupFailure:
		return "setup_failure"
	case ScenarioUsageLimit:
		return "usage_limit"
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

	// discordThreadID is the task's discord_thread_id, if it has one --
	// carried through to AlertPayload so the blocked alert routes into the
	// task's own thread instead of the top-level webhook.
	discordThreadID string

	// usageLimit is non-nil only when this invocation's claude process hit
	// the usage limit -- carried through to AlertPayload.UsageLimit
	// regardless of which scenario fires, so a future caller could in
	// principle inspect it even on a scenario the override logic in
	// ProcessTask didn't retag as ScenarioUsageLimit.
	usageLimit *UsageLimitInfo
}

// AlertPayload is the crash-fallback alert's content -- a short Discord
// message plus a fuller .md attachment, since a bare "blocked" write with
// no further detail isn't diagnosable on its own. This file builds the
// content; the notify package is what actually sends it (async, with
// retry).
type AlertPayload struct {
	Slug     string
	Repo     string
	Scenario Scenario
	Stage    string

	// DiscordThreadID is the task's discord_thread_id, if it has one --
	// the caller routes this alert there instead of the top-level webhook.
	DiscordThreadID string

	// UsageLimit is non-nil exactly when Scenario == ScenarioUsageLimit --
	// DiscordMessage/AttachmentMarkdown use it to report the known reset
	// time, if any. Kept as a separate field (rather than inferred solely
	// from Scenario) so callers outside this package can check it directly.
	UsageLimit *UsageLimitInfo

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
// the rest. mentionID and araDevMentionID are the Discord user IDs to
// @-mention (Devara and Ara-Dev): one webhook post mentions both, so the
// `dev` gateway picks up its own mention and auto-threads its reply --
// the 2026-08-31 decision.
func (p AlertPayload) DiscordMessage(mentionID, araDevMentionID string) string {
	if p.Scenario == ScenarioUsageLimit {
		// Devara only, never Ara-Dev: there's nothing for the `dev` gateway
		// to unblock here -- the cause is external and clears on its own
		// once the reset time (if known) passes.
		return fmt.Sprintf("<@%s> Task `%s` (%s) hit the Claude usage limit -- the pipeline is **paused** until it resets%s. See attached for details.",
			mentionID, p.Slug, p.Repo, resetsAtSuffix(p.UsageLimit))
	}
	if p.Scenario == ScenarioDoneWithoutPR {
		return fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) reported **done** but no `pr_url` was found -- marked **blocked** instead, no crash occurred. See attached for details.",
			mentionID, araDevMentionID, p.Slug, p.Repo)
	}
	return fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) is **blocked** -- %s during %s. See attached for details.",
		mentionID, araDevMentionID, p.Slug, p.Repo, p.Scenario, p.Stage)
}

// resetsAtSuffix is DiscordMessage's " (resets at ...)" clause for a usage-
// limit block, or "" if the reset time is unknown.
func resetsAtSuffix(u *UsageLimitInfo) string {
	if u == nil || u.ResetsAt == nil {
		return ""
	}
	return fmt.Sprintf(" (resets at %s)", u.ResetsAt.UTC().Format(time.RFC3339))
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
	if p.Scenario == ScenarioDoneWithoutPR {
		b.WriteString("- **Cause**: CC self-reported `status: done` but left `pr_url` empty -- no crash occurred, claude exited cleanly. The work may be real but is unverified as shipped (no commit/push/PR confirmed).\n")
	}
	if p.Scenario == ScenarioUsageLimit {
		reset := "unknown"
		if p.UsageLimit != nil && p.UsageLimit.ResetsAt != nil {
			reset = p.UsageLimit.ResetsAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "- **Cause**: claude reported hitting the Claude usage limit (rate_limit_info.status != \"allowed\", or a result event with is_error+429) -- not a crash. The whole pipeline is paused until the limit resets (%s); this task will auto-resume via `--resume` once it does.\n", reset)
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
	cmd := exec.Command("gh", "pr", "view", branchName, "--json", "state,url")
	cmd.Dir = repoPath
	out, err := cmd.CombinedOutput()
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
			// record before things went wrong.
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

	if err := preserveBlockedWork(info.repoPath, info.branchName); err != nil {
		fmt.Printf("[runner] task %s: failed to preserve blocked work: %v\n", job.Slug, err)
	}

	if info.scenario == ScenarioUsageLimit && deps.Pause != nil {
		// Records this task for main.go's later auto-resume: once the
		// limit resets, every note recorded here flips blocked ->
		// blocker_resolved and the normal claim path picks it back up via
		// --resume.
		deps.Pause.AddBlockedNote(job.NotePath)
	}

	branchExists, hasUncommitted := inspectRepoState(info.repoPath, info.branchName)
	payload := AlertPayload{
		Slug:                  job.Slug,
		Repo:                  job.Repo,
		Scenario:              info.scenario,
		Stage:                 info.stage,
		DiscordThreadID:       info.discordThreadID,
		UsageLimit:            info.usageLimit,
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
		deps.OnBlocked(job, payload)
	}

	fmt.Printf("[runner] task %s: blocked (%s) at stage %q\n", job.Slug, info.scenario, info.stage)
	return fmt.Errorf("runner: task %s blocked (%s) at stage %q", job.Slug, info.scenario, info.stage)
}

func errorIsIdleTimeout(err error) bool {
	return err != nil && errors.Is(err, ErrIdleTimeout)
}
