package notetask

import (
	"strings"
	"testing"
	"time"
)

const sampleNote = `---
status: ready
repo: test-repo
auto_merge: false
session_id: null
pr_url: null
last_updated: null
created: 2026-09-15
ready_at: null
---

Add a one-line comment to README.md.

## Work Log
- Started work.

## Runner Log
- 2026-09-15T10:03:00Z — queued
- 2026-09-15T10:04:12Z — in_progress
`

func TestParseBasic(t *testing.T) {
	note, err := Parse([]byte(sampleNote))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if note.Frontmatter.Status != "ready" {
		t.Errorf("Status = %q, want %q", note.Frontmatter.Status, "ready")
	}
	if note.Frontmatter.SessionID != nil {
		t.Errorf("SessionID = %v, want nil", *note.Frontmatter.SessionID)
	}
	if note.Prompt != "Add a one-line comment to README.md." {
		t.Errorf("Prompt = %q", note.Prompt)
	}
	if !note.HasWorkLog || note.WorkLog != "- Started work." {
		t.Errorf("WorkLog = %q (has=%v)", note.WorkLog, note.HasWorkLog)
	}
	if !note.HasRunnerLog || len(note.RunnerLog) != 2 {
		t.Fatalf("RunnerLog = %+v (has=%v)", note.RunnerLog, note.HasRunnerLog)
	}
	if note.RunnerLog[0].Event != "queued" || note.RunnerLog[1].Event != "in_progress" {
		t.Errorf("RunnerLog events = %q, %q", note.RunnerLog[0].Event, note.RunnerLog[1].Event)
	}
}

func TestParseMinimalNote(t *testing.T) {
	minimal := "---\nstatus: draft\nrepo: test-repo\nauto_merge: false\nsession_id: null\npr_url: null\nlast_updated: null\ncreated: 2026-09-15\nready_at: null\n---\n\nJust a prompt, no sections yet.\n"
	note, err := Parse([]byte(minimal))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if note.HasWorkLog || note.HasRunnerLog {
		t.Errorf("expected no body sections, got HasWorkLog=%v HasRunnerLog=%v", note.HasWorkLog, note.HasRunnerLog)
	}
	if note.Prompt != "Just a prompt, no sections yet." {
		t.Errorf("Prompt = %q", note.Prompt)
	}

	out, err := note.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if strings.Contains(string(out), "## Work Log") || strings.Contains(string(out), "## Runner Log") {
		t.Errorf("Serialize invented a body section that wasn't present:\n%s", out)
	}
}

func TestParseMalformedFrontmatterFails(t *testing.T) {
	bad := "---\nstatus: [unterminated\n---\n\nBody.\n"
	if _, err := Parse([]byte(bad)); err == nil {
		t.Fatal("expected an error parsing malformed frontmatter YAML, got nil")
	}
}

func TestParseMissingDelimiterFails(t *testing.T) {
	if _, err := Parse([]byte("no frontmatter here")); err == nil {
		t.Fatal("expected an error for missing frontmatter delimiter, got nil")
	}
}

func TestAppendRunnerLogAndRoundTrip(t *testing.T) {
	note, err := Parse([]byte(sampleNote))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	AppendRunnerLog(note, "done", time.Date(2026, 9, 15, 10, 22, 47, 0, time.UTC))

	out, err := note.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}

	reparsed, err := Parse(out)
	if err != nil {
		t.Fatalf("re-Parse after Bytes(): %v\n%s", err, out)
	}
	if len(reparsed.RunnerLog) != 3 {
		t.Fatalf("RunnerLog after append = %d entries, want 3: %+v", len(reparsed.RunnerLog), reparsed.RunnerLog)
	}
	if reparsed.RunnerLog[2].Event != "done" {
		t.Errorf("last RunnerLog event = %q, want %q", reparsed.RunnerLog[2].Event, "done")
	}
}

func TestSortByReadyThenCreated(t *testing.T) {
	mk := func(readyAt, created string) *Note {
		var ra *string
		if readyAt != "" {
			ra = &readyAt
		}
		return &Note{Frontmatter: Frontmatter{ReadyAt: ra, Created: created}}
	}
	notes := []*Note{
		mk("2026-09-15T12:00:00Z", "2026-09-14"),
		mk("2026-09-15T10:00:00Z", "2026-09-13"),
		mk("2026-09-15T10:00:00Z", "2026-09-10"), // same ready_at, older created -> should come first among ties
	}
	SortByReadyThenCreated(notes)
	if notes[0].Frontmatter.Created != "2026-09-10" {
		t.Errorf("notes[0].Created = %q, want tie-break winner 2026-09-10", notes[0].Frontmatter.Created)
	}
	if notes[2].Frontmatter.Created != "2026-09-14" {
		t.Errorf("notes[2].Created = %q, want latest ready_at last", notes[2].Frontmatter.Created)
	}
}
