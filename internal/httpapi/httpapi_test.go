package httpapi

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/worker"
)

func TestHealthAlwaysOK(t *testing.T) {
	s := New(make(chan struct{}, 1), nil)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestDispatchWakesAndCoalesces(t *testing.T) {
	wake := make(chan struct{}, 1)
	s := New(wake, nil)
	ts := httptest.NewServer(s)
	defer ts.Close()

	post := func() *http.Response {
		resp, err := http.Post(ts.URL+"/dispatch", "", nil)
		if err != nil {
			t.Fatalf("POST /dispatch: %v", err)
		}
		return resp
	}

	// First call: replies immediately and signals wake.
	resp1 := post()
	if resp1.StatusCode != http.StatusAccepted {
		t.Errorf("first POST /dispatch status = %d, want 202", resp1.StatusCode)
	}

	// Second call before anyone drains wake: still replies immediately
	// (non-blocking send), and the extra signal is correctly dropped since
	// the channel is already full.
	done := make(chan struct{})
	go func() {
		resp2 := post()
		if resp2.StatusCode != http.StatusAccepted {
			t.Errorf("second POST /dispatch status = %d, want 202", resp2.StatusCode)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("second POST /dispatch blocked -- handleDispatch must never block on a full wake channel")
	}

	select {
	case <-wake:
	default:
		t.Fatal("wake channel never received a signal from POST /dispatch")
	}
	// Channel should now be empty -- the second POST's signal was
	// correctly coalesced away, not queued.
	select {
	case <-wake:
		t.Fatal("wake channel had a second signal queued -- coalescing failed")
	default:
	}
}

func TestDispatchRejectsNonPost(t *testing.T) {
	s := New(make(chan struct{}, 1), nil)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/dispatch")
	if err != nil {
		t.Fatalf("GET /dispatch: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /dispatch status = %d, want 405", resp.StatusCode)
	}
}

func TestStatusTasksReportsOnlyBusyRepos(t *testing.T) {
	globalSlots := worker.NewGlobalSlots(1)
	idleWorker := worker.New("idle-repo", globalSlots, nil)
	busyWorker := worker.New("busy-repo", globalSlots, func(job worker.Job) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	})
	workers := worker.Workers{"idle-repo": idleWorker, "busy-repo": busyWorker}
	workers.StartAll()
	busyWorker.Enqueue(worker.Job{Repo: "busy-repo", Slug: "busy-slug", NotePath: "Tasks/busy-slug.md"})
	time.Sleep(20 * time.Millisecond) // let it actually start

	s := New(make(chan struct{}, 1), workers)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status/tasks")
	if err != nil {
		t.Fatalf("GET /status/tasks: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got map[string]TaskStatusWire
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if _, ok := got["idle-repo"]; ok {
		t.Errorf("idle-repo present in response, want it omitted: %+v", got)
	}
	busy, ok := got["busy-repo"]
	if !ok {
		t.Fatalf("busy-repo missing from response: %+v", got)
	}
	if busy.Slug != "busy-slug" {
		t.Errorf("busy-repo.Slug = %q, want %q", busy.Slug, "busy-slug")
	}
}
