// Package runner implements processTask -- the actual per-task work,
// replacing the stub wired in Phases 3-5. Phase 6: setup (claim, pull,
// branch, note-copy) + invocation (claude -p, session_id capture). Any
// process exit is treated as terminal for now; merge-back, the idle
// watchdog, and crash-fallback branching land in Phases 7-9. See
// Design - Runner.md's runner.sh section for the full behavior this
// reproduces.
package runner

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// ErrIdleTimeout is returned by invokeClaude (and surfaces through
// ProcessTask) when the idle watchdog kills the process group after
// IdleTimeout of silence on the stream-json output -- distinct from a
// plain process-exit error so callers (Phase 9's crash-fallback alert in
// particular) can say "the runner killed it for going idle," not just show
// a bare exit code.
var ErrIdleTimeout = errors.New("runner: idle timeout exceeded, process group killed")

// ActivityReporter is the subset of worker.RepoWorker that ProcessTask
// needs to report progress -- an interface so this package doesn't need
// worker's concrete type for anything except the Job it's handed (and
// worker already depends on dispatch, not runner, so runner depending on
// worker for the Job type is fine -- no cycle).
type ActivityReporter interface {
	SetStage(stage string)
	TouchActivity()
}

// Deps holds everything ProcessTask needs beyond the job itself.
type Deps struct {
	Vault *vaultgit.Vault
	Repos map[string]config.RepoConfig

	// ClaudeBin is the claude CLI's path/name -- overridable so Phase 8's
	// tests can point it at a fake stub script instead of the real binary.
	ClaudeBin string

	// IdleTimeout is how long the stream-json output may go silent before
	// the watchdog kills the process group. Same shared setting as the
	// watcher's own staleness threshold, per Design - Runner.md's
	// "Idle-window value" decision -- one config key (IDLE_TIMEOUT_MINUTES),
	// read by both run modes.
	IdleTimeout time.Duration
}

func (d Deps) claudeBin() string {
	if d.ClaudeBin == "" {
		return "claude"
	}
	return d.ClaudeBin
}

func (d Deps) idleTimeout() time.Duration {
	if d.IdleTimeout <= 0 {
		return 15 * time.Minute // sane fallback; main.go normally sets this from config
	}
	return d.IdleTimeout
}

// copyFileName is the note-copy's filename inside the target repo clone --
// deliberately distinctive so it's obvious in a directory listing and
// unlikely to collide with anything real.
func copyFileName(slug string) string {
	return fmt.Sprintf(".pmrunner-task-%s.md", slug)
}

// ProcessTask is the real per-task handler. Signature matches
// worker.ProcessFunc once bound to a specific Deps+reporter via a closure
// in main.go (worker.RepoWorker itself satisfies ActivityReporter).
func ProcessTask(deps Deps, reporter ActivityReporter, job worker.Job) error {
	reporter.SetStage("setup")

	repoCfg, ok := deps.Repos[job.Repo]
	if !ok {
		return fmt.Errorf("runner: no repo config for repo key %q (job %s)", job.Repo, job.Slug)
	}

	note, err := deps.Vault.ReadNote(job.NotePath)
	if err != nil {
		return fmt.Errorf("runner: reading note %s: %w", job.NotePath, err)
	}

	resume := note.Frontmatter.SessionID != nil && *note.Frontmatter.SessionID != ""

	if !resume {
		err := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: start %s", job.Slug), func(n *notetask.Note) error {
			n.Frontmatter.Status = "in_progress"
			notetask.AppendRunnerLog(n, "in_progress", time.Now())
			return nil
		})
		if err != nil {
			return fmt.Errorf("runner: writing in_progress for %s: %w", job.Slug, err)
		}
		// Re-read so later logic sees the just-written state (frontmatter
		// status, in particular) rather than the pre-claim snapshot.
		note, err = deps.Vault.ReadNote(job.NotePath)
		if err != nil {
			return fmt.Errorf("runner: re-reading note %s after in_progress write: %w", job.NotePath, err)
		}
	}

	branchName := fmt.Sprintf("task/%s-%s", job.Slug, time.Now().Format("2006-01-02"))
	if !resume {
		if err := pullAndBranch(repoCfg, branchName); err != nil {
			return fmt.Errorf("runner: setting up branch for %s: %w", job.Slug, err)
		}
	}

	copyName := copyFileName(job.Slug)
	copyPath := filepath.Join(repoCfg.Path, copyName)
	if err := writeNoteCopy(note, copyPath); err != nil {
		return fmt.Errorf("runner: writing note-copy for %s: %w", job.Slug, err)
	}
	if err := excludeFromGit(repoCfg.Path, copyName); err != nil {
		return fmt.Errorf("runner: excluding note-copy for %s: %w", job.Slug, err)
	}

	reporter.SetStage("invocation")

	prompt := buildPrompt(note, copyName, resume)
	var priorSessionID string
	if resume {
		priorSessionID = *note.Frontmatter.SessionID
	}

	sessionID, exitErr := invokeClaude(deps.claudeBin(), repoCfg.Path, prompt, priorSessionID, deps.idleTimeout(), reporter)
	if sessionID != "" {
		fmt.Printf("[runner] task %s: captured session_id=%s\n", job.Slug, sessionID)
	}
	if exitErr != nil {
		fmt.Printf("[runner] task %s: claude exited with error: %v\n", job.Slug, exitErr)
	} else {
		fmt.Printf("[runner] task %s: claude exited cleanly\n", job.Slug)
	}

	reporter.SetStage("result read-back")

	// Delete the copy unconditionally at the end, regardless of which path
	// below is taken -- same "always clean up" fix Background.md calls for
	// (the bash design's known gap around .task-result.json only being
	// cleaned up after the first read).
	defer func() {
		if err := os.Remove(copyPath); err != nil && !os.IsNotExist(err) {
			fmt.Printf("[runner] task %s: failed to delete note-copy %s: %v\n", job.Slug, copyPath, err)
		}
	}()

	copyAfter, copyErr := readNoteCopy(copyPath)
	switch {
	case copyErr != nil:
		// No copy, or one that fails to parse -- Phase 9 is where this
		// becomes real crash-fallback handling (blocked + alert). For now,
		// just log it; Phase 6's contract ("treat any exit as terminal, just
		// log") still applies to this scenario until Phase 9 lands.
		fmt.Printf("[runner] task %s: reading back note-copy failed (Phase 9 will turn this into a real crash fallback): %v\n", job.Slug, copyErr)
		if exitErr != nil {
			return fmt.Errorf("runner: claude invocation for %s: %w", job.Slug, exitErr)
		}
		return fmt.Errorf("runner: task %s finished but its note-copy could not be read back: %w", job.Slug, copyErr)

	case !isTerminalStatus(copyAfter.Frontmatter.Status):
		// Parsed fine, but CC never reached done/blocked/failed -- also
		// Phase 9's scenario 2, not yet real fallback logic here.
		fmt.Printf("[runner] task %s: copy has non-terminal status %q (Phase 9 will turn this into a real crash fallback)\n", job.Slug, copyAfter.Frontmatter.Status)
		return fmt.Errorf("runner: task %s's copy never reached a terminal status (got %q)", job.Slug, copyAfter.Frontmatter.Status)
	}

	// Happy path: merge status/pr_url/Work Log from the copy into the real
	// vault note, overlaying the runner's own independently-captured
	// session_id (never the copy's -- it was never sourced from there to
	// begin with), and append the terminal-status Runner Log line in the
	// same commit.
	terminalStatus := copyAfter.Frontmatter.Status
	mergeErr := deps.Vault.WriteNote(job.NotePath, fmt.Sprintf("runner: %s %s", terminalStatus, job.Slug), func(n *notetask.Note) error {
		n.Frontmatter.Status = terminalStatus
		n.Frontmatter.PRURL = copyAfter.Frontmatter.PRURL
		if sessionID != "" {
			n.Frontmatter.SessionID = &sessionID
		}
		n.HasWorkLog = copyAfter.HasWorkLog
		n.WorkLog = copyAfter.WorkLog
		notetask.AppendRunnerLog(n, terminalStatus, time.Now())
		return nil
	})
	if mergeErr != nil {
		return fmt.Errorf("runner: merging back task %s (status=%s): %w", job.Slug, terminalStatus, mergeErr)
	}

	fmt.Printf("[runner] task %s: merged back status=%s pr_url=%v\n", job.Slug, terminalStatus, derefStr(copyAfter.Frontmatter.PRURL))
	return nil
}

func isTerminalStatus(status string) bool {
	switch status {
	case "done", "blocked", "failed":
		return true
	default:
		return false
	}
}

func derefStr(s *string) string {
	if s == nil {
		return "<nil>"
	}
	return *s
}

// readNoteCopy reads and parses the note-copy from the target repo.
// Malformed or missing YAML is a hard error -- see Design - Runner.md's
// crash-fallback section: a parse failure is treated identically to no
// copy at all, no partial credit.
func readNoteCopy(copyPath string) (*notetask.Note, error) {
	data, err := os.ReadFile(copyPath)
	if err != nil {
		return nil, fmt.Errorf("reading note-copy: %w", err)
	}
	note, err := notetask.Parse(data)
	if err != nil {
		return nil, fmt.Errorf("parsing note-copy: %w", err)
	}
	return note, nil
}

func pullAndBranch(repoCfg config.RepoConfig, branchName string) error {
	run := func(args ...string) error {
		cmd := exec.Command("git", args...)
		cmd.Dir = repoCfg.Path
		cmd.Env = vaultgit.CleanGitEnv()
		out, err := cmd.CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %s (in %s): %w\n%s", strings.Join(args, " "), repoCfg.Path, err, out)
		}
		return nil
	}
	if err := run("checkout", repoCfg.DefaultBranch); err != nil {
		return err
	}
	if err := run("pull", "origin", repoCfg.DefaultBranch); err != nil {
		return err
	}
	if err := run("checkout", "-b", branchName); err != nil {
		return err
	}
	return nil
}

// writeNoteCopy writes the note's content into the target repo, minus the
// Runner Log -- CC never sees runner-only bookkeeping (see Background.md's
// note-copy decision).
func writeNoteCopy(note *notetask.Note, copyPath string) error {
	copyNote := &notetask.Note{
		Frontmatter: note.Frontmatter,
		Prompt:      note.Prompt,
		HasWorkLog:  note.HasWorkLog,
		WorkLog:     note.WorkLog,
		// HasRunnerLog deliberately left false: never copied.
	}
	out, err := copyNote.Bytes()
	if err != nil {
		return fmt.Errorf("serializing note-copy: %w", err)
	}
	return os.WriteFile(copyPath, out, 0o644)
}

// excludeFromGit adds name to the target repo clone's .git/info/exclude,
// idempotently, before CC ever runs -- so git add -A/-a and even a plain
// `git add <file>` (without -f) skip the copy automatically. Local-only,
// per-clone, never a tracked file (see Background.md).
func excludeFromGit(repoPath, name string) error {
	excludePath := filepath.Join(repoPath, ".git", "info", "exclude")
	existing, err := os.ReadFile(excludePath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("reading %s: %w", excludePath, err)
	}
	for _, line := range strings.Split(string(existing), "\n") {
		if strings.TrimSpace(line) == name {
			return nil // already present
		}
	}
	f, err := os.OpenFile(excludePath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("opening %s: %w", excludePath, err)
	}
	defer f.Close()
	if _, err := f.WriteString(name + "\n"); err != nil {
		return fmt.Errorf("appending to %s: %w", excludePath, err)
	}
	return nil
}

// buildPrompt constructs what gets passed to `claude -p`. CC has zero PM
// vault access (see Background.md), so it's pointed at the note-copy
// instead for anything it needs to read or write about task state.
func buildPrompt(note *notetask.Note, copyName string, resume bool) string {
	var b strings.Builder
	if resume {
		b.WriteString(fmt.Sprintf(
			"This is a resumed session for a task tracked in the file `%s` in this repository's root (already git-ignored -- do not commit it). Re-read that file now, including your own prior Work Log entries, to reconstruct context, then continue the task.\n\n", copyName))
	} else {
		b.WriteString(fmt.Sprintf(
			"You are working on a task tracked in the file `%s` in this repository's root (already git-ignored -- do not commit it). It holds your task's frontmatter (status/repo/auto_merge/pr_url/session_id/etc.) and body (the task prompt below, plus a \"## Work Log\" section for your own narrative).\n\n"+
				"As you work, append entries to its \"## Work Log\" section describing what you did. When finished, update its frontmatter: set `status` to `done` if you succeeded (and set `pr_url` if you opened one), `blocked` if you need human input (explain what you need in the Work Log), or `failed` if you're giving up. Never touch `session_id` -- that field is managed externally.\n\n"+
				"Task:\n%s\n", copyName, note.Prompt))
	}
	return b.String()
}

// invokeClaude runs `claude -p` (fresh or --resume), parses the first
// stream-json line for session_id, and waits for exit -- guarded by an idle
// watchdog: idleTimeout resets on every stream-json line (a long-but-alive
// task keeps resetting it forever), and fires only after that long a
// silence, killing the whole process group (not just the top-level PID) so
// a Bash-tool-spawned child claude itself started can't survive as an
// orphan. See Design - Runner.md's "Timeout / watchdog and kill mechanism"
// section -- this is an idle watchdog, not a flat deadline, for exactly the
// reason given there: a fixed wall-clock timeout can't tell "genuinely
// hung" from "still working."
//
// Returns the captured session_id (best-effort -- Phase 9 handles the case
// where the process died before ever emitting one) and either the
// process's own exit error, or ErrIdleTimeout if the watchdog fired.
func invokeClaude(claudeBin, dir, prompt, resumeSessionID string, idleTimeout time.Duration, reporter ActivityReporter) (string, error) {
	args := []string{"-p", prompt, "--output-format", "stream-json", "--verbose", "--dangerously-skip-permissions"}
	if resumeSessionID != "" {
		args = append(args, "--resume", resumeSessionID)
	}

	cmd := exec.Command(claudeBin, args...)
	cmd.Dir = dir
	// New process group: claude's own PID becomes its group leader, so
	// killing -PID (the negative of the group leader's PID) reaches every
	// descendant it spawned (e.g. a Bash tool call's subshell), not just
	// claude itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return "", fmt.Errorf("starting claude: %w", err)
	}

	idleTimer := time.NewTimer(idleTimeout)
	defer idleTimer.Stop()
	killDone := make(chan struct{})
	var watchdogFired atomic.Bool
	go func() {
		select {
		case <-idleTimer.C:
			// Set before killing, not after -- ProcessTask/tests check this
			// only once cmd.Wait() has returned, which (for a watchdog kill)
			// can't happen until this signal is actually sent, so ordering
			// it first here means it's never observed as still false.
			watchdogFired.Store(true)
			pgid := cmd.Process.Pid
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		case <-killDone:
		}
	}()

	var sessionID string
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := scanner.Text()

		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(idleTimeout)

		if reporter != nil {
			reporter.TouchActivity()
		}
		if line == "" {
			continue
		}
		var evt map[string]any
		if err := json.Unmarshal([]byte(line), &evt); err != nil {
			continue // not every line need be JSON we care about
		}
		if sessionID == "" {
			if sid, ok := evt["session_id"].(string); ok && sid != "" {
				sessionID = sid
			}
		}
	}

	waitErr := cmd.Wait()
	close(killDone)

	if watchdogFired.Load() {
		return sessionID, ErrIdleTimeout
	}
	return sessionID, waitErr
}
