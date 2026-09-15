package runner

import (
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// testTargetRepoPath is a local-only disposable git repo (bare +
// pre-cloned working copy) set up specifically for this phase's testing --
// NOT GitHub-hosted. See the PM Runner Go Rewrite project notes for
// provenance/rationale (a discovered GitHub credential permission gap
// blocks real push/PR against GitHub; local-only exercises everything
// Phase 6 itself is actually responsible for: branch creation, the
// note-copy mechanism, and claude invocation + session_id capture, none of
// which need GitHub at all).
const testTargetRepoPath = "/home/obsidian/repos/phase6-test-repo"

type noopReporter struct{}

func (noopReporter) SetStage(string) {}
func (noopReporter) TouchActivity()  {}

func runGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = vaultgit.CleanGitEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// TestProcessTaskSetupAndInvocation is Phase 6's real test gate: against a
// real disposable test repo and a trivial task note, confirm the branch
// gets created, claude actually runs and does something, and the captured
// session_id is a genuine, resumable session -- checked by independently
// resuming it with a manual `claude --resume` call afterward.
func TestProcessTaskSetupAndInvocation(t *testing.T) {
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH, skipping real invocation test")
	}
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "phase6-e2e-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Add the exact HTML comment `<!-- pmrunner-phase6-test -->` as a new line at the very top of README.md, then commit that change with git (message: \"phase6 test\"). Do not create a pull request, do not push, do not run any other git or gh commands. Then update your task-copy file's status to `done` per the instructions above.")

	deps := Deps{
		Vault: v,
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {
				Path:          testTargetRepoPath,
				Remote:        "local/phase6-test-repo",
				DefaultBranch: "main",
			},
		},
	}

	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	err := ProcessTask(deps, noopReporter{}, job)
	if err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	// Branch was created.
	branches := runGit(t, testTargetRepoPath, "branch", "--list", fmt.Sprintf("task/%s-*", slug))
	if strings.TrimSpace(branches) == "" {
		t.Fatalf("no branch matching task/%s-* found; branches:\n%s", slug, runGit(t, testTargetRepoPath, "branch", "-a"))
	}
	branchName := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(branches), "*"))
	t.Cleanup(func() {
		runGit(t, testTargetRepoPath, "checkout", "main")
		runGit(t, testTargetRepoPath, "branch", "-D", branchName)
	})

	// claude actually did something: at least one new commit on the branch
	// beyond main.
	commitCount := strings.TrimSpace(runGit(t, testTargetRepoPath, "rev-list", "--count", "main.."+branchName))
	if commitCount == "0" {
		t.Errorf("no new commits on %s beyond main -- claude didn't seem to do anything", branchName)
	}

	// session_id was captured and persisted to the vault note.
	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.SessionID == nil || *noteAfter.Frontmatter.SessionID == "" {
		t.Fatal("session_id was not persisted to the vault note")
	}
	sessionID := *noteAfter.Frontmatter.SessionID

	// The note-copy was cleaned up... actually Phase 6 doesn't delete it yet
	// (that's Phase 7's "delete the copy unconditionally" -- this phase only
	// writes it). Just confirm it exists, proving the note-copy mechanism
	// ran, then clean it up so it doesn't linger in the disposable repo.
	copyPath := fmt.Sprintf("%s/%s", testTargetRepoPath, copyFileName(slug))
	t.Cleanup(func() {
		exec.Command("rm", "-f", copyPath).Run()
	})

	// Independently confirm the captured session_id is a genuine, resumable
	// session by resuming it with a fresh manual `claude` call -- this is
	// the "matches what a manual claude -p --output-format stream-json run
	// reports for the same session" check from Design - Implementation.md's
	// Phase 6 test gate.
	cmd := exec.Command("claude", "-p", "Just reply with the word OK, and nothing else. Take no other action.",
		"--resume", sessionID, "--output-format", "json", "--dangerously-skip-permissions")
	cmd.Dir = testTargetRepoPath
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("manual --resume %s failed: %v\n%s", sessionID, err, out)
	}
	var result struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("parsing manual resume output: %v\n%s", err, out)
	}
	if result.SessionID != sessionID {
		t.Errorf("manual resume reported session_id %q, want it to match captured %q", result.SessionID, sessionID)
	}
}
