package runner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/testvault"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// fakeUnitCodingEngine simulates a unit's coding session: every invocation
// makes one real commit on the current branch (so head_sha actually
// advances round to round, the same way a real fix commit would), edits
// the note-copy to append this unit's own `### <id>` Handoff entry (parsed
// out of buildUnitPrompt's own text -- a plain feedback-only resume prompt
// has no such marker, so those calls leave Handoff untouched, same as a
// real session wouldn't rewrite it mid-fix) and sets status: done, and
// returns a fresh, stable session_id every call -- resumeSessionID is
// never acted on besides being recorded, since there's no real session to
// actually resume here.
type fakeUnitCodingEngine struct {
	copyName string

	mu         sync.Mutex
	calls      int
	resumeSeen []string
}

func (f *fakeUnitCodingEngine) Name() string { return "fake-unit-coding" }

func (f *fakeUnitCodingEngine) BuildCommand(dir, prompt, resumeSessionID string) (*exec.Cmd, string) {
	f.mu.Lock()
	f.calls++
	n := f.calls
	f.resumeSeen = append(f.resumeSeen, resumeSessionID)
	f.mu.Unlock()

	marker := filepath.Join(dir, ".fake-unit-calls")
	_ = os.WriteFile(marker, []byte(fmt.Sprintf("%d\n", n)), 0o644)
	_ = exec.Command("git", "-C", dir, "add", ".fake-unit-calls").Run()
	_ = exec.Command("git", "-C", dir, "commit", "-m", fmt.Sprintf("fake unit coding round %d", n)).Run()

	copyPath := filepath.Join(dir, f.copyName)
	if data, err := os.ReadFile(copyPath); err == nil {
		if note, err := notetask.Parse(data); err == nil {
			if id := extractUnitIDFromPrompt(prompt); id != "" && !strings.Contains(note.Handoff, "### "+id) {
				entry := fmt.Sprintf("### %s\n\nDid round %d of work.", id, n)
				if strings.TrimSpace(note.Handoff) == "" {
					note.Handoff = entry
				} else {
					note.Handoff = strings.TrimSpace(note.Handoff) + "\n\n" + entry
				}
				note.HasHandoff = true
			}
			note.Frontmatter.Status = "done"
			if out, err := note.Bytes(); err == nil {
				_ = os.WriteFile(copyPath, out, 0o644)
			}
		}
	}

	return exec.Command("true"), fmt.Sprintf("fake-unit-session-%d", n)
}

func (f *fakeUnitCodingEngine) ParseLine(line string, state *engine.RunState) {}

func (f *fakeUnitCodingEngine) ClassifyExit(waitErr error, stderrTail string, state engine.RunState) error {
	return waitErr
}

func (f *fakeUnitCodingEngine) BuildReviewCommand(dir, prompt string) *exec.Cmd { return exec.Command("true") }

func (f *fakeUnitCodingEngine) ParseReviewAnswer(stdout []byte) (string, error) { return "", nil }

func (f *fakeUnitCodingEngine) StartupTimeout() time.Duration { return 0 }

func (f *fakeUnitCodingEngine) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeUnitCodingEngine) resumeSessionsSeen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.resumeSeen...)
}

// extractUnitIDFromPrompt pulls the unit id out of buildUnitPrompt's own
// "one unit (`<id>`: ..." text -- "" if prompt isn't shaped that way (a
// plain feedback/verify-output resume prompt, which has no such marker).
func extractUnitIDFromPrompt(prompt string) string {
	const marker = "one unit (`"
	i := strings.Index(prompt, marker)
	if i == -1 {
		return ""
	}
	rest := prompt[i+len(marker):]
	j := strings.Index(rest, "`")
	if j == -1 {
		return ""
	}
	return rest[:j]
}

// fakeUnitReviewEngine scripts a fixed sequence of review verdicts --
// verdicts[i] answers the i-th call; the last entry repeats for any call
// beyond the end of the slice.
type fakeUnitReviewEngine struct {
	verdicts []string
	calls    atomic.Int32
}

func (f *fakeUnitReviewEngine) Name() string { return "fake-unit-review" }

func (f *fakeUnitReviewEngine) BuildCommand(dir, prompt, resumeSessionID string) (*exec.Cmd, string) {
	return exec.Command("true"), ""
}

func (f *fakeUnitReviewEngine) ParseLine(line string, state *engine.RunState) {}

func (f *fakeUnitReviewEngine) ClassifyExit(waitErr error, stderrTail string, state engine.RunState) error {
	return waitErr
}

func (f *fakeUnitReviewEngine) BuildReviewCommand(dir, prompt string) *exec.Cmd { return exec.Command("true") }

func (f *fakeUnitReviewEngine) ParseReviewAnswer(stdout []byte) (string, error) {
	i := int(f.calls.Add(1)) - 1
	if i >= len(f.verdicts) {
		i = len(f.verdicts) - 1
	}
	return f.verdicts[i], nil
}

func (f *fakeUnitReviewEngine) StartupTimeout() time.Duration { return 0 }

const unitLoopTestRepoKey = "phase6-test-repo"

func unitLoopRepoCfg() config.RepoConfig {
	return config.RepoConfig{Path: testTargetRepoPath, Remote: "local/phase6-test-repo", DefaultBranch: "main"}
}

// twoUnitPlan is a trivial two-unit plan whose verify commands always pass
// -- used by tests whose subject matter is the review/session loop, not
// verify itself.
const twoUnitPlan = "Two units.\n\n```yaml\n" +
	"- id: U1\n  title: Do part one\n  paths: [internal/foo.go]\n  accept: \"part one works\"\n  verify: \"true\"\n" +
	"- id: U2\n  title: Do part two\n  paths: [internal/bar.go]\n  depends_on: [U1]\n  accept: \"part two works\"\n  verify: \"true\"\n" +
	"```\n"

// oneUnitPlan is a single-unit plan, verify always passing.
const oneUnitPlan = "One unit.\n\n```yaml\n" +
	"- id: U1\n  title: Do the only part\n  paths: [internal/foo.go]\n  accept: \"it works\"\n  verify: \"true\"\n" +
	"```\n"

// seedUnitLoopNote seeds a fresh multi-unit task note (in_progress,
// multi_unit: true, the given Plan body and units: frontmatter) into the
// throwaway test vault, returning the synced Vault and the note's
// vault-relative path.
func seedUnitLoopNote(t *testing.T, slug, planBody string, units []notetask.UnitState, currentUnit string) (*vaultgit.Vault, string) {
	t.Helper()
	relPath := fmt.Sprintf("Tasks/%s.md", slug)

	v := vaultgit.New(testvault.Path, "master")
	if err := v.Sync(); err != nil {
		t.Fatalf("sync: %v", err)
	}

	multiUnitTrue := true
	testvault.Seed(t, relPath, notetask.Frontmatter{
		Status: "in_progress", Repo: unitLoopTestRepoKey, Created: "2026-11-06",
		MultiUnit: &multiUnitTrue,
	}, "Trivial multi-unit task.")

	err := v.WriteNote(relPath, "test: seed plan+units "+slug, func(n *notetask.Note) error {
		n.HasPlan = true
		n.Plan = planBody
		n.Frontmatter.Units = units
		cur := currentUnit
		n.Frontmatter.CurrentUnit = &cur
		return nil
	})
	if err != nil {
		t.Fatalf("seeding plan+units: %v", err)
	}
	return v, relPath
}

// setupUnitLoopBranch creates and checks out the task branch runUnitLoop
// expects to already be sitting on (same contract as ProcessTask's own
// pullAndBranch call before it ever reaches processMultiUnit), returning
// that branch's starting commit SHA.
func setupUnitLoopBranch(t *testing.T, slug string) (branchName, startSHA string) {
	t.Helper()
	branchName = "task/" + slug
	cleanupTaskBranch(t, slug)
	if err := pullAndBranch(unitLoopRepoCfg(), branchName); err != nil {
		t.Fatalf("pullAndBranch: %v", err)
	}
	sha, err := gitRevParse(testTargetRepoPath, branchName)
	if err != nil {
		t.Fatalf("gitRevParse: %v", err)
	}
	return branchName, sha
}

// TestUnitLoopTwoUnitsHappyPath confirms a clean two-unit run: both units
// reach done, in plan order, and `## Handoff` accumulates "### U1" before
// "### U2".
func TestUnitLoopTwoUnitsHappyPath(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "unitloop-happy-" + run

	units := []notetask.UnitState{
		{ID: "U1", Status: notetask.UnitStatusPending},
		{ID: "U2", Status: notetask.UnitStatusPending},
	}
	v, relPath := seedUnitLoopNote(t, slug, twoUnitPlan, units, "U1")
	branchName, _ := setupUnitLoopBranch(t, slug)

	codingEng := &fakeUnitCodingEngine{copyName: copyFileName(slug)}
	reviewEng := &fakeUnitReviewEngine{verdicts: []string{"VERDICT: APPROVE\n\nfine."}}

	deps := Deps{Vault: v, Repos: map[string]config.RepoConfig{unitLoopTestRepoKey: unitLoopRepoCfg()}}
	job := worker.Job{NotePath: relPath, Repo: unitLoopTestRepoKey, Slug: slug}

	if err := runUnitLoop(deps, noopReporter{}, job, unitLoopRepoCfg(), branchName, codingEng, reviewEng); err != nil {
		t.Fatalf("runUnitLoop: %v", err)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	for _, u := range noteAfter.Frontmatter.Units {
		if u.Status != notetask.UnitStatusDone {
			t.Errorf("unit %q status = %q, want %q", u.ID, u.Status, notetask.UnitStatusDone)
		}
	}
	if noteAfter.Frontmatter.CurrentUnit != nil {
		t.Errorf("current_unit = %v, want nil", *noteAfter.Frontmatter.CurrentUnit)
	}
	i1 := strings.Index(noteAfter.Handoff, "### U1")
	i2 := strings.Index(noteAfter.Handoff, "### U2")
	if i1 == -1 || i2 == -1 || i1 > i2 {
		t.Errorf("Handoff = %q, want \"### U1\" before \"### U2\"", noteAfter.Handoff)
	}
}

// TestUnitLoopConcernsResumesSameSession confirms a CONCERNS verdict
// resumes the exact same coding session_id the unit's build round
// captured, counts one review round, and a following APPROVE finishes the
// unit.
func TestUnitLoopConcernsResumesSameSession(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "unitloop-concerns-" + run

	units := []notetask.UnitState{{ID: "U1", Status: notetask.UnitStatusPending}}
	v, relPath := seedUnitLoopNote(t, slug, oneUnitPlan, units, "U1")
	branchName, _ := setupUnitLoopBranch(t, slug)

	codingEng := &fakeUnitCodingEngine{copyName: copyFileName(slug)}
	reviewEng := &fakeUnitReviewEngine{verdicts: []string{"VERDICT: CONCERNS\n\nfix it.", "VERDICT: APPROVE\n\nfine now."}}

	deps := Deps{Vault: v, Repos: map[string]config.RepoConfig{unitLoopTestRepoKey: unitLoopRepoCfg()}}
	job := worker.Job{NotePath: relPath, Repo: unitLoopTestRepoKey, Slug: slug}

	if err := runUnitLoop(deps, noopReporter{}, job, unitLoopRepoCfg(), branchName, codingEng, reviewEng); err != nil {
		t.Fatalf("runUnitLoop: %v", err)
	}

	resumes := codingEng.resumeSessionsSeen()
	if len(resumes) < 2 {
		t.Fatalf("coding engine saw %d calls, want at least 2 (fresh build + CONCERNS resume): %v", len(resumes), resumes)
	}
	const firstSession = "fake-unit-session-1"
	if resumes[1] != firstSession {
		t.Errorf("second coding call's resumeSessionID = %q, want %q (U1's own session_id from round 1)", resumes[1], firstSession)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	u, ok := findUnitState(noteAfter.Frontmatter.Units, "U1")
	if !ok {
		t.Fatal("unit U1 missing from units after the loop")
	}
	if u.Status != notetask.UnitStatusDone {
		t.Errorf("unit U1 status = %q, want %q", u.Status, notetask.UnitStatusDone)
	}
	if u.ReviewRounds != 1 {
		t.Errorf("unit U1 review_rounds = %d, want 1", u.ReviewRounds)
	}
}

// TestUnitLoopVerifyFailureCountsRound confirms a failing verify command
// resumes the coding session with its output and counts one review round.
func TestUnitLoopVerifyFailureCountsRound(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "unitloop-verifyfail-" + run

	// Passes only once the fake coding engine's own round counter (written
	// to .fake-unit-calls and committed every invocation) reaches 2 --
	// fails on the fresh build (round 1), passes after the verify-failure
	// resume (round 2).
	plan := "One unit.\n\n```yaml\n" +
		"- id: U1\n  title: Do the only part\n  paths: [internal/foo.go]\n  accept: \"it works\"\n" +
		"  verify: \"test $(cat .fake-unit-calls 2>/dev/null || echo 0) -ge 2\"\n" +
		"```\n"

	units := []notetask.UnitState{{ID: "U1", Status: notetask.UnitStatusPending}}
	v, relPath := seedUnitLoopNote(t, slug, plan, units, "U1")
	branchName, _ := setupUnitLoopBranch(t, slug)

	codingEng := &fakeUnitCodingEngine{copyName: copyFileName(slug)}
	reviewEng := &fakeUnitReviewEngine{verdicts: []string{"VERDICT: APPROVE\n\nfine."}}

	deps := Deps{Vault: v, Repos: map[string]config.RepoConfig{unitLoopTestRepoKey: unitLoopRepoCfg()}}
	job := worker.Job{NotePath: relPath, Repo: unitLoopTestRepoKey, Slug: slug}

	if err := runUnitLoop(deps, noopReporter{}, job, unitLoopRepoCfg(), branchName, codingEng, reviewEng); err != nil {
		t.Fatalf("runUnitLoop: %v", err)
	}

	if got := codingEng.callCount(); got != 2 {
		t.Errorf("coding engine ran %d times, want 2 (fresh build + verify-failure resume)", got)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	u, ok := findUnitState(noteAfter.Frontmatter.Units, "U1")
	if !ok {
		t.Fatal("unit U1 missing from units after the loop")
	}
	if u.ReviewRounds != 1 {
		t.Errorf("unit U1 review_rounds = %d, want 1 (one verify-failure round)", u.ReviewRounds)
	}
	if u.Status != notetask.UnitStatusDone {
		t.Errorf("unit U1 status = %q, want %q (verify passed on round 2, review approved)", u.Status, notetask.UnitStatusDone)
	}
}

// TestUnitLoopPerUnitCapBlocks confirms 3 straight CONCERNS verdicts block
// the task, preserve the branch's work, and alert naming the unit that hit
// the cap.
func TestUnitLoopPerUnitCapBlocks(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "unitloop-cap-" + run

	units := []notetask.UnitState{{ID: "U1", Status: notetask.UnitStatusPending}}
	v, relPath := seedUnitLoopNote(t, slug, oneUnitPlan, units, "U1")
	branchName, _ := setupUnitLoopBranch(t, slug)

	codingEng := &fakeUnitCodingEngine{copyName: copyFileName(slug)}
	reviewEng := &fakeUnitReviewEngine{verdicts: []string{
		"VERDICT: CONCERNS\n\nfix 1.",
		"VERDICT: CONCERNS\n\nfix 2.",
		"VERDICT: CONCERNS\n\nfix 3.",
	}}

	var gotAlert *AlertPayload
	deps := Deps{
		Vault: v,
		Repos: map[string]config.RepoConfig{unitLoopTestRepoKey: unitLoopRepoCfg()},
		OnBlocked: func(j worker.Job, payload AlertPayload) {
			gotAlert = &payload
		},
	}
	job := worker.Job{NotePath: relPath, Repo: unitLoopTestRepoKey, Slug: slug}

	err := runUnitLoop(deps, noopReporter{}, job, unitLoopRepoCfg(), branchName, codingEng, reviewEng)
	if err == nil {
		t.Fatal("runUnitLoop returned nil error, want an error once the per-unit cap is hit")
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	if noteAfter.Frontmatter.Status != "blocked" {
		t.Errorf("status = %q, want %q", noteAfter.Frontmatter.Status, "blocked")
	}
	u, ok := findUnitState(noteAfter.Frontmatter.Units, "U1")
	if !ok {
		t.Fatal("unit U1 missing from units after the loop")
	}
	if u.Status != notetask.UnitStatusBlocked {
		t.Errorf("unit U1 status = %q, want %q", u.Status, notetask.UnitStatusBlocked)
	}

	if gotAlert == nil {
		t.Fatal("OnBlocked was never called")
	}
	if !strings.Contains(gotAlert.ExitErrText, "U1") {
		t.Errorf("alert ExitErrText = %q, want it to name unit U1", gotAlert.ExitErrText)
	}
	if gotAlert.PreserveErr != "" {
		t.Errorf("PreserveErr = %q, want the branch's work to have been preserved cleanly", gotAlert.PreserveErr)
	}
}

// TestUnitLoopTaskBudgetBlocks confirms the task-wide round budget
// (2 * len(units)) blocks a unit needing one more round even when that
// unit's own per-unit cap is nowhere close -- 4 rounds already spent
// across 2 units, a 5th (for either unit) exceeds the budget.
func TestUnitLoopTaskBudgetBlocks(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "unitloop-budget-" + run

	units := []notetask.UnitState{
		{ID: "U1", Status: notetask.UnitStatusPending, ReviewRounds: 0},
		{ID: "U2", Status: notetask.UnitStatusDone, ReviewRounds: 4},
	}
	// U1's verify always fails, so its very first round would need a
	// second one -- the task budget (2*2=4, already fully spent by U2)
	// blocks it before a per-unit cap (3) ever would.
	failingPlan := "Two units.\n\n```yaml\n" +
		"- id: U1\n  title: Do part one\n  paths: [internal/foo.go]\n  accept: \"part one works\"\n  verify: \"false\"\n" +
		"- id: U2\n  title: Do part two\n  paths: [internal/bar.go]\n  depends_on: [U1]\n  accept: \"part two works\"\n  verify: \"true\"\n" +
		"```\n"
	v, relPath := seedUnitLoopNote(t, slug, failingPlan, units, "U1")
	branchName, _ := setupUnitLoopBranch(t, slug)

	codingEng := &fakeUnitCodingEngine{copyName: copyFileName(slug)}
	reviewEng := &fakeUnitReviewEngine{verdicts: []string{"VERDICT: APPROVE\n\nunused."}}

	var gotAlert *AlertPayload
	deps := Deps{
		Vault: v,
		Repos: map[string]config.RepoConfig{unitLoopTestRepoKey: unitLoopRepoCfg()},
		OnBlocked: func(j worker.Job, payload AlertPayload) {
			gotAlert = &payload
		},
	}
	job := worker.Job{NotePath: relPath, Repo: unitLoopTestRepoKey, Slug: slug}

	err := runUnitLoop(deps, noopReporter{}, job, unitLoopRepoCfg(), branchName, codingEng, reviewEng)
	if err == nil {
		t.Fatal("runUnitLoop returned nil error, want an error once the task budget is hit")
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
	if !strings.Contains(gotAlert.ExitErrText, "budget") {
		t.Errorf("alert ExitErrText = %q, want it to name the task-wide budget", gotAlert.ExitErrText)
	}
}

// TestUnitLoopResumeAtBuilding confirms a note left at U2 building with a
// session_id already recorded resumes that exact session, and U1 (already
// done) is never touched.
func TestUnitLoopResumeAtBuilding(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "unitloop-resumebuilding-" + run

	branchName, startSHA := setupUnitLoopBranch(t, slug)

	units := []notetask.UnitState{
		{ID: "U1", Status: notetask.UnitStatusDone, BaseSHA: startSHA, HeadSHA: startSHA, SessionID: "sess-u1-done"},
		{ID: "U2", Status: notetask.UnitStatusBuilding, BaseSHA: startSHA, SessionID: "sess-u2-prior"},
	}
	v, relPath := seedUnitLoopNote(t, slug, twoUnitPlan, units, "U2")

	codingEng := &fakeUnitCodingEngine{copyName: copyFileName(slug)}
	reviewEng := &fakeUnitReviewEngine{verdicts: []string{"VERDICT: APPROVE\n\nfine."}}

	deps := Deps{Vault: v, Repos: map[string]config.RepoConfig{unitLoopTestRepoKey: unitLoopRepoCfg()}}
	job := worker.Job{NotePath: relPath, Repo: unitLoopTestRepoKey, Slug: slug}

	if err := runUnitLoop(deps, noopReporter{}, job, unitLoopRepoCfg(), branchName, codingEng, reviewEng); err != nil {
		t.Fatalf("runUnitLoop: %v", err)
	}

	resumes := codingEng.resumeSessionsSeen()
	if len(resumes) == 0 {
		t.Fatal("coding engine was never called for U2")
	}
	if resumes[0] != "sess-u2-prior" {
		t.Errorf("first coding call's resumeSessionID = %q, want %q (U2's prior session_id)", resumes[0], "sess-u2-prior")
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	u1, _ := findUnitState(noteAfter.Frontmatter.Units, "U1")
	if u1.SessionID != "sess-u1-done" {
		t.Errorf("U1 session_id = %q, want it untouched (%q) -- U1 must never re-run", u1.SessionID, "sess-u1-done")
	}
	u2, _ := findUnitState(noteAfter.Frontmatter.Units, "U2")
	if u2.Status != notetask.UnitStatusDone {
		t.Errorf("U2 status = %q, want %q", u2.Status, notetask.UnitStatusDone)
	}
}

// TestUnitLoopResumeAtReview confirms a note left at U2 review re-reviews
// with no coding call at all.
func TestUnitLoopResumeAtReview(t *testing.T) {
	testvault.SkipIfAbsent(t)
	t.Cleanup(testvault.Lock(t))

	run := fmt.Sprintf("%d", time.Now().UnixNano())
	slug := "unitloop-resumereview-" + run

	branchName, startSHA := setupUnitLoopBranch(t, slug)

	units := []notetask.UnitState{
		{ID: "U1", Status: notetask.UnitStatusDone, BaseSHA: startSHA, HeadSHA: startSHA, SessionID: "sess-u1-done"},
		{ID: "U2", Status: notetask.UnitStatusReview, BaseSHA: startSHA, HeadSHA: startSHA, SessionID: "sess-u2-done"},
	}
	v, relPath := seedUnitLoopNote(t, slug, twoUnitPlan, units, "U2")

	codingEng := &fakeUnitCodingEngine{copyName: copyFileName(slug)}
	reviewEng := &fakeUnitReviewEngine{verdicts: []string{"VERDICT: APPROVE\n\nfine."}}

	deps := Deps{Vault: v, Repos: map[string]config.RepoConfig{unitLoopTestRepoKey: unitLoopRepoCfg()}}
	job := worker.Job{NotePath: relPath, Repo: unitLoopTestRepoKey, Slug: slug}

	if err := runUnitLoop(deps, noopReporter{}, job, unitLoopRepoCfg(), branchName, codingEng, reviewEng); err != nil {
		t.Fatalf("runUnitLoop: %v", err)
	}

	if got := codingEng.callCount(); got != 0 {
		t.Errorf("coding engine ran %d times, want 0 -- resuming at review must not make a coding call", got)
	}
	if got := reviewEng.calls.Load(); got != 1 {
		t.Errorf("review engine ran %d times, want 1", got)
	}

	noteAfter, err := v.ReadNote(relPath)
	if err != nil {
		t.Fatalf("reading note back: %v", err)
	}
	u2, ok := findUnitState(noteAfter.Frontmatter.Units, "U2")
	if !ok {
		t.Fatal("unit U2 missing from units after the loop")
	}
	if u2.Status != notetask.UnitStatusDone {
		t.Errorf("U2 status = %q, want %q", u2.Status, notetask.UnitStatusDone)
	}
	if noteAfter.Frontmatter.CurrentUnit != nil {
		t.Errorf("current_unit = %v, want nil once both units are done", *noteAfter.Frontmatter.CurrentUnit)
	}
}

// TestUnitVerifyTimeoutKillsChildren confirms a verify command that
// outlives its timeout has every descendant killed, not just the `sh`
// wrapper exec.CommandContext alone would reach -- `sleep 300 & wait`
// backgrounds a child sleep process directly under sh, so a kill scoped to
// sh's own PID would leave that sleep running on the host indefinitely
// (the bug this fix addresses). A 1-second timeout against a 300-second
// sleep makes the pass/fail obvious well within the test's own budget.
func TestUnitVerifyTimeoutKillsChildren(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not on PATH, skipping")
	}

	_, err := runUnitVerify(t.TempDir(), "sleep 300 & wait", time.Second)
	if err == nil {
		t.Fatal("runUnitVerify returned nil error, want a timeout error")
	}

	// Give the SIGKILL a brief moment to actually land before checking.
	time.Sleep(200 * time.Millisecond)

	out, _ := exec.Command("pgrep", "-f", "sleep 300").CombinedOutput()
	if strings.TrimSpace(string(out)) != "" {
		t.Errorf("pgrep -f 'sleep 300' still finds a process after the verify timeout: %q", out)
		exec.Command("pkill", "-f", "sleep 300").Run()
	}
}
