package runner

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/jev"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/pause"
	"github.com/madevara24/random-bs-go/internal/plangate"
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

// planReviewMessageServer stands in for PostToThread's target and records
// each posted message's content on the returned buffered channel.
func planReviewMessageServer(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	messages := make(chan string, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal([]byte(r.FormValue("payload_json")), &payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		messages <- payload.Content
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprint(w, `{"id":"42"}`)
	}))
	t.Cleanup(srv.Close)
	return srv, messages
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

	reporter := &recordingReporter{}
	if err := ProcessTask(deps, reporter, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	wantStages := []string{
		"plan: planner round 1",
		"plan: gate",
		"unit U1 (1/2): building",
		"unit U1 (1/2): verify",
		"unit U1 (1/2): review round 1",
		"unit U2 (2/2): building",
		"unit U2 (2/2): verify",
		"unit U2 (2/2): review round 1",
		"finish: opening PR",
	}
	stages := reporter.stagesSeen()
	// ProcessTask reports its generic setup/invocation stages before it
	// reaches the multi-unit branch; the single-session path still relies
	// on those, so assert they're unchanged, then the multi-unit stages
	// themselves start right after them.
	wantPrefix := []string{"setup", "invocation"}
	if len(stages) < len(wantPrefix) || !reflect.DeepEqual(stages[:len(wantPrefix)], wantPrefix) {
		t.Fatalf("reported stages = %v, want prefix %v", stages, wantPrefix)
	}
	if got := stages[len(wantPrefix):]; !reflect.DeepEqual(got, wantStages) {
		t.Errorf("reported stages = %v, want %v", got, wantStages)
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

// TestPlannerBlockResumes confirms a multi-unit note blocked in the
// planner before any ## Plan was ever written -- so resume (SessionID /
// multiUnitContinuing) is false exactly like a fresh claim -- doesn't fail
// setup when task/<slug> is already sitting in the clone from that earlier,
// now-blocked run. Before RBG-51's fix, ProcessTask always ran
// pullAndBranch here, whose `checkout -b` fails outright on an existing
// branch (confirmed live 2026-11-09); a human had to delete the branch by
// hand before a blocker_resolved retry could proceed. The fix makes
// ProcessTask take the same checkoutTaskBranch resume path a real resume
// uses whenever that branch already exists, so setup succeeds and the
// planner runs again.
func TestPlannerBlockResumes(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "plannerblockresume-" + run
	relPath := fmt.Sprintf("Tasks/%s.md", slug)
	branchName := "task/" + slug

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	multiUnitTrue := true
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "blocker_resolved", Repo: "phase6-test-repo", Created: "2026-11-06",
		MultiUnit: &multiUnitTrue,
	}, "Trivial multi-unit task.")

	cleanupTaskBranch(t, slug)
	// Simulates the state a blocked planner round left behind: task/<slug>
	// already exists in the clone (pullAndBranch's own checkout -b, run by
	// that earlier ProcessTask call), but nothing was ever committed to it
	// -- planner sessions aren't allowed to commit -- and no ## Plan was
	// ever written to the note.
	if err := pullAndBranch(testRepoDeps()["phase6-test-repo"], branchName); err != nil {
		t.Fatalf("pullAndBranch (simulating the earlier blocked run): %v", err)
	}

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
	t.Cleanup(func() {
		exec.Command("git", "-C", testTargetRepoPath, "push", "origin", "--delete", branchName).Run()
	})

	binDir := t.TempDir()
	writeFakeGhPRCreate(t, filepath.Join(binDir, "gh"), filepath.Join(binDir, "gh-calls.log"), "https://github.com/fake-owner/fake-repo/pull/1")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v (setup must not fail just because %s already exists)", err, branchName)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	// fakePlannerEngine doubles as the unit loop's coding/review engine
	// too (see its own doc comment), so planner.calls counts U1/U2's
	// builds as well -- plan_round is the reliable signal that the
	// planner itself only needed its first round.
	if noteAfter.Frontmatter.PlanRound > 1 {
		t.Errorf("plan_round = %d, want 1 (the planner's first round should have parsed and cleared the gate)", noteAfter.Frontmatter.PlanRound)
	}
	if noteAfter.Frontmatter.Status != "done" {
		t.Errorf("status = %q, want %q (planner ran, gate passed, unit loop finished)", noteAfter.Frontmatter.Status, "done")
	}
}

// TestPlanReviewStopMessageAndGroupedReasons pins the stop branch's exact
// Discord body (mention, category headings in display order, bullets within
// each category) and confirms the same grouping is written to the note's
// plan_gate_reasons field, replacing any previous round's value.
func TestPlanReviewStopMessageAndGroupedReasons(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "planreviewstop-" + run
	repo := "phase6-test-repo"
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "queued", Repo: repo, Created: "2026-11-06", PlanRound: 2,
		PlanGateReasons: map[string][]string{
			"Errors":       {"stale first-round reason"},
			"Old category": {"stale"},
		},
	}, "Do the thing.")

	note, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading seeded note: %v", err)
	}
	note.HasPlan = true
	note.Plan = "# Plan\n\nDo the thing in two units."

	discordSrv, messages := planReviewMessageServer(t)
	deps := Deps{
		Vault:           v,
		ForumWebhookURL: discordSrv.URL,
		DiscordUserID:   "12345",
	}
	job := worker.Job{NotePath: relPath, Repo: repo, Slug: slug}
	reasons := []plangate.Reason{
		{Category: plangate.CategoryHardRules, Text: `unit "U1" touches "db/migrations/0003.sql", which matches hard-stop pattern "migrations/"`},
		{Category: plangate.CategoryHardRules, Text: "plan has 9 units, over the 8-unit hard limit"},
		{Category: plangate.CategoryJevClassifier, Text: "risky=0.90 is a clear yes"},
		{Category: plangate.CategoryPlannerQuestions, Text: "approach=none"},
		{Category: plangate.CategoryErrors, Text: `missing answer for "ambiguous"`},
	}
	result := plangate.Result{Stop: true, Categorized: reasons}
	for _, r := range reasons {
		result.Reasons = append(result.Reasons, r.Text)
	}

	if err := stopForPlanReview(deps, job, note, testRepoDeps()[repo], result, "plan_gate risky=0.90 ambiguous=missing approach=none(0.90) stop=true"); err != nil {
		t.Fatalf("stopForPlanReview: %v", err)
	}

	var message string
	select {
	case message = <-messages:
	case <-time.After(5 * time.Second):
		t.Fatal("no plan-review message was posted")
	}
	wantMessage := "<@12345> Task `" + slug + "` (" + repo + ")'s plan needs review.\n" +
		"\n" +
		"**Hard rules**\n" +
		"- unit \"U1\" touches \"db/migrations/0003.sql\", which matches hard-stop pattern \"migrations/\"\n" +
		"- plan has 9 units, over the 8-unit hard limit\n" +
		"**Jev classifier**\n" +
		"- risky=0.90 is a clear yes\n" +
		"**Planner questions**\n" +
		"- approach=none\n" +
		"**Errors**\n" +
		"- missing answer for \"ambiguous\"\n" +
		"\n" +
		"React ✅ to approve, or ❌ and write your feedback under `### Round 2` in `## Plan Review`."
	if message != wantMessage {
		t.Errorf("message =\n%s\n\nwant:\n%s", message, wantMessage)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "plan_review" {
		t.Errorf("status = %q, want plan_review", noteAfter.Frontmatter.Status)
	}
	wantReasons := map[string][]string{
		"Hard rules": {
			`unit "U1" touches "db/migrations/0003.sql", which matches hard-stop pattern "migrations/"`,
			"plan has 9 units, over the 8-unit hard limit",
		},
		"Jev classifier":    {"risky=0.90 is a clear yes"},
		"Planner questions": {"approach=none"},
		"Errors":            {`missing answer for "ambiguous"`},
	}
	if !reflect.DeepEqual(noteAfter.Frontmatter.PlanGateReasons, wantReasons) {
		t.Errorf("plan_gate_reasons = %#v, want %#v", noteAfter.Frontmatter.PlanGateReasons, wantReasons)
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

// TestPlannerRetryPromptHasParseError confirms a round whose plan fails to
// parse (here, a list of maps for `accept` -- still invalid under RBG-51's
// lenient list-of-strings parsing) has that parse error folded into the
// very next round's prompt (buildPlannerPrompt's lastParseErr branch). The
// planner used to get no information about why its plan was rejected on
// retry, which is why it kept repeating the same mistake every round.
func TestPlannerRetryPromptHasParseError(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "plannerretryerr-" + run
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
	planner := &promptCapturingPlannerEngine{fakePlannerEngine: &fakePlannerEngine{
		copyName: copyFileName(slug),
		planBody: func(round int) string {
			if round == 1 {
				return "Bad round.\n\n```yaml\n" +
					"- id: U1\n  title: Do part one\n  verify: \"true\"\n" +
					"  accept:\n    - step: a\n    - step: b\n" +
					"```\n"
			}
			return cleanPlanBody(round)
		},
	}}

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

	binDir := t.TempDir()
	writeFakeGhPRCreate(t, filepath.Join(binDir, "gh"), filepath.Join(binDir, "gh-calls.log"), "https://github.com/fake-owner/fake-repo/pull/1")
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	if err := ProcessTask(deps, noopReporter{}, job); err != nil {
		t.Fatalf("ProcessTask: %v", err)
	}

	if len(planner.prompts) < 2 {
		t.Fatalf("planner ran %d times, want at least 2 (round 1's bad plan, round 2's retry)", len(planner.prompts))
	}
	if !strings.Contains(planner.prompts[1], "cannot unmarshal") || !strings.Contains(planner.prompts[1], "accept") {
		t.Errorf("round 2 prompt missing round 1's parse error text: %q", planner.prompts[1])
	}
}

// TestPlannerEngineErrorBlocks confirms a planner engine whose invocation
// itself exits non-zero still ends the task at blocked, never in_progress
// -- it may count as a plan round first, exactly as an unusable-plan
// parse failure does today (fakeFailingPlannerEngine never touches the
// note-copy, so runPlanner's own round-retry loop is what notices), but it
// must never return with the note still at in_progress.
func TestPlannerEngineErrorBlocks(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "plannerengineerr-" + run
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

	planner := &fakeFailingPlannerEngine{}

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
		t.Fatal("ProcessTask returned nil error, want an error after the planner engine's invocation failed")
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status == "in_progress" {
		t.Errorf("status = %q, want never in_progress after a planner engine failure", noteAfter.Frontmatter.Status)
	}
	if noteAfter.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "blocked")
	}

	if blockedPayload == nil {
		t.Fatal("OnBlocked was never called")
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

func (f *fakeUsageLimitPlannerEngine) BuildReviewCommand(dir, prompt string) *exec.Cmd {
	return exec.Command("true")
}

func (f *fakeUsageLimitPlannerEngine) ParseReviewAnswer(stdout []byte) (string, error) {
	return "", nil
}

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

// TestPlannerFailureOpensBlocker pins the numbered-blocker contract for
// blockPlannerTask's blocked write: "### Blocker 1" with the
// "planner_failure: <reason>" Cause line, payload.BlockerN, and
// " (Blocker 1)" in the resulting Discord message.
func TestPlannerFailureOpensBlocker(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "plannerblocker-" + run
	relPath := fmt.Sprintf("Tasks/(phase6-test-repo) Planner Blocker Test %s.md", run)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "in_progress", Repo: "phase6-test-repo", Created: "2026-11-06",
	}, "Planner numbered-blocker test -- blockPlannerTask is called directly.")

	var gotAlert *AlertPayload
	deps := Deps{
		Vault: v,
		OnBlocked: func(j worker.Job, payload AlertPayload) {
			gotAlert = &payload
		},
	}
	job := worker.Job{NotePath: relPath, Repo: "phase6-test-repo", Slug: slug}

	err := blockPlannerTask(deps, job, "plan rejected after 3 rounds")
	if err == nil {
		t.Fatal("blockPlannerTask returned nil error, want an error for a blocked task")
	}
	if gotAlert == nil {
		t.Fatal("OnBlocked was never called")
	}
	if gotAlert.Scenario != ScenarioPlannerFailure {
		t.Errorf("alert scenario = %v, want %v", gotAlert.Scenario, ScenarioPlannerFailure)
	}
	assertNumberedBlocker(t, v, relPath, gotAlert, "planner_failure: plan rejected after 3 rounds", true)
}
