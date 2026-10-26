// Tests for RBG-34's engine selection: the runner-level resolution glue
// (resolveCodingEngineName/resolveReviewEngineName), writing `engine:` at
// run start (never earlier, never on resume), and blocking before the
// branch is touched on an unknown coding or review engine name.
package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

func notePtr(fm notetask.Frontmatter) *notetask.Note {
	return &notetask.Note{Frontmatter: fm}
}

func strp(s string) *string { return &s }

// TestResolveCodingEngineName covers all three precedence levels plus the
// last-resort default: job.Engine (set by dispatch) -> the note's own
// `engine:` -> fallbackCodingEngineName.
func TestResolveCodingEngineName(t *testing.T) {
	notePinned := notePtr(notetask.Frontmatter{Engine: strp("note-engine")})
	noteUnset := notePtr(notetask.Frontmatter{})

	cases := []struct {
		name string
		job  worker.Job
		note *notetask.Note
		want string
	}{
		{"job.Engine wins over the note's pin", worker.Job{Engine: "job-engine"}, notePinned, "job-engine"},
		{"note's pin used when job.Engine is empty", worker.Job{}, notePinned, "note-engine"},
		{"fallback when both are empty", worker.Job{}, noteUnset, fallbackCodingEngineName},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveCodingEngineName(c.job, c.note); got != c.want {
				t.Errorf("resolveCodingEngineName(%+v, ...) = %q, want %q", c.job, got, c.want)
			}
		})
	}
}

// TestResolveReviewEngineName covers all three precedence levels: the
// note's own `review_engine:` -> the repo's "review_engine" ->
// defaultReviewEngine.
func TestResolveReviewEngineName(t *testing.T) {
	notePinned := notePtr(notetask.Frontmatter{ReviewEngine: strp("note-review-engine")})
	noteUnset := notePtr(notetask.Frontmatter{})
	repoWithOverride := config.RepoConfig{ReviewEngine: "repo-review-engine"}
	repoWithoutOverride := config.RepoConfig{}

	cases := []struct {
		name    string
		note    *notetask.Note
		repoCfg config.RepoConfig
		dflt    string
		want    string
	}{
		{"note's pin wins over everything", notePinned, repoWithOverride, "default-review-engine", "note-review-engine"},
		{"repo override wins when note unset", noteUnset, repoWithOverride, "default-review-engine", "repo-review-engine"},
		{"global default when both unset", noteUnset, repoWithoutOverride, "default-review-engine", "default-review-engine"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveReviewEngineName(c.note, c.repoCfg, c.dflt); got != c.want {
				t.Errorf("resolveReviewEngineName(...) = %q, want %q", got, c.want)
			}
		})
	}
}

// TestProcessTaskWritesEngineOnFreshRun confirms a fresh run's claim write
// (status -> in_progress) also writes `engine:` when the note didn't
// already pin one.
func TestProcessTaskWritesEngineOnFreshRun(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "engine-fresh-" + run
	relPath := fmt.Sprintf("Tasks/(phase6-test-repo) Engine Fresh Test %s.md", run)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Engine-write-on-fresh-run test task -- the fake claude stub exits immediately, this prompt is never read by anything real.")

	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")
	writeStubScript(t, scriptPath, "exit 1")

	deps := Deps{
		Vault:  v,
		Engine: engine.NewClaude(scriptPath),
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	branchName := "task/" + slug
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
	})

	// Error expected (the stub exits 1 with no copy -> crash fallback ->
	// blocked) -- irrelevant to this test's own assertion.
	_ = ProcessTask(deps, noopReporter{}, job)

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Engine == nil || *noteAfter.Frontmatter.Engine != "claude" {
		t.Errorf("Engine = %v, want %q", noteAfter.Frontmatter.Engine, "claude")
	}
}

// TestProcessTaskResumeKeepsExistingEngine confirms a resume never
// rewrites the note's existing `engine:` -- only the fresh-run claim write
// ever sets it.
func TestProcessTaskResumeKeepsExistingEngine(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "engine-resume-" + run
	relPath := fmt.Sprintf("Tasks/(phase6-test-repo) Engine Resume Test %s.md", run)
	branchName := "task/" + slug

	// Pre-create and push the task branch -- standing in for a prior
	// session's preserved work, so checkoutTaskBranch's resume path has a
	// real branch to fetch+track.
	runGit(t, testTargetRepoPath, "checkout", "main")
	runGit(t, testTargetRepoPath, "checkout", "-b", branchName)
	runGit(t, testTargetRepoPath, "push", "-u", "origin", branchName)
	runGit(t, testTargetRepoPath, "checkout", "main")
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
	})

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	priorSessionID := "prior-session-id"
	pinnedEngine := "claude"
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "in_progress", Repo: "phase6-test-repo", Created: "2026-09-15",
		SessionID: &priorSessionID, Engine: &pinnedEngine,
	}, "Resume-keeps-existing-engine test task -- the fake claude stub exits immediately, this prompt is never read by anything real.")

	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")
	writeStubScript(t, scriptPath, "exit 1")

	deps := Deps{
		Vault:  v,
		Engine: engine.NewClaude(scriptPath),
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	copyName := copyFileName(slug)
	t.Cleanup(func() {
		exec.Command("rm", "-f", filepath.Join(testTargetRepoPath, copyName)).Run()
	})

	_ = ProcessTask(deps, noopReporter{}, job)

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Engine == nil || *noteAfter.Frontmatter.Engine != "claude" {
		t.Errorf("Engine = %v, want unchanged %q", noteAfter.Frontmatter.Engine, "claude")
	}
}

// TestProcessTaskUnknownEngineBlocksBeforeBranchTouched confirms a typo'd
// `engine:` ends the task at status: blocked through the setup fallback's
// new setupStageResolveEngine stage, with the "unknown engine" message,
// and -- since that stage fails before any branch/repo touch -- the
// target clone never leaves its default branch and no task branch is ever
// created.
func TestProcessTaskUnknownEngineBlocksBeforeBranchTouched(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "engine-unknown-" + run
	relPath := fmt.Sprintf("Tasks/(phase6-test-repo) Engine Unknown Test %s.md", run)
	branchName := "task/" + slug

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	unknownEngine := "nope"
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15", Engine: &unknownEngine,
	}, "Unknown-engine test task -- ProcessTask must never actually invoke anything for this note.")

	runGit(t, testTargetRepoPath, "checkout", "main")

	var gotJob *worker.Job
	var gotAlert *AlertPayload
	deps := Deps{
		Vault: v,
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
		},
		OnBlocked: func(j worker.Job, p AlertPayload) { gotJob = &j; gotAlert = &p },
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	if err := ProcessTask(deps, noopReporter{}, job); err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "blocked")
	}
	if !noteAfter.HasWorkLog || !strings.Contains(noteAfter.WorkLog, `unknown engine "nope" (known: claude)`) {
		t.Errorf("Work Log = %q, want it to contain the unknown-engine message", noteAfter.WorkLog)
	}

	if gotJob == nil || gotAlert == nil {
		t.Fatal("OnBlocked was never called")
	}
	if !strings.Contains(gotAlert.ExitErrText, `unknown engine "nope" (known: claude)`) {
		t.Errorf("alert ExitErrText = %q, want it to contain the unknown-engine message", gotAlert.ExitErrText)
	}

	// The branch was never touched: still on main, no task/<slug> branch.
	current := strings.TrimSpace(runGit(t, testTargetRepoPath, "branch", "--show-current"))
	if current != "main" {
		t.Errorf("current branch = %q, want %q (unknown engine must block before the branch is touched)", current, "main")
	}
	branches := runGit(t, testTargetRepoPath, "branch", "--list", branchName)
	if strings.TrimSpace(branches) != "" {
		t.Errorf("branch %s exists, want it never created", branchName)
	}
}

// TestProcessTaskUnknownReviewEngineBlocksBeforeCodingRun confirms a
// typo'd `review_engine:` blocks the task through the same
// setupStageResolveEngine stage -- before the coding engine is ever
// invoked, not only once the merge gate starts.
func TestProcessTaskUnknownReviewEngineBlocksBeforeCodingRun(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "review-engine-unknown-" + run
	relPath := fmt.Sprintf("Tasks/(phase6-test-repo) Review Engine Unknown Test %s.md", run)
	branchName := "task/" + slug

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	unknownReviewEngine := "nope-review"
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15", ReviewEngine: &unknownReviewEngine,
	}, "Unknown-review-engine test task -- ProcessTask must never actually invoke the coding engine for this note.")

	runGit(t, testTargetRepoPath, "checkout", "main")

	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")
	markerPath := filepath.Join(tmpDir, "coding-engine-was-invoked")
	writeStubScript(t, scriptPath, fmt.Sprintf("touch %q\nexit 1", markerPath))

	deps := Deps{
		Vault:  v,
		Engine: engine.NewClaude(scriptPath),
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	if err := ProcessTask(deps, noopReporter{}, job); err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "blocked")
	}
	if !noteAfter.HasWorkLog || !strings.Contains(noteAfter.WorkLog, `unknown engine "nope-review" (known: claude)`) {
		t.Errorf("Work Log = %q, want it to contain the unknown-review-engine message", noteAfter.WorkLog)
	}

	if _, err := os.Stat(markerPath); !os.IsNotExist(err) {
		t.Errorf("coding engine stub was invoked (marker file exists) -- an unknown review_engine: must block before the coding run")
	}

	current := strings.TrimSpace(runGit(t, testTargetRepoPath, "branch", "--show-current"))
	if current != "main" {
		t.Errorf("current branch = %q, want %q (unknown review_engine must block before the branch is touched)", current, "main")
	}
	branches := runGit(t, testTargetRepoPath, "branch", "--list", branchName)
	if strings.TrimSpace(branches) != "" {
		t.Errorf("branch %s exists, want it never created", branchName)
	}
}
