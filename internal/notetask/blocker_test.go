package notetask

import (
	"strings"
	"testing"
	"time"
)

var (
	blockerT1 = time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	blockerT2 = time.Date(2026, 11, 10, 13, 30, 0, 0, time.UTC)
)

func newBlockerNote() *Note {
	return &Note{
		Frontmatter: Frontmatter{Status: "in_progress", Repo: "test-repo", Created: "2026-11-10"},
		Prompt:      "Do the thing.",
		HasWorkLog:  true,
		WorkLog:     "- Working.",
	}
}

func TestAppendBlocker(t *testing.T) {
	note := newBlockerNote()

	got := AppendBlocker(note, "first cause", blockerT1)
	if got != 1 {
		t.Fatalf("first AppendBlocker returned %d, want 1", got)
	}
	if note.Frontmatter.BlockerRound != 1 {
		t.Errorf("BlockerRound after first = %d, want 1", note.Frontmatter.BlockerRound)
	}
	if !note.HasBlockers {
		t.Fatal("AppendBlocker didn't set HasBlockers")
	}
	wantFirst := "### Blocker 1\n- Blocked: 2026-11-10T12:00:00Z\n- Cause: first cause\n\n#### Resolution\n"
	if !strings.Contains(note.Blockers, wantFirst) {
		t.Errorf("Blockers missing first block in the expected shape.\ngot:\n%s\nwant block:\n%s", note.Blockers, wantFirst)
	}

	got = AppendBlocker(note, "second cause", blockerT2)
	if got != 2 {
		t.Fatalf("second AppendBlocker returned %d, want 2", got)
	}
	if note.Frontmatter.BlockerRound != 2 {
		t.Errorf("BlockerRound after second = %d, want 2", note.Frontmatter.BlockerRound)
	}
	wantSecond := "### Blocker 2\n- Blocked: 2026-11-10T13:30:00Z\n- Cause: second cause\n\n#### Resolution\n"
	if !strings.Contains(note.Blockers, wantSecond) {
		t.Errorf("Blockers missing second block in the expected shape.\ngot:\n%s\nwant block:\n%s", note.Blockers, wantSecond)
	}
	if idx1, idx2 := strings.Index(note.Blockers, "### Blocker 1"), strings.Index(note.Blockers, "### Blocker 2"); !(idx1 < idx2) {
		t.Errorf("blocker blocks out of order: 1 at %d, 2 at %d\n%s", idx1, idx2, note.Blockers)
	}
	if !strings.Contains(note.Blockers, "#### Resolution\n\n### Blocker 2") {
		t.Errorf("second block not separated from the first resolution by a blank line:\n%s", note.Blockers)
	}
	if got := strings.Count(note.Blockers, "#### Resolution"); got != 2 {
		t.Errorf("#### Resolution count = %d, want 2\n%s", got, note.Blockers)
	}
}

func TestAppendBlockerIntoExistingEmptySection(t *testing.T) {
	note := newBlockerNote()
	note.HasBlockers = true
	note.Blockers = ""

	AppendBlocker(note, "cause", blockerT1)
	if strings.HasPrefix(note.Blockers, "\n") {
		t.Errorf("first block starts with a blank line:\n%q", note.Blockers)
	}
	if !strings.HasPrefix(note.Blockers, "### Blocker 1\n") {
		t.Errorf("first block doesn't start with the heading:\n%q", note.Blockers)
	}
}

func TestCurrentBlockerResolution(t *testing.T) {
	t.Run("no blocker round", func(t *testing.T) {
		note := newBlockerNote()
		if got := CurrentBlockerResolution(note); got != "" {
			t.Errorf("CurrentBlockerResolution = %q, want \"\" for BlockerRound 0", got)
		}
	})

	t.Run("blank resolution", func(t *testing.T) {
		note := newBlockerNote()
		AppendBlocker(note, "cause", blockerT1)
		if got := CurrentBlockerResolution(note); got != "" {
			t.Errorf("CurrentBlockerResolution = %q, want \"\" for a blank resolution", got)
		}
	})

	t.Run("reads only the current blocker", func(t *testing.T) {
		note := newBlockerNote()
		AppendBlocker(note, "first cause", blockerT1)
		if !FillCurrentBlockerResolution(note, "first answer") {
			t.Fatal("FillCurrentBlockerResolution didn't fill the first blocker")
		}
		if got := CurrentBlockerResolution(note); got != "first answer" {
			t.Fatalf("CurrentBlockerResolution = %q, want %q", got, "first answer")
		}

		AppendBlocker(note, "second cause", blockerT2)
		if got := CurrentBlockerResolution(note); got != "" {
			t.Errorf("CurrentBlockerResolution = %q, want \"\" for the new blank blocker, not the older resolution", got)
		}
		if !FillCurrentBlockerResolution(note, "second answer") {
			t.Fatal("FillCurrentBlockerResolution didn't fill the second blocker")
		}
		if got := CurrentBlockerResolution(note); got != "second answer" {
			t.Errorf("CurrentBlockerResolution = %q, want %q (only the current blocker's resolution)", got, "second answer")
		}
	})

	t.Run("stops at next blocker heading", func(t *testing.T) {
		note := &Note{
			Frontmatter: Frontmatter{BlockerRound: 1},
			HasBlockers: true,
			Blockers: "### Blocker 1\n- Blocked: 2026-11-10T12:00:00Z\n- Cause: c1\n\n" +
				"#### Resolution\nfirst answer\n\n" +
				"### Blocker 2\n- Blocked: 2026-11-10T13:00:00Z\n- Cause: c2\n\n" +
				"#### Resolution\nsecond answer",
		}
		if got := CurrentBlockerResolution(note); got != "first answer" {
			t.Errorf("CurrentBlockerResolution = %q, want %q", got, "first answer")
		}
	})

	t.Run("missing blocker heading", func(t *testing.T) {
		note := &Note{
			Frontmatter: Frontmatter{BlockerRound: 2},
			HasBlockers: true,
			Blockers:    "### Blocker 1\n- Cause: c\n\n#### Resolution\nfirst answer",
		}
		if got := CurrentBlockerResolution(note); got != "" {
			t.Errorf("CurrentBlockerResolution = %q, want \"\" when the current heading is missing", got)
		}
	})

	t.Run("missing resolution heading", func(t *testing.T) {
		note := &Note{
			Frontmatter: Frontmatter{BlockerRound: 1},
			HasBlockers: true,
			Blockers:    "### Blocker 1\n- Cause: c",
		}
		if got := CurrentBlockerResolution(note); got != "" {
			t.Errorf("CurrentBlockerResolution = %q, want \"\" when the resolution heading is missing", got)
		}
	})
}

func TestFillCurrentBlockerResolution(t *testing.T) {
	t.Run("fills an empty resolution", func(t *testing.T) {
		note := newBlockerNote()
		AppendBlocker(note, "cause", blockerT1)

		if !FillCurrentBlockerResolution(note, "  Auto: claude usage limit reset.  ") {
			t.Fatal("FillCurrentBlockerResolution returned false for an empty resolution")
		}
		if !note.HasBlockers {
			t.Error("FillCurrentBlockerResolution didn't set HasBlockers")
		}
		if got := CurrentBlockerResolution(note); got != "Auto: claude usage limit reset." {
			t.Errorf("resolution = %q, want trimmed %q", got, "Auto: claude usage limit reset.")
		}
	})

	t.Run("never overwrites existing text", func(t *testing.T) {
		note := newBlockerNote()
		AppendBlocker(note, "cause", blockerT1)
		FillCurrentBlockerResolution(note, "human answer")

		if FillCurrentBlockerResolution(note, "auto answer") {
			t.Fatal("FillCurrentBlockerResolution overwrote an existing resolution")
		}
		if got := CurrentBlockerResolution(note); got != "human answer" {
			t.Errorf("resolution = %q, want the original %q", got, "human answer")
		}
	})

	t.Run("whitespace-only resolution counts as empty", func(t *testing.T) {
		note := &Note{
			Frontmatter: Frontmatter{BlockerRound: 1},
			HasBlockers: true,
			Blockers:    "### Blocker 1\n- Cause: c\n\n#### Resolution\n   \n",
		}
		if !FillCurrentBlockerResolution(note, "auto answer") {
			t.Fatal("FillCurrentBlockerResolution returned false for a whitespace-only resolution")
		}
		if got := CurrentBlockerResolution(note); got != "auto answer" {
			t.Errorf("resolution = %q, want %q", got, "auto answer")
		}
	})

	t.Run("only touches the current blocker", func(t *testing.T) {
		note := newBlockerNote()
		AppendBlocker(note, "first cause", blockerT1)
		FillCurrentBlockerResolution(note, "first answer")
		AppendBlocker(note, "second cause", blockerT2)

		if !FillCurrentBlockerResolution(note, "second answer") {
			t.Fatal("FillCurrentBlockerResolution didn't fill the second blocker")
		}
		if idx1, idx2 := strings.Index(note.Blockers, "first answer"), strings.Index(note.Blockers, "second answer"); !(idx1 != -1 && idx2 != -1 && idx1 < idx2) {
			t.Errorf("resolutions missing or out of order:\n%s", note.Blockers)
		}
		if got := CurrentBlockerResolution(note); got != "second answer" {
			t.Errorf("CurrentBlockerResolution = %q, want %q", got, "second answer")
		}
	})

	t.Run("no-op cases", func(t *testing.T) {
		if FillCurrentBlockerResolution(newBlockerNote(), "text") {
			t.Error("filled a note with BlockerRound 0")
		}

		note := newBlockerNote()
		AppendBlocker(note, "cause", blockerT1)
		if FillCurrentBlockerResolution(note, "   ") {
			t.Error("filled with blank text")
		}

		noHeading := &Note{
			Frontmatter: Frontmatter{BlockerRound: 1},
			HasBlockers: true,
			Blockers:    "### Blocker 1\n- Cause: c",
		}
		if FillCurrentBlockerResolution(noHeading, "text") {
			t.Error("filled a blocker with no #### Resolution heading")
		}

		noBlock := &Note{
			Frontmatter: Frontmatter{BlockerRound: 3},
			HasBlockers: true,
			Blockers:    "### Blocker 1\n- Cause: c\n\n#### Resolution\nold answer",
		}
		if FillCurrentBlockerResolution(noBlock, "text") {
			t.Error("filled when the current blocker heading was missing")
		}
	})
}

func TestBlockersSectionRoundTrips(t *testing.T) {
	note, err := Parse([]byte(legacyFinishedNote))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	AppendBlocker(note, "self-reported", blockerT1)
	AppendBlocker(note, "open_pr_failure: push", blockerT2)
	FillCurrentBlockerResolution(note, "Rebased and re-ran.")

	out, err := note.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	idxBlockers := strings.Index(string(out), "## Blockers\n")
	idxWorkLog := strings.Index(string(out), "## Work Log\n")
	idxRunnerLog := strings.Index(string(out), "## Runner Log\n")
	if idxBlockers == -1 {
		t.Fatalf("serialized note has no ## Blockers section:\n%s", out)
	}
	if !(idxBlockers < idxWorkLog && idxWorkLog < idxRunnerLog) {
		t.Errorf("sections out of order: blockers=%d workLog=%d runnerLog=%d\n%s", idxBlockers, idxWorkLog, idxRunnerLog, out)
	}

	reparsed, err := Parse(out)
	if err != nil {
		t.Fatalf("re-Parse after Bytes(): %v\n%s", err, out)
	}
	if !reparsed.HasBlockers {
		t.Fatal("reparsed note lost HasBlockers")
	}
	if reparsed.Frontmatter.BlockerRound != 2 {
		t.Errorf("reparsed BlockerRound = %d, want 2", reparsed.Frontmatter.BlockerRound)
	}
	if got := CurrentBlockerResolution(reparsed); got != "Rebased and re-ran." {
		t.Errorf("reparsed current resolution = %q, want %q", got, "Rebased and re-ran.")
	}
	if !strings.Contains(reparsed.Blockers, "### Blocker 1") || !strings.Contains(reparsed.Blockers, "### Blocker 2") {
		t.Errorf("reparsed Blockers lost a block:\n%s", reparsed.Blockers)
	}

	// The re-parsed note must serialize to exactly the same bytes.
	again, err := reparsed.Bytes()
	if err != nil {
		t.Fatalf("second Bytes: %v", err)
	}
	if string(again) != string(out) {
		t.Errorf("blockers round-trip not byte-identical.\ngot:\n%s\nwant:\n%s", again, out)
	}
}

func TestNoteWithoutBlockersHasNoSectionOrField(t *testing.T) {
	note, err := Parse([]byte(legacyFinishedNote))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if note.HasBlockers {
		t.Fatal("legacy note parsed with HasBlockers=true")
	}
	if note.Frontmatter.BlockerRound != 0 {
		t.Errorf("legacy note BlockerRound = %d, want 0", note.Frontmatter.BlockerRound)
	}

	out, err := note.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if string(out) != legacyFinishedNote {
		t.Errorf("a note without blockers didn't serialize byte-identically.\ngot:\n%s\nwant:\n%s", out, legacyFinishedNote)
	}
	if strings.Contains(string(out), "## Blockers") {
		t.Errorf("serialized note invented a ## Blockers section:\n%s", out)
	}
	if strings.Contains(string(out), "blocker_round") {
		t.Errorf("serialized note invented a blocker_round field:\n%s", out)
	}
}

func TestParseBlockersBeforeWorkLog(t *testing.T) {
	raw := `---
status: blocked
repo: test-repo
auto_merge: false
engine: null
review_engine: null
session_id: null
pr_url: null
last_updated: null
created: "2026-11-10"
ready_at: null
discord_thread_id: null
blocker_round: 1
---

Do the thing.

## Blockers
### Blocker 1
- Blocked: 2026-11-10T12:00:00Z
- Cause: self-reported

#### Resolution
Filled in by a human.

## Work Log
- Blocked for a while.

## Runner Log
- 2026-11-10T12:00:00Z — blocked
`

	note, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !note.HasBlockers || note.Frontmatter.BlockerRound != 1 {
		t.Fatalf("HasBlockers=%v BlockerRound=%d, want true/1", note.HasBlockers, note.Frontmatter.BlockerRound)
	}
	if got := CurrentBlockerResolution(note); got != "Filled in by a human." {
		t.Errorf("CurrentBlockerResolution = %q", got)
	}
	if note.WorkLog != "- Blocked for a while." {
		t.Errorf("WorkLog = %q, want the Work Log section only", note.WorkLog)
	}
	if strings.Contains(note.Blockers, "Work Log") {
		t.Errorf("Blockers section swallowed the Work Log:\n%s", note.Blockers)
	}

	out, err := note.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if string(out) != raw {
		t.Errorf("round-trip not byte-identical.\ngot:\n%s\nwant:\n%s", out, raw)
	}
}
