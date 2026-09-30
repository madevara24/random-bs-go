package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/notify"
	"github.com/madevara24/random-bs-go/internal/runner"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// capturedDiscordAlert is what capturingDiscordServer parses out of one
// postDiscordAlert multipart request -- the message content and whether a
// file1 part was present, which is all newTerminalHandler's tests below
// need to assert on.
type capturedDiscordAlert struct {
	content string
	hasFile bool
}

// capturingDiscordServer stands in for the real Discord webhook: it parses
// the payload_json + fileN multipart shape postDiscordAlert sends, and
// signals gotAlert once one full request has been captured.
func capturingDiscordServer(t *testing.T) (*httptest.Server, <-chan capturedDiscordAlert) {
	t.Helper()
	alerts := make(chan capturedDiscordAlert, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var captured capturedDiscordAlert
		mr, err := r.MultipartReader()
		if err != nil {
			t.Errorf("parsing multipart request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		for {
			part, err := mr.NextPart()
			if err == io.EOF {
				break
			}
			if err != nil {
				t.Errorf("reading multipart part: %v", err)
				break
			}
			switch part.FormName() {
			case "payload_json":
				body, _ := io.ReadAll(part)
				var payload struct {
					Content string `json:"content"`
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Errorf("unmarshaling payload_json: %v", err)
				}
				captured.content = payload.Content
			case "file1":
				captured.hasFile = true
			}
		}
		w.WriteHeader(http.StatusOK)
		alerts <- captured
	}))
	return srv, alerts
}

func waitForAlert(t *testing.T, alerts <-chan capturedDiscordAlert) capturedDiscordAlert {
	t.Helper()
	select {
	case a := <-alerts:
		return a
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the Discord alert to fire")
		return capturedDiscordAlert{}
	}
}

// TestTerminalHandlerDone covers this task's core behavior: a `done`
// message must carry the PR link and drop the .md attachment entirely,
// with wording that depends on auto_merge -- manual-merge wording when
// false, auto-merge-loop wording when true, since OnTerminal fires before
// the merge gate runs and can't claim the PR already merged. It must also
// mention only Devara, not Ara-Dev -- a done task needs no help, so there's
// no reason for the `dev` Hermes gateway to auto-thread a reply to it.
func TestTerminalHandlerDone(t *testing.T) {
	const prURL = "https://github.com/example/repo/pull/42"

	for _, tc := range []struct {
		name       string
		autoMerge  bool
		wantSubstr string
	}{
		{name: "manual merge", autoMerge: false, wantSubstr: "manual merge"},
		{name: "auto merge running", autoMerge: true, wantSubstr: "auto-merge loop is running"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, alerts := capturingDiscordServer(t)
			defer srv.Close()

			cfg := &config.Config{DiscordUserID: "12345", DiscordAraDevUserID: "67890"}
			notifier := &notify.Notifier{WebhookURL: srv.URL}
			handler := newTerminalHandler(cfg, notifier)

			handler(worker.Job{Repo: "repo-a", Slug: "task-1"}, "done", "irrelevant work log", tc.autoMerge, prURL)

			got := waitForAlert(t, alerts)
			if got.hasFile {
				t.Errorf("done message attached a file, want none")
			}
			if !strings.Contains(got.content, prURL) {
				t.Errorf("message %q missing PR URL %q", got.content, prURL)
			}
			if !strings.Contains(got.content, tc.wantSubstr) {
				t.Errorf("message %q missing wording %q", got.content, tc.wantSubstr)
			}
			if !strings.Contains(got.content, "<@12345>") {
				t.Errorf("message %q missing Devara mention", got.content)
			}
			if strings.Contains(got.content, "<@67890>") {
				t.Errorf("message %q has Ara-Dev mention, want none for a done task", got.content)
			}
		})
	}
}

// TestTerminalHandlerBlockedAndFailedKeepAttachment confirms blocked/failed
// still get the .md work-log attachment -- only `done` drops it.
func TestTerminalHandlerBlockedAndFailedKeepAttachment(t *testing.T) {
	for _, status := range []string{"blocked", "failed"} {
		t.Run(status, func(t *testing.T) {
			srv, alerts := capturingDiscordServer(t)
			defer srv.Close()

			cfg := &config.Config{DiscordUserID: "12345", DiscordAraDevUserID: "67890"}
			notifier := &notify.Notifier{WebhookURL: srv.URL}
			handler := newTerminalHandler(cfg, notifier)

			handler(worker.Job{Repo: "repo-a", Slug: "task-1"}, status, "something went wrong", false, "")

			got := waitForAlert(t, alerts)
			if !got.hasFile {
				t.Errorf("%s message dropped the .md attachment, want it kept", status)
			}
			if !strings.Contains(got.content, "<@12345>") {
				t.Errorf("message %q missing Devara mention", got.content)
			}
			if !strings.Contains(got.content, "<@67890>") {
				t.Errorf("message %q missing Ara-Dev mention", got.content)
			}
		})
	}
}

// TestBuildWorkersWiresOnPanicAndOnError is this package's wiring test: for
// every configured repo, buildWorkers must hand back a RepoWorker whose
// OnPanic and OnError are both non-nil -- OnError already defaults to a
// non-nil stub in worker.New, but OnPanic didn't, until this task wired
// newPanicHandler in. A production regression here means a panic inside
// ProcessTask goes back to worker.New's log-only default: no blocked
// write, no Discord alert.
func TestBuildWorkersWiresOnPanicAndOnError(t *testing.T) {
	cfg := &config.Config{
		Repos: map[string]config.RepoConfig{
			"repo-a": {Path: "/tmp/repo-a", DefaultBranch: "main"},
			"repo-b": {Path: "/tmp/repo-b", DefaultBranch: "main"},
		},
	}

	globalSlots := worker.NewGlobalSlots(1)
	onPanic := func(worker.Job, any) {}
	workers := buildWorkers(cfg, globalSlots, runner.Deps{}, onPanic)

	if len(workers) != len(cfg.Repos) {
		t.Fatalf("got %d workers, want %d", len(workers), len(cfg.Repos))
	}
	for key, rw := range workers {
		if rw.OnPanic == nil {
			t.Errorf("worker %q: OnPanic is nil", key)
		}
		if rw.OnError == nil {
			t.Errorf("worker %q: OnError is nil", key)
		}
	}
}

// TestPanicHandlerWritesBlockedAndAlerts forces a panic in a stub Process
// function through a real RepoWorker and confirms newPanicHandler's
// callback -- the thing runRunner wires into RepoWorker.OnPanic -- writes
// status: blocked (with the recovered panic value in the Work Log) and
// fires the Discord alert, same as worker.New's previous log-only default
// silently failed to do.
func TestPanicHandlerWritesBlockedAndAlerts(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	discord, alerts := capturingDiscordServer(t)
	defer discord.Close()

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "onpanic-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "in_progress", Repo: "phase6-test-repo", Created: "2026-09-15",
	}, "OnPanic wiring test task -- Process below panics unconditionally.")

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("initial sync: %v", err)
	}
	// Deliberately no Vault on the notifier: Send's own async follow-up
	// write (appending "notified" to the Runner Log) goes through a raw,
	// unsynchronized git call in testvault's cleanup once this test returns,
	// and racing that fire-and-forget goroutine's git commit/push against
	// cleanup's git rm is exactly the kind of flake this test doesn't need
	// -- the blocked write below (the thing this test actually verifies)
	// happens synchronously, before Send is even called.
	notifier := &notify.Notifier{WebhookURL: discord.URL}

	globalSlots := worker.NewGlobalSlots(1)
	rw := worker.New("phase6-test-repo", globalSlots, func(job worker.Job) error {
		panic("boom: simulated ProcessTask panic")
	})
	rw.OnPanic = newPanicHandler(v, notifier, "12345", "67890")

	go rw.Run()
	rw.Enqueue(worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug})

	got := waitForAlert(t, alerts)
	if !strings.Contains(got.content, "<@12345>") {
		t.Errorf("message %q missing Devara mention", got.content)
	}
	if !strings.Contains(got.content, "<@67890>") {
		t.Errorf("message %q missing Ara-Dev mention", got.content)
	}

	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if note.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", note.Frontmatter.Status, "blocked")
	}
	if !note.HasWorkLog || !strings.Contains(note.WorkLog, "boom: simulated ProcessTask panic") {
		t.Errorf("Work Log missing the recovered panic value: %q", note.WorkLog)
	}
}
