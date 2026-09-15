package worker

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestRepoWorkerFIFOConcurrencyAndPanicRecovery pushes several jobs across
// two repos' queues directly (no dispatcher) and confirms, per Phase 3's
// test gate in Design - Implementation.md: FIFO draining order per repo,
// global concurrency never exceeding N across both repos combined, and a
// forced panic inside the stub task being caught by recover() without
// killing the goroutine -- the next job on that same repo still processes
// afterward.
func TestRepoWorkerFIFOConcurrencyAndPanicRecovery(t *testing.T) {
	const globalCap = 2

	var (
		mu           sync.Mutex
		startedOrder = map[string][]string{} // repoKey -> slugs in the order Process was entered
		panicked     []Job

		current   int32
		maxAtOnce int32
	)

	globalSlots := NewGlobalSlots(globalCap)

	process := func(job Job) error {
		n := atomic.AddInt32(&current, 1)
		defer atomic.AddInt32(&current, -1)
		for {
			m := atomic.LoadInt32(&maxAtOnce)
			if n <= m || atomic.CompareAndSwapInt32(&maxAtOnce, m, n) {
				break
			}
		}

		mu.Lock()
		startedOrder[job.Repo] = append(startedOrder[job.Repo], job.Slug)
		mu.Unlock()

		time.Sleep(50 * time.Millisecond)

		if job.Slug == "repo-a-panic" {
			panic("forced panic for test")
		}
		return nil
	}

	wA := New("repo-a", globalSlots, process)
	wA.OnPanic = func(job Job, r any) {
		mu.Lock()
		panicked = append(panicked, job)
		mu.Unlock()
	}
	wB := New("repo-b", globalSlots, process)

	go wA.Run()
	go wB.Run()

	// repo-a: a1, a2, then a panicking job, then a3 -- a3 must still run.
	wA.Enqueue(Job{Repo: "repo-a", Slug: "repo-a-1", NotePath: "Tasks/a1.md"})
	wA.Enqueue(Job{Repo: "repo-a", Slug: "repo-a-2", NotePath: "Tasks/a2.md"})
	wA.Enqueue(Job{Repo: "repo-a", Slug: "repo-a-panic", NotePath: "Tasks/apanic.md"})
	wA.Enqueue(Job{Repo: "repo-a", Slug: "repo-a-3", NotePath: "Tasks/a3.md"})

	// repo-b: three plain jobs, interleaved concurrently with repo-a.
	wB.Enqueue(Job{Repo: "repo-b", Slug: "repo-b-1", NotePath: "Tasks/b1.md"})
	wB.Enqueue(Job{Repo: "repo-b", Slug: "repo-b-2", NotePath: "Tasks/b2.md"})
	wB.Enqueue(Job{Repo: "repo-b", Slug: "repo-b-3", NotePath: "Tasks/b3.md"})

	deadline := time.After(5 * time.Second)
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
waitLoop:
	for {
		select {
		case <-tick.C:
			mu.Lock()
			done := len(startedOrder["repo-a"]) == 4 && len(startedOrder["repo-b"]) == 3
			mu.Unlock()
			if done {
				break waitLoop
			}
		case <-deadline:
			mu.Lock()
			t.Fatalf("timed out waiting for all jobs to process; got repo-a=%v repo-b=%v",
				startedOrder["repo-a"], startedOrder["repo-b"])
			mu.Unlock()
		}
	}
	// Let the panicking job's own runOne fully unwind (its recover/defers)
	// before asserting on panicked.
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()

	wantA := []string{"repo-a-1", "repo-a-2", "repo-a-panic", "repo-a-3"}
	if fmt.Sprint(startedOrder["repo-a"]) != fmt.Sprint(wantA) {
		t.Errorf("repo-a FIFO order = %v, want %v", startedOrder["repo-a"], wantA)
	}
	wantB := []string{"repo-b-1", "repo-b-2", "repo-b-3"}
	if fmt.Sprint(startedOrder["repo-b"]) != fmt.Sprint(wantB) {
		t.Errorf("repo-b FIFO order = %v, want %v", startedOrder["repo-b"], wantB)
	}

	if len(panicked) != 1 || panicked[0].Slug != "repo-a-panic" {
		t.Errorf("panicked = %+v, want exactly one entry for repo-a-panic", panicked)
	}

	// The job enqueued after the panicking one still ran -- proof the
	// panic didn't kill repo-a's Run goroutine.
	found := false
	for _, s := range startedOrder["repo-a"] {
		if s == "repo-a-3" {
			found = true
		}
	}
	if !found {
		t.Errorf("repo-a-3 never ran -- panic likely killed the worker goroutine")
	}

	if maxAtOnce > globalCap {
		t.Errorf("observed %d tasks running concurrently, want <= %d (global slot cap)", maxAtOnce, globalCap)
	}
	if maxAtOnce < 2 {
		t.Logf("warning: never observed 2 concurrent tasks (maxAtOnce=%d) -- concurrency assertion is weak on this run", maxAtOnce)
	}
}
