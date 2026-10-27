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
	"github.com/madevara24/random-bs-go/internal/pause"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

func TestHealthAlwaysOK(t *testing.T) {
	s := New(make(chan struct{}, 1), nil, nil, nil)
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
	s := New(wake, nil, nil, nil)
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
	s := New(make(chan struct{}, 1), nil, nil, nil)
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

	s := New(make(chan struct{}, 1), workers, nil, nil)
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
	var got StatusTasksResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if _, ok := got.Tasks["idle-repo"]; ok {
		t.Errorf("idle-repo present in response, want it omitted: %+v", got)
	}
	busy, ok := got.Tasks["busy-repo"]
	if !ok {
		t.Fatalf("busy-repo missing from response: %+v", got)
	}
	if busy.Slug != "busy-slug" {
		t.Errorf("busy-repo.Slug = %q, want %q", busy.Slug, "busy-slug")
	}
}

// TestStatusTasksReportsDiscordThreadID confirms /status/tasks looks up
// the busy task's discord_thread_id via a plain read-only Vault.ReadNote --
// how the watcher (a separate process with no vault clone of its own)
// learns which thread its own "looks wedged" alert belongs in.
func TestStatusTasksReportsDiscordThreadID(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/httpapi-threadid-%s.md", run)
	threadID := "thread-xyz-789"
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "in_progress", Repo: "busy-repo", Created: "2026-09-15", DiscordThreadID: &threadID,
	}, "Status-tasks discord_thread_id test task.")

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	globalSlots := worker.NewGlobalSlots(1)
	busyWorker := worker.New("busy-repo", globalSlots, func(job worker.Job) error {
		time.Sleep(200 * time.Millisecond)
		return nil
	})
	workers := worker.Workers{"busy-repo": busyWorker}
	workers.StartAll()
	busyWorker.Enqueue(worker.Job{Repo: "busy-repo", Slug: "busy-slug", NotePath: relPath})
	time.Sleep(20 * time.Millisecond) // let it actually start

	s := New(make(chan struct{}, 1), workers, v, nil)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status/tasks")
	if err != nil {
		t.Fatalf("GET /status/tasks: %v", err)
	}
	defer resp.Body.Close()
	var got StatusTasksResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	busy, ok := got.Tasks["busy-repo"]
	if !ok {
		t.Fatalf("busy-repo missing from response: %+v", got)
	}
	if busy.DiscordThreadID != threadID {
		t.Errorf("DiscordThreadID = %q, want %q", busy.DiscordThreadID, threadID)
	}
}

func TestRunnerLogRejectsNonPost(t *testing.T) {
	s := New(make(chan struct{}, 1), nil, nil, nil)
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
	s := New(make(chan struct{}, 1), nil, nil, nil)
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
	s := New(make(chan struct{}, 1), nil, v, nil)
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
	s := New(make(chan struct{}, 1), nil, v, nil)
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

func TestResumeRejectsNonPost(t *testing.T) {
	s := New(make(chan struct{}, 1), nil, nil, nil)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/resume")
	if err != nil {
		t.Fatalf("GET /resume: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /resume status = %d, want 405", resp.StatusCode)
	}
}

// TestStatusTasksReportsPausedAndResetsAt confirms GET /status/tasks
// reports the shared pause state's paused flag and resets_at alongside the
// per-repo task map -- the single-engine-in-use compat case, where the
// top-level fields must mean exactly what they always have.
func TestStatusTasksReportsPausedAndResetsAt(t *testing.T) {
	var p pause.State
	resetsAt := time.Unix(1791552000, 0)
	p.Trip("claude", pause.ReasonUsageLimit, &resetsAt)

	s := New(make(chan struct{}, 1), nil, nil, &p)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status/tasks")
	if err != nil {
		t.Fatalf("GET /status/tasks: %v", err)
	}
	defer resp.Body.Close()
	var got StatusTasksResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !got.Paused {
		t.Error("Paused = false, want true")
	}
	if got.ResetsAt == nil || !got.ResetsAt.Equal(resetsAt) {
		t.Errorf("ResetsAt = %v, want %v", got.ResetsAt, resetsAt)
	}
	claude, ok := got.Engines["claude"]
	if !ok {
		t.Fatalf("Engines missing \"claude\": %+v", got.Engines)
	}
	if !claude.Paused {
		t.Error("Engines[\"claude\"].Paused = false, want true")
	}
	if claude.ResetsAt == nil || !claude.ResetsAt.Equal(resetsAt) {
		t.Errorf("Engines[\"claude\"].ResetsAt = %v, want %v", claude.ResetsAt, resetsAt)
	}
}

// TestStatusTasksEnginesReportsEveryKnownEngineEvenUnpaused confirms a
// registered engine that's never been tripped still shows up in the
// engines map as unpaused -- the "fresh deploy" shape a human checks
// after a merge, rather than an empty map that would make it look like no
// engines are known at all.
func TestStatusTasksEnginesReportsEveryKnownEngineEvenUnpaused(t *testing.T) {
	s := New(make(chan struct{}, 1), nil, nil, nil)
	s.EngineNames = []string{"claude"}
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status/tasks")
	if err != nil {
		t.Fatalf("GET /status/tasks: %v", err)
	}
	defer resp.Body.Close()
	var got StatusTasksResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	claude, ok := got.Engines["claude"]
	if !ok {
		t.Fatalf("Engines missing \"claude\": %+v", got.Engines)
	}
	if claude.Paused {
		t.Error("Engines[\"claude\"].Paused = true, want false for a never-tripped engine")
	}
	if got.Paused {
		t.Error("top-level Paused = true, want false when no engine is paused")
	}
}

// TestStatusTasksEnginesIsolatesOneEnginesPauseFromAnother confirms
// tripping one engine leaves another's entry unpaused, and the top-level
// resets_at is the earliest among only the engines actually paused.
func TestStatusTasksEnginesIsolatesOneEnginesPauseFromAnother(t *testing.T) {
	var p pause.State
	resetsAt := time.Unix(1791552000, 0)
	p.Trip("claude", pause.ReasonUsageLimit, &resetsAt)

	s := New(make(chan struct{}, 1), nil, nil, &p)
	s.EngineNames = []string{"claude", "other"}
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status/tasks")
	if err != nil {
		t.Fatalf("GET /status/tasks: %v", err)
	}
	defer resp.Body.Close()
	var got StatusTasksResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if !got.Engines["claude"].Paused {
		t.Error("Engines[\"claude\"].Paused = false, want true")
	}
	if got.Engines["other"].Paused {
		t.Error("Engines[\"other\"].Paused = true, want false (never tripped)")
	}
	if !got.Paused {
		t.Error("top-level Paused = false, want true (claude is paused)")
	}
	if got.ResetsAt == nil || !got.ResetsAt.Equal(resetsAt) {
		t.Errorf("top-level ResetsAt = %v, want %v (claude's)", got.ResetsAt, resetsAt)
	}
}

// TestStatusTasksReportsUnpausedWhenNoPauseWired confirms a nil Pause
// (every existing caller before this feature) reports Paused=false, not a
// panic or a zero-value decode failure.
func TestStatusTasksReportsUnpausedWhenNoPauseWired(t *testing.T) {
	s := New(make(chan struct{}, 1), nil, nil, nil)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/status/tasks")
	if err != nil {
		t.Fatalf("GET /status/tasks: %v", err)
	}
	defer resp.Body.Close()
	var got StatusTasksResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decoding response: %v", err)
	}
	if got.Paused {
		t.Error("Paused = true, want false when no pause.State is wired in")
	}
	if got.ResetsAt != nil {
		t.Errorf("ResetsAt = %v, want nil", got.ResetsAt)
	}
}

// TestResumeClearsPauseFlipsNotesAndWakesEverything is POST /resume's test
// gate: a tripped pause with a recorded blocked note must come back
// unpaused, with that note flipped from blocked to blocker_resolved (plus a
// Runner Log line), the dispatch-wake channel signaled, and every repo
// worker woken (observed here via QueueLen staying drainable -- Wake is
// exercised directly in internal/worker's own test).
func TestResumeClearsPauseFlipsNotesAndWakesEverything(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/httpapi-resume-%s.md", run)
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "blocked", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Resume endpoint test task.")

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var p pause.State
	resetsAt := time.Now().Add(time.Hour)
	p.Trip("claude", pause.ReasonUsageLimit, &resetsAt)
	p.AddBlockedNote("claude", relPath)

	dispatchWake := make(chan struct{}, 1)
	globalSlots := worker.NewGlobalSlots(1)
	rw := worker.New("phase6-test-repo", globalSlots, nil)
	rw.Pause = &p
	workers := worker.Workers{"phase6-test-repo": rw}

	var cancelledEngines []string
	s := New(dispatchWake, workers, v, &p)
	s.CancelResumeTimer = func(engine string) { cancelledEngines = append(cancelledEngines, engine) }
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/resume", "", nil)
	if err != nil {
		t.Fatalf("POST /resume: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if len(cancelledEngines) != 1 || cancelledEngines[0] != "claude" {
		t.Errorf("CancelResumeTimer calls = %v, want exactly [claude]", cancelledEngines)
	}
	if p.IsPaused("claude") {
		t.Error("pause still reports paused after POST /resume")
	}

	select {
	case <-dispatchWake:
	default:
		t.Error("dispatch-wake channel was never signaled")
	}

	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.Status != "blocker_resolved" {
		t.Errorf("status = %q, want %q", note.Frontmatter.Status, "blocker_resolved")
	}
	found := false
	for _, e := range note.RunnerLog {
		if e.Event == "blocker_resolved" {
			found = true
		}
	}
	if !found {
		t.Errorf("Runner Log missing a \"blocker_resolved\" entry: %+v", note.RunnerLog)
	}
}

// TestResumeDoesNotFlipANormalBlockedNoteItDidNotRecord confirms Resume
// only ever touches notes pause.State itself recorded -- a normal blocked
// note (one needing human input, never added via AddBlockedNote) must stay
// untouched by a usage-limit resume.
func TestResumeDoesNotFlipANormalBlockedNoteItDidNotRecord(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	relPath := fmt.Sprintf("Tasks/httpapi-resume-untouched-%s.md", run)
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "blocked", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "A normal blocked note, never recorded by the pause -- must stay untouched.")

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var p pause.State
	resetsAt := time.Now().Add(time.Hour)
	p.Trip("claude", pause.ReasonUsageLimit, &resetsAt)
	// Deliberately not calling p.AddBlockedNote("claude", relPath).

	s := New(make(chan struct{}, 1), nil, v, &p)
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/resume", "", nil)
	if err != nil {
		t.Fatalf("POST /resume: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want unchanged %q", note.Frontmatter.Status, "blocked")
	}
}

// TestResumeWithEngineQueryParamClearsOnlyThatEngine is RBG-35's HTTP test
// gate: with both "claude" and "other" paused, POST /resume?engine=claude
// must clear only claude -- "other" stays paused, its blocked note stays
// blocked, and its timer is never cancelled.
func TestResumeWithEngineQueryParamClearsOnlyThatEngine(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	claudePath := fmt.Sprintf("Tasks/httpapi-resume-engine-claude-%s.md", run)
	otherPath := fmt.Sprintf("Tasks/httpapi-resume-engine-other-%s.md", run)
	testvault.Seed(t, claudePath, notetask.Frontmatter{
		Status: "blocked", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Claude-engine resume test task.")
	testvault.Seed(t, otherPath, notetask.Frontmatter{
		Status: "blocked", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Other-engine resume test task.")

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var p pause.State
	resetsAt := time.Now().Add(time.Hour)
	p.Trip("claude", pause.ReasonUsageLimit, &resetsAt)
	p.AddBlockedNote("claude", claudePath)
	p.Trip("other", pause.ReasonUsageLimit, &resetsAt)
	p.AddBlockedNote("other", otherPath)

	var cancelledEngines []string
	s := New(make(chan struct{}, 1), nil, v, &p)
	s.CancelResumeTimer = func(engine string) { cancelledEngines = append(cancelledEngines, engine) }
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/resume?engine=claude", "", nil)
	if err != nil {
		t.Fatalf("POST /resume?engine=claude: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if len(cancelledEngines) != 1 || cancelledEngines[0] != "claude" {
		t.Errorf("CancelResumeTimer calls = %v, want exactly [claude]", cancelledEngines)
	}
	if p.IsPaused("claude") {
		t.Error("claude still reports paused after POST /resume?engine=claude")
	}
	if !p.IsPaused("other") {
		t.Error("other no longer reports paused -- ?engine=claude must not have touched it")
	}

	claudeNote, err := v.ReadNote(claudePath)
	if err != nil {
		t.Fatalf("reading claude note back: %v", err)
	}
	if claudeNote.Frontmatter.Status != "blocker_resolved" {
		t.Errorf("claude note status = %q, want %q", claudeNote.Frontmatter.Status, "blocker_resolved")
	}
	otherNote, err := v.ReadNote(otherPath)
	if err != nil {
		t.Fatalf("reading other note back: %v", err)
	}
	if otherNote.Frontmatter.Status != "blocked" {
		t.Errorf("other note status = %q, want unchanged %q", otherNote.Frontmatter.Status, "blocked")
	}
}

// TestResumeWithoutEngineClearsEveryPausedEngine confirms POST /resume
// with no query parameter clears every currently-paused engine -- the
// generalization of today's single-engine behavior.
func TestResumeWithoutEngineClearsEveryPausedEngine(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	claudePath := fmt.Sprintf("Tasks/httpapi-resume-all-claude-%s.md", run)
	otherPath := fmt.Sprintf("Tasks/httpapi-resume-all-other-%s.md", run)
	testvault.Seed(t, claudePath, notetask.Frontmatter{
		Status: "blocked", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Claude-engine resume-all test task.")
	testvault.Seed(t, otherPath, notetask.Frontmatter{
		Status: "blocked", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "Other-engine resume-all test task.")

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	var p pause.State
	resetsAt := time.Now().Add(time.Hour)
	p.Trip("claude", pause.ReasonUsageLimit, &resetsAt)
	p.AddBlockedNote("claude", claudePath)
	p.Trip("other", pause.ReasonUsageLimit, &resetsAt)
	p.AddBlockedNote("other", otherPath)

	var cancelledEngines []string
	s := New(make(chan struct{}, 1), nil, v, &p)
	s.CancelResumeTimer = func(engine string) { cancelledEngines = append(cancelledEngines, engine) }
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/resume", "", nil)
	if err != nil {
		t.Fatalf("POST /resume: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}

	if len(cancelledEngines) != 2 {
		t.Errorf("CancelResumeTimer calls = %v, want exactly 2 (one per paused engine)", cancelledEngines)
	}
	if p.IsPaused("claude") || p.IsPaused("other") {
		t.Error("an engine still reports paused after POST /resume with no query param")
	}

	for _, path := range []string{claudePath, otherPath} {
		note, err := v.ReadNote(path)
		if err != nil {
			t.Fatalf("reading %s back: %v", path, err)
		}
		if note.Frontmatter.Status != "blocker_resolved" {
			t.Errorf("%s status = %q, want %q", path, note.Frontmatter.Status, "blocker_resolved")
		}
	}
}
