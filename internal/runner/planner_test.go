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

	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/jev"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/pause"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// cleanPlanBody is a valid two-unit plan: no hard-rule-triggering paths,
// well under the 8-unit cap, and every unit has a non-empty id/title/verify.
func cleanPlanBody(round int) string {
	return "Two units.\n\n```yaml\n" +
		"- id: U1\n  title: Do part one\n  paths: [internal/foo.go]\n  verify: \"true\"\n" +
		"- id: U2\n  title: Do part two\n  paths: [internal/bar.go]\n  depends_on: [U1]\n  verify: \"true\"\n" +
		"```\n"
}

// invalidPlanBody never parses as a plan units list, on any round -- used
// to drive runPlanner through all 3 rounds into blocked.
func invalidPlanBody(round int) string {
	return "This round's plan is bogus.\n\n```yaml\nnot a list of units\n```\n"
}

// discordThreadServer starts an httptest server standing in for a forum
// webhook's PostToThread target, always answering with the given message
// id.
func discordThreadServer(t *testing.T, messageID string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"id":%q}`, messageID)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestPlannerCleanPlanSeedsUnits confirms a plan that clears the gate
// (clear low risky/ambiguous, no hard-rule hits) seeds `units` in plan
// order and, per RBG-45's hook-in, hands straight into the unit loop
// rather than stopping there -- with fakePlannerEngine standing in for
// both the coding and review engine (always approving, see its own doc
// comment), both units run to completion in this same ProcessTask call.
func TestPlannerCleanPlanSeedsUnits(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "plannerclean-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	multiUnitTrue := true
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-11-06",
		MultiUnit: &multiUnitTrue,
	}, "Trivial multi-unit task.")

	jevSrv, _ := newJevServer(t, `{"answers":{"risky":{"noul":0.1},"ambiguous":{"noul":0.1}}}`)
	planner := &fakePlannerEngine{copyName: copyFileName(slug), planBody: cleanPlanBody}

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

	// Every unit reaching done also reaches this task's finish step (RBG-46):
	// it opens the task's one PR for real (git push + `gh pr create`), so a
	// fake `gh` stands in for the real GitHub-hosted remote
	// testTargetRepoPath doesn't have.
	binDir := t.TempDir()
	fakePRURL := "https://github.com/fake-owner/fake-repo/pull/1"
	writeFakeGhPRCreate(t, filepath.Join(binDir, "gh"), filepath.Join(binDir, "gh-calls.log"), fakePRURL)
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if len(noteAfter.Frontmatter.Units) != 2 {
		t.Fatalf("units = %+v, want 2 entries", noteAfter.Frontmatter.Units)
	}
	for _, u := range noteAfter.Frontmatter.Units {
		if u.Status != notetask.UnitStatusDone {
			t.Errorf("unit %q status = %q, want %q (the unit loop should have run both to completion)", u.ID, u.Status, notetask.UnitStatusDone)
		}
	}
	if noteAfter.Frontmatter.Units[0].ID != "U1" || noteAfter.Frontmatter.Units[1].ID != "U2" {
		t.Errorf("units = %+v, want order U1, U2", noteAfter.Frontmatter.Units)
	}
	if noteAfter.Frontmatter.CurrentUnit != nil {
		t.Errorf("current_unit = %v, want nil once every unit is done", *noteAfter.Frontmatter.CurrentUnit)
	}
	if noteAfter.Frontmatter.Status != "done" {
		t.Errorf("status = %q, want %q (the finish step should have opened the PR)", noteAfter.Frontmatter.Status, "done")
	}
	if noteAfter.Frontmatter.PRURL == nil || *noteAfter.Frontmatter.PRURL != fakePRURL {
		t.Errorf("pr_url = %v, want %q", noteAfter.Frontmatter.PRURL, fakePRURL)
	}
}

// TestPlannerRiskyPlanStops confirms a clear "risky" answer stops the
// gate: status flips to plan_review, plan_message_id is recorded from the
// (fake) webhook's response, Plan Review ends with "### Round 1", and the
// Runner Log carries the plan_gate line.
func TestPlannerRiskyPlanStops(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "plannerrisky-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	multiUnitTrue := true
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-11-06",
		MultiUnit: &multiUnitTrue,
	}, "Trivial multi-unit task.")

	jevSrv, _ := newJevServer(t, `{"answers":{"risky":{"noul":0.9},"ambiguous":{"noul":0.1}}}`)
	discordSrv := discordThreadServer(t, "42")
	planner := &fakePlannerEngine{copyName: copyFileName(slug), planBody: cleanPlanBody}

	deps := Deps{
		Vault:            v,
		Repos:            testRepoDeps(),
		EngineRegistry:   map[string]engine.Engine{"claude": planner},
		MultiUnitEnabled: true,
		Jev:              &jev.Client{BaseURL: jevSrv.URL, APIKey: "test-key"},
		ForumWebhookURL:  discordSrv.URL,
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	cleanupTaskBranch(t, slug)

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "plan_review" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "plan_review")
	}
	if noteAfter.Frontmatter.PlanMessageID == nil || *noteAfter.Frontmatter.PlanMessageID != "42" {
		t.Errorf("plan_message_id = %v, want %q", noteAfter.Frontmatter.PlanMessageID, "42")
	}
	if !strings.HasSuffix(strings.TrimSpace(noteAfter.PlanReview), "### Round 1") {
		t.Errorf("Plan Review = %q, want it to end with %q", noteAfter.PlanReview, "### Round 1")
	}
	foundGateLine := false
	for _, e := range noteAfter.RunnerLog {
		if strings.HasPrefix(e.Event, "plan_gate") {
			foundGateLine = true
		}
	}
	if !foundGateLine {
		t.Errorf("Runner Log missing a plan_gate line: %+v", noteAfter.RunnerLog)
	}
}

// TestPlannerNoJevKeyStops confirms an empty TypeSafe key makes the gate
// always stop, same as any other Jev-unreachable case.
func TestPlannerNoJevKeyStops(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "plannernokey-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	multiUnitTrue := true
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-11-06",
		MultiUnit: &multiUnitTrue,
	}, "Trivial multi-unit task.")

	planner := &fakePlannerEngine{copyName: copyFileName(slug), planBody: cleanPlanBody}

	deps := Deps{
		Vault:            v,
		Repos:            testRepoDeps(),
		EngineRegistry:   map[string]engine.Engine{"claude": planner},
		MultiUnitEnabled: true,
		Jev:              &jev.Client{APIKey: ""},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	cleanupTaskBranch(t, slug)

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "plan_review" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "plan_review")
	}
}

// TestPlannerBadYAMLBlocksAfter3 confirms a planner that never produces a
// parseable plan exhausts all 3 rounds and ends up blocked, with the
// alert naming the parse error.
func TestPlannerBadYAMLBlocksAfter3(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "plannerbadyaml-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	multiUnitTrue := true
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-11-06",
		MultiUnit: &multiUnitTrue,
	}, "Trivial multi-unit task.")

	planner := &fakePlannerEngine{copyName: copyFileName(slug), planBody: invalidPlanBody}

	var blockedPayload *AlertPayload
	deps := Deps{
		Vault:            v,
		Repos:            testRepoDeps(),
		EngineRegistry:   map[string]engine.Engine{"claude": planner},
		MultiUnitEnabled: true,
		Jev:              &jev.Client{APIKey: ""},
		OnBlocked: func(job worker.Job, payload AlertPayload) {
			blockedPayload = &payload
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	cleanupTaskBranch(t, slug)

	if err := ProcessTask(deps, noopReporter{}, job); err == nil {
		t.Fatal("ProcessTask returned nil error, want an error after exhausting the plan rounds")
	}

	if got := planner.calls.Load(); got != 3 {
		t.Errorf("planner engine ran %d times, want exactly 3", got)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "blocked")
	}

	if blockedPayload == nil {
		t.Fatal("OnBlocked was never called")
	}
	if !strings.Contains(blockedPayload.ExitErrText, "unusable plan") {
		t.Errorf("alert ExitErrText = %q, want it to name the parse error", blockedPayload.ExitErrText)
	}
}

// fakeUsageLimitPlannerEngine always reports a usage limit, regardless of
// how its underlying process actually exits -- a minimal fake for driving
// runPlanner straight into the usage-limit branch without needing a real
// claude invocation or stream-json fixture.
type fakeUsageLimitPlannerEngine struct {
	calls atomic.Int32
}

func (f *fakeUsageLimitPlannerEngine) Name() string { return "fake-planner-usagelimit" }

func (f *fakeUsageLimitPlannerEngine) BuildCommand(dir, prompt, resumeSessionID string) (*exec.Cmd, string) {
	f.calls.Add(1)
	return exec.Command("true"), ""
}

func (f *fakeUsageLimitPlannerEngine) ParseLine(line string, state *engine.RunState) {}

func (f *fakeUsageLimitPlannerEngine) ClassifyExit(waitErr error, stderrTail string, state engine.RunState) error {
	return &engine.UsageLimitError{}
}

func (f *fakeUsageLimitPlannerEngine) BuildReviewCommand(dir, prompt string) *exec.Cmd { return exec.Command("true") }

func (f *fakeUsageLimitPlannerEngine) ParseReviewAnswer(stdout []byte) (string, error) { return "", nil }

func (f *fakeUsageLimitPlannerEngine) StartupTimeout() time.Duration { return 0 }

// TestPlannerUsageLimitBlocksForAutoResume is RBG-45's fix for runPlanner's
// usage-limit handling: hitting the limit must stop the round loop right
// there (plan_round never increments, since the engine never got a chance
// to write a plan at all) and block through the same path a crashed
// coding invocation would -- which also registers the note for main.go's
// auto-resume once the limit resets. Before the fix, this fell through to
// the plan-parse check, burned a round, and immediately re-ran the
// (still-paused) planner, reaching blocked after 3 instant rounds instead
// of waiting for the reset.
func TestPlannerUsageLimitBlocksForAutoResume(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "plannerusagelimit-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	multiUnitTrue := true
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: "phase6-test-repo", Created: "2026-11-06",
		MultiUnit: &multiUnitTrue,
	}, "Trivial multi-unit task.")

	planner := &fakeUsageLimitPlannerEngine{}
	var p pause.State

	var gotJob *worker.Job
	var gotAlert *AlertPayload
	deps := Deps{
		Vault:            v,
		Repos:            testRepoDeps(),
		EngineRegistry:   map[string]engine.Engine{"claude": planner},
		MultiUnitEnabled: true,
		Jev:              &jev.Client{APIKey: ""},
		Pause:            &p,
		OnBlocked: func(j worker.Job, payload AlertPayload) {
			gotJob = &j
			gotAlert = &payload
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	cleanupTaskBranch(t, slug)

	if err := ProcessTask(deps, noopReporter{}, job); err == nil {
		t.Fatal("ProcessTask returned nil error, want an error for a blocked task")
	}

	if got := planner.calls.Load(); got != 1 {
		t.Errorf("planner engine ran %d times, want exactly 1 (the usage limit must stop the round loop, not retry instantly)", got)
	}

	assertBlocked(t, v, relPath, ScenarioUsageLimit, gotJob, gotAlert)

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.PlanRound != 0 {
		t.Errorf("plan_round = %d, want 0 (unchanged -- the usage limit must not burn a round)", noteAfter.Frontmatter.PlanRound)
	}

	// Trip/AddBlockedNote are keyed by the resolved plan-engine *role* name
	// (planEngineName in runPlanner -- "claude" here, since nothing pins
	// plan_engine and PlanEngineDefault is unset), not the fake engine's
	// own Name() -- same distinction resolveCodingEngineName/
	// resolveReviewEngineName draw elsewhere in this package.
	if !p.IsPaused("claude") {
		t.Error("pause was not tripped by the planner's usage-limit block")
	}
	snap := p.Snapshot("claude")
	found := false
	for _, n := range snap.BlockedNotes {
		if n == relPath {
			found = true
		}
	}
	if !found {
		t.Errorf("BlockedNotes = %v, want it to contain %q (registered for auto-resume)", snap.BlockedNotes, relPath)
	}
}

// promptCapturingPlannerEngine wraps fakePlannerEngine, recording every
// prompt BuildCommand was invoked with, in call order --
// TestRejectedReplansWithFeedback's own way of inspecting what
// resolvePlanReview's replan path actually handed the planner engine.
// Reused as the unit loop's coding/review engine too once the replanned
// plan clears the gate (same single-fake-engine pattern multiunit_test.go
// itself uses), so prompts[0] specifically -- not whatever ran last -- is
// the planner round's own prompt.
type promptCapturingPlannerEngine struct {
	*fakePlannerEngine
	prompts []string
}

func (f *promptCapturingPlannerEngine) BuildCommand(dir, prompt, resumeSessionID string) (*exec.Cmd, string) {
	f.prompts = append(f.prompts, prompt)
	return f.fakePlannerEngine.BuildCommand(dir, prompt, resumeSessionID)
}

// TestRejectedPastCapBlocks confirms a plan rejected at its third round --
// maxPlanRounds's own cap, shared with runPlanner's unusable-plan retries --
// blocks the task rather than spending a 4th round, and fires the usual
// alert. Calls processMultiUnit directly (bypassing dispatch's generic
// claim write and ProcessTask's own setup, same as multiunit_test.go's
// other direct-call tests) since resolvePlanReview's decision comes from
// the round's own review text, not frontmatter.Status.
func TestRejectedPastCapBlocks(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "rejectedcap-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	multiUnitTrue := true
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "plan_rejected", Repo: "phase6-test-repo", Created: "2026-11-06",
		MultiUnit: &multiUnitTrue, PlanRound: 3,
	}, "Trivial multi-unit task.")

	if err := v.WriteNote(relPath, "test: seed plan+review "+slug, func(n *notetask.Note) error {
		n.HasPlan = true
		n.Plan = cleanPlanBody(3)
		n.HasPlanReview = true
		n.PlanReview = "### Round 3\n\nUse a different approach for U1."
		return nil
	}); err != nil {
		t.Fatalf("seeding plan+review: %v", err)
	}

	var blockedPayload *AlertPayload
	deps := Deps{
		Vault:            v,
		Repos:            testRepoDeps(),
		MultiUnitEnabled: true,
		OnBlocked: func(job worker.Job, payload AlertPayload) {
			blockedPayload = &payload
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note: %v", err)
	}

	if err := processMultiUnit(deps, noopReporter{}, job, note, testRepoDeps()["phase6-test-repo"], "task/"+slug, nil, nil); err == nil {
		t.Fatal("processMultiUnit returned nil error, want an error for a blocked task")
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "blocked")
	}
	if noteAfter.Frontmatter.PlanRound != 3 {
		t.Errorf("plan_round = %d, want unchanged 3 (no 4th round ever attempted)", noteAfter.Frontmatter.PlanRound)
	}
	if blockedPayload == nil {
		t.Fatal("OnBlocked was never called")
	}
}

// TestRejectedReplansWithFeedback confirms a plan rejected at round 1 --
// under the cap -- increments plan_round and re-runs the planner with that
// round's own review text folded into the prompt (buildPlannerPrompt's own
// round>1 branch), then runs the gate again on whatever the planner wrote
// this time.
func TestRejectedReplansWithFeedback(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "rejectedreplan-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	multiUnitTrue := true
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "plan_rejected", Repo: "phase6-test-repo", Created: "2026-11-06",
		MultiUnit: &multiUnitTrue, PlanRound: 1,
	}, "Trivial multi-unit task.")

	// The rejected round's own plan text is deliberately different from
	// what the fake planner engine writes on replan (cleanPlanBody) --
	// otherwise runPlanner's "plan_written" merge write would have nothing
	// to commit (identical content), which fails outright rather than
	// exercising the replan path this test is actually about.
	if err := v.WriteNote(relPath, "test: seed plan+review "+slug, func(n *notetask.Note) error {
		n.HasPlan = true
		n.Plan = "Stale rejected plan.\n\n```yaml\n- id: U0\n  title: stale\n  verify: \"true\"\n```\n"
		n.HasPlanReview = true
		n.PlanReview = "### Round 1\n\nuse X instead of Y"
		return nil
	}); err != nil {
		t.Fatalf("seeding plan+review: %v", err)
	}

	jevSrv, _ := newJevServer(t, `{"answers":{"risky":{"noul":0.1},"ambiguous":{"noul":0.1}}}`)
	planner := &promptCapturingPlannerEngine{fakePlannerEngine: &fakePlannerEngine{copyName: copyFileName(slug), planBody: cleanPlanBody}}

	deps := Deps{
		Vault:            v,
		Repos:            testRepoDeps(),
		EngineRegistry:   map[string]engine.Engine{"claude": planner},
		MultiUnitEnabled: true,
		Jev:              &jev.Client{BaseURL: jevSrv.URL, APIKey: "test-key"},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}
	branchName, _ := setupUnitLoopBranch(t, slug)
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
	})

	// The replanned plan clears the gate (low risky/ambiguous) and seeds
	// units straight into the unit loop, which reaches this task's finish
	// step -- a fake `gh` stands in for the real GitHub-hosted remote
	// testTargetRepoPath doesn't have.
	binDir := t.TempDir()
	writeFakeGhPRCreate(t, filepath.Join(binDir, "gh"), filepath.Join(binDir, "gh-calls.log"), "https://github.com/fake-owner/fake-repo/pull/1")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note: %v", err)
	}

	if err := processMultiUnit(deps, noopReporter{}, job, note, testRepoDeps()["phase6-test-repo"], branchName, planner, planner); err != nil {
		t.Fatalf("processMultiUnit: %v", err)
	}

	if len(planner.prompts) == 0 {
		t.Fatal("planner was never invoked")
	}
	if !strings.Contains(planner.prompts[0], "use X instead of Y") {
		t.Errorf("planner's own (first) prompt missing round 1's review text: %q", planner.prompts[0])
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.PlanRound != 2 {
		t.Errorf("plan_round = %d, want 2", noteAfter.Frontmatter.PlanRound)
	}
}
