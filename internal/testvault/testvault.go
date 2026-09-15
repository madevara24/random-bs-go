// Package testvault provides shared test-support helpers for exercising
// real git operations against one throwaway PM-vault-shaped bare+working
// clone pair (never the real production PM vault -- see Storage & Repos.md
// in the notes vault). Deliberately not a _test.go file: `go test ./...`
// runs each package's tests in its own process, in parallel by default, and
// more than one package (vaultgit, dispatch) needs real-git integration
// tests against this same on-disk clone -- an in-memory mutex can't
// coordinate across those separate test binaries, only a real flock can.
package testvault

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// Path is the throwaway test vault's working clone -- created once for this
// rewrite, set up outside the repo (see the PM Runner Go Rewrite project
// notes for provenance). Its Tasks/ directory is scratch space: tests seed
// uniquely-named notes per run and never assume a clean slate.
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
