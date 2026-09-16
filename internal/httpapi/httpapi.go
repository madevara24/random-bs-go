// Package httpapi is the runner daemon's local HTTP surface: an inbound
// trigger from the git hook (POST /dispatch), a liveness probe
// (GET /health), and (POST /runner-log) the write side of the watcher's
// Runner Log outcome, since the watcher runs in a separate OS process from
// the one that actually owns the vault clone. Every endpoint here is meant
// to bind localhost only, no auth (same-user, same-machine trust
// boundary).
package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
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

	// Vault backs POST /runner-log -- the same *vaultgit.Vault instance
	// the rest of the runner uses, so the write goes through the one
	// in-process mutex that actually serializes every git-touching
	// operation against this clone. Nil is valid (the route then reports
	// 503) for callers that don't need /runner-log, like most of this
	// package's own tests.
	Vault *vaultgit.Vault

	mux *http.ServeMux
}

// New builds a Server. dispatchWake must be the same channel the daemon's
// dispatch-pass loop is ranging over.
func New(dispatchWake chan struct{}, workers worker.Workers, vault *vaultgit.Vault) *Server {
	s := &Server{DispatchWake: dispatchWake, Workers: workers, Vault: vault, mux: http.NewServeMux()}
	s.mux.HandleFunc("/dispatch", s.handleDispatch)
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/status/tasks", s.handleStatusTasks)
	s.mux.HandleFunc("/runner-log", s.handleRunnerLog)
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
	Slug           string    `json:"slug"`
	Repo           string    `json:"repo"`
	StartedAt      time.Time `json:"started_at"`
	LastActivityAt time.Time `json:"last_activity_at"`
	Stage          string    `json:"stage"`
}

// handleStatusTasks reads every RepoWorker.currentTask under its own
// RWMutex and returns the per-repo map (nil entries for idle repos are
// omitted, not returned as null, to keep the payload small) -- this is
// Tier 2 of the watcher's checks, only called after Tier 1 (/health)
// already succeeded, so a real delay here specifically means "stuck on a
// per-repo lock," not "daemon down."
func (s *Server) handleStatusTasks(w http.ResponseWriter, r *http.Request) {
	out := map[string]TaskStatusWire{}
	for repoKey, rw := range s.Workers {
		ts := rw.CurrentTask()
		if ts == nil {
			continue
		}
		out[repoKey] = TaskStatusWire{
			Slug:           ts.Slug,
			Repo:           ts.Repo,
			StartedAt:      ts.StartedAt,
			LastActivityAt: ts.LastActivityAt,
			Stage:          ts.Stage,
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
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
