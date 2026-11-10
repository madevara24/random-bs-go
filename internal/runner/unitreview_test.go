package runner

import (
	"strings"
	"testing"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
)

// TestUnitReviewTimeoutAccessor covers RBG-55: a zero or negative
// Deps.UnitReviewTimeout falls back to 15 minutes, and a configured value is
// returned unchanged.
func TestUnitReviewTimeoutAccessor(t *testing.T) {
	cases := []struct {
		name string
		set  time.Duration
		want time.Duration
	}{
		{name: "zero falls back", set: 0, want: 15 * time.Minute},
		{name: "negative falls back", set: -time.Minute, want: 15 * time.Minute},
		{name: "configured value wins", set: 25 * time.Minute, want: 25 * time.Minute},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := Deps{UnitReviewTimeout: tc.set}
			if got := deps.unitReviewTimeout(); got != tc.want {
				t.Errorf("unitReviewTimeout() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestBuildUnitReviewPromptDiffOnly guards RBG-55's prompt contract: the
// reviewer is told the diff is all it needs and must not read repository
// files, run commands, or run tests -- the unit's verify command already
// passed on this commit. The VERDICT format and the unit's acceptance/diff
// must still be present.
func TestBuildUnitReviewPromptDiffOnly(t *testing.T) {
	unit := notetask.PlanUnit{
		ID:     "U1",
		Title:  "claim-message",
		Accept: "The claim message names the unit and its repo.",
	}
	const diff = "diff --git a/foo.go b/foo.go\n+new line\n"

	prompt := buildUnitReviewPrompt(unit, diff)

	for _, want := range []string{
		"diff below is the complete change and everything needed to judge it",
		"do not read repository files",
		"do not run any command",
		"do not run tests",
		"verify command has already passed on this commit",
		"exact first line `VERDICT: APPROVE` or `VERDICT: CONCERNS`",
		unit.Title,
		unit.Accept,
		diff,
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q:\n%s", want, prompt)
		}
	}
}
