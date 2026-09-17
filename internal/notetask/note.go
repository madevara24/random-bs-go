// Package notetask parses and serializes a PM task note: YAML frontmatter
// plus three body sections in order -- the freeform task prompt, "## Work
// Log" (CC's own narrative), and "## Runner Log" (runner-only, append-only,
// never sourced from or written to CC's note-copy).
package notetask

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Frontmatter mirrors the task note schema exactly, field order included --
// yaml.v3 marshals struct fields in declaration order. Nullable fields are
// *string so an absent value round-trips as YAML `null`, not an empty
// string.
type Frontmatter struct {
	Status      string  `yaml:"status"`
	Repo        string  `yaml:"repo"`
	AutoMerge   bool    `yaml:"auto_merge"`
	SessionID   *string `yaml:"session_id"`
	PRURL       *string `yaml:"pr_url"`
	LastUpdated *string `yaml:"last_updated"`
	Created     string  `yaml:"created"`
	ReadyAt     *string `yaml:"ready_at"`
}

// RunnerLogEntry is one line of the "## Runner Log" section.
type RunnerLogEntry struct {
	Timestamp time.Time
	Event     string
}

// Note is a parsed task note. HasWorkLog/HasRunnerLog distinguish "section
// absent" from "section present but empty" -- a freshly authored draft note
// has neither section, and Serialize must not invent them; AppendRunnerLog
// is what turns HasRunnerLog on.
type Note struct {
	Path        string // absolute path on disk; empty if not yet written
	Frontmatter Frontmatter
	Prompt      string

	HasWorkLog bool
	WorkLog    string

	HasRunnerLog bool
	RunnerLog    []RunnerLogEntry
}

const (
	workLogHeading   = "## Work Log"
	runnerLogHeading = "## Runner Log"
)

// Parse splits raw note bytes into frontmatter + body sections. A malformed
// or missing frontmatter delimiter, or YAML that fails to parse, is a hard
// error -- callers (the runner's crash-fallback logic in particular) rely on
// this being all-or-nothing, never partial-credit on a syntax error.
func Parse(data []byte) (*Note, error) {
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	lines := strings.Split(text, "\n")

	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return nil, fmt.Errorf("notetask: missing opening frontmatter delimiter")
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "---" {
			end = i
			break
		}
	}
	if end == -1 {
		return nil, fmt.Errorf("notetask: unterminated frontmatter (no closing \"---\")")
	}

	fmText := strings.Join(lines[1:end], "\n")
	var fm Frontmatter
	if err := yaml.Unmarshal([]byte(fmText), &fm); err != nil {
		return nil, fmt.Errorf("notetask: parsing frontmatter YAML: %w", err)
	}

	bodyLines := lines[end+1:]
	// Drop one leading blank line (the blank line conventionally separating
	// the closing "---" from the body), if present.
	if len(bodyLines) > 0 && strings.TrimSpace(bodyLines[0]) == "" {
		bodyLines = bodyLines[1:]
	}

	workLogLine, runnerLogLine := -1, -1
	for i, l := range bodyLines {
		t := strings.TrimSpace(l)
		if t == workLogHeading && workLogLine == -1 {
			workLogLine = i
		} else if t == runnerLogHeading && runnerLogLine == -1 {
			runnerLogLine = i
		}
	}

	note := &Note{Frontmatter: fm}

	promptEnd := len(bodyLines)
	if workLogLine != -1 {
		promptEnd = workLogLine
	} else if runnerLogLine != -1 {
		promptEnd = runnerLogLine
	}
	note.Prompt = strings.TrimSpace(strings.Join(bodyLines[:promptEnd], "\n"))

	if workLogLine != -1 {
		note.HasWorkLog = true
		workLogEnd := len(bodyLines)
		if runnerLogLine != -1 {
			workLogEnd = runnerLogLine
		}
		note.WorkLog = strings.TrimSpace(strings.Join(bodyLines[workLogLine+1:workLogEnd], "\n"))
	}

	if runnerLogLine != -1 {
		note.HasRunnerLog = true
		section := strings.TrimSpace(strings.Join(bodyLines[runnerLogLine+1:], "\n"))
		entries, err := parseRunnerLog(section)
		if err != nil {
			return nil, err
		}
		note.RunnerLog = entries
	}

	return note, nil
}

func parseRunnerLog(section string) ([]RunnerLogEntry, error) {
	var entries []RunnerLogEntry
	if section == "" {
		return entries, nil
	}
	for _, line := range strings.Split(section, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "- ")
		line = strings.TrimPrefix(line, "-")
		line = strings.TrimSpace(line)
		parts := strings.SplitN(line, " — ", 2)
		if len(parts) != 2 {
			return nil, fmt.Errorf("notetask: malformed Runner Log line: %q", line)
		}
		ts, err := time.Parse(time.RFC3339, strings.TrimSpace(parts[0]))
		if err != nil {
			return nil, fmt.Errorf("notetask: malformed Runner Log timestamp %q: %w", parts[0], err)
		}
		entries = append(entries, RunnerLogEntry{Timestamp: ts, Event: strings.TrimSpace(parts[1])})
	}
	return entries, nil
}

// Bytes serializes the note back to disk-ready form. Sections not present
// (HasWorkLog/HasRunnerLog false) are omitted entirely, not written as
// empty headings -- a note dispatch only touches frontmatter on should come
// back byte-identical in its body.
func (n *Note) Bytes() ([]byte, error) {
	var buf strings.Builder

	fmBytes, err := yaml.Marshal(n.Frontmatter)
	if err != nil {
		return nil, fmt.Errorf("notetask: marshaling frontmatter: %w", err)
	}
	buf.WriteString("---\n")
	buf.Write(fmBytes)
	buf.WriteString("---\n\n")

	buf.WriteString(strings.TrimRight(n.Prompt, "\n"))
	buf.WriteString("\n")

	if n.HasWorkLog {
		buf.WriteString("\n" + workLogHeading + "\n")
		if wl := strings.TrimSpace(n.WorkLog); wl != "" {
			buf.WriteString(wl)
			buf.WriteString("\n")
		}
	}

	if n.HasRunnerLog {
		buf.WriteString("\n" + runnerLogHeading + "\n")
		for _, e := range n.RunnerLog {
			buf.WriteString(fmt.Sprintf("- %s — %s\n", e.Timestamp.UTC().Format(time.RFC3339), e.Event))
		}
	}

	return []byte(buf.String()), nil
}

// AppendRunnerLog appends one "timestamp — event" line to the note's Runner
// Log, creating the section if it doesn't exist yet. Touches nothing else.
func AppendRunnerLog(note *Note, event string, ts time.Time) {
	note.HasRunnerLog = true
	note.RunnerLog = append(note.RunnerLog, RunnerLogEntry{Timestamp: ts.UTC(), Event: event})
}

// SortByReadyThenCreated orders notes oldest-ready_at-first, tie-broken by
// oldest-created -- the dispatcher's claim ordering. Notes with an empty
// ready_at sort last (shouldn't happen in practice since the dispatcher
// stamps it before this ordering step ever runs, but fail safe rather than
// panic on a malformed/edited-by-hand note).
func SortByReadyThenCreated(notes []*Note) {
	sort.SliceStable(notes, func(i, j int) bool {
		ri, rj := readyAtOf(notes[i]), readyAtOf(notes[j])
		if !ri.Equal(rj) {
			return ri.Before(rj)
		}
		ci, cj := createdOf(notes[i]), createdOf(notes[j])
		return ci.Before(cj)
	})
}

var maxTime = time.Unix(1<<62, 0)

func readyAtOf(n *Note) time.Time {
	if n.Frontmatter.ReadyAt == nil || *n.Frontmatter.ReadyAt == "" {
		return maxTime
	}
	t, err := time.Parse(time.RFC3339, *n.Frontmatter.ReadyAt)
	if err != nil {
		return maxTime
	}
	return t
}

func createdOf(n *Note) time.Time {
	if n.Frontmatter.Created == "" {
		return maxTime
	}
	// created may be a bare date (2026-09-15) or a full timestamp.
	if t, err := time.Parse(time.RFC3339, n.Frontmatter.Created); err == nil {
		return t
	}
	if t, err := time.Parse("2006-01-02", n.Frontmatter.Created); err == nil {
		return t
	}
	return maxTime
}
