package worker

import (
	"fmt"

	"github.com/madevara24/random-bs-go/internal/dispatch"
)

// Workers maps a repo key to its RepoWorker and implements
// dispatch.Enqueuer, bridging dispatch's own Job type to worker.Job. This
// is the one place worker depends on dispatch -- safe, since dispatch never
// imports worker (its Enqueuer interface exists specifically to avoid
// that), so there's no cycle.
type Workers map[string]*RepoWorker

// Enqueue implements dispatch.Enqueuer.
func (w Workers) Enqueue(repoKey string, job dispatch.Job) {
	rw, ok := w[repoKey]
	if !ok {
		fmt.Printf("[worker] no RepoWorker registered for repo %q, dropping job %s\n", repoKey, job.Slug)
		return
	}
	rw.Enqueue(Job{NotePath: job.NotePath, Repo: job.Repo, Slug: job.Slug})
}

// StartAll launches every RepoWorker's Run() goroutine. Call once, at boot.
func (w Workers) StartAll() {
	for _, rw := range w {
		go rw.Run()
	}
}
