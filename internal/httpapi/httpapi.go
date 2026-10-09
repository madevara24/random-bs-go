// Package httpapi is the runner daemon's local HTTP surface: an inbound
// trigger from the git hook (POST /dispatch), a liveness probe
// (GET /health), (POST /runner-log) the write side of the watcher's Runner
// Log outcome, since the watcher runs in a separate OS process from the one
// that actually owns the vault clone, and (POST /resume) the manual
// counterpart to main.go's usage-limit auto-resume timer. Every endpoint
// here is meant to bind localhost only, no auth (same-user, same-machine
// trust boundary).
package httpapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/pause"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// Server is the runner's HTTP surface.
type Server struct {
	// DispatchWake is a size-1 buffered channel; POST /dispatch sends a
	// non-blocking, coalesced wake signal into it. The daemon's dispatch-
	// pass goroutine (see internal/daemon) is the consumer.
	DispatchWake chan struct{}

	// Workers backs GET /status/tasks -- reads each RepoWorker.currentTask
	// under its own RWMutex. May be nil (the route then reports an empty
	// map), which is fine for callers that only need /dispatch and
	// /health.
	Workers worker.Workers

	// Vault backs POST /runner-log and POST /resume -- the same
	// *vaultgit.Vault instance the rest of the runner uses, so the write
	// goes through the one in-process mutex that actually serializes every
	// git-touching operation against this clone. Nil is valid (the routes
	// then report 503) for callers that don't need either, like most of
	// this package's own tests.
	Vault *vaultgit.Vault

	// Pause is the shared pipeline-pause flag. Nil means POST /resume and
	// GET /status/tasks's paused/resets_at fields are no-ops/zero values --
	// fine for callers that don't need this feature.
	Pause *pause.State

	// CancelResumeTimer, if set, is called by POST /resume (and by Resume,
	// its underlying method) before doing anything else -- main.go wires
	// this to stop its own auto-resume timer, so a manual resume can never
	// race the timer into resuming twice. nil is a safe no-op.
	CancelResumeTimer func()

	mux *http.ServeMux
}

// New builds a Server. dispatchWake must be the same channel the daemon's
// dispatch-pass loop is ranging over.
func New(dispatchWake chan struct{}, workers worker.Workers, vault *vaultgit.Vault, p *pause.State) *Server {
	s := &Server{DispatchWake: dispatchWake, Workers: workers, Vault: vault, Pause: p, mux: http.NewServeMux()}
	s.mux.HandleFunc("/dispatch", s.handleDispatch)
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/status/tasks", s.handleStatusTasks)
	s.mux.HandleFunc("/runner-log", s.handleRunnerLog)
	s.mux.HandleFunc("/resume", s.handleResume)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleDispatch replies immediately regardless of whether a pass was
// already pending -- the actual scan/claim/enqueue work happens later, in
// the dedicated dispatch-pass goroutine, never inline in this handler.
func (s *Server) handleDispatch(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	select {
	case s.DispatchWake <- struct{}{}:
	default:
		// A pass is already pending; the extra signal is correctly dropped.
	}
	w.WriteHeader(http.StatusAccepted)
	_, _ = w.Write([]byte("dispatch pass requested\n"))
}

// handleHealth is daemon-liveness only: hardcoded 200, touches zero locks
// and zero per-repo state, by construction -- the watcher relies on this
// being lock-free so a wedged per-repo goroutine can never make this
// endpoint itself hang. The real payload (per-repo status) is
// GET /status/tasks; this route never grows one.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// TaskStatusWire is GET /status/tasks's per-repo JSON shape.
type TaskStatusWire struct {
	Slug            string    `json:"slug"`
	Repo            string    `json:"repo"`
	StartedAt       time.Time `json:"started_at"`
	LastActivityAt  time.Time `json:"last_activity_at"`
	Stage           string    `json:"stage"`
	DiscordThreadID string    `json:"discord_thread_id"`
}

// StatusTasksResponse is GET /status/tasks's full JSON shape: the per-repo
// task map (unchanged), plus top-level visibility into the shared pause --
// Paused/ResetsAt mirror pause.State.Snapshot() so a human (or the watcher,
// in principle) can see a stuck-looking queue is actually just waiting on
// the usage limit to clear, not wedged.
type StatusTasksResponse struct {
	Paused   bool                      `json:"paused"`
	ResetsAt *time.Time                `json:"resets_at,omitempty"`
	Tasks    map[string]TaskStatusWire `json:"tasks"`
}

// handleStatusTasks reads every RepoWorker.currentTask under its own
// RWMutex and returns the per-repo map (nil entries for idle repos are
// omitted, not returned as null, to keep the payload small) -- this is
// Tier 2 of the watcher's checks, only called after Tier 1 (/health)
// already succeeded, so a real delay here specifically means "stuck on a
// per-repo lock," not "daemon down."
//
// DiscordThreadID is looked up here, via a plain read-only Vault.ReadNote
// of the task's own note -- the watcher runs in a separate OS process with
// no vault clone of its own (see notify.Notifier.RunnerLogURL's doc
// comment for the two-process git-lock collision that already ruled out
// giving it one), so this is how it learns which thread its own "looks
// wedged" alert belongs in without reintroducing that collision:
// ReadNote never takes Vault's write mutex, only WriteNote does.
func (s *Server) handleStatusTasks(w http.ResponseWriter, r *http.Request) {
	out := map[string]TaskStatusWire{}
	for repoKey, rw := range s.Workers {
		ts := rw.CurrentTask()
		if ts == nil {
			continue
		}
		out[repoKey] = TaskStatusWire{
			Slug:            ts.Slug,
			Repo:            ts.Repo,
			StartedAt:       ts.StartedAt,
			LastActivityAt:  ts.LastActivityAt,
			Stage:           ts.Stage,
			DiscordThreadID: s.discordThreadIDFor(ts.NotePath),
		}
	}
	snap := s.Pause.Snapshot()
	resp := StatusTasksResponse{Paused: snap.Paused, ResetsAt: snap.ResetsAt, Tasks: out}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

// discordThreadIDFor best-effort reads notePath's discord_thread_id --
// empty on a nil Vault, an empty notePath, a read failure, or a note that
// never got a thread (claimed before per-task threads existed, or whose
// claim post failed). Never an error: a missing thread ID just means the
// caller falls back to the plain top-level webhook, same as always.
func (s *Server) discordThreadIDFor(notePath string) string {
	if s.Vault == nil || notePath == "" {
		return ""
	}
	note, err := s.Vault.ReadNote(notePath)
	if err != nil || note.Frontmatter.DiscordThreadID == nil {
		return ""
	}
	return *note.Frontmatter.DiscordThreadID
}

// runnerLogRequest is POST /runner-log's JSON body.
type runnerLogRequest struct {
	NotePath string `json:"note_path"`
	Event    string `json:"event"`
}

// handleRunnerLog appends one Runner Log line ("notified" or
// "notify_failed") to the named note, via the runner's own Vault -- the
// watcher calls this instead of writing to the vault clone itself, since
// it runs in a different OS process and has no way to share the mutex that
// actually serializes git operations against this clone.
func (s *Server) handleRunnerLog(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if s.Vault == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("runner has no vault configured\n"))
		return
	}
	var req runnerLogRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("malformed JSON body\n"))
		return
	}
	if req.NotePath == "" || (req.Event != "notified" && req.Event != "notify_failed") {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte("note_path required, event must be \"notified\" or \"notify_failed\"\n"))
		return
	}
	err := s.Vault.WriteNote(req.NotePath, "runner: "+req.Event, func(note *notetask.Note) error {
		notetask.AppendRunnerLog(note, req.Event, time.Now())
		return nil
	})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// handleResume is the manual counterpart to main.go's usage-limit
// auto-resume timer: a human calling this before the timer fires resumes
// the pipeline immediately and cancels the timer, so it can never fire a
// second time afterward.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	flipped, err := s.Resume()
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{"resumed_notes": flipped})
}

// Resume is POST /resume's mechanics, also called directly by main.go's
// auto-resume timer on fire (after it sends its own "reset passed" Discord
// message) so both paths share exactly one resume implementation, in this
// order: cancel any main.go-owned timer (so a fire racing a concurrent
// manual call can't resume twice), clear the pause and collect the notes it
// recorded, flip each from blocked to blocker_resolved with a Runner Log
// line, then wake the dispatch loop and every repo worker so the normal
// claim path picks everything back up (via --resume, since session_id is
// already set).
//
// Flipping a note that isn't currently "blocked" is deliberately skipped,
// not forced -- if a human already hand-resolved it some other way between
// the trip and this call, forcing blocker_resolved over whatever they did
// would stomp on that.
func (s *Server) Resume() ([]string, error) {
	if s.CancelResumeTimer != nil {
		s.CancelResumeTimer()
	}
	notePaths := s.Pause.Clear()

	var flipped []string
	for _, notePath := range notePaths {
		if s.Vault == nil {
			continue
		}
		err := s.Vault.WriteNote(notePath, "runner: blocker_resolved (usage limit reset)", func(n *notetask.Note) error {
			if n.Frontmatter.Status != "blocked" {
				return nil
			}
			n.Frontmatter.Status = "blocker_resolved"
			notetask.AppendRunnerLog(n, "blocker_resolved", time.Now())
			return nil
		})
		if err != nil {
			return flipped, fmt.Errorf("httpapi: resuming %s: %w", notePath, err)
		}
		flipped = append(flipped, notePath)
	}

	select {
	case s.DispatchWake <- struct{}{}:
	default:
	}
	for _, rw := range s.Workers {
		rw.Wake()
	}

	return flipped, nil
}
