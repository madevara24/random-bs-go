package httpapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

func TestHealthAlwaysOK(t *testing.T) {
	s := New(make(chan struct{}, 1), nil, nil)
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
	s := New(wake, nil, nil)
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
	s := New(make(chan struct{}, 1), nil, nil)
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

	s := New(make(chan struct{}, 1), workers, nil)
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

func TestRunnerLogRejectsNonPost(t *testing.T) {
	s := New(make(chan struct{}, 1), nil, nil)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/runner-log")
	if err != nil {
		t.Fatalf("GET /runner-log: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /runner-log status = %d, want 405", resp.StatusCode)
	}
}

func TestRunnerLogWithoutVaultReturns503(t *testing.T) {
	s := New(make(chan struct{}, 1), nil, nil)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/runner-log", "application/json",
		bytes.NewReader([]byte(`{"note_path":"Tasks/x.md","event":"notified"}`)))
	if err != nil {
		t.Fatalf("POST /runner-log: %v", err)
	}
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 when Server has no Vault", resp.StatusCode)
	}
}

func TestRunnerLogRejectsInvalidEvent(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	s := New(make(chan struct{}, 1), nil, v)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/runner-log", "application/json",
		bytes.NewReader([]byte(`{"note_path":"Tasks/x.md","event":"bogus"}`)))
	if err != nil {
		t.Fatalf("POST /runner-log: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for an invalid event", resp.StatusCode)
	}
}

// TestRunnerLogAppendsOutcomeToNote is the point of the whole endpoint --
// the watcher can't write this itself (see Design - Runner.md's
// with-vault-lock.sh gap), so this confirms the runner-side handler
// actually performs the write it exists to do, against a real note in the
// real throwaway test vault.
func TestRunnerLogAppendsOutcomeToNote(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/httpapi-runnerlog-%s.md", run)
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "done", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Runner-log endpoint test task.")

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	s := New(make(chan struct{}, 1), nil, v)
	ts := httptest.NewServer(s)
	defer ts.Close()

	body, _ := json.Marshal(map[string]string{"note_path": relPath, "event": "notify_failed"})
	resp, err := http.Post(ts.URL+"/runner-log", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("POST /runner-log: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	found := false
	for _, e := range note.RunnerLog {
		if e.Event == "notify_failed" {
			found = true
		}
	}
	if !found {
		t.Errorf("Runner Log missing \"notify_failed\" after POST /runner-log: %+v", note.RunnerLog)
	}
}
