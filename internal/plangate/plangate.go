// Package plangate holds the pure decision rules that sit between a task
// and Jev (internal/jev), TypeSafe's judgment model, at two points: triage
// (should a task run as multi-unit?) and the plan gate (may a planner's
// unit list run without human approval?). Nothing here calls Jev itself --
// callers build the questions, call jev.Client.Ask, and pass the answers
// in.
package plangate

import (
	"fmt"
	"strings"

	"github.com/madevara24/random-bs-go/internal/jev"
	"github.com/madevara24/random-bs-go/internal/notetask"
)

// sizeInstructions is TriageQuestion's "size" noul instructions, verbatim
// per the task spec -- changing the wording changes what Jev is asked, so
// it's kept as a named constant rather than inlined twice (here and in any
// test asserting it).
const sizeInstructions = "Is the work described in `task.prompt` too large for one coding session to finish, i.e. does it span several independent changes that each need their own reading, editing, and testing?"

// TriageQuestion returns the single question asked at triage time: is this
// task too big for one coding session (and so should run as multi-unit)?
func TriageQuestion() map[string]jev.Question {
	return map[string]jev.Question{
		"size": {Type: "noul", Instructions: sizeInstructions},
	}
}

// DecideTriage turns TriageQuestion's answer into a multi-unit decision.
// askErr set, or a missing/non-noul "size" answer, both fail safe to
// single-unit (false). The returned logLine is always safe to write to the
// task note/log as-is.
func DecideTriage(answers map[string]jev.Answer, askErr error) (multiUnit bool, logLine string) {
	if askErr != nil {
		return false, fmt.Sprintf("triage multi_unit=false size=error: %v", askErr)
	}
	size, ok := answers["size"]
	if !ok || size.Noul == nil {
		return false, "triage multi_unit=false size=error: missing size answer"
	}
	multiUnit = *size.Noul >= 0.5
	return multiUnit, fmt.Sprintf("triage multi_unit=%t size=%.2f", multiUnit, *size.Noul)
}

// riskyInstructions is GateQuestions' "risky" noul instructions, verbatim.
const riskyInstructions = "Does any unit in `plan.units` change data, database schemas, authentication, infrastructure, deploy configuration, or a public API contract?"

// ambiguousInstructions is GateQuestions' "ambiguous" noul instructions,
// verbatim.
const ambiguousInstructions = "Does `plan` rest on a guess about what `task.prompt` meant, where another reasonable reading would produce a different plan?"

// GateQuestions returns the full question set for the plan gate: the
// planner's own choice questions (asked again here so Jev can judge their
// confidence against the finished plan) plus two fixed nouls, "risky" and
// "ambiguous".
func GateQuestions(plannerQuestions map[string]jev.Question) map[string]jev.Question {
	questions := make(map[string]jev.Question, len(plannerQuestions)+2)
	for id, q := range plannerQuestions {
		questions[id] = q
	}
	questions["risky"] = jev.Question{Type: "noul", Instructions: riskyInstructions}
	questions["ambiguous"] = jev.Question{Type: "noul", Instructions: ambiguousInstructions}
	return questions
}

// hardRulePaths are substrings that, when present in a plan unit's path,
// make that unit unconditionally gate-stopping regardless of what Jev
// says -- migrations, workflows, and Dockerfiles are exactly the changes a
// human should see before they run unattended.
var hardRulePaths = []string{"migrations/", ".github/workflows/", "Dockerfile"}

// maxUnitsWithoutReason is the unit count above which HardRules adds a
// reason purely for plan size, independent of what the units touch.
const maxUnitsWithoutReason = 8

// HardRules returns one reason per plan unit whose path matches a
// hard-stop pattern (migrations/, .github/workflows/, or Dockerfile), plus
// one more reason if the plan has more than 8 units. An empty result means
// no hard rule fired -- it does not by itself mean the gate passes, see
// DecideGate.
func HardRules(units []notetask.PlanUnit) []string {
	var reasons []string
	for _, u := range units {
		for _, path := range u.Paths {
			for _, pattern := range hardRulePaths {
				if strings.Contains(path, pattern) {
					reasons = append(reasons, fmt.Sprintf("unit %q touches %q, which matches hard-stop pattern %q", u.ID, path, pattern))
				}
			}
		}
	}
	if len(units) > maxUnitsWithoutReason {
		reasons = append(reasons, fmt.Sprintf("plan has %d units, over the %d-unit hard limit", len(units), maxUnitsWithoutReason))
	}
	return reasons
}

// Category is the section a stop reason is displayed under in the
// plan-review message. The values are the headings' text verbatim.
type Category string

const (
	// CategoryHardRules is everything HardRules returns: hard-stop path
	// matches and the over-limit unit count.
	CategoryHardRules Category = "Hard rules"
	// CategoryJevClassifier is a risky/ambiguous noul that is a clear yes
	// or unresolved.
	CategoryJevClassifier Category = "Jev classifier"
	// CategoryPlannerQuestions is a planner choice question that is
	// =none, low confidence, or has a small top-two probability gap.
	CategoryPlannerQuestions Category = "Planner questions"
	// CategoryErrors is Jev being unavailable and every missing answer.
	CategoryErrors Category = "Errors"
)

// Reason is one stop reason together with the category it belongs to.
type Reason struct {
	Category Category
	Text     string
}

// Result is DecideGate's verdict: Stop is true if a human must approve the
// plan before it runs, and Reasons names every condition that triggered
// that, one entry per condition. Categorized carries the same reasons, in
// the same order, each tagged with its display category; Reasons stays the
// flat text existing callers and the Runner Log expect.
type Result struct {
	Stop        bool
	Reasons     []string
	Categorized []Reason
}

// noulResolvedLow and noulResolvedHigh bound the "unresolved" band for a
// risky/ambiguous noul answer: anywhere from 0.35 to 0.65 inclusive counts
// as Jev being unsure, which stops the gate exactly like a clear "yes"
// (>= 0.65) does -- only a clear "no" (< 0.35) lets that noul pass.
const noulResolvedLow = 0.35
const noulResolvedHigh = 0.65
const noulStopThreshold = 0.65

// choiceMinConfidence is the minimum confidence a planner choice answer
// must have to pass the gate.
const choiceMinConfidence = 0.6

// choiceMinProbabilityGap is the minimum gap required between a choice
// answer's top two probabilities to pass the gate -- a close second place
// means Jev itself isn't sure, same as low confidence.
const choiceMinProbabilityGap = 0.15

// DecideGate combines HardRules' output with Jev's gate answers into a
// single stop/go verdict. hard is HardRules' return value. answers is the
// full answer set for GateQuestions' question set. plannerQuestionIDs
// names which answer IDs are the planner's own choice questions (as
// opposed to "risky"/"ambiguous"). askErr, if set, means Jev was never
// reached at all -- every other check is skipped since there are no
// answers to check, and the gate stops on that alone.
func DecideGate(hard []string, answers map[string]jev.Answer, plannerQuestionIDs []string, askErr error) Result {
	if askErr != nil {
		text := fmt.Sprintf("jev unavailable: %v", askErr)
		return Result{
			Stop:        true,
			Reasons:     []string{text},
			Categorized: []Reason{{Category: CategoryErrors, Text: text}},
		}
	}

	var reasons []Reason
	for _, text := range hard {
		reasons = append(reasons, Reason{Category: CategoryHardRules, Text: text})
	}

	for _, id := range []string{"risky", "ambiguous"} {
		a, ok := answers[id]
		if !ok || a.Noul == nil {
			reasons = append(reasons, Reason{Category: CategoryErrors, Text: fmt.Sprintf("missing answer for %q", id)})
			continue
		}
		p := *a.Noul
		switch {
		case p >= noulStopThreshold:
			reasons = append(reasons, Reason{Category: CategoryJevClassifier, Text: fmt.Sprintf("%s=%.2f is a clear yes", id, p)})
		case p >= noulResolvedLow && p <= noulResolvedHigh:
			reasons = append(reasons, Reason{Category: CategoryJevClassifier, Text: fmt.Sprintf("%s=%.2f is unresolved", id, p)})
		}
	}

	for _, id := range plannerQuestionIDs {
		a, ok := answers[id]
		if !ok {
			reasons = append(reasons, Reason{Category: CategoryErrors, Text: fmt.Sprintf("missing answer for %q", id)})
			continue
		}
		if a.Choice == "none" {
			reasons = append(reasons, Reason{Category: CategoryPlannerQuestions, Text: fmt.Sprintf("%s=none", id)})
			continue
		}
		if a.Confidence < choiceMinConfidence {
			reasons = append(reasons, Reason{Category: CategoryPlannerQuestions, Text: fmt.Sprintf("%s confidence=%.2f is below %.2f", id, a.Confidence, choiceMinConfidence)})
			continue
		}
		if gap, ok := topTwoGap(a.Probabilities); ok && gap < choiceMinProbabilityGap {
			reasons = append(reasons, Reason{Category: CategoryPlannerQuestions, Text: fmt.Sprintf("%s top two probabilities differ by only %.2f", id, gap)})
		}
	}

	result := Result{Stop: len(reasons) > 0, Categorized: reasons}
	if len(reasons) > 0 {
		result.Reasons = make([]string, len(reasons))
		for i, r := range reasons {
			result.Reasons[i] = r.Text
		}
	}
	return result
}

// topTwoGap returns the gap between the two highest values in probs, and
// false if probs has fewer than two entries.
func topTwoGap(probs map[string]float64) (float64, bool) {
	if len(probs) < 2 {
		return 0, false
	}
	top1, top2 := -1.0, -1.0
	for _, p := range probs {
		switch {
		case p > top1:
			top2 = top1
			top1 = p
		case p > top2:
			top2 = p
		}
	}
	return top1 - top2, true
}
