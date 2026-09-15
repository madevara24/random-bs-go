// Package httpapi is the runner daemon's local HTTP surface: an inbound
// trigger from the git hook (POST /dispatch) and a liveness probe
// (GET /health). See Background.md's "Trigger model" and "Runner <-> watcher
// communication" sections -- both endpoints are meant to bind localhost
// only, no auth (same-user, same-machine trust boundary).
package httpapi

import (
	"net/http"
)

// Server is the runner's HTTP surface.
type Server struct {
	// DispatchWake is a size-1 buffered channel; POST /dispatch sends a
	// non-blocking, coalesced wake signal into it. The daemon's dispatch-
	// pass goroutine (see internal/daemon) is the consumer.
	DispatchWake chan struct{}

	mux *http.ServeMux
}

// New builds a Server. dispatchWake must be the same channel the daemon's
// dispatch-pass loop is ranging over.
func New(dispatchWake chan struct{}) *Server {
	s := &Server{DispatchWake: dispatchWake, mux: http.NewServeMux()}
	s.mux.HandleFunc("/dispatch", s.handleDispatch)
	s.mux.HandleFunc("/health", s.handleHealth)
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
