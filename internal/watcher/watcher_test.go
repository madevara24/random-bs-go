package watcher

import (
	"fmt"
	"net"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/httpapi"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/runner"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

const testTargetRepoPath = "/home/obsidian/repos/phase6-test-repo"

// TestCheckHealthConnectionRefused kills the "runner" entirely (a
// listener bound then immediately closed, so nothing answers) and confirms
// the watcher reports connection-refused, not a timeout -- Phase 12's
// first test gate.
func TestCheckHealthConnectionRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}
	addr := ln.Addr().String()
	ln.Close() // nothing listening now -- simulates the runner process being dead

	w := &Watcher{BaseURL: "http://" + addr, HealthHTTPTimeout: 2 * time.Second}
	result, err := w.CheckHealth()
	if result != HealthConnectionRefused {
		t.Errorf("CheckHealth result = %v (err=%v), want HealthConnectionRefused", result, err)
	}
}

// TestCheckHealthOKWhenServing confirms the happy path doesn't misreport.
func TestCheckHealthOKWhenServing(t *testing.T) {
	srv := httptest.NewServer(httpapi.New(make(chan struct{}, 1), nil))
	defer srv.Close()

	w := &Watcher{BaseURL: srv.URL, HealthHTTPTimeout: 2 * time.Second}
	result, err := w.CheckHealth()
	if result != HealthOK || err != nil {
		t.Errorf("CheckHealth = %v, %v, want HealthOK, nil", result, err)
	}
}

// TestScanForUnnotifiedFlagsManuallyWrittenDoneNote manually writes a note
// to done without a Runner Log "notified" line and confirms the direct
// scan flags it -- Phase 12's third test gate. Using ageThreshold=0 models
// "within one scan interval" -- the scan itself runs on every interval
// regardless of a note's age, so a zero threshold is the faithful way to
// test "does this note get flagged the moment a scan looks at it," leaving
// the real staleness age check exercised separately below.
func TestScanForUnnotifiedFlagsManuallyWrittenDoneNote(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/phase12-unnotified-%s.md", run)
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "done", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Manually completed, notify never ran.")

	w := &Watcher{Vault: v}
	found, err := w.ScanForUnnotified(0)
	if err != nil {
		t.Fatalf("ScanForUnnotified: %v", err)
	}
	var match *UnnotifiedNote
	for i := range found {
		if found[i].RelPath == relPath {
			match = &found[i]
		}
	}
	if match == nil {
		t.Fatalf("note %s not flagged as unnotified; got: %+v", relPath, found)
	}
	if match.Status != "done" {
		t.Errorf("flagged status = %q, want %q", match.Status, "done")
	}
}

// TestScanForUnnotifiedIgnoresAlreadyNotified confirms a note with a
// "notified" Runner Log line is correctly excluded -- otherwise the
// previous test would be nearly meaningless (everything terminal would
// always be flagged).
func TestScanForUnnotifiedIgnoresAlreadyNotified(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/phase12-notified-%s.md", run)
	note := &notetask.Note{
		Frontmatter: notetask.Frontmatter{Status: "done", Repo: "phase6-test-repo", Created: "2026-09-15"},
		Prompt:      "Completed and properly notified.",
	}
	notetask.AppendRunnerLog(note, "done", time.Now())
	notetask.AppendRunnerLog(note, "notified", time.Now())
	out, err := note.Bytes()
	if err != nil {
		t.Fatalf("serializing: %v", err)
	}
	absPath := filepath.Join(testvault.Path, relPath)
	if err := os.WriteFile(absPath, out, 0o644); err != nil {
		t.Fatalf("writing: %v", err)
	}
	runGitSeed(t, relPath)
	t.Cleanup(func() { runGitCleanup(t, relPath) })

	w := &Watcher{Vault: v}
	found, err := w.ScanForUnnotified(0)
	if err != nil {
		t.Fatalf("ScanForUnnotified: %v", err)
	}
	for _, f := range found {
		if f.RelPath == relPath {
			t.Errorf("already-notified note %s was incorrectly flagged", relPath)
		}
	}
}

// TestCheckStatusTasksFlagsStaleBeforeRunnerWatchdog runs a real task
// against a fake claude stub that goes silent forever (Phase 8-style),
// with the *runner's own* idle watchdog set to a long window (so it won't
// kill the task during this test), and confirms the watcher's own
// /status/tasks staleness check -- using a much shorter threshold -- flags
// it as stale well before the runner would ever intervene. Phase 12's
// second test gate.
func TestCheckStatusTasksFlagsStaleBeforeRunnerWatchdog(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	tmpDir := t.TempDir()
	scriptPath := filepath.Join(tmpDir, "fake-claude.sh")
	pidFile := filepath.Join(tmpDir, "stub.pid")
	script := fmt.Sprintf("#!/usr/bin/env bash\n"+
		"echo $$ > %q\n"+
		"echo '{\"type\":\"system\",\"subtype\":\"init\",\"session_id\":\"fake-stale-session\"}'\n"+
		"sleep 999999\n", pidFile)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("writing stub: %v", err)
	}
	// invokeClaude runs this in its own process group (Setpgid) and never
	// gets a chance to be killed by this test's own logic -- unlike the
	// idle-watchdog test, nothing here ever lets IdleTimeout(1h) elapse.
	// Kill the whole group directly in cleanup so this stub doesn't outlive
	// the test as an orphan `sleep 999999`.
	t.Cleanup(func() {
		data, err := os.ReadFile(pidFile)
		if err != nil {
			return
		}
		var pid int
		fmt.Sscanf(string(data), "%d", &pid)
		if pid > 0 {
			syscall.Kill(-pid, syscall.SIGKILL)
		}
	})

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "phase12-stale-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Stale-detection test task.")

	deps := runner.Deps{
		Vault:       v,
		ClaudeBin:   scriptPath,
		IdleTimeout: 1 * time.Hour, // deliberately long -- the runner's own watchdog must NOT fire during this test
		Repos: map[string]config.RepoConfig{
			"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
		},
	}

	branchName := "task/" + slug + "-" + time.Now().Format("2006-01-02")
	t.Cleanup(func() {
		runGitPlain(testTargetRepoPath, "checkout", "main")
		runGitPlain(testTargetRepoPath, "branch", "-D", branchName)
		runGitPlain(testTargetRepoPath, "push", "origin", "--delete", branchName)
		os.Remove(filepath.Join(testTargetRepoPath, ".pmrunner-task-"+slug+".md"))
	})

	globalSlots := worker.NewGlobalSlots(1)
	workers := worker.Workers{}
	rw := worker.New("phase6-test-repo", globalSlots, nil)
	rw.Process = func(job worker.Job) error { return runner.ProcessTask(deps, rw, job) }
	workers["phase6-test-repo"] = rw
	workers.StartAll()
	rw.Enqueue(worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug})

	srv := httptest.NewServer(httpapi.New(make(chan struct{}, 1), workers))
	defer srv.Close()

	const watcherIdleTimeout = 500 * time.Millisecond
	w := &Watcher{BaseURL: srv.URL, IdleTimeout: watcherIdleTimeout, StatusHTTPTimeout: 2 * time.Second}

	// CurrentTask() becomes non-nil (Stage: "setup") the instant runOne
	// starts -- well before ProcessTask has actually pulled/branched/
	// written the copy and gotten as far as invoking the stub. A frozen
	// Stage/LastActivityAt at that point is ambiguous: it could mean
	// "genuinely idle" or just "hasn't started its real work yet," and
	// both look identical from the outside. Wait specifically for
	// Stage == "invocation" -- the one signal that unambiguously means
	// git setup finished and the fake claude stub is actually running --
	// before starting the staleness countdown.
	deadline := time.Now().Add(15 * time.Second)
	for {
		tasks, _, err := w.CheckStatusTasks()
		if err != nil {
			t.Fatalf("CheckStatusTasks (waiting for invocation stage): %v", err)
		}
		if ts, ok := tasks["phase6-test-repo"]; ok && ts.Stage == "invocation" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("task never reached the invocation stage (stuck in git setup?); last tasks=%+v", tasks)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Give the stub a brief moment to actually print its init line (which
	// stamps LastActivityAt) before starting the staleness countdown.
	time.Sleep(200 * time.Millisecond)

	// Now that activity has genuinely gone silent, wait past the watcher's
	// own (short) idle threshold -- still far, far short of the runner's
	// 1-hour watchdog window -- and confirm it gets flagged.
	time.Sleep(watcherIdleTimeout + 300*time.Millisecond)

	_, stale, err := w.CheckStatusTasks()
	if err != nil {
		t.Fatalf("CheckStatusTasks: %v", err)
	}
	found := false
	for _, s := range stale {
		if s.RepoKey == "phase6-test-repo" && s.Slug == slug {
			found = true
		}
	}
	if !found {
		t.Fatalf("task %s not flagged stale; stale=%+v", slug, stale)
	}
}

func runGitPlainT(t *testing.T, dir string, args ...string) {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
}

func runGitPlain(dir string, args ...string) {
	exec.Command("git", append([]string{"-C", dir}, args...)...).Run()
}

func runGitSeed(t *testing.T, relPath string) {
	t.Helper()
	runGitPlainT(t, testvault.Path, "add", relPath)
	runGitPlainT(t, testvault.Path, "commit", "-m", "test: seed "+relPath)
	runGitPlainT(t, testvault.Path, "push", "origin", "HEAD:master")
}

func runGitCleanup(t *testing.T, relPath string) {
	t.Helper()
	runGitPlainT(t, testvault.Path, "rm", "-q", relPath)
	runGitPlainT(t, testvault.Path, "commit", "-m", "test: clean up "+relPath)
	runGitPlainT(t, testvault.Path, "push", "origin", "HEAD:master")
}
