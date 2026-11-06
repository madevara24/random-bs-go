package notetask

import (
	"fmt"
	"strings"

	"gopkg.in/yaml.v3"
)

// PlanUnit is one entry in a multi-unit task's plan -- the unit list a
// planner session writes into the "## Plan" section's ```yaml fenced block,
// one unit per later coding session.
type PlanUnit struct {
	ID        string   `yaml:"id"`
	Title     string   `yaml:"title"`
	Paths     []string `yaml:"paths"`
	DependsOn []string `yaml:"depends_on"`
	Accept    string   `yaml:"accept"`
	Verify    string   `yaml:"verify"`
}

const planYAMLFenceOpen = "```yaml"
const planFenceClose = "```"

// ParsePlanUnits parses the first ```yaml fenced block in planSection (the
// Note.Plan text) as a YAML list of PlanUnit. It errors if there is no such
// block, the YAML in it is invalid, the resulting list is empty, an id
// repeats, or a depends_on names an id not present in the list.
func ParsePlanUnits(planSection string) ([]PlanUnit, error) {
	block, ok := firstFencedBlock(planSection, planYAMLFenceOpen)
	if !ok {
		return nil, fmt.Errorf("notetask: no %s fenced block in Plan section", planYAMLFenceOpen)
	}

	var units []PlanUnit
	if err := yaml.Unmarshal([]byte(block), &units); err != nil {
		return nil, fmt.Errorf("notetask: parsing plan units YAML: %w", err)
	}
	if len(units) == 0 {
		return nil, fmt.Errorf("notetask: plan units list is empty")
	}

	ids := make(map[string]bool, len(units))
	for _, u := range units {
		if ids[u.ID] {
			return nil, fmt.Errorf("notetask: duplicate plan unit id %q", u.ID)
		}
		ids[u.ID] = true
	}
	for _, u := range units {
		for _, dep := range u.DependsOn {
			if !ids[dep] {
				return nil, fmt.Errorf("notetask: plan unit %q depends_on unknown id %q", u.ID, dep)
			}
		}
	}

	return units, nil
}

// firstFencedBlock returns the contents of the first fenced block opened by
// openFence (e.g. "```yaml") and closed by a bare "```" line, or false if no
// such block exists.
func firstFencedBlock(text, openFence string) (string, bool) {
	lines := strings.Split(text, "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == openFence {
			start = i
			break
		}
	}
	if start == -1 {
		return "", false
	}
	for i := start + 1; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == planFenceClose {
			return strings.Join(lines[start+1:i], "\n"), true
		}
	}
	return "", false
}

const roundHeadingPrefix = "### "

// CurrentRoundReview returns the trimmed text under the heading
// "### Round <round>" up to the next "### " heading (or the end of
// planReview), or "" if that heading is missing or has no content.
func CurrentRoundReview(planReview string, round int) string {
	heading := fmt.Sprintf("### Round %d", round)
	lines := strings.Split(planReview, "\n")

	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == heading {
			start = i
			break
		}
	}
	if start == -1 {
		return ""
	}

	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), roundHeadingPrefix) {
			end = i
			break
		}
	}
	return strings.TrimSpace(strings.Join(lines[start+1:end], "\n"))
}

// AppendPlanReviewRound appends an empty "### Round <round>" heading to the
// note's Plan Review section, creating the section if it doesn't exist yet.
func AppendPlanReviewRound(n *Note, round int) {
	n.HasPlanReview = true
	heading := fmt.Sprintf("### Round %d", round)
	if strings.TrimSpace(n.PlanReview) == "" {
		n.PlanReview = heading
		return
	}
	n.PlanReview = strings.TrimRight(n.PlanReview, "\n") + "\n\n" + heading
}
