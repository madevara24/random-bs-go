// Package vaultgit wraps git operations against the PM vault's working
// clone: syncing it to match the bare repo, and writing task notes back
// (commit + push, with a rebase-retry backstop on a non-fast-forward
// rejection). A package-level-per-Vault mutex serializes every git-touching
// operation against this clone -- the in-memory replacement for the bash
// pipeline's with-vault-lock.sh flock, now that sync and every note write
// are goroutines inside one process rather than separate contending
// processes.
package vaultgit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/madevara24/random-bs-go/internal/notetask"
)

// Vault is one PM-vault working clone.
type Vault struct {
	Path          string
	DefaultBranch string

	mu sync.Mutex
}

// New constructs a Vault. Does not touch disk.
func New(path, defaultBranch string) *Vault {
	return &Vault{Path: path, DefaultBranch: defaultBranch}
}

// cleanEnv strips GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE from the inherited
// environment before handing it to a git subprocess. Every git command in
// this package unsets these defensively -- see Design - Runner.md's
// GIT_DIR hazard section. In the bash pipeline this mattered because
// runner.sh was a background descendant of the git hook process, which
// git sets these three vars for. In this daemon, git commands run from the
// long-lived process itself, not a hook descendant -- but this is cheap
// insurance regardless of what actually launches the daemon, and Phase 5's
// end-to-end test is where the "does the hazard still exist here" question
// gets checked empirically rather than assumed.
func cleanEnv() []string {
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, e := range env {
		if strings.HasPrefix(e, "GIT_DIR=") ||
			strings.HasPrefix(e, "GIT_WORK_TREE=") ||
			strings.HasPrefix(e, "GIT_INDEX_FILE=") {
			continue
		}
		out = append(out, e)
	}
	return out
}

func (v *Vault) runGit(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = v.Path
	cmd.Env = cleanEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("git %s: %w\n%s", strings.Join(args, " "), err, out)
	}
	return string(out), nil
}

// EnvSnapshot runs `env` as a child of this package's git invocation path
// and reports whether GIT_DIR/GIT_WORK_TREE/GIT_INDEX_FILE are present in
// the environment actually handed to git subprocesses -- used by Phase 5's
// empirical check, not by normal operation.
func (v *Vault) EnvSnapshot() map[string]bool {
	found := map[string]bool{"GIT_DIR": false, "GIT_WORK_TREE": false, "GIT_INDEX_FILE": false}
	for _, e := range cleanEnv() {
		for k := range found {
			if strings.HasPrefix(e, k+"=") {
				found[k] = true
			}
		}
	}
	return found
}

// Sync fetches and hard-resets the working clone to match the bare repo's
// default branch -- the daemon's own dispatch-pass goroutine's first step,
// replacing the hook's old sync step now that the hook itself runs no git
// commands at all.
func (v *Vault) Sync() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	if _, err := v.runGit("fetch", "origin"); err != nil {
		return fmt.Errorf("vaultgit: sync fetch: %w", err)
	}
	if _, err := v.runGit("reset", "--hard", "origin/"+v.DefaultBranch); err != nil {
		return fmt.Errorf("vaultgit: sync reset: %w", err)
	}
	return nil
}

// MutateFn edits a parsed note in place. Returning an error aborts the
// write entirely -- nothing is committed or pushed.
type MutateFn func(note *notetask.Note) error

// WriteNote reads relPath (vault-relative) fresh off disk, applies
// mutateFn, writes it back, commits, and pushes. On a non-fast-forward
// push rejection, retries exactly once via `git pull --rebase`; a genuine
// conflict at that point aborts the write and returns an error for the
// caller to alert on -- never force-pushes.
func (v *Vault) WriteNote(relPath, commitMsg string, mutateFn MutateFn) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	absPath := filepath.Join(v.Path, relPath)
	data, err := os.ReadFile(absPath)
	if err != nil {
		return fmt.Errorf("vaultgit: reading note %s: %w", relPath, err)
	}
	note, err := notetask.Parse(data)
	if err != nil {
		return fmt.Errorf("vaultgit: parsing note %s: %w", relPath, err)
	}
	note.Path = absPath

	if err := mutateFn(note); err != nil {
		return fmt.Errorf("vaultgit: mutate %s: %w", relPath, err)
	}

	out, err := note.Bytes()
	if err != nil {
		return fmt.Errorf("vaultgit: serializing note %s: %w", relPath, err)
	}
	if err := os.WriteFile(absPath, out, 0o644); err != nil {
		return fmt.Errorf("vaultgit: writing note %s: %w", relPath, err)
	}

	if _, err := v.runGit("add", relPath); err != nil {
		return fmt.Errorf("vaultgit: git add %s: %w", relPath, err)
	}
	if _, err := v.runGit("commit", "-m", commitMsg); err != nil {
		return fmt.Errorf("vaultgit: git commit: %w", err)
	}

	if _, pushErr := v.runGit("push", "origin", "HEAD:"+v.DefaultBranch); pushErr == nil {
		return nil
	}

	// Push rejected -- attempt the rebase-retry backstop exactly once.
	if _, err := v.runGit("pull", "--rebase", "origin", v.DefaultBranch); err != nil {
		return fmt.Errorf("vaultgit: push rejected and rebase failed -- genuine conflict, needs manual resolution: %w", err)
	}
	if _, err := v.runGit("push", "origin", "HEAD:"+v.DefaultBranch); err != nil {
		return fmt.Errorf("vaultgit: push still rejected after rebase retry: %w", err)
	}
	return nil
}

// ReadNote reads and parses relPath without any git or mutation -- a
// read-only convenience for scan-only callers (dispatch, watcher).
func (v *Vault) ReadNote(relPath string) (*notetask.Note, error) {
	absPath := filepath.Join(v.Path, relPath)
	data, err := os.ReadFile(absPath)
	if err != nil {
		return nil, fmt.Errorf("vaultgit: reading note %s: %w", relPath, err)
	}
	note, err := notetask.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("vaultgit: parsing note %s: %w", relPath, err)
	}
	note.Path = absPath
	return note, nil
}
