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

// ScenarioPreserveFailure: ProcessTask's self-reported-blocked path (CC
// itself set status: blocked and exited cleanly) whose own call to
// preserveBlockedWork failed, with no crash scenario of any other kind to
// report alongside it -- the only case this tag is used for. A
// crash-fallback scenario (the ones above) whose preserveBlockedWork call
// also fails keeps its own scenario rather than retagging to this one;
// AlertPayload.PreserveErr carries the failure either way, see
// handleCrashFallback and ProcessTask's self-reported-blocked block.
const ScenarioPreserveFailure Scenario = 6

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
	case ScenarioPreserveFailure:
		return "preserve_failure"
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

	// engineName is the coding engine this invocation actually ran --
	// carried through to AlertPayload.EngineName so the alert/Work Log
	// text names the real engine instead of a hardcoded "claude".
	engineName string

	// Set only for ScenarioNonTerminal: whatever did parse from the copy,
	// to fold into the vault note before marking blocked.
	haveCopyToUse bool
	partialCopy   *notetask.Note

	// discordThreadID is the task's discord_thread_id, if it has one --
	// carried through to AlertPayload so the blocked alert routes into the
	// task's own thread instead of the top-level webhook.
	discordThreadID string

	// usageLimit is non-nil only when this invocation's coding engine hit
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

	// EngineName is the coding engine this invocation actually ran --
	// "claude" today, since that's the only engine that exists, but
	// DiscordMessage/AttachmentMarkdown always name it explicitly rather
	// than hardcoding "Claude"/"CC" so a second engine's alerts read
	// correctly without any further change here. Empty only for
	// ScenarioPreserveFailure (preserveFailurePayload never names an
	// engine in its own text).
	EngineName string

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

	// PreserveErr is preserveBlockedWork's error text, or "" when it
	// succeeded (or had nothing to preserve). Empty is the byte-identical
	// case for DiscordMessage, which renders exactly as it did before this
	// field existed; AttachmentMarkdown always gains a one-line "Preserved"
	// summary ("yes" when this is empty). Non-empty means the blocked
	// task's work exists only in the clone's uncommitted tree at RepoPath,
	// which pullAndBranch will reset away the moment the next task in this
	// repo starts -- confirmed live 2026-10-26 (RBG-33), where a `git add`
	// permission error left real work silently unrecoverable.
	PreserveErr string

	// RepoPath is the target repo clone's filesystem path -- only rendered
	// when PreserveErr is set, so a human knows exactly where to go rescue
	// the uncommitted work before it's lost.
	RepoPath string
}

// DiscordMessage is the short, one-line summary -- the attachment carries
// the rest. mentionID and araDevMentionID are the Discord user IDs to
// @-mention (Devara and Ara-Dev): one webhook post mentions both, so the
// `dev` gateway picks up its own mention and auto-threads its reply --
// the 2026-08-31 decision.
//
// A non-empty PreserveErr overrides every scenario-specific branch below,
// ScenarioUsageLimit's Devara-only mention included: a failed preserve
// means the work survives nowhere but the clone's dirty tree, about to be
// reset away by the next task in this repo, which needs a human's hand
// regardless of why the task blocked in the first place.
func (p AlertPayload) DiscordMessage(mentionID, araDevMentionID string) string {
	if p.PreserveErr != "" {
		return fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) is **blocked**, and the blocked work was **not preserved**: %s. The work exists only in the clone's uncommitted tree at %s -- the next task in this repo will reset it away. See attached for details.",
			mentionID, araDevMentionID, p.Slug, p.Repo, p.PreserveErr, p.RepoPath)
	}
	if p.Scenario == ScenarioUsageLimit {
		// Devara only, never Ara-Dev: there's nothing for the `dev` gateway
		// to unblock here -- the cause is external and clears on its own
		// once the reset time (if known) passes.
		return fmt.Sprintf("<@%s> Task `%s` (%s) hit the %s usage limit -- the pipeline is **paused** until it resets%s. See attached for details.",
			mentionID, p.Slug, p.Repo, p.EngineName, p.UsageLimit.ResetsAtSuffix())
	}
	if p.Scenario == ScenarioDoneWithoutPR {
		return fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) reported **done** but no `pr_url` was found -- marked **blocked** instead, no crash occurred. See attached for details.",
			mentionID, araDevMentionID, p.Slug, p.Repo)
	}
	return fmt.Sprintf("<@%s> <@%s> Task `%s` (%s) is **blocked** -- %s during %s. See attached for details.",
		mentionID, araDevMentionID, p.Slug, p.Repo, p.Scenario, p.Stage)
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
		fmt.Fprintf(&b, "- **Cause**: the %s session self-reported `status: done` but left `pr_url` empty -- no crash occurred, %s exited cleanly. The work may be real but is unverified as shipped (no commit/push/PR confirmed).\n", p.EngineName, p.EngineName)
	}
	if p.Scenario == ScenarioUsageLimit {
		reset := "unknown"
		if p.UsageLimit != nil && p.UsageLimit.ResetsAt != nil {
			reset = p.UsageLimit.ResetsAt.UTC().Format(time.RFC3339)
		}
		fmt.Fprintf(&b, "- **Cause**: %s reported hitting the %s usage limit (rate_limit_info.status neither \"allowed\" nor \"allowed_warning\", or a result event with is_error+429) -- not a crash. The whole pipeline is paused until the limit resets (%s); this task will auto-resume via `--resume` once it does.\n", p.EngineName, p.EngineName, reset)
	}
	fmt.Fprintf(&b, "- **Exit code**: %d\n", p.ExitCode)
	if p.ExitErrText != "" {
		fmt.Fprintf(&b, "- **Exit error**: %s\n", p.ExitErrText)
	}
	fmt.Fprintf(&b, "- **Branch exists**: %v\n", p.BranchExists)
	fmt.Fprintf(&b, "- **Uncommitted changes in target repo**: %v\n", p.HasUncommittedChanges)
	fmt.Fprintf(&b, "- **PR state**: %s\n", p.PRState)
	if p.PreserveErr == "" {
		b.WriteString("- **Preserved**: yes\n")
	} else {
		fmt.Fprintf(&b, "- **Preserved**: %s -- the work exists only in the uncommitted tree at %s; the next task in this repo will reset it away.\n", p.PreserveErr, p.RepoPath)
	}
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

	var preserveErrText string
	if err := preserveBlockedWork(info.repoPath, info.branchName); err != nil {
		preserveErrText = err.Error()
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
		EngineName:            info.engineName,
		UsageLimit:            info.usageLimit,
		WatchdogKilled:        errorIsIdleTimeout(info.exitErr),
		ExitCode:              exitCodeOf(info.exitErr),
		StderrTail:            info.stderrTail,
		BranchExists:          branchExists,
		HasUncommittedChanges: hasUncommitted,
		PRState:               checkPRState(info.repoPath, info.branchName),
		LogPointer:            "(per-task transcript logging not yet built -- see Design - Runner.md's Open section)",
		PreserveErr:           preserveErrText,
		RepoPath:              info.repoPath,
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

// preserveFailurePayload builds the AlertPayload for ProcessTask's
// self-reported-blocked path (runner.go) when its own call to
// preserveBlockedWork fails -- that path has no crash scenario of its own
// to report, only the preserve failure itself, so it shares
// handleCrashFallback's building blocks (inspectRepoState, checkPRState,
// the log-pointer placeholder) here instead of duplicating them, so both
// call sites render the same not-preserved warning in DiscordMessage/
// AttachmentMarkdown.
func preserveFailurePayload(job worker.Job, repoPath, branchName, discordThreadID string, preserveErr error) AlertPayload {
	branchExists, hasUncommitted := inspectRepoState(repoPath, branchName)
	return AlertPayload{
		Slug:                  job.Slug,
		Repo:                  job.Repo,
		Scenario:              ScenarioPreserveFailure,
		Stage:                 "preserving blocked work",
		DiscordThreadID:       discordThreadID,
		ExitCode:              -1,
		BranchExists:          branchExists,
		HasUncommittedChanges: hasUncommitted,
		PRState:               checkPRState(repoPath, branchName),
		LogPointer:            "(per-task transcript logging not yet built -- see Design - Runner.md's Open section)",
		PreserveErr:           preserveErr.Error(),
		RepoPath:              repoPath,
	}
}
