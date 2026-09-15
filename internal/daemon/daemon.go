// Package daemon wires together the pieces built in Phases 1-4 into the
// runner's actual boot sequence: sync the vault, reconcile any notes
// already at "queued" from a prior run into the in-memory queues, and only
// then be ready to accept dispatch triggers. Not named in
// Design - Implementation.md's package table, which stops at internal/
// dispatch, worker, runner, notify, watcher, httpapi -- added as thin glue
// so cmd/pmrunner/main.go doesn't have to hold this orchestration itself
// and so it stays independently testable.
package daemon

import (
	"fmt"

	"github.com/madevara24/random-bs-go/internal/dispatch"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// Runner holds everything the runner mode's boot sequence and dispatch
// passes need.
type Runner struct {
	Vault   *vaultgit.Vault
	Workers worker.Workers

	// DispatchWake is the size-1 buffered channel the HTTP /dispatch
	// handler (Phase 5) sends into. RunDispatchLoop is the consumer.
	DispatchWake chan struct{}
}

// NewRunner constructs a Runner. Does not touch disk or start any
// goroutines.
func NewRunner(vault *vaultgit.Vault, workers worker.Workers) *Runner {
	return &Runner{Vault: vault, Workers: workers, DispatchWake: make(chan struct{}, 1)}
}

// Boot runs the startup sequence required before the daemon may accept any
// dispatch trigger: sync-then-reconcile, never the reverse -- reconciling
// against a stale local clone would miss anything that landed on the bare
// repo since the daemon's last run (see Design - Runner.md's
// with-vault-lock.sh section). Callers must call Workers.StartAll() first
// (or at least before relying on queued jobs actually draining) --
// ReconcileQueued only needs the map to exist to enqueue into it; the
// buffered wake channel means goroutine start order relative to this call
// doesn't matter, but not starting them at all would just leave the queue
// full forever.
func (r *Runner) Boot() error {
	if err := r.Vault.Sync(); err != nil {
		return fmt.Errorf("daemon: boot sync: %w", err)
	}
	if err := dispatch.ReconcileQueued(r.Vault, r.Workers); err != nil {
		return fmt.Errorf("daemon: boot reconcile: %w", err)
	}
	return nil
}

// RunDispatchPass runs one full dispatch pass (sync -> scan -> claim ->
// enqueue) against this Runner's vault and workers.
func (r *Runner) RunDispatchPass() error {
	return dispatch.RunDispatchPass(r.Vault, r.Workers)
}

// RunDispatchLoop blocks forever, running exactly one dispatch pass each
// time DispatchWake fires, coalescing any signals that arrive while a pass
// is already running (the channel is size-1, so extra sends while this
// loop is busy inside RunDispatchPass are simply dropped by the sender --
// see httpapi's handleDispatch). Meant to run in its own goroutine, started
// once at boot alongside the HTTP server. onError is called (non-fatal)
// whenever a pass returns an error; the loop keeps running regardless.
func (r *Runner) RunDispatchLoop(onError func(error)) {
	for range r.DispatchWake {
		if err := r.RunDispatchPass(); err != nil {
			if onError != nil {
				onError(err)
			}
		}
	}
}
