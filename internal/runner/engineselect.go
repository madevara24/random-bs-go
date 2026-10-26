// Engine selection: resolving which engine codes a task and which engine
// reviews its PR, and validating both names against the registry before
// the target repo clone is ever touched (see setupfallback.go's
// setupStageResolveEngine). Sibling to internal/engineresolve's pure
// precedence functions -- this file is the runner-specific plumbing
// around them (job/note access, the registry lookup, the error text).
package runner

import (
	"fmt"
	"sort"
	"strings"

	"github.com/madevara24/random-bs-go/internal/config"
	"github.com/madevara24/random-bs-go/internal/engine"
	"github.com/madevara24/random-bs-go/internal/engineresolve"
	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/worker"
)

// fallbackCodingEngineName is resolveCodingEngineName's last-resort
// default when neither job.Engine nor the note's own `engine:` is set --
// only reachable by a caller that bypasses dispatch's own resolution
// entirely (e.g. a test constructing worker.Job by hand); every real
// dispatch-claimed job already carries a resolved name.
const fallbackCodingEngineName = "claude"

// resolveCodingEngineName picks the name ProcessTask looks up in the
// registry for the coding run (and for ResumeWithFeedback): job.Engine --
// resolved by dispatch at claim time -- first, else the note's own
// `engine:` pin, else fallbackCodingEngineName.
func resolveCodingEngineName(job worker.Job, note *notetask.Note) string {
	if job.Engine != "" {
		return job.Engine
	}
	if note != nil && note.Frontmatter.Engine != nil && *note.Frontmatter.Engine != "" {
		return *note.Frontmatter.Engine
	}
	return fallbackCodingEngineName
}

// resolveReviewEngineName picks the name the merge gate looks up in the
// registry for the review: the note's own `review_engine:` pin -> the
// repo's configured review_engine -> defaultReviewEngine. Resolved
// independently of resolveCodingEngineName -- a task can code with one
// engine and be reviewed by another.
func resolveReviewEngineName(note *notetask.Note, repoCfg config.RepoConfig, defaultReviewEngine string) string {
	var noteReviewEngine string
	if note != nil && note.Frontmatter.ReviewEngine != nil {
		noteReviewEngine = *note.Frontmatter.ReviewEngine
	}
	return engineresolve.Review(noteReviewEngine, repoCfg.ReviewEngine, defaultReviewEngine)
}

// lookupEngine resolves name in registry, or an "unknown engine" error
// naming every registered engine -- the exact message
// setupStageResolveEngine's alert/Work Log text uses.
func lookupEngine(registry map[string]engine.Engine, name string) (engine.Engine, error) {
	eng, ok := registry[name]
	if ok {
		return eng, nil
	}
	return nil, fmt.Errorf("unknown engine %q (known: %s)", name, strings.Join(sortedEngineNames(registry), ", "))
}

func sortedEngineNames(registry map[string]engine.Engine) []string {
	names := make([]string, 0, len(registry))
	for n := range registry {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
