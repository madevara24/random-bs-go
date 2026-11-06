package runner

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/jev"
	"github.com/madevara24/random-bs-go/internal/notetask"
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
// (clear low risky/ambiguous, no hard-rule hits) seeds `units` as pending
// entries in plan order and sets current_unit to the first one.
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
	cleanupTaskBranch(t, slug)

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
		if u.Status != notetask.UnitStatusPending {
			t.Errorf("unit %q status = %q, want %q", u.ID, u.Status, notetask.UnitStatusPending)
		}
	}
	if noteAfter.Frontmatter.Units[0].ID != "U1" || noteAfter.Frontmatter.Units[1].ID != "U2" {
		t.Errorf("units = %+v, want order U1, U2", noteAfter.Frontmatter.Units)
	}
	if noteAfter.Frontmatter.CurrentUnit == nil || *noteAfter.Frontmatter.CurrentUnit != "U1" {
		t.Errorf("current_unit = %v, want %q", noteAfter.Frontmatter.CurrentUnit, "U1")
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
