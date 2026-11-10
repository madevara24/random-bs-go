package plangate

import (
	"errors"
	"strings"
	"testing"

	"github.com/madevara24/random-bs-go/internal/jev"
	"github.com/madevara24/random-bs-go/internal/notetask"
)

func noul(p float64) jev.Answer { return jev.Answer{Type: "noul", Noul: &p} }

func TestDecideTriage(t *testing.T) {
	t.Run("at threshold is multi-unit", func(t *testing.T) {
		multi, _ := DecideTriage(map[string]jev.Answer{"size": noul(0.5)}, nil)
		if !multi {
			t.Error("want multiUnit=true at size=0.5")
		}
	})
	t.Run("just under threshold is single-unit", func(t *testing.T) {
		multi, _ := DecideTriage(map[string]jev.Answer{"size": noul(0.49)}, nil)
		if multi {
			t.Error("want multiUnit=false at size=0.49")
		}
	})
	t.Run("ask error fails safe to single-unit", func(t *testing.T) {
		multi, logLine := DecideTriage(nil, errors.New("boom"))
		if multi {
			t.Error("want multiUnit=false on askErr")
		}
		if !strings.Contains(logLine, "size=error:") {
			t.Errorf("logLine = %q, want it to contain %q", logLine, "size=error:")
		}
	})
	t.Run("missing size answer fails safe to single-unit", func(t *testing.T) {
		multi, _ := DecideTriage(map[string]jev.Answer{}, nil)
		if multi {
			t.Error("want multiUnit=false when size is missing")
		}
	})
}

func TestHardRules(t *testing.T) {
	t.Run("a migrations path is one reason", func(t *testing.T) {
		units := []notetask.PlanUnit{{ID: "u1", Paths: []string{"db/migrations/0003.sql"}}}
		reasons := HardRules(units)
		if len(reasons) != 1 {
			t.Errorf("reasons = %v, want 1", reasons)
		}
	})
	t.Run("more than 8 units is one reason", func(t *testing.T) {
		var units []notetask.PlanUnit
		for i := 0; i < 9; i++ {
			units = append(units, notetask.PlanUnit{ID: "u", Paths: []string{"internal/foo.go"}})
		}
		reasons := HardRules(units)
		if len(reasons) != 1 {
			t.Errorf("reasons = %v, want 1", reasons)
		}
	})
	t.Run("a clean plan has no reasons", func(t *testing.T) {
		units := []notetask.PlanUnit{
			{ID: "u1", Paths: []string{"internal/foo.go"}},
			{ID: "u2", Paths: []string{"internal/bar.go"}},
			{ID: "u3", Paths: []string{"internal/baz.go"}},
		}
		reasons := HardRules(units)
		if len(reasons) != 0 {
			t.Errorf("reasons = %v, want none", reasons)
		}
	})
}

func TestDecideGate(t *testing.T) {
	cases := []struct {
		name       string
		hard       []string
		answers    map[string]jev.Answer
		plannerIDs []string
		askErr     error
		wantStop   bool
	}{
		{
			name:     "clear risky and ambiguous does not stop",
			answers:  map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.2)},
			wantStop: false,
		},
		{
			name:     "risky clear yes stops",
			answers:  map[string]jev.Answer{"risky": noul(0.7), "ambiguous": noul(0.2)},
			wantStop: true,
		},
		{
			name:     "ambiguous unresolved stops",
			answers:  map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.5)},
			wantStop: true,
		},
		{
			name:       "planner choice low confidence stops",
			answers:    map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.2), "approach": {Type: "choice", Choice: "a", Confidence: 0.55, Probabilities: map[string]float64{"a": 0.55, "b": 0.45}}},
			plannerIDs: []string{"approach"},
			wantStop:   true,
		},
		{
			name:       "planner choice close top two stops",
			answers:    map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.2), "approach": {Type: "choice", Choice: "a", Confidence: 0.8, Probabilities: map[string]float64{"a": 0.45, "b": 0.40, "c": 0.15}}},
			plannerIDs: []string{"approach"},
			wantStop:   true,
		},
		{
			name:       "planner choice none stops",
			answers:    map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.2), "approach": {Type: "choice", Choice: "none", Confidence: 0.9}},
			plannerIDs: []string{"approach"},
			wantStop:   true,
		},
		{
			name:     "hard rule stops even with clear jev",
			hard:     []string{"unit touches migrations/"},
			answers:  map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.2)},
			wantStop: true,
		},
		{
			name:     "ask error stops",
			askErr:   errors.New("rate limited"),
			wantStop: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := DecideGate(tc.hard, tc.answers, tc.plannerIDs, tc.askErr)
			if result.Stop != tc.wantStop {
				t.Errorf("Stop = %v, want %v (reasons: %v)", result.Stop, tc.wantStop, result.Reasons)
			}
			if result.Stop && len(result.Reasons) == 0 {
				t.Error("Stop=true but no reasons given")
			}
		})
	}
}

func TestDecideGateCategories(t *testing.T) {
	units := []notetask.PlanUnit{{ID: "u1", Paths: []string{"db/migrations/0003.sql"}}}
	var tooMany []notetask.PlanUnit
	for i := 0; i < 9; i++ {
		tooMany = append(tooMany, notetask.PlanUnit{ID: "u", Paths: []string{"internal/foo.go"}})
	}

	cases := []struct {
		name       string
		hard       []string
		answers    map[string]jev.Answer
		plannerIDs []string
		askErr     error
		want       []Reason
	}{
		{
			name:    "hard-stop path is Hard rules",
			hard:    HardRules(units),
			answers: map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.2)},
			want: []Reason{{
				Category: CategoryHardRules,
				Text:     `unit "u1" touches "db/migrations/0003.sql", which matches hard-stop pattern "migrations/"`,
			}},
		},
		{
			name:    "over-unit-limit reason is Hard rules",
			hard:    HardRules(tooMany),
			answers: map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.2)},
			want: []Reason{{
				Category: CategoryHardRules,
				Text:     "plan has 9 units, over the 8-unit hard limit",
			}},
		},
		{
			name:    "risky clear yes is Jev classifier",
			answers: map[string]jev.Answer{"risky": noul(0.7), "ambiguous": noul(0.2)},
			want: []Reason{{
				Category: CategoryJevClassifier,
				Text:     "risky=0.70 is a clear yes",
			}},
		},
		{
			name:    "ambiguous unresolved is Jev classifier",
			answers: map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.5)},
			want: []Reason{{
				Category: CategoryJevClassifier,
				Text:     "ambiguous=0.50 is unresolved",
			}},
		},
		{
			name:       "planner none is Planner questions",
			answers:    map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.2), "approach": {Type: "choice", Choice: "none", Confidence: 0.9}},
			plannerIDs: []string{"approach"},
			want: []Reason{{
				Category: CategoryPlannerQuestions,
				Text:     "approach=none",
			}},
		},
		{
			name:       "planner low confidence is Planner questions",
			answers:    map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.2), "approach": {Type: "choice", Choice: "a", Confidence: 0.55, Probabilities: map[string]float64{"a": 0.55, "b": 0.45}}},
			plannerIDs: []string{"approach"},
			want: []Reason{{
				Category: CategoryPlannerQuestions,
				Text:     "approach confidence=0.55 is below 0.60",
			}},
		},
		{
			name:       "planner small top-two gap is Planner questions",
			answers:    map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.2), "approach": {Type: "choice", Choice: "a", Confidence: 0.8, Probabilities: map[string]float64{"a": 0.45, "b": 0.40, "c": 0.15}}},
			plannerIDs: []string{"approach"},
			want: []Reason{{
				Category: CategoryPlannerQuestions,
				Text:     "approach top two probabilities differ by only 0.05",
			}},
		},
		{
			name:    "missing noul answer is Errors",
			answers: map[string]jev.Answer{"risky": noul(0.1)},
			want: []Reason{{
				Category: CategoryErrors,
				Text:     `missing answer for "ambiguous"`,
			}},
		},
		{
			name:       "missing planner answer is Errors",
			answers:    map[string]jev.Answer{"risky": noul(0.1), "ambiguous": noul(0.2)},
			plannerIDs: []string{"approach"},
			want: []Reason{{
				Category: CategoryErrors,
				Text:     `missing answer for "approach"`,
			}},
		},
		{
			name:   "jev unavailable is Errors",
			askErr: errors.New("rate limited"),
			want: []Reason{{
				Category: CategoryErrors,
				Text:     "jev unavailable: rate limited",
			}},
		},
		{
			name: "mixed case keeps each reason with its category in order",
			hard: HardRules(units),
			answers: map[string]jev.Answer{
				"risky":     noul(0.7),
				"ambiguous": noul(0.2),
				"approach":  {Type: "choice", Choice: "none", Confidence: 0.9},
			},
			plannerIDs: []string{"approach", "missing"},
			want: []Reason{
				{Category: CategoryHardRules, Text: `unit "u1" touches "db/migrations/0003.sql", which matches hard-stop pattern "migrations/"`},
				{Category: CategoryJevClassifier, Text: "risky=0.70 is a clear yes"},
				{Category: CategoryPlannerQuestions, Text: "approach=none"},
				{Category: CategoryErrors, Text: `missing answer for "missing"`},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := DecideGate(tc.hard, tc.answers, tc.plannerIDs, tc.askErr)
			if !result.Stop {
				t.Fatal("Stop = false, want true")
			}
			if len(result.Reasons) != len(result.Categorized) {
				t.Fatalf("Reasons has %d entries, Categorized has %d", len(result.Reasons), len(result.Categorized))
			}
			for i, want := range tc.want {
				if i >= len(result.Categorized) {
					t.Fatalf("Categorized has %d entries, want at least %d", len(result.Categorized), len(tc.want))
				}
				got := result.Categorized[i]
				if got != want {
					t.Errorf("Categorized[%d] = %+v, want %+v", i, got, want)
				}
				if result.Reasons[i] != want.Text {
					t.Errorf("Reasons[%d] = %q, want %q", i, result.Reasons[i], want.Text)
				}
			}
			if len(result.Categorized) != len(tc.want) {
				t.Errorf("Categorized = %+v, want %+v", result.Categorized, tc.want)
			}
		})
	}
}

func TestRenderPlanReviewMessage(t *testing.T) {
	t.Run("all four categories render in fixed order", func(t *testing.T) {
		reasons := []Reason{
			{Category: CategoryErrors, Text: `missing answer for "missing"`},
			{Category: CategoryPlannerQuestions, Text: "approach=none"},
			{Category: CategoryHardRules, Text: `unit "u1" touches "db/migrations/0003.sql", which matches hard-stop pattern "migrations/"`},
			{Category: CategoryJevClassifier, Text: "risky=0.70 is a clear yes"},
		}
		got := RenderPlanReviewMessage("<@123>", "rbg-53", "random-bs-go", 2, reasons)
		want := "<@123> Task `rbg-53` (random-bs-go)'s plan needs review.\n" +
			"\n" +
			"**Hard rules**\n" +
			"- unit \"u1\" touches \"db/migrations/0003.sql\", which matches hard-stop pattern \"migrations/\"\n" +
			"**Jev classifier**\n" +
			"- risky=0.70 is a clear yes\n" +
			"**Planner questions**\n" +
			"- approach=none\n" +
			"**Errors**\n" +
			"- missing answer for \"missing\"\n" +
			"\n" +
			"React ✅ to approve, or ❌ and write your feedback under `### Round 2` in `## Plan Review`."
		if got != want {
			t.Errorf("RenderPlanReviewMessage() =\n%s\n\nwant:\n%s", got, want)
		}
	})

	t.Run("empty categories are omitted, not printed as headings", func(t *testing.T) {
		reasons := []Reason{
			{Category: CategoryJevClassifier, Text: "ambiguous=0.50 is unresolved"},
			{Category: CategoryErrors, Text: "jev unavailable: rate limited"},
		}
		got := RenderPlanReviewMessage("", "slug", "repo", 1, reasons)
		want := "Task `slug` (repo)'s plan needs review.\n" +
			"\n" +
			"**Jev classifier**\n" +
			"- ambiguous=0.50 is unresolved\n" +
			"**Errors**\n" +
			"- jev unavailable: rate limited\n" +
			"\n" +
			"React ✅ to approve, or ❌ and write your feedback under `### Round 1` in `## Plan Review`."
		if got != want {
			t.Errorf("RenderPlanReviewMessage() =\n%s\n\nwant:\n%s", got, want)
		}
		if strings.Contains(got, "**Hard rules**") {
			t.Errorf("message contains an empty Hard rules heading:\n%s", got)
		}
		if strings.Contains(got, "**Planner questions**") {
			t.Errorf("message contains an empty Planner questions heading:\n%s", got)
		}
	})

	t.Run("mention is present or absent without extra whitespace", func(t *testing.T) {
		reasons := []Reason{{Category: CategoryHardRules, Text: "hard reason"}}
		withMention := RenderPlanReviewMessage("<@123>", "slug", "repo", 1, reasons)
		if !strings.HasPrefix(withMention, "<@123> Task `slug`") {
			t.Errorf("with mention, message starts %q, want the mention then the task line", withMention)
		}
		withoutMention := RenderPlanReviewMessage("", "slug", "repo", 1, reasons)
		if !strings.HasPrefix(withoutMention, "Task `slug`") {
			t.Errorf("without mention, message starts %q, want the task line with no leading space", withoutMention)
		}
	})
}
