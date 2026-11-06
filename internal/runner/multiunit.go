// The multi-unit path's entry points: triage (deciding whether a fresh
// task should run as multi-unit) and processMultiUnit (ProcessTask's
// branch once a task is triaged multi-unit). Both gated behind
// Deps.MultiUnitEnabled -- off by default, see internal/config/config.go's
// MultiUnitEnabled doc comment. Sibling to planner.go, which this file
// hands off to for the "no ## Plan yet" case.
package runner

import (
	"context"
	"strings"
	"time"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/jev"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/plangate"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// triageTimeout bounds how long triagePath waits on Jev's systemone
// endpoint -- a hung triage call must never stall a fresh task's setup
// indefinitely.
const triageTimeout = 30 * time.Second

// oneLineMaxRunes is the rune cap every Jev-derived Runner Log line goes
// through oneLine with.
const oneLineMaxRunes = 200

// oneLine collapses s to one line (newlines/tabs/carriage returns become
// spaces) and cuts it to at most max runes, appending "…" if it was cut.
// jev.Client.Ask's own error text embeds Jev's raw HTTP response body
// verbatim, which can contain newlines -- and notetask.Parse hard-fails on
// a multi-line Runner Log entry (see notetask's parseRunnerLog), which
// would make the whole note unreadable. Every Jev-derived string this
// package writes to the Runner Log goes through this first.
func oneLine(s string, max int) string {
	replacer := strings.NewReplacer("\n", " ", "\t", " ", "\r", " ")
	out := replacer.Replace(s)
	runes := []rune(out)
	if len(runes) <= max {
		return out
	}
	if max <= 0 {
		return ""
	}
	return string(runes[:max-1]) + "…"
}

// triagePath decides whether job's task should run as multi-unit, asking
// Jev only when every one of these holds: the flag is on, this is a fresh
// run (no session_id yet), and the author never pinned multi_unit
// themselves. Every other case returns (false, "") -- nothing happened
// worth recording, and in particular an author override is honored
// exactly, with zero Jev requests.
func triagePath(deps Deps, job worker.Job, note *notetask.Note) (multiUnit bool, logLine string) {
	if !deps.MultiUnitEnabled {
		return false, ""
	}
	if note.Frontmatter.SessionID != nil && *note.Frontmatter.SessionID != "" {
		return false, ""
	}
	if note.Frontmatter.MultiUnit != nil {
		return *note.Frontmatter.MultiUnit, ""
	}

	state := map[string]any{
		"task": map[string]any{
			"title":  taskTitle(job.NotePath),
			"prompt": note.Prompt,
		},
		"repo": job.Repo,
	}

	answers, askErr := askJev(deps.Jev, state, plangate.TriageQuestion(), triageTimeout)
	multiUnit, logLine = plangate.DecideTriage(answers, askErr)
	return multiUnit, oneLine(logLine, oneLineMaxRunes)
}

// askJev is triagePath/runPlanGate's shared call shape: a nil client
// (Deps.Jev never wired in) is treated exactly like jev.ErrNoAPIKey rather
// than panicking, the same fail-safe-to-single-session/always-stop
// behavior an empty TYPESAFE_API_KEY produces via the real client.
func askJev(client *jev.Client, state any, questions map[string]jev.Question, timeout time.Duration) (map[string]jev.Answer, error) {
	if client == nil {
		return nil, jev.ErrNoAPIKey
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return client.Ask(ctx, state, questions)
}

// processMultiUnit is ProcessTask's branch for a task triaged multi-unit:
// "no ## Plan yet" runs the planner through the plan gate (which itself
// hands a clean pass straight into runUnitLoop, see planner.go's
// runPlanGate), and "units already seeded, not all done" re-enters
// runUnitLoop directly -- the normal resume path, and also how a task
// whose status flipped blocked -> blocker_resolved (main.go's auto-resume,
// e.g. after a usage limit cleared mid-unit) picks back up at
// current_unit: ProcessTask's own resume branch reaches here exactly the
// same way regardless of which status string got it there.
func processMultiUnit(deps Deps, reporter ActivityReporter, job worker.Job, note *notetask.Note, repoCfg config.RepoConfig, branchName string, codingEng, reviewEng engine.Engine) error {
	if !note.HasPlan || strings.TrimSpace(note.Plan) == "" {
		return runPlanner(deps, reporter, job, note, repoCfg, branchName, codingEng, reviewEng)
	}
	if len(note.Frontmatter.Units) > 0 && !allUnitsDone(note.Frontmatter.Units) {
		return runUnitLoop(deps, reporter, job, repoCfg, branchName, codingEng, reviewEng)
	}
	return nil
}
