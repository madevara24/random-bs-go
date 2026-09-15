package vaultgit

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
)

// testVaultPath points at a throwaway bare+working clone pair set up
// specifically for exercising this package against real git, per Phase 1's
// test gate in Design - Implementation.md ("against a real (or throwaway
// test) bare+working clone pair"). Never the real PM vault -- see
// Storage & Repos.md for why a second, unrelated clone shouldn't touch
// production task notes.
const testVaultPath = "/home/obsidian/pmrunner-go-test-vault"

func skipIfNoTestVault(t *testing.T) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(testVaultPath, ".git")); err != nil {
		t.Skipf("throwaway test vault not present at %s, skipping real-git integration test: %v", testVaultPath, err)
	}
}

func seedNote(t *testing.T, v *Vault, relPath string, fm notetask.Frontmatter, prompt string) {
	t.Helper()
	absPath := filepath.Join(v.Path, relPath)
	note := &notetask.Note{Frontmatter: fm, Prompt: prompt}
	out, err := note.Bytes()
	if err != nil {
		t.Fatalf("seedNote: serializing: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(absPath), 0o755); err != nil {
		t.Fatalf("seedNote: mkdir: %v", err)
	}
	if err := os.WriteFile(absPath, out, 0o644); err != nil {
		t.Fatalf("seedNote: write: %v", err)
	}
	runGitT(t, v.Path, "add", relPath)
	runGitT(t, v.Path, "commit", "-m", "seed "+relPath)
	runGitT(t, v.Path, "push", "origin", "HEAD:"+v.DefaultBranch)
}

func runGitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = cleanEnv()
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s (in %s): %v\n%s", strings.Join(args, " "), dir, err, out)
	}
	return string(out)
}

// TestRoundTrip parses -> mutates status -> appends a Runner Log line ->
// writes, then confirms both the on-disk file and the git log reflect it.
func TestRoundTrip(t *testing.T) {
	skipIfNoTestVault(t)
	v := New(testVaultPath, "master")

	relPath := fmt.Sprintf("Tasks/phase1-roundtrip-%d.md", time.Now().UnixNano())
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	seedNote(t, v, relPath, notetask.Frontmatter{
		Status:  "ready",
		Repo:    "test-repo",
		Created: "2026-09-15",
	}, "Do the thing.")

	err := v.WriteNote(relPath, "phase1 roundtrip: claim", func(n *notetask.Note) error {
		n.Frontmatter.Status = "queued"
		notetask.AppendRunnerLog(n, "queued", time.Date(2026, 9, 15, 10, 0, 0, 0, time.UTC))
		return nil
	})
	if err != nil {
		t.Fatalf("WriteNote: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(v.Path, relPath))
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	note, err := notetask.Parse(data)
	if err != nil {
		t.Fatalf("parsing back: %v", err)
	}
	if note.Frontmatter.Status != "queued" {
		t.Errorf("status = %q, want %q", note.Frontmatter.Status, "queued")
	}
	if len(note.RunnerLog) != 1 || note.RunnerLog[0].Event != "queued" {
		t.Errorf("RunnerLog = %+v, want one \"queued\" entry", note.RunnerLog)
	}

	log := runGitT(t, v.Path, "log", "--oneline", "-1")
	if !strings.Contains(log, "phase1 roundtrip: claim") {
		t.Errorf("git log HEAD = %q, want it to contain the commit message", log)
	}
}

// TestConcurrentSyncAndWriteNote runs Sync() and WriteNote() concurrently
// from two goroutines on different notes and confirms no corrupted commit --
// this is what the mutex exists to prevent, per Phase 1's test gate.
func TestConcurrentSyncAndWriteNote(t *testing.T) {
	skipIfNoTestVault(t)
	v := New(testVaultPath, "master")

	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}

	const n = 10
	relPaths := make([]string, n)
	for i := 0; i < n; i++ {
		relPaths[i] = fmt.Sprintf("Tasks/phase1-concurrent-%d-%d.md", time.Now().UnixNano(), i)
		seedNote(t, v, relPaths[i], notetask.Frontmatter{
			Status:  "ready",
			Repo:    "test-repo",
			Created: "2026-09-15",
		}, "Concurrent write test "+strconv.Itoa(i))
	}

	var wg sync.WaitGroup
	errs := make([]error, 0, n+5)
	var errMu sync.Mutex
	recordErr := func(err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		errs = append(errs, err)
		errMu.Unlock()
	}

	// Fire several concurrent Syncs alongside concurrent WriteNotes on
	// distinct notes -- exactly the two contenders the mutex has to
	// serialize.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recordErr(v.Sync())
		}()
	}
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := v.WriteNote(relPaths[i], fmt.Sprintf("phase1 concurrent claim %d", i), func(note *notetask.Note) error {
				note.Frontmatter.Status = "queued"
				notetask.AppendRunnerLog(note, "queued", time.Now())
				return nil
			})
			recordErr(err)
		}(i)
	}
	wg.Wait()

	if len(errs) > 0 {
		t.Fatalf("concurrent Sync/WriteNote produced %d error(s), want 0: %v", len(errs), errs)
	}

	// Confirm every note actually landed as "queued" -- if the mutex failed
	// to serialize, a lost update here (one write clobbering another, or a
	// reset --hard racing a commit) is exactly what would show up as
	// something other than all n notes present and correct.
	if err := v.Sync(); err != nil {
		t.Fatalf("final sync: %v", err)
	}
	for _, rp := range relPaths {
		data, err := os.ReadFile(filepath.Join(v.Path, rp))
		if err != nil {
			t.Fatalf("note %s missing after concurrent writes: %v", rp, err)
		}
		note, err := notetask.Parse(data)
		if err != nil {
			t.Fatalf("note %s failed to parse after concurrent writes (corrupted?): %v", rp, err)
		}
		if note.Frontmatter.Status != "queued" {
			t.Errorf("note %s status = %q, want %q", rp, note.Frontmatter.Status, "queued")
		}
	}

	// git fsck confirms no corrupted objects resulted from racing writers.
	if out, err := exec.Command("git", "-C", v.Path, "fsck", "--full").CombinedOutput(); err != nil {
		t.Fatalf("git fsck reported a problem: %v\n%s", err, out)
	}
}
