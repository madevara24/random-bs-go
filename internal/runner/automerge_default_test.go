package runner

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

func boolPtr(b bool) *bool { return &b }

func TestResolveAutoMerge(t *testing.T) {
	cases := []struct {
		name        string
		taskValue   *bool
		repoDefault bool
		want        bool
	}{
		{"unset falls back to repo default true", nil, true, true},
		{"unset falls back to repo default false", nil, false, false},
		{"explicit false overrides repo default true", boolPtr(false), true, false},
		{"explicit true overrides repo default false", boolPtr(true), false, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resolveAutoMerge(c.taskValue, c.repoDefault); got != c.want {
				t.Errorf("resolveAutoMerge(%v, %v) = %v, want %v", c.taskValue, c.repoDefault, got, c.want)
			}
		})
	}
}

// runAutoMergeDefaultCase seeds a task note with the given task-level
// auto_merge value (nil meaning the key is absent from the note entirely)
// against a repo configured with repoDefault as its auto_merge_default,
// runs it through ProcessTask against a fake-claude stub that reports
// status: done with a pr_url, and reports whether the merge gate was
// actually invoked -- the observable effect of RBG-14's resolution rule.
func runAutoMergeDefaultCase(t *testing.T, slugPrefix string, taskAutoMerge *bool, repoDefault bool) bool {
	t.Helper()
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := slugPrefix + "-" + run
	relPath := fmt.Sprintf("Tasks/(phase6-test-repo) Auto Merge Default Test %s.md", run)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15", AutoMerge: taskAutoMerge,
	}, "auto_merge_default resolution test -- the fake claude stub reports done+pr_url, this prompt is never read by anything real.")

	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")
	copyName := copyFileName(slug)
	writeStubScript(t, scriptPath, fmt.Sprintf(
		"printf -- '---\\nstatus: done\\nrepo: phase6-test-repo\\ncreated: \"2026-09-15\"\\npr_url: \"local-test://fake-pr\"\\n---\\nauto_merge_default resolution test.\\n\\n## Work Log\\n\\nDid the thing.\\n' > %q\nexit 0",
		filepath.Join(testTargetRepoPath, copyName)))

	var gateCalled bool
	deps := Deps{
		Vault:     v,
		ClaudeBin: scriptPath,
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main", AutoMergeDefault: repoDefault},
		},
		MergeGateFactory: func(repoCfg config.RepoConfig, job worker.Job, branchName, sessionID, prURL string) MergeGateOps {
			gateCalled = true
			return &fakeMergeGateOps{verdicts: []ReviewVerdict{VerdictApprove}, ciConclusion: "success"}
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	branchName := "task/" + slug + "-" + time.Now().Format("2006-01-02")
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
		exec.Command("rm", "-f", filepath.Join(testTargetRepoPath, copyName)).Run()
	})

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}
	return gateCalled
}

// TestAutoMergeDefaultAppliesWhenTaskOmitsIt covers RBG-14's first verify
// bullet: no auto_merge key in the note, repo default true -> merge gate
// runs.
func TestAutoMergeDefaultAppliesWhenTaskOmitsIt(t *testing.T) {
	if got := runAutoMergeDefaultCase(t, "rbg14-unset", nil, true); !got {
		t.Error("merge gate did not run; want it to run when the task omits auto_merge and the repo default is true")
	}
}

// TestAutoMergeExplicitFalseOverridesDefault covers RBG-14's second verify
// bullet: auto_merge: false in the note, repo default true -> merge gate
// does not run.
func TestAutoMergeExplicitFalseOverridesDefault(t *testing.T) {
	if got := runAutoMergeDefaultCase(t, "rbg14-false", boolPtr(false), true); got {
		t.Error("merge gate ran; want it not to run when the task explicitly sets auto_merge: false, regardless of the repo default")
	}
}

// TestAutoMergeExplicitTrueOverridesDefault covers RBG-14's third verify
// bullet: auto_merge: true in the note, repo default false -> merge gate
// runs.
func TestAutoMergeExplicitTrueOverridesDefault(t *testing.T) {
	if got := runAutoMergeDefaultCase(t, "rbg14-true", boolPtr(true), false); !got {
		t.Error("merge gate did not run; want it to run when the task explicitly sets auto_merge: true, regardless of the repo default")
	}
}
