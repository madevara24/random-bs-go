// Package taskmeta holds the task-note scan helpers shared by dispatch,
// watcher, and runner -- the vault-relative Tasks/ directory name, the set
// of terminal statuses, and the two different things a task note's path
// gets turned into: a git-ref-safe slug, and a human-readable title.
package taskmeta

import (
	"path/filepath"
	"strings"
)

// TasksDir is the vault-relative directory every task note lives under.
const TasksDir = "Tasks"

// IsTerminalStatus reports whether status is one a task note never leaves
// once reached.
func IsTerminalStatus(status string) bool {
	switch status {
	case "done", "blocked", "failed":
		return true
	default:
		return false
	}
}

// Slugify turns an arbitrary task-note title into the git-ref-safe,
// filename-safe form every existing MDC task branch already uses (e.g.
// "(MDC) PR35 Review Follow-up R2 (Fold ask TestMain, delete main_test.go)"
// -> "mdc-pr35-review-follow-up-r2-fold-ask-testmain-delete-main_test-go"):
// lowercase, any run of characters outside [a-z0-9_] collapsed to one
// hyphen, leading/trailing hyphens trimmed. Found missing 2026-09-17 when
// the first note ever processed by the Go runner in production (title had
// spaces and parens) produced an invalid `git checkout -b` branch name --
// every prior MDC task had gone through the old bash pipeline, which did
// slugify, so this gap had never been exercised before.
func Slugify(s string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
			prevHyphen = false
			continue
		}
		if !prevHyphen {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	return strings.Trim(b.String(), "-")
}

// SlugFromPath derives the git-ref-safe branch slug from a task note's
// vault-relative path.
func SlugFromPath(relPath string) string {
	base := filepath.Base(relPath)
	name := strings.TrimSuffix(base, filepath.Ext(base))
	return Slugify(name)
}

// TitleFromPath derives the display title from a task note's vault-relative
// path -- just the filename with the .md extension stripped, no slugifying.
func TitleFromPath(relPath string) string {
	base := filepath.Base(relPath)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
