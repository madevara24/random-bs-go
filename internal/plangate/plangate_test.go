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
