// Package testvault provides shared test-support helpers for exercising
// real git operations against one throwaway PM-vault-shaped bare+working
// clone pair (never the real production PM vault). Deliberately not a
// _test.go file: `go test ./...` runs each package's tests in its own
// process, in parallel by default, and more than one package (vaultgit,
// dispatch) needs real-git integration tests against this same on-disk
// clone -- an in-memory mutex can't coordinate across those separate test
// binaries, only a real flock can.
package testvault

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/madevara24/random-bs-go/internal/notetask"
)

// Path is the throwaway test vault's working clone -- set up outside the
// repo, on-disk, per checkout. Its Tasks/ directory is scratch space:
// tests seed uniquely-named notes per run and never assume a clean slate.
const Path = "/home/obsidian/pmrunner-go-test-vault"

const lockFilePath = "/home/obsidian/pmrunner-go-test-vault.git/test-suite.lock"

// SkipIfAbsent skips the calling test if the throwaway vault isn't present
// on this machine (e.g. a future CI run with no VPS filesystem access).
func SkipIfAbsent(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(Path, ".git")); err != nil {
		t.Skipf("throwaway test vault not present at %s, skipping: %v", Path, err)
	}
}

// Lock acquires an exclusive flock over the whole test vault for the
// duration of one test -- real git operations against a shared clone from
// two concurrent test-binary processes (one per package under `go test
// ./...`) would otherwise race the same way two concurrent bash pipeline
// processes would without with-vault-lock.sh. Returns an unlock func;
// callers should `defer unlock()` immediately.
func Lock(t *testing.T) (unlock func()) {
	t.Helper()
	f, err := os.OpenFile(lockFilePath, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		t.Fatalf("testvault: opening lock file %s: %v", lockFilePath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		t.Fatalf("testvault: flock %s: %v", lockFilePath, err)
	}
	return func() {
		syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}

func runGit(t *testing.T, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = Path
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), Path, err, out)
	}
	return string(out)
}

// Seed writes a task note into the throwaway test vault, commits, and
// pushes it -- and registers a t.Cleanup to delete it again (git rm,
// commit, push) once the calling test finishes. Every phase's tests share
// this one throwaway vault (see Path above), so leaving seeded notes behind
// would otherwise make Tasks/ grow without bound across every test run
// ever taken on this box, slowing every later scan and spamming logs with
// "no RepoWorker registered" noise for repo keys that stopped existing
// commits ago.
func Seed(t *testing.T, relPath string, fm notetask.Frontmatter, prompt string) {
	t.Helper()
	absPath := filepath.Join(Path, relPath)
	note := &notetask.Note{Frontmatter: fm, Prompt: prompt}
	out, err := note.Bytes()
	if err != nil {
		t.Fatalf("testvault.Seed: serializing %s: %v", relPath, err)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatalf("testvault.Seed: mkdir for %s: %v", relPath, err)
	}
	if err := os.WriteFile(absPath, out, 0o644); err != nil {
		t.Fatalf("testvault.Seed: writing %s: %v", relPath, err)
	}
	runGit(t, "add", relPath)
	runGit(t, "commit", "-m", "test: seed "+relPath)
	runGit(t, "push", "origin", "HEAD:master")

	t.Cleanup(func() {
		runGit(t, "rm", "-q", relPath)
		runGit(t, "commit", "-m", "test: clean up "+relPath)
		runGit(t, "push", "origin", "HEAD:master")
	})
}
