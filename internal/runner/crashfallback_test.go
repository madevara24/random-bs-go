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
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// writeStubScript writes an executable fake-claude script at path that
// always prints a valid init line (so session_id capture keeps working
// even on the crash-fallback paths) before doing whatever extraBody says.
func writeStubScript(t *testing.T, path, extraBody string) {
	t.Helper()
	script := "#!/usr/bin/env bash\n" +
		"echo '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"fake-crash-session\"}'\n" +
		extraBody + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub script: %v", err)
	}
}

// setupCrashTest seeds a task note and returns everything a crash-fallback
// scenario test needs. slugPrefix distinguishes the three scenarios in the
// shared throwaway vault/target repo.
//
// The note's real filename deliberately does NOT match "Tasks/<slug>.md" --
// it uses the "(repo) Title" convention with parentheses and spaces, same
// as real vault notes -- so that a regression which reconstructs the path
// from the slug (rather than using job.NotePath) fails loudly instead of
// passing by coincidence.
func setupCrashTest(t *testing.T, slugPrefix, claudeBin string) (Deps, worker.Job, *vaultgit.Vault, string) {
	t.Helper()
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := slugPrefix + "-" + run
	relPath := fmt.Sprintf("Tasks/(phase6-test-repo) Crash Fallback Test %s.md", run)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Crash-fallback test task -- the fake claude stub decides what happens, this prompt is never actually read by anything real.")

	deps := Deps{
		Vault:     v,
		ClaudeBin: claudeBin,
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	branchName := "task/" + slug + "-" + time.Now().Format("2006-01-02")
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
		exec.Command("rm", "-f", filepath.Join(testTargetRepoPath, copyFileName(slug))).Run()
	})

	return deps, job, v, slug
}

func assertBlocked(t *testing.T, v *vaultgit.Vault, relPath string, wantScenario Scenario, gotJob *worker.Job, gotAlert *AlertPayload) {
	t.Helper()
	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", note.Frontmatter.Status, "blocked")
	}
	hasBlocked := false
	for _, e := range note.RunnerLog {
		if e.Event == "blocked" {
			hasBlocked = true
		}
	}
	if !hasBlocked {
		t.Errorf("Runner Log missing a \"blocked\" entry: %+v", note.RunnerLog)
	}
	if gotJob == nil {
		t.Fatal("OnBlocked was never called")
	}
	if gotJob.NotePath != relPath {
		t.Errorf("OnBlocked job.NotePath = %q, want %q (must not be reconstructed from the slug)", gotJob.NotePath, relPath)
	}
	if gotAlert == nil {
		t.Fatal("OnBlocked was never called")
	}
	if gotAlert.Scenario != wantScenario {
		t.Errorf("alert scenario = %v, want %v", gotAlert.Scenario, wantScenario)
	}
}

// TestAlertPayloadDiscordMessageMentionsBoth confirms DiscordMessage
// includes both the Devara and Ara-Dev mentions on every scenario branch --
// the 2026-08-31 decision replaced the old default-profile `hermes -z`
// first-responder call with a single webhook post mentioning both, so the
// `dev` gateway's own mention must land on every crash-fallback message
// shape, not just the common one.
func TestAlertPayloadDiscordMessageMentionsBoth(t *testing.T) {
	for _, tc := range []struct {
		name    string
		payload AlertPayload
	}{
		{name: "blocked", payload: AlertPayload{Slug: "task-1", Repo: "repo-a", Scenario: ScenarioNoCopy, Stage: "result read-back"}},
		{name: "done without PR", payload: AlertPayload{Slug: "task-1", Repo: "repo-a", Scenario: ScenarioDoneWithoutPR}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.payload.DiscordMessage("12345", "67890")
			if !strings.Contains(got, "<@12345>") {
				t.Errorf("message %q missing Devara mention", got)
			}
			if !strings.Contains(got, "<@67890>") {
				t.Errorf("message %q missing Ara-Dev mention", got)
			}
		})
	}
}

// TestCrashFallbackScenario1NoCopy forces scenario 1 (copy absent at
// read-back) by having the stub remove the copy the runner wrote before
// invocation, then exit -- functionally the same end state the doc's
// "process killed before writing the copy" describes (copy absent when the
// runner reads back), adapted for the note-copy model where the runner
// itself writes the copy as input before invocation, not CC as output (see
// the PM Runner Go Rewrite project notes for why this needed adapting from
// the original .task-result.json framing).
func TestCrashFallbackScenario1NoCopy(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	deps, job, v, slug := setupCrashTest(t, "phase9-s1", scriptPath)
	copyName := copyFileName(slug)
	writeStubScript(t, scriptPath, fmt.Sprintf("rm -f %q\nexit 1", filepath.Join(testTargetRepoPath, copyName)))

	var gotJob *worker.Job
	var gotAlert *AlertPayload
	deps.OnBlocked = func(j worker.Job, p AlertPayload) { gotJob = &j; gotAlert = &p }

	err := ProcessTask(deps, noopReporter{}, job)
	if err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}
	assertBlocked(t, v, job.NotePath, ScenarioNoCopy, gotJob, gotAlert)
}

// TestCrashFallbackScenario2NonTerminal forces scenario 2 (copy parses
// fine but never reaches a terminal status) by having the stub leave the
// copy completely untouched -- it was written by the runner with
// status: in_progress before invocation, so doing nothing to it reproduces
// exactly this scenario.
func TestCrashFallbackScenario2NonTerminal(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	deps, job, v, _ := setupCrashTest(t, "phase9-s2", scriptPath)
	writeStubScript(t, scriptPath, "exit 0") // touches nothing; copy stays in_progress

	var gotJob *worker.Job
	var gotAlert *AlertPayload
	deps.OnBlocked = func(j worker.Job, p AlertPayload) { gotJob = &j; gotAlert = &p }

	err := ProcessTask(deps, noopReporter{}, job)
	if err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}
	assertBlocked(t, v, job.NotePath, ScenarioNonTerminal, gotJob, gotAlert)
}

// TestCrashFallbackScenario4DoneWithoutPR forces scenario 4 (copy reports
// status: done but pr_url is empty) by having the stub overwrite the copy
// with a done status, a Work Log entry, and no pr_url at all -- reproducing
// the live gap where CC did real work and set status: done but never ran
// git commit/push or gh pr create. The real vault note must end up blocked,
// not merged in as a clean done, and the Work Log content should still be
// folded in.
func TestCrashFallbackScenario4DoneWithoutPR(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	deps, job, v, slug := setupCrashTest(t, "phase9-s4", scriptPath)
	copyName := copyFileName(slug)
	writeStubScript(t, scriptPath, fmt.Sprintf(
		"printf -- '---\\nstatus: done\\nrepo: phase6-test-repo\\ncreated: \"2026-09-15\"\\npr_url: null\\n---\\nCrash-fallback test task.\\n\\n## Work Log\\n\\nDid the real work but forgot to open a PR.\\n' > %q\nexit 0",
		filepath.Join(testTargetRepoPath, copyName)))

	var gotJob *worker.Job
	var gotAlert *AlertPayload
	deps.OnBlocked = func(j worker.Job, p AlertPayload) { gotJob = &j; gotAlert = &p }

	err := ProcessTask(deps, noopReporter{}, job)
	if err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}
	assertBlocked(t, v, job.NotePath, ScenarioDoneWithoutPR, gotJob, gotAlert)

	note, err := v.ReadNote(job.NotePath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.Status == "done" {
		t.Error("status = done, want it not to have been merged in as a clean done")
	}
	if !note.HasWorkLog || !strings.Contains(note.WorkLog, "forgot to open a PR") {
		t.Errorf("Work Log content from the copy was not folded in: %+v", note.WorkLog)
	}
}

// TestCrashFallbackScenario3ParseFailure forces scenario 3 (copy exists
// but fails to parse) by having the stub overwrite the copy with malformed
// YAML.
func TestCrashFallbackScenario3ParseFailure(t *testing.T) {
	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")

	deps, job, v, slug := setupCrashTest(t, "phase9-s3", scriptPath)
	copyName := copyFileName(slug)
	writeStubScript(t, scriptPath, fmt.Sprintf(
		"printf -- '---\\nstatus: [unterminated\\n---\\nbroken\\n' > %q\nexit 0",
		filepath.Join(testTargetRepoPath, copyName)))

	var gotJob *worker.Job
	var gotAlert *AlertPayload
	deps.OnBlocked = func(j worker.Job, p AlertPayload) { gotJob = &j; gotAlert = &p }

	err := ProcessTask(deps, noopReporter{}, job)
	if err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}
	assertBlocked(t, v, job.NotePath, ScenarioParseFailure, gotJob, gotAlert)
}
