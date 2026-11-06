package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
)

// TestWriteNoteCopyCarriesPlanSectionsStripsRunnerLog confirms writeNoteCopy
// forwards Plan/Plan Review/Handoff into the note-copy CC reads, alongside
// the existing Work Log, while Runner Log -- runner-only bookkeeping --
// never reaches the copy.
func TestWriteNoteCopyCarriesPlanSectionsStripsRunnerLog(t *testing.T) {
	note := &notetask.Note{
		Frontmatter:   notetask.Frontmatter{Status: "in_progress", Repo: "test-repo", Created: "2026-11-06"},
		Prompt:        "Do the thing.",
		HasPlan:       true,
		Plan:          "- U1: do part one\n- U2: do part two",
		HasPlanReview: true,
		PlanReview:    "### Round 1\nlooks fine",
		HasHandoff:    true,
		Handoff:       "U1 done, start U2 from commit abc123.",
		HasWorkLog:    true,
		WorkLog:       "- Started work.",
		HasRunnerLog:  true,
		RunnerLog: []notetask.RunnerLogEntry{
			{Timestamp: time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), Event: "queued"},
		},
	}

	copyPath := filepath.Join(t.TempDir(), "note-copy.md")
	if err := writeNoteCopy(note, copyPath); err != nil {
		t.Fatalf("writeNoteCopy: %v", err)
	}

	out, err := os.ReadFile(copyPath)
	if err != nil {
		t.Fatalf("reading note-copy: %v", err)
	}
	copyText := string(out)

	for _, want := range []string{"## Plan", "## Plan Review", "## Handoff", "## Work Log"} {
		if !strings.Contains(copyText, want) {
			t.Errorf("note-copy missing %q:\n%s", want, copyText)
		}
	}
	if strings.Contains(copyText, "## Runner Log") {
		t.Errorf("note-copy leaked Runner Log:\n%s", copyText)
	}
	if strings.Contains(copyText, "queued") {
		t.Errorf("note-copy leaked a Runner Log event:\n%s", copyText)
	}
}
