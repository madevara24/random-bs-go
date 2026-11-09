package notetask

import (
	"strings"
	"testing"
)

const validPlanSection = "Split into two units.\n\n```yaml\n" +
	"- id: U1\n" +
	"  title: Add the schema\n" +
	"  paths: [\"internal/notetask\"]\n" +
	"  depends_on: []\n" +
	"  accept: Fields parse and round-trip.\n" +
	"  verify: go test ./internal/notetask/...\n" +
	"- id: U2\n" +
	"  title: Wire up dispatch\n" +
	"  paths: [\"internal/runner\"]\n" +
	"  depends_on: [\"U1\"]\n" +
	"  accept: Dispatch reads the new fields.\n" +
	"  verify: go test ./internal/runner/...\n" +
	"```\n"

func TestParsePlanUnits(t *testing.T) {
	t.Run("valid block", func(t *testing.T) {
		units, err := ParsePlanUnits(validPlanSection)
		if err != nil {
			t.Fatalf("ParsePlanUnits: %v", err)
		}
		if len(units) != 2 {
			t.Fatalf("len(units) = %d, want 2", len(units))
		}
		want0 := PlanUnit{
			ID:        "U1",
			Title:     "Add the schema",
			Paths:     []string{"internal/notetask"},
			DependsOn: []string{},
			Accept:    "Fields parse and round-trip.",
			Verify:    "go test ./internal/notetask/...",
		}
		if units[0].ID != want0.ID || units[0].Title != want0.Title || units[0].Accept != want0.Accept || units[0].Verify != want0.Verify {
			t.Errorf("units[0] = %+v, want %+v", units[0], want0)
		}
		if len(units[0].Paths) != 1 || units[0].Paths[0] != "internal/notetask" {
			t.Errorf("units[0].Paths = %+v", units[0].Paths)
		}
		if len(units[0].DependsOn) != 0 {
			t.Errorf("units[0].DependsOn = %+v, want empty", units[0].DependsOn)
		}
		if units[1].ID != "U2" || len(units[1].DependsOn) != 1 || units[1].DependsOn[0] != "U1" {
			t.Errorf("units[1] = %+v", units[1])
		}
	})

	t.Run("no block", func(t *testing.T) {
		if _, err := ParsePlanUnits("Just prose, no fenced block."); err == nil {
			t.Fatal("expected an error for a Plan section with no yaml fence")
		}
	})

	t.Run("invalid YAML", func(t *testing.T) {
		section := "```yaml\n- id: U1\n  title: [unterminated\n```\n"
		if _, err := ParsePlanUnits(section); err == nil {
			t.Fatal("expected an error for invalid YAML in the fenced block")
		}
	})

	t.Run("empty list", func(t *testing.T) {
		section := "```yaml\n[]\n```\n"
		if _, err := ParsePlanUnits(section); err == nil {
			t.Fatal("expected an error for an empty plan unit list")
		}
	})

	t.Run("duplicate id", func(t *testing.T) {
		section := "```yaml\n" +
			"- id: U1\n  title: First\n  depends_on: []\n" +
			"- id: U1\n  title: Second\n  depends_on: []\n" +
			"```\n"
		_, err := ParsePlanUnits(section)
		if err == nil {
			t.Fatal("expected an error for a duplicate unit id")
		}
		if !strings.Contains(err.Error(), "U1") {
			t.Errorf("error %q doesn't mention the duplicated id", err)
		}
	})

	t.Run("unknown depends_on", func(t *testing.T) {
		section := "```yaml\n" +
			"- id: U1\n  title: Only unit\n  depends_on: [\"U9\"]\n" +
			"```\n"
		_, err := ParsePlanUnits(section)
		if err == nil {
			t.Fatal("expected an error for depends_on naming an unknown id")
		}
		if !strings.Contains(err.Error(), "U9") {
			t.Errorf("error %q doesn't mention the unknown id", err)
		}
	})
}

func TestParsePlanUnitsListFields(t *testing.T) {
	t.Run("accept and verify as lists", func(t *testing.T) {
		section := "```yaml\n" +
			"- id: U1\n" +
			"  title: Add the schema\n" +
			"  depends_on: []\n" +
			"  accept:\n    - a\n    - b\n" +
			"  verify:\n    - x\n    - y\n" +
			"```\n"
		units, err := ParsePlanUnits(section)
		if err != nil {
			t.Fatalf("ParsePlanUnits: %v", err)
		}
		if units[0].Accept != "a\nb" {
			t.Errorf("Accept = %q, want %q", units[0].Accept, "a\nb")
		}
		if units[0].Verify != "x && y" {
			t.Errorf("Verify = %q, want %q", units[0].Verify, "x && y")
		}
	})

	t.Run("plain strings unchanged", func(t *testing.T) {
		units, err := ParsePlanUnits(validPlanSection)
		if err != nil {
			t.Fatalf("ParsePlanUnits: %v", err)
		}
		if units[0].Accept != "Fields parse and round-trip." {
			t.Errorf("Accept = %q", units[0].Accept)
		}
		if units[0].Verify != "go test ./internal/notetask/..." {
			t.Errorf("Verify = %q", units[0].Verify)
		}
	})

	t.Run("accept as a map is still an error", func(t *testing.T) {
		section := "```yaml\n" +
			"- id: U1\n" +
			"  title: Add the schema\n" +
			"  depends_on: []\n" +
			"  accept:\n    k: v\n" +
			"  verify: go test ./...\n" +
			"```\n"
		if _, err := ParsePlanUnits(section); err == nil {
			t.Fatal("expected an error for accept written as a map")
		}
	})
}

func TestCurrentRoundReview(t *testing.T) {
	const planReview = "### Round 1\nno\n### Round 2\n"

	tests := []struct {
		round int
		want  string
	}{
		{1, "no"},
		{2, ""},
		{3, ""},
	}
	for _, tt := range tests {
		if got := CurrentRoundReview(planReview, tt.round); got != tt.want {
			t.Errorf("CurrentRoundReview(round=%d) = %q, want %q", tt.round, got, tt.want)
		}
	}
}

func TestAppendPlanReviewRound(t *testing.T) {
	note := &Note{}
	if note.HasPlanReview {
		t.Fatal("expected no Plan Review section on a fresh note")
	}

	AppendPlanReviewRound(note, 1)
	if !note.HasPlanReview {
		t.Fatal("AppendPlanReviewRound didn't set HasPlanReview")
	}

	AppendPlanReviewRound(note, 2)

	idx1 := strings.Index(note.PlanReview, "### Round 1")
	idx2 := strings.Index(note.PlanReview, "### Round 2")
	if idx1 == -1 || idx2 == -1 {
		t.Fatalf("PlanReview missing a round heading: %q", note.PlanReview)
	}
	if idx1 >= idx2 {
		t.Errorf("PlanReview headings out of order: %q", note.PlanReview)
	}
}
