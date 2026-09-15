// Package watcher implements the pmwatch mode's real checks: the two-tier
// HTTP poll (Design - Watcher.md) plus the independent direct vault scan
// for gap 3. Closes the three gaps from PM Runner Monitoring Gaps: dead
// pipeline, wedged session, lost success notice.
package watcher

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
)

// HealthResult classifies the outcome of the Tier 1 /health check.
type HealthResult int

const (
	// HealthOK: the daemon answered 200 -- alive.
	HealthOK HealthResult = iota
	// HealthConnectionRefused: nothing is listening -- the daemon process
	// is dead. Gap 1.
	HealthConnectionRefused
	// HealthTimeout: a TCP connection succeeded but the request never
	// completed in time -- the daemon's own runtime is wedged at a level
	// below any single task's lock. Rarer and more serious than a single
	// stale task.
	HealthTimeout
	// HealthOther: some other failure (unexpected status code, etc.) --
	// treated conservatively, same as a timeout for alerting purposes.
	HealthOther
)

func (r HealthResult) String() string {
	switch r {
	case HealthOK:
		return "ok"
	case HealthConnectionRefused:
		return "connection_refused"
	case HealthTimeout:
		return "timeout"
	default:
		return "other"
	}
}

// TaskStatus mirrors httpapi.TaskStatusWire -- watcher deliberately doesn't
// import httpapi (that would be backwards: httpapi is the runner's own
// surface, watcher is a separate consumer process in production, even
// though today they're compiled into the same binary) -- decodes the same
// JSON shape independently.
type TaskStatus struct {
	Slug           string    `json:"slug"`
	Repo           string    `json:"repo"`
	StartedAt      time.Time `json:"started_at"`
	LastActivityAt time.Time `json:"last_activity_at"`
	Stage          string    `json:"stage"`
}

// StaleTask is a task flagged by gap 2 -- wedged session.
type StaleTask struct {
	RepoKey string
	TaskStatus
	IdleFor time.Duration
}

// UnnotifiedNote is a note flagged by gap 3 -- lost success notice.
type UnnotifiedNote struct {
	RelPath string
	Slug    string
	Status  string
	Age     time.Duration
}

// Watcher holds everything the pmwatch checks need.
type Watcher struct {
	BaseURL string // e.g. "http://127.0.0.1:8420"
	Vault   *vaultgit.Vault

	// IdleTimeout is the single shared staleness threshold -- same value
	// as the runner's own idle-watchdog window, per Design - Watcher.md's
	// decision that a task the watcher calls "stale" is exactly the task
	// the runner is about to kill anyway. Used for both gap 2 and gap 3.
	IdleTimeout time.Duration

	// HealthHTTPTimeout/StatusHTTPTimeout are the short per-call timeouts
	// for the two tiers -- distinct from IdleTimeout, which is about task
	// staleness, not HTTP round-trip time. Sensible defaults apply if zero.
	HealthHTTPTimeout time.Duration
	StatusHTTPTimeout time.Duration
}

func (w *Watcher) healthTimeout() time.Duration {
	if w.HealthHTTPTimeout > 0 {
		return w.HealthHTTPTimeout
	}
	return 5 * time.Second
}

func (w *Watcher) statusTimeout() time.Duration {
	if w.StatusHTTPTimeout > 0 {
		return w.StatusHTTPTimeout
	}
	return 5 * time.Second
}

// CheckHealth is Tier 1: daemon-liveness only. Connection-refused and
// timeout-despite-connect are reported as distinct HealthResults, per
// Design - Watcher.md's requirement that these are meaningful, differently
// alertable signals.
func (w *Watcher) CheckHealth() (HealthResult, error) {
	client := &http.Client{Timeout: w.healthTimeout()}
	resp, err := client.Get(w.BaseURL + "/health")
	if err != nil {
		return classifyHealthErr(err), err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return HealthOther, fmt.Errorf("unexpected status %d", resp.StatusCode)
	}
	return HealthOK, nil
}

func classifyHealthErr(err error) HealthResult {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return HealthTimeout
	}
	if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(err.Error(), "connection refused") {
		return HealthConnectionRefused
	}
	return HealthOther
}

// CheckStatusTasks is Tier 2, called only if Tier 1 already succeeded:
// reads every repo's current task and flags any whose LastActivityAt is
// older than IdleTimeout -- gap 2, wedged session.
func (w *Watcher) CheckStatusTasks() (map[string]TaskStatus, []StaleTask, error) {
	client := &http.Client{Timeout: w.statusTimeout()}
	resp, err := client.Get(w.BaseURL + "/status/tasks")
	if err != nil {
		return nil, nil, fmt.Errorf("watcher: GET /status/tasks: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil, fmt.Errorf("watcher: GET /status/tasks: unexpected status %d", resp.StatusCode)
	}

	var raw map[string]TaskStatus
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, nil, fmt.Errorf("watcher: decoding /status/tasks response: %w", err)
	}

	now := time.Now()
	var stale []StaleTask
	for repoKey, ts := range raw {
		idleFor := now.Sub(ts.LastActivityAt)
		if idleFor > w.IdleTimeout {
			stale = append(stale, StaleTask{RepoKey: repoKey, TaskStatus: ts, IdleFor: idleFor})
		}
	}
	return raw, stale, nil
}

const tasksDir = "Tasks"

func isTerminalStatus(status string) bool {
	switch status {
	case "done", "blocked", "failed":
		return true
	default:
		return false
	}
}

func slugFromPath(relPath string) string {
	base := filepath.Base(relPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// ScanForUnnotified is gap 3, independent of the two HTTP tiers above --
// a direct read of Tasks/ (not through the daemon's HTTP surface at all,
// since RepoWorker.currentTask is already cleared by the time this
// matters, per Background.md's Runner Log decision). Flags any note at a
// terminal status whose Runner Log has neither "notified" nor
// "notify_failed" yet, past ageThreshold.
func (w *Watcher) ScanForUnnotified(ageThreshold time.Duration) ([]UnnotifiedNote, error) {
	pattern := filepath.Join(w.Vault.Path, tasksDir, "*.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, fmt.Errorf("watcher: scanning %s: %w", pattern, err)
	}

	now := time.Now()
	var out []UnnotifiedNote
	for _, m := range matches {
		data, err := os.ReadFile(m)
		if err != nil {
			continue
		}
		note, err := notetask.Parse(data)
		if err != nil {
			continue // malformed note -- not this scan's job to fix
		}
		if !isTerminalStatus(note.Frontmatter.Status) {
			continue
		}
		hasNotified := false
		for _, e := range note.RunnerLog {
			if e.Event == "notified" || e.Event == "notify_failed" {
				hasNotified = true
				break
			}
		}
		if hasNotified {
			continue
		}

		age := ageThreshold // conservative default if mtime is unavailable
		if info, statErr := os.Stat(m); statErr == nil {
			age = now.Sub(info.ModTime())
		}
		if age < ageThreshold {
			continue
		}

		relPath, relErr := filepath.Rel(w.Vault.Path, m)
		if relErr != nil {
			relPath = m
		}
		out = append(out, UnnotifiedNote{
			RelPath: relPath,
			Slug:    slugFromPath(relPath),
			Status:  note.Frontmatter.Status,
			Age:     age,
		})
	}
	return out, nil
}
