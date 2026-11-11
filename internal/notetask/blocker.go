package notetask

import (
	"fmt"
	"strings"
	"time"
)

const (
	// blockerBlockHeadingPrefix starts every numbered blocker block, e.g.
	// "### Blocker 3". The trailing space keeps it from matching a longer
	// heading such as "### Blockers".
	blockerBlockHeadingPrefix = "### Blocker "
	resolutionHeading         = "#### Resolution"
)

// AppendBlocker numbers and appends a new blocker block to the note's
// "## Blockers" section, creating the section if it doesn't exist yet. It
// increments blocker_round and returns the new round number. The block is:
//
//	### Blocker <n>
//	- Blocked: <ts, RFC 3339 UTC>
//	- Cause: <cause>
//
//	#### Resolution
//
// The resolution is left blank for a human to fill in; see
// FillCurrentBlockerResolution.
func AppendBlocker(note *Note, cause string, ts time.Time) int {
	note.Frontmatter.BlockerRound++
	round := note.Frontmatter.BlockerRound
	note.HasBlockers = true

	block := fmt.Sprintf("%s%d\n- Blocked: %s\n- Cause: %s\n\n%s\n",
		blockerBlockHeadingPrefix, round, ts.UTC().Format(time.RFC3339), cause, resolutionHeading)

	if strings.TrimSpace(note.Blockers) == "" {
		note.Blockers = block
		return round
	}
	note.Blockers = strings.TrimRight(note.Blockers, "\n") + "\n\n" + block
	return round
}

// CurrentBlockerResolution returns the trimmed text under the current
// blocker's "#### Resolution" heading -- the block numbered by
// note.Frontmatter.BlockerRound -- up to the next "### " heading or the end
// of the section. It returns "" when blocker_round is 0, when that heading
// is missing, or when the resolution is blank. An older blocker's
// resolution is never returned.
func CurrentBlockerResolution(note *Note) string {
	if note.Frontmatter.BlockerRound == 0 {
		return ""
	}
	resolution, ok := blockerResolutionText(note.Blockers, note.Frontmatter.BlockerRound)
	if !ok {
		return ""
	}
	return strings.TrimSpace(resolution)
}

// FillCurrentBlockerResolution writes text under the current blocker's
// "#### Resolution" heading, but only when that resolution is currently
// blank -- a resolution a human (or an earlier auto-resume) already filled
// in is left untouched. It returns whether it wrote anything. Callers use
// this before flipping a note to blocker_resolved so the dispatch resume
// check sees a non-empty resolution.
func FillCurrentBlockerResolution(note *Note, text string) bool {
	text = strings.TrimSpace(text)
	if note.Frontmatter.BlockerRound == 0 || text == "" {
		return false
	}

	lines := strings.Split(note.Blockers, "\n")
	blockStart, blockEnd := blockerBlockBounds(lines, note.Frontmatter.BlockerRound)
	if blockStart == -1 {
		return false
	}
	resStart, resEnd := resolutionBounds(lines, blockStart, blockEnd)
	if resStart == -1 {
		return false
	}
	if strings.TrimSpace(strings.Join(lines[resStart+1:resEnd], "\n")) != "" {
		return false
	}

	filled := make([]string, 0, len(lines)+2)
	filled = append(filled, lines[:resStart+1]...)
	filled = append(filled, strings.Split(text, "\n")...)
	filled = append(filled, "")
	filled = append(filled, lines[resEnd:]...)

	note.HasBlockers = true
	note.Blockers = strings.TrimSpace(strings.Join(filled, "\n"))
	return true
}

// blockerBlockBounds finds the line range of the "### Blocker <round>"
// block in lines: start is its heading line, end is the next "### " heading
// or the end of the section. It returns -1, -1 if the heading is absent.
func blockerBlockBounds(lines []string, round int) (start, end int) {
	heading := fmt.Sprintf("%s%d", blockerBlockHeadingPrefix, round)
	start = -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			start = i
			break
		}
	}
	if start == -1 {
		return -1, -1
	}

	end = len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), blockerBlockHeadingPrefix) {
			end = i
			break
		}
	}
	return start, end
}

// resolutionBounds finds the "#### Resolution" heading inside the block
// lines[blockStart:blockEnd]. The resolution text runs from just after that
// heading to the end of the block (the next "### " heading or the end of
// the section). It returns -1, -1 if the heading is absent.
func resolutionBounds(lines []string, blockStart, blockEnd int) (start, end int) {
	for i := blockStart + 1; i < blockEnd; i++ {
		if strings.TrimSpace(lines[i]) == resolutionHeading {
			return i, blockEnd
		}
	}
	return -1, -1
}

// blockerResolutionText returns the raw (untrimmed) text of blocker
// round's resolution, and whether that blocker and its resolution heading
// were both found.
func blockerResolutionText(blockers string, round int) (string, bool) {
	lines := strings.Split(blockers, "\n")
	blockStart, blockEnd := blockerBlockBounds(lines, round)
	if blockStart == -1 {
		return "", false
	}
	resStart, resEnd := resolutionBounds(lines, blockStart, blockEnd)
	if resStart == -1 {
		return "", false
	}
	return strings.Join(lines[resStart+1:resEnd], "\n"), true
}
