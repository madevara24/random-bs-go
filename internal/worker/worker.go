// Package worker implements the per-repo goroutine that replaces
// worker.sh + spawn-worker.sh: one persistent goroutine per onboarded repo,
// alive for the daemon's life, blocking on a wake channel and draining its
// own FIFO queue, gated by a single shared global-concurrency semaphore.
// See Design - Runner.md's worker.sh section for the full mapping.
package worker

import (
	"fmt"
	"sync"
	"time"
)

// Job is one unit of work a RepoWorker processes. Deliberately its own type
// (not dispatch.Job) so this package never has to import dispatch --
// DispatchAdapter (adapter.go) is the only place that bridges the two.
type Job struct {
	NotePath string
	Repo     string
	Slug     string
}

// TaskStatus is what a RepoWorker exposes about its in-flight task, for the
// watcher's /status/tasks endpoint (Phase 12). Nil on RepoWorker.currentTask
// means idle. LastActivityAt is stamped by the runner's stream-json reader
// (Phase 8) -- one write path, two consumers (the idle watchdog and the
// watcher).
type TaskStatus struct {
	Slug           string
	Repo           string
	StartedAt      time.Time
	LastActivityAt time.Time
	Stage          string
}

// ProcessFunc does the actual task work. Phase 3 wires in a stub; Phase 6+
// replaces it with the real runner.ProcessTask. Errors are worker's own
// bookkeeping only -- by the time ProcessFunc returns (as opposed to
// panicking), the runner has already written the note's terminal status and
// fired its own alert as side effects; see Design - Runner.md's
// "Scope vs. runner.sh" section.
type ProcessFunc func(job Job) error

// NewGlobalSlots creates the shared global-concurrency semaphore. Created
// once at daemon boot and passed by reference (not copied) to every
// RepoWorker -- this is what makes the cap actually global across repos.
func NewGlobalSlots(n int) chan struct{} {
	return make(chan struct{}, n)
}

// RepoWorker is one repo's persistent worker goroutine: a mutex-guarded
// FIFO queue, a buffered-1 wake channel, and its own currentTask, guarded
// by its own RWMutex since the processing goroutine writes it while the
// watcher-facing HTTP handler (Phase 12) reads it from a different
// goroutine.
type RepoWorker struct {
	RepoKey     string
	Process     ProcessFunc
	globalSlots chan struct{}

	// OnPanic is called from runOne's recover() when Process panics --
	// Process never got the chance to write anything, so this is the
	// fallback of last resort. Phase 3's stub just logs; Phase 9's real
	// runner-level fallback gets wired in by whatever constructs the
	// RepoWorker (main.go), keeping this package free of any dependency on
	// runner/notify.
	OnPanic func(job Job, recovered any)

	// OnError is called when Process returns a non-nil error. Purely
	// internal bookkeeping (per Design - Runner.md: "not load-bearing") --
	// the runner already handled the user-visible side of any failure
	// before returning.
	OnError func(job Job, err error)

	mu    sync.Mutex
	queue []Job

	wake chan struct{}

	statusMu    sync.RWMutex
	currentTask *TaskStatus
}

// New constructs a RepoWorker. globalSlots must be shared (the same channel
// instance) across every RepoWorker for the cap to be meaningfully global.
func New(repoKey string, globalSlots chan struct{}, process ProcessFunc) *RepoWorker {
	return &RepoWorker{
		RepoKey:     repoKey,
		Process:     process,
		globalSlots: globalSlots,
		wake:        make(chan struct{}, 1),
		OnPanic: func(job Job, r any) {
			fmt.Printf("[worker %s] (stub) panic fallback for %s: %v\n", repoKey, job.Slug, r)
		},
		OnError: func(job Job, err error) {
			fmt.Printf("[worker %s] task %s returned error: %v\n", repoKey, job.Slug, err)
		},
	}
}

// Enqueue appends a job to this repo's queue and sends a non-blocking,
// coalesced wake signal -- a full (size-1) wake channel means a drain is
// already pending, so the extra signal is correctly dropped, not queued.
func (w *RepoWorker) Enqueue(job Job) {
	w.mu.Lock()
	w.queue = append(w.queue, job)
	w.mu.Unlock()

	select {
	case w.wake <- struct{}{}:
	default:
	}
}

// QueueLen reports the current queue depth -- used by startup reconciliation
// (Phase 4) and tests.
func (w *RepoWorker) QueueLen() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return len(w.queue)
}

func (w *RepoWorker) popJob() (Job, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.queue) == 0 {
		return Job{}, false
	}
	job := w.queue[0]
	w.queue = w.queue[1:]
	return job, true
}

// CurrentTask returns a copy of the in-flight task's status, or nil if
// idle. Safe to call from any goroutine.
func (w *RepoWorker) CurrentTask() *TaskStatus {
	w.statusMu.RLock()
	defer w.statusMu.RUnlock()
	if w.currentTask == nil {
		return nil
	}
	cp := *w.currentTask
	return &cp
}

// SetStage updates the in-flight task's stage and stamps LastActivityAt --
// exported so the runner package (Phase 6+) can report progress without
// worker needing to know anything about what a "stage" means.
func (w *RepoWorker) SetStage(stage string) {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	if w.currentTask != nil {
		w.currentTask.Stage = stage
		w.currentTask.LastActivityAt = time.Now()
	}
}

// TouchActivity stamps LastActivityAt without changing Stage -- the write
// path the runner's stream-json reader uses on every line (Phase 8).
func (w *RepoWorker) TouchActivity() {
	w.statusMu.Lock()
	defer w.statusMu.Unlock()
	if w.currentTask != nil {
		w.currentTask.LastActivityAt = time.Now()
	}
}

func (w *RepoWorker) setCurrentTask(t *TaskStatus) {
	w.statusMu.Lock()
	w.currentTask = t
	w.statusMu.Unlock()
}

// Run blocks on the wake channel, fully drains the queue on each wake, and
// blocks again -- forever. Meant to be launched once per repo, in its own
// goroutine, at daemon boot: `go rw.Run()`.
func (w *RepoWorker) Run() {
	for range w.wake {
		for {
			job, ok := w.popJob()
			if !ok {
				break
			}
			w.runOne(job)
		}
	}
}

// runOne acquires a global slot, runs one job through Process with its own
// panic recovery, and releases the slot -- a panic here can never escape to
// kill this repo's Run goroutine, which would otherwise silently stop that
// whole repo until a full daemon restart.
func (w *RepoWorker) runOne(job Job) {
	w.globalSlots <- struct{}{}
	defer func() { <-w.globalSlots }()

	now := time.Now()
	w.setCurrentTask(&TaskStatus{Slug: job.Slug, Repo: job.Repo, StartedAt: now, LastActivityAt: now, Stage: "starting"})
	defer w.setCurrentTask(nil)

	defer func() {
		if r := recover(); r != nil {
			if w.OnPanic != nil {
				w.OnPanic(job, r)
			}
		}
	}()

	if err := w.Process(job); err != nil {
		if w.OnError != nil {
			w.OnError(job, err)
		}
	}
}
