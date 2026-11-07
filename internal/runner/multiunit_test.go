package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/jev"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// fakeCodingEngine simulates "today's single coding run" for triage-path
// integration tests: BuildCommand writes a clean done+pr_url copy, so
// ProcessTask's merge-back finishes without error -- letting these tests
// assert purely on the triage outcome (multi_unit/Runner Log/Jev request
// count) without a real claude invocation.
type fakeCodingEngine struct {
	copyName string
	calls    atomic.Int32
}

func (f *fakeCodingEngine) Name() string { return "fake-coding" }

func (f *fakeCodingEngine) BuildCommand(dir, prompt, resumeSessionID string) (*exec.Cmd, string) {
	f.calls.Add(1)
	copyPath := filepath.Join(dir, f.copyName)
	if data, err := os.ReadFile(copyPath); err == nil {
		if note, err := notetask.Parse(data); err == nil {
			note.Frontmatter.Status = "done"
			prURL := "fake://pr/1"
			note.Frontmatter.PRURL = &prURL
			if out, err := note.Bytes(); err == nil {
				_ = os.WriteFile(copyPath, out, 0o644)
			}
		}
	}
	return exec.Command("true"), ""
}

func (f *fakeCodingEngine) ParseLine(line string, state *engine.RunState) {}

func (f *fakeCodingEngine) ClassifyExit(waitErr error, stderrTail string, state engine.RunState) error {
	return waitErr
}

func (f *fakeCodingEngine) BuildReviewCommand(dir, prompt string) *exec.Cmd { return exec.Command("true") }

func (f *fakeCodingEngine) ParseReviewAnswer(stdout []byte) (string, error) { return "", nil }

func (f *fakeCodingEngine) StartupTimeout() time.Duration { return 0 }

// fakePlannerEngine simulates a planner session: BuildCommand reads the
// note-copy's own plan_round (0 means round 1) and writes planBody(round)
// into its "## Plan" section, then sets status: done -- the exact contract
// buildPlannerPrompt asks a real planner engine to follow.
type fakePlannerEngine struct {
	copyName string
	planBody func(round int) string
	calls    atomic.Int32
}

func (f *fakePlannerEngine) Name() string { return "fake-planner" }

func (f *fakePlannerEngine) BuildCommand(dir, prompt, resumeSessionID string) (*exec.Cmd, string) {
	f.calls.Add(1)
	copyPath := filepath.Join(dir, f.copyName)
	if data, err := os.ReadFile(copyPath); err == nil {
		if note, err := notetask.Parse(data); err == nil {
			round := note.Frontmatter.PlanRound
			if round == 0 {
				round = 1
			}
			note.HasPlan = true
			note.Plan = f.planBody(round)
			note.Frontmatter.Status = "done"
			if out, err := note.Bytes(); err == nil {
				_ = os.WriteFile(copyPath, out, 0o644)
			}
		}
	}
	return exec.Command("true"), ""
}

func (f *fakePlannerEngine) ParseLine(line string, state *engine.RunState) {}

func (f *fakePlannerEngine) ClassifyExit(waitErr error, stderrTail string, state engine.RunState) error {
	return waitErr
}

func (f *fakePlannerEngine) BuildReviewCommand(dir, prompt string) *exec.Cmd { return exec.Command("true") }

// ParseReviewAnswer always approves -- fakePlannerEngine is reused as the
// unit loop's coding/review engine (via processMultiUnit's hook-in,
// RBG-45) whenever a test's registry only registers one engine under
// "claude", since that's every one of coding/review/plan's default
// resolution. A planner-shaped fake has no way to make real commits per
// round, so an always-CONCERNS answer would never let a unit's review
// resolve -- always-APPROVE instead lets these tests reach a clean
// terminal state without needing a second, dedicated fake per test.
func (f *fakePlannerEngine) ParseReviewAnswer(stdout []byte) (string, error) {
	return "VERDICT: APPROVE\n\nfake planner engine always approves.", nil
}

func (f *fakePlannerEngine) StartupTimeout() time.Duration { return 0 }

// fakeFailingPlannerEngine simulates a planner session whose invocation
// itself exits non-zero -- it never touches the note-copy, so
// runPlanner's own round-retry loop (an unusable plan, same as a parse
// failure) is what eventually blocks the task, exactly as it does today;
// RBG-49's requirement is only that it still must, never leaving the note
// at in_progress.
type fakeFailingPlannerEngine struct {
	calls atomic.Int32
}

func (f *fakeFailingPlannerEngine) Name() string { return "fake-planner-crash" }

func (f *fakeFailingPlannerEngine) BuildCommand(dir, prompt, resumeSessionID string) (*exec.Cmd, string) {
	f.calls.Add(1)
	return exec.Command("false"), ""
}

func (f *fakeFailingPlannerEngine) ParseLine(line string, state *engine.RunState) {}

func (f *fakeFailingPlannerEngine) ClassifyExit(waitErr error, stderrTail string, state engine.RunState) error {
	return waitErr
}

func (f *fakeFailingPlannerEngine) BuildReviewCommand(dir, prompt string) *exec.Cmd { return exec.Command("true") }

func (f *fakeFailingPlannerEngine) ParseReviewAnswer(stdout []byte) (string, error) { return "", nil }

func (f *fakeFailingPlannerEngine) StartupTimeout() time.Duration { return 0 }

// newJevServer starts an httptest server that always answers Jev requests
// with responseJSON, regardless of which questions were actually asked --
// a single generous canned answer set (every id either test needs) covers
// both a triage call and a gate call from the same server. Returns the hit
// counter so callers can assert on how many requests actually landed.
func newJevServer(t *testing.T, responseJSON string) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(responseJSON))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

func testRepoDeps() map[string]config.RepoConfig {
	return map[string]config.RepoConfig{
		"phase6-test-repo": {Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"},
	}
}

func cleanupTaskBranch(t *testing.T, slug string) {
	t.Helper()
	branchName := "task/" + slug
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "checkout", "main").Run()
		exec.Command("git", "-C", testTargetRepoPath, "branch", "-D", branchName).Run()
	})
}

// TestJevErrorLogIsOneLine confirms a Jev error whose text embeds a
// multi-line HTTP response body (jev.Client.Ask wraps the raw response
// verbatim on a non-2xx, non-retryable status) still comes out of
// triagePath as a single Runner Log line of at most 200 runes -- and that
// appending that line to a note and re-parsing it still works, since
// notetask.Parse hard-fails on a multi-line Runner Log entry.
func TestJevErrorLogIsOneLine(t *testing.T) {
	multiLineBody := "line one of the error\nline two of the error\nline three, which keeps going and going and going and going and going and going and going and going and going and going until it is well past two hundred runes all on its own, easily."
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(multiLineBody))
	}))
	defer srv.Close()

	deps := Deps{
		MultiUnitEnabled: true,
		Jev:              &jev.Client{BaseURL: srv.URL, APIKey: "test-key"},
	}
	note := &notetask.Note{
		Frontmatter: notetask.Frontmatter{Status: "queued", Repo: "some-repo", Created: "2026-11-06"},
		Prompt:      "Do the thing.",
	}
	job := worker.Job{NotePath: "Tasks/some-task.md", Repo: "some-repo", Slug: "some-task"}

	multiUnit, logLine := triagePath(deps, job, note)
	if multiUnit {
		t.Error("want multiUnit=false on a Jev error")
	}
	if strings.ContainsAny(logLine, "\n\r\t") {
		t.Errorf("logLine contains a raw newline/tab: %q", logLine)
	}
	if r := []rune(logLine); len(r) > 200 {
		t.Errorf("logLine is %d runes, want at most 200: %q", len(r), logLine)
	}

	notetask.AppendRunnerLog(note, logLine, time.Now())
	out, err := note.Bytes()
	if err != nil {
		t.Fatalf("note.Bytes: %v", err)
	}
	if _, err := notetask.Parse(out); err != nil {
		t.Fatalf("note no longer parses after appending the triage log line: %v\n%s", err, out)
	}
}

// TestFlagOffIgnoresMultiUnit confirms that with MULTI_UNIT_ENABLED off,
// a note's own multi_unit: true is ignored entirely -- today's single
// coding run happens and zero Jev requests are made.
func TestFlagOffIgnoresMultiUnit(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "flagoff-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	multiUnitTrue := true
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-11-06",
		MultiUnit: &multiUnitTrue,
	}, "Trivial task.")

	jevSrv, jevHits := newJevServer(t, `{"answers":{}}`)
	codingEngine := &fakeCodingEngine{copyName: copyFileName(slug)}

	deps := Deps{
		Vault:            v,
		Repos:            testRepoDeps(),
		EngineRegistry:   map[string]engine.Engine{"claude": codingEngine},
		MultiUnitEnabled: false,
		Jev:              &jev.Client{BaseURL: jevSrv.URL, APIKey: "test-key"},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	cleanupTaskBranch(t, slug)

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	if codingEngine.calls.Load() == 0 {
		t.Error("coding engine never ran -- want today's single coding run even with multi_unit: true in the note")
	}
	if got := jevHits.Load(); got != 0 {
		t.Errorf("Jev was hit %d times, want 0 with the flag off", got)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "done" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "done")
	}
}

// TestTriageLargeGoesMultiUnit confirms a "size" noul of 0.8 flips the note
// to multi_unit: true and routes into the planner.
func TestTriageLargeGoesMultiUnit(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "triagelarge-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-11-06",
	}, "Trivial task.")

	jevSrv, _ := newJevServer(t, `{"answers":{"size":{"noul":0.8},"risky":{"noul":0.1},"ambiguous":{"noul":0.1}}}`)

	planner := &fakePlannerEngine{
		copyName: copyFileName(slug),
		planBody: func(round int) string {
			return "Two units.\n\n```yaml\n" +
				"- id: U1\n  title: Do part one\n  paths: [internal/foo.go]\n  verify: \"true\"\n" +
				"- id: U2\n  title: Do part two\n  paths: [internal/bar.go]\n  depends_on: [U1]\n  verify: \"true\"\n" +
				"```\n"
		},
	}

	deps := Deps{
		Vault:            v,
		Repos:            testRepoDeps(),
		EngineRegistry:   map[string]engine.Engine{"claude": planner},
		MultiUnitEnabled: true,
		Jev:              &jev.Client{BaseURL: jevSrv.URL, APIKey: "test-key"},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	branchName := "task/" + slug
	cleanupTaskBranch(t, slug)
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
	})

	// A clear gate pass seeds units and runs the unit loop to completion
	// (fakePlannerEngine always approves its own review, verify is "true"),
	// which also reaches this task's finish step (RBG-46): a fake `gh`
	// stands in for the real GitHub-hosted remote testTargetRepoPath
	// doesn't have.
	binDir := t.TempDir()
	writeFakeGhPRCreate(t, filepath.Join(binDir, "gh"), filepath.Join(binDir, "gh-calls.log"), "https://github.com/fake-owner/fake-repo/pull/1")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	if planner.calls.Load() == 0 {
		t.Error("planner engine never ran")
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.MultiUnit == nil || !*noteAfter.Frontmatter.MultiUnit {
		t.Errorf("multi_unit = %v, want true", noteAfter.Frontmatter.MultiUnit)
	}
	if !noteAfter.HasPlan || strings.TrimSpace(noteAfter.Plan) == "" {
		t.Error("Plan section missing after the planner ran")
	}
}

// TestTriageJevDownStaysSingle confirms a Jev 500 fails triage safe to
// single-unit: multi_unit ends up false, today's coding run happens, and
// the Runner Log records the error.
func TestTriageJevDownStaysSingle(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "jevdown-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-11-06",
	}, "Trivial task.")

	var hits atomic.Int32
	jevSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer jevSrv.Close()

	codingEngine := &fakeCodingEngine{copyName: copyFileName(slug)}

	deps := Deps{
		Vault:            v,
		Repos:            testRepoDeps(),
		EngineRegistry:   map[string]engine.Engine{"claude": codingEngine},
		MultiUnitEnabled: true,
		Jev:              &jev.Client{BaseURL: jevSrv.URL, APIKey: "test-key"},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	cleanupTaskBranch(t, slug)

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	if got := hits.Load(); got == 0 {
		t.Error("Jev was never hit")
	}
	if codingEngine.calls.Load() == 0 {
		t.Error("coding engine never ran")
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.MultiUnit == nil || *noteAfter.Frontmatter.MultiUnit {
		t.Errorf("multi_unit = %v, want false", noteAfter.Frontmatter.MultiUnit)
	}
	found := false
	for _, e := range noteAfter.RunnerLog {
		if strings.Contains(e.Event, "size=error:") {
			found = true
		}
	}
	if !found {
		t.Errorf("Runner Log missing a %q entry: %+v", "size=error:", noteAfter.RunnerLog)
	}
}

// TestTriageOverrideSkipsJev confirms an author-set multi_unit: false
// skips Jev entirely -- zero requests.
func TestTriageOverrideSkipsJev(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "override-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	multiUnitFalse := false
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-11-06",
		MultiUnit: &multiUnitFalse,
	}, "Trivial task.")

	jevSrv, jevHits := newJevServer(t, `{"answers":{}}`)
	codingEngine := &fakeCodingEngine{copyName: copyFileName(slug)}

	deps := Deps{
		Vault:            v,
		Repos:            testRepoDeps(),
		EngineRegistry:   map[string]engine.Engine{"claude": codingEngine},
		MultiUnitEnabled: true,
		Jev:              &jev.Client{BaseURL: jevSrv.URL, APIKey: "test-key"},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	cleanupTaskBranch(t, slug)

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	if got := jevHits.Load(); got != 0 {
		t.Errorf("Jev was hit %d times, want 0 with an author override", got)
	}
	if codingEngine.calls.Load() == 0 {
		t.Error("coding engine never ran")
	}
}

// TestResumeNeverRetriages confirms a resumed task (session_id already
// set) never re-triages, even with multi_unit: true in the note.
func TestResumeNeverRetriages(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "resume-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	multiUnitTrue := true
	sessionID := "prior-session-abc"
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "in_progress", Repo: "phase6-test-repo", Created: "2026-11-06",
		SessionID: &sessionID, MultiUnit: &multiUnitTrue,
	}, "Trivial task.")

	jevSrv, jevHits := newJevServer(t, `{"answers":{}}`)

	deps := Deps{
		Vault:            v,
		Repos:            testRepoDeps(),
		MultiUnitEnabled: true,
		Jev:              &jev.Client{BaseURL: jevSrv.URL, APIKey: "test-key"},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	// A resumed run with no preserved branch anywhere (local or on
	// origin) fails setup at checkoutTaskBranch -- expected, not the point
	// of this test, which only cares that triage never ran before that.
	_ = ProcessTask(deps, noopReporter{}, job)

	if got := jevHits.Load(); got != 0 {
		t.Errorf("Jev was hit %d times on a resume, want 0", got)
	}
}

// writeFakeGhPRCreate writes an executable fake `gh` script that answers
// `gh pr create` by appending one static "call" line to logPath (a plain
// invocation counter -- the real --body argument can itself contain
// embedded newlines, which would otherwise corrupt a line-count check) and
// echoing prURL (gh pr create's own real behavior: print the new PR's URL,
// and nothing else, to stdout) -- any other invocation fails loudly so a
// test using this notices if it ever calls gh for anything else.
func writeFakeGhPRCreate(t *testing.T, path, logPath, prURL string) {
	t.Helper()
	script := fmt.Sprintf(`#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "create" ]; then
  echo call >> %q
  echo %q
  exit 0
fi
echo "fake gh: unexpected invocation: $*" >&2
exit 1
`, logPath, prURL)
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake gh script: %v", err)
	}
}

// writeFakeGhPRCreateFails is writeFakeGhPRCreate's failure counterpart --
// `gh pr create` exits 1 (TestOpenPRFailureBlocks' subject), any other
// invocation also fails loudly.
func writeFakeGhPRCreateFails(t *testing.T, path string) {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "pr" ] && [ "$2" = "create" ]; then
  echo "fake gh: pr create deliberately fails" >&2
  exit 1
fi
echo "fake gh: unexpected invocation: $*" >&2
exit 1
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing fake gh script: %v", err)
	}
}

// TestMultiUnitDoneOpensOnePR confirms that once every unit in a
// multi-unit task reaches done, runUnitLoopAndFinish opens exactly one PR
// (git push then `gh pr create`, see multiunit.go's openTaskPR) and the
// vault note ends status: done with that PR's URL.
func TestMultiUnitDoneOpensOnePR(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "multiunit-donepr-" + run

	units := []notetask.UnitState{
		{ID: "U1", Status: notetask.UnitStatusPending},
		{ID: "U2", Status: notetask.UnitStatusPending},
	}
	v, relPath := seedUnitLoopNote(t, slug, twoUnitPlan, units, "U1")
	branchName, _ := setupUnitLoopBranch(t, slug)
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
	})

	binDir := t.TempDir()
	logPath := filepath.Join(binDir, "gh-calls.log")
	fakePRURL := "https://github.com/fake-owner/fake-repo/pull/99"
	writeFakeGhPRCreate(t, filepath.Join(binDir, "gh"), logPath, fakePRURL)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	codingEng := &fakeUnitCodingEngine{copyName: copyFileName(slug)}
	reviewEng := &fakeUnitReviewEngine{verdicts: []string{"VERDICT: APPROVE\n\nfine."}}

	deps := Deps{Vault: v, Repos: map[string]config.RepoConfig{unitLoopTestRepoKey: unitLoopRepoCfg()}}
	job := worker.Job{NotePath: relPath, Repo: unitLoopTestRepoKey, Slug: slug}

	if err := runUnitLoopAndFinish(deps, noopReporter{}, job, unitLoopRepoCfg(), branchName, codingEng, reviewEng); err != nil {
		t.Fatalf("runUnitLoopAndFinish: %v", err)
	}

	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("reading gh call log: %v", err)
	}
	calls := strings.Split(strings.TrimSpace(string(logData)), "\n")
	if len(calls) != 1 {
		t.Errorf("gh pr create called %d time(s), want exactly 1: %q", len(calls), logData)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "done" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "done")
	}
	if noteAfter.Frontmatter.PRURL == nil || *noteAfter.Frontmatter.PRURL != fakePRURL {
		t.Errorf("pr_url = %v, want %q", noteAfter.Frontmatter.PRURL, fakePRURL)
	}
}

// TestOpenPRFailureBlocks confirms that a `gh pr create` failure after
// every unit is done blocks the task (not a crash scenario of any other
// kind), fires the usual alert, and leaves the branch preserved -- the
// git push openTaskPR does before gh pr create already landed the unit
// loop's work on origin, so there's nothing left to lose.
func TestOpenPRFailureBlocks(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "multiunit-prfail-" + run

	units := []notetask.UnitState{{ID: "U1", Status: notetask.UnitStatusPending}}
	v, relPath := seedUnitLoopNote(t, slug, oneUnitPlan, units, "U1")
	branchName, _ := setupUnitLoopBranch(t, slug)
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
	})

	binDir := t.TempDir()
	writeFakeGhPRCreateFails(t, filepath.Join(binDir, "gh"))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	codingEng := &fakeUnitCodingEngine{copyName: copyFileName(slug)}
	reviewEng := &fakeUnitReviewEngine{verdicts: []string{"VERDICT: APPROVE\n\nfine."}}

	var gotAlert *AlertPayload
	deps := Deps{
		Vault: v,
		Repos: map[string]config.RepoConfig{unitLoopTestRepoKey: unitLoopRepoCfg()},
		OnBlocked: func(j worker.Job, payload AlertPayload) {
			gotAlert = &payload
		},
	}
	job := worker.Job{NotePath: relPath, Repo: unitLoopTestRepoKey, Slug: slug}

	err := runUnitLoopAndFinish(deps, noopReporter{}, job, unitLoopRepoCfg(), branchName, codingEng, reviewEng)
	if err == nil {
		t.Fatal("runUnitLoopAndFinish returned nil error, want an error when gh pr create fails")
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "blocked")
	}

	if gotAlert == nil {
		t.Fatal("OnBlocked was never called")
	}
	if gotAlert.Scenario != ScenarioOpenPRFailure {
		t.Errorf("alert scenario = %v, want %v", gotAlert.Scenario, ScenarioOpenPRFailure)
	}
	if gotAlert.PreserveErr != "" {
		t.Errorf("PreserveErr = %q, want the branch to have been preserved cleanly", gotAlert.PreserveErr)
	}

	if err := exec.Command("git", "-C", testTargetRepoPath, "fetch", "origin", branchName).Run(); err != nil {
		t.Errorf("branch %s not preserved on origin: %v", branchName, err)
	}
}
