// Preserving a blocked task's work: once the runner decides a task is
// blocked -- crash-fallback or CC's own self-report -- whatever the
// clone's working tree holds on the task branch is the only copy of that
// work. The same clone gets reused for the next task, and pullAndBranch's
// clean-before-branching step (runner.go) resets and cleans it before that
// next task ever branches, so anything left uncommitted here would
// otherwise just vanish (or, before that clean step existed, leak into the
// next task's tree -- the MLT-27/MLT-28 incident this file exists for).
// Committing and pushing here, before the runner moves on, is what makes a
// blocked task's work survive onto origin instead.
package runner

import (
	"fmt"
	"os/exec"
	"strings"

	"github.com/madevara24/random-bs-go/internal/vaultgit"
)

// blockPreserveExcludePaths lists paths that must never be swept into a
// blocked-task's preserve commit, even by `git add -A` -- known
// tool-side-effect paths that are already *tracked* in some target repos
// (e.g. .serena/project.yml gets rewritten as a side effect of the Serena
// MCP tool merely being used during the session). The note-copy doesn't
// need an entry here: it's untracked and already kept out of `git add` via
// .git/info/exclude (excludeFromGit), which can't help with paths that are
// already tracked.
var blockPreserveExcludePaths = []string{".serena", ".claude"}

// preserveBlockedWork commits and pushes the task branch's current working
// tree, so a resumed session (via checkoutTaskBranch) or a human looking at
// the PR sees the clone's exact state as of the block.
//
// Only acts if the clone is currently checked out on branchName -- a setup
// failure that never got as far as creating the branch has nothing to
// preserve, and this must never commit or push on behalf of whatever other
// branch the clone happens to be sitting on.
func preserveBlockedWork(repoPath, branchName string) error {
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoPath
		cmd.Env = vaultgit.CleanGitEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s (in %s): %w\n%s", strings.Join(args, " "), repoPath, err, out)
		}
		return nil
	}

	current, err := currentGitBranch(repoPath)
	if err != nil {
		return fmt.Errorf("preserve blocked work: resolving current branch: %w", err)
	}
	if current != branchName {
		return nil
	}

	addArgs := []string{"add", "-A", "--", "."}
	for _, p := range blockPreserveExcludePaths {
		addArgs = append(addArgs, ":(exclude)"+p)
	}
	if err := run(addArgs...); err != nil {
		return fmt.Errorf("preserve blocked work: git add: %w", err)
	}

	staged, err := hasStagedChanges(repoPath)
	if err != nil {
		return fmt.Errorf("preserve blocked work: checking staged changes: %w", err)
	}
	if staged {
		if err := run("commit", "-m", "runner: preserve blocked task work"); err != nil {
			return fmt.Errorf("preserve blocked work: git commit: %w", err)
		}
	}

	// Always push, even with nothing freshly committed here -- a prior
	// resume attempt may have committed but never pushed before crashing
	// again, and a no-op push against an up-to-date remote still succeeds.
	if err := run("push", "-u", "origin", branchName); err != nil {
		return fmt.Errorf("preserve blocked work: git push: %w", err)
	}
	return nil
}

func currentGitBranch(repoPath string) (string, error) {
	out, err := exec.Command("git", "-C", repoPath, "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse --abbrev-ref HEAD (in %s): %w", repoPath, err)
	}
	return strings.TrimSpace(string(out)), nil
}

func hasStagedChanges(repoPath string) (bool, error) {
	out, err := exec.Command("git", "-C", repoPath, "diff", "--cached", "--name-only").Output()
	if err != nil {
		return false, fmt.Errorf("git diff --cached --name-only (in %s): %w", repoPath, err)
	}
	return strings.TrimSpace(string(out)) != "", nil
}
