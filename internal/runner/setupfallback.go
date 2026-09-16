// Setup-phase failure handling: ProcessTask's error path for the five
// steps that run before Claude is ever invoked (claim write, re-read,
// branch setup, note-copy write, git-exclude). Sibling to
// crashfallback.go's handleCrashFallback, which covers the three scenarios
// where Claude *did* run but didn't finish cleanly -- this file exists
// because, before it, a setup failure just returned a bare error that
// reached only worker's local log: the vault note was left stuck at
// whatever status the last successful step left it, forever, with no
// alert. Confirmed live in production 2026-09-17 (see task notes): an
// invalid branch name made pullAndBranch's final `checkout -b` fail, and
// the note sat at in_progress until someone noticed by hand.
package runner

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// ScenarioSetupFailure covers all five pre-invocation setup steps -- a new
// family of blocked scenario, sibling to crashfallback.go's three
// post-invocation ones (see that file's Scenario doc comment), not a
// replacement for any of them.
const ScenarioSetupFailure Scenario = 3

// setupStage names one of the five setup steps that run before Claude is
// ever invoked.
type setupStage int

const (
	setupStageWriteInProgress setupStage = iota
	setupStageRereadNote
	setupStagePullAndBranch
	setupStageWriteNoteCopy
	setupStageExcludeFromGit
)

func (s setupStage) String() string {
	switch s {
	case setupStageWriteInProgress:
		return "writing status: in_progress"
	case setupStageRereadNote:
		return "re-reading note after in_progress write"
	case setupStagePullAndBranch:
		return "pulling default branch and creating task branch"
	case setupStageWriteNoteCopy:
		return "writing note-copy into target repo"
	case setupStageExcludeFromGit:
		return "excluding note-copy from git"
	default:
		return "unknown setup stage"
	}
}

// repoTouched reports whether this stage's failure could have left the
// target repo clone in a state that needs restoring (a branch checked
// out, a note-copy file written) -- the first two stages fail before the
// runner has touched the target repo clone at all, so there's nothing
// there to restore.
func (s setupStage) repoTouched() bool {
	return s >= setupStagePullAndBranch
}

// setupFailureInfo is everything handleSetupFailure needs to best-effort
// restore the target repo and build the blocked-alert content.
type setupFailureInfo struct {
	stage      setupStage
	err        error
	repoCfg    config.RepoConfig
	branchName string // empty if this stage never got as far as computing one
	copyPath   string // empty if this stage never got as far as writing one

	// resume is true when this ProcessTask call is resuming a prior
	// session (pullAndBranch never ran this invocation -- the task branch,
	// if it exists, predates this run and holds real prior work). Only
	// writeNoteCopy/excludeFromGit can fail on a resume, since those are
	// the only two of the five steps that still run when resuming; cleanup
	// must never delete that branch in that case.
	resume bool
}

// handleSetupFailure is ProcessTask's error path for the five setup steps
// that run before Claude is ever invoked. It writes status: blocked to the
// vault note (best-effort -- see below), best-effort restores the target
// repo, and always fires the Discord alert regardless of whether the
// blocked write itself succeeded. That last part matters most for a
// failure at the very first WriteNote call: if the vault write is what's
// broken, a second write to set blocked may fail the exact same way, but
// Discord doesn't depend on the vault being writable at all, so it must
// still fire either way.
func handleSetupFailure(deps Deps, job worker.Job, info setupFailureInfo) error {
	if info.stage.repoTouched() {
		restoreRepo(job, info)
	}

	entry := fmt.Sprintf("%s: setup failed at stage %q: %v", time.Now().UTC().Format(time.RFC3339), info.stage, info.err)
	writeErr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: blocked %s (setup: %s)", job.Slug, info.stage), func(n *notetask.Note) error {
		n.Frontmatter.Status = "blocked"
		if strings.TrimSpace(n.WorkLog) == "" {
			n.WorkLog = entry
		} else {
			n.WorkLog = strings.TrimSpace(n.WorkLog) + "\n" + entry
		}
		n.HasWorkLog = true
		notetask.AppendRunnerLog(n, "blocked", time.Now())
		return nil
	})
	if writeErr != nil {
		fmt.Printf("[runner] task %s: failed to write blocked status after setup failure at %q: %v\n", job.Slug, info.stage, writeErr)
	}

	payload := AlertPayload{
		Slug:        job.Slug,
		Repo:        job.Repo,
		Scenario:    ScenarioSetupFailure,
		Stage:       "setup: " + info.stage.String(),
		ExitCode:    -1,
		ExitErrText: info.err.Error(),
		PRState:     "unknown (task never reached invocation)",
		LogPointer:  "(per-task transcript logging not yet built -- see Design - Runner.md's Open section)",
	}
	if info.stage.repoTouched() {
		payload.BranchExists, payload.HasUncommittedChanges = inspectRepoState(info.repoCfg.Path, info.branchName)
	}

	// Always fires, independent of writeErr above -- see doc comment.
	if deps.OnBlocked != nil {
		deps.OnBlocked(payload)
	}

	fmt.Printf("[runner] task %s: blocked (setup failure at %q): %v\n", job.Slug, info.stage, info.err)
	return fmt.Errorf("runner: task %s blocked (setup failure at %q): %w", job.Slug, info.stage, info.err)
}

// restoreRepo best-effort undoes whatever the target repo clone had done
// to it before the failure: deletes the note-copy file if present, and --
// only for a non-resume run, see setupFailureInfo.resume -- deletes the
// task branch and checks the clone back onto its default branch. Every
// step here is best-effort and only logged on failure -- never let a
// failed cleanup block the alert handleSetupFailure fires next.
func restoreRepo(job worker.Job, info setupFailureInfo) {
	if info.copyPath != "" {
		if err := os.Remove(info.copyPath); err != nil && !os.IsNotExist(err) {
			fmt.Printf("[runner] task %s: cleanup: failed to remove note-copy %s: %v\n", job.Slug, info.copyPath, err)
		}
	}

	if info.resume {
		// pullAndBranch never ran this invocation -- the task branch (if
		// it exists at all) predates this run and holds a prior session's
		// real work, not something this failure left behind. Leave it and
		// the current checkout alone.
		return
	}

	runGit := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = info.repoCfg.Path
		cmd.Env = vaultgit.CleanGitEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s (in %s): %w\n%s", strings.Join(args, " "), info.repoCfg.Path, err, out)
		}
		return nil
	}

	if err := runGit("checkout", info.repoCfg.DefaultBranch); err != nil {
		fmt.Printf("[runner] task %s: cleanup: failed to check out default branch %s: %v\n", job.Slug, info.repoCfg.DefaultBranch, err)
	}
	if info.branchName != "" {
		if err := runGit("branch", "-D", info.branchName); err != nil {
			fmt.Printf("[runner] task %s: cleanup: failed to delete branch %s: %v\n", job.Slug, info.branchName, err)
		}
	}
}
