// Package httpapi is the runner daemon's local HTTP surface: an inbound
// trigger from the git hook (POST /dispatch) and a liveness probe
// (GET /health). See Background.md's "Trigger model" and "Runner <-> watcher
// communication" sections -- both endpoints are meant to bind localhost
// only, no auth (same-user, same-machine trust boundary).
package httpapi

import (
	"encoding/json"
	"net/http"
	"time"

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
	// /health, like Phase 5's own tests.
	Workers worker.Workers

	mux *http.ServeMux
}

// New builds a Server. dispatchWake must be the same channel the daemon's
// dispatch-pass loop is ranging over.
func New(dispatchWake chan struct{}, workers worker.Workers) *Server {
	s := &Server{DispatchWake: dispatchWake, Workers: workers, mux: http.NewServeMux()}
	s.mux.HandleFunc("/dispatch", s.handleDispatch)
	s.mux.HandleFunc("/health", s.handleHealth)
	s.mux.HandleFunc("/status/tasks", s.handleStatusTasks)
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// handleDispatch replies immediately regardless of whether a pass was
// already pending -- the actual scan/claim/enqueue work happens later, in
// the dedicated dispatch-pass goroutine, never inline in this handler (see
// Design - Runner.md's dispatcher.sh section: "never runs inline in the
// HTTP handler").
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
// and zero per-repo state, by construction -- Phase 12's watcher relies on
// this being lock-free so a wedged per-repo goroutine can never make this
// endpoint itself hang. The real payload (per-repo status) is
// GET /status/tasks, added in Phase 12; this route never grows one.
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
// omitted, not returned as null, to keep the payload small) -- Tier 2 of
// the watcher's two-tier design (Design - Watcher.md): only called after
// Tier 1 (/health) already succeeded, so a real delay here specifically
// means "stuck on a per-repo lock," not "daemon down."
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
