package runner

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// TestReviewEnginePiOnNoteSelectsPiForReview is RBG-36's merge-gate test
// gate: a note with `review_engine: pi` pinned (auto_merge: true, so the
// gate actually runs) must hand the merge gate a pi *engine.PiEngine for
// the review, even though the coding run itself used claude -- the two
// are resolved independently (resolveReviewEngineName), exactly as a
// claude-coded/pi-reviewed task is meant to work. The coding engine is a
// fake-claude stub that reports done+pr_url immediately; the merge gate
// itself is a fake MergeGateFactory that records which engine it was
// handed and fails the review round immediately (no real gh/pi call), so
// this test only exercises engine *resolution*, not a real review.
func TestReviewEnginePiOnNoteSelectsPiForReview(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "review-engine-pi-" + run
	relPath := fmt.Sprintf("Tasks/(phase6-test-repo) Review Engine Pi Test %s.md", run)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	pinnedReviewEngine := "pi"
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15",
		AutoMerge: boolPtr(true), ReviewEngine: &pinnedReviewEngine,
	}, "review_engine: pi resolution test -- the fake claude stub reports done+pr_url, this prompt is never read by anything real.")

	tmpDir := t.TempDir()
	claudeStub := filepath.Join(tmpDir, "fake-claude.sh")
	copyName := copyFileName(slug)
	writeStubScript(t, claudeStub, fmt.Sprintf(
		"printf -- '---\\nstatus: done\\nrepo: phase6-test-repo\\ncreated: \"2026-09-15\"\\npr_url: \"local-test://fake-pr\"\\n---\\nreview_engine: pi resolution test.\\n\\n## Work Log\\n\\nDid the thing.\\n' > %q\nexit 0",
		filepath.Join(testTargetRepoPath, copyName)))

	var gotReviewEngineName string
	var gotCodingEngineName string
	deps := Deps{
		Vault: v,
		EngineRegistry: map[string]engine.Engine{
			"claude": engine.NewClaude(claudeStub),
			"pi":     engine.NewPi("", "", "", tmpDir, 0),
		},
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
		},
		MergeGateFactory: func(repoCfg config.RepoConfig, job worker.Job, branchName, sessionID, prURL string, codingEngine, reviewEngine engine.Engine) MergeGateOps {
			gotCodingEngineName = codingEngine.Name()
			gotReviewEngineName = reviewEngine.Name()
			// Fail the very first round deliberately -- this test only
			// cares which engine was selected, never about a real review.
			return &fakeMergeGateOps{verdicts: []ReviewVerdict{VerdictConcerns}, ciConclusion: "success"}
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	branchName := "task/" + slug
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
		exec.Command("rm", "-f", filepath.Join(testTargetRepoPath, copyName)).Run()
	})

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	if gotCodingEngineName != "claude" {
		t.Errorf("coding engine handed to the merge gate = %q, want %q", gotCodingEngineName, "claude")
	}
	if gotReviewEngineName != "pi" {
		t.Errorf("review engine handed to the merge gate = %q, want %q (from the note's review_engine: pin)", gotReviewEngineName, "pi")
	}
}
