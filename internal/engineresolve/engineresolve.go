// Package engineresolve picks which named engine governs a task's coding
// run and which named engine reviews its PR in the merge gate -- both
// resolved by the same three-level precedence (an explicit pin first, a
// repo-level default second, a global default last), but independently of
// each other: a task can code with one engine and be reviewed by another.
//
// Deliberately dependency-free (no notetask/config/dispatch/runner
// import) so dispatch and runner can both call it without either
// package importing the other.
package engineresolve

// Coding resolves which engine codes/resumes a task: the note's own
// engine pin (noteEngine, "" if unset) -> the repo's configured engine
// (repoEngine, "" if unset) -> defaultEngine.
func Coding(noteEngine, repoEngine, defaultEngine string) string {
	return resolve(noteEngine, repoEngine, defaultEngine)
}

// Review resolves which engine reviews a task's PR in the merge gate: the
// note's own review_engine pin -> the repo's configured review_engine ->
// defaultReviewEngine.
func Review(noteReviewEngine, repoReviewEngine, defaultReviewEngine string) string {
	return resolve(noteReviewEngine, repoReviewEngine, defaultReviewEngine)
}

func resolve(pinned, repoDefault, globalDefault string) string {
	if pinned != "" {
		return pinned
	}
	if repoDefault != "" {
		return repoDefault
	}
	return globalDefault
}
