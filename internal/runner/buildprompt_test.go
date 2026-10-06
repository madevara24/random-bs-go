package runner

import (
	"strings"
	"testing"

	"github.com/madevara24/random-bs-go/internal/notetask"
)

// TestBuildPromptCarriesPRContract guards against the prompt-contract gap
// (RBG-21): both the fresh and resumed prompts must state that the
// deliverable is a committed, pushed, PR-opened branch, and that a `done`
// status with no `pr_url` is treated as blocked. Without this, a session
// can "finish" the code change locally and report done without ever
// pushing or opening a PR.
func TestBuildPromptCarriesPRContract(t *testing.T) {
	note := &notetask.Note{
		Frontmatter: notetask.Frontmatter{Status: "ready"},
		Prompt:      "Do the thing.",
	}

	for _, resume := range []bool{false, true} {
		prompt := buildPrompt(note, "copy.md", resume, "main")

		if !strings.Contains(prompt, "pushed to `origin`") || !strings.Contains(prompt, "pull request") {
			t.Errorf("resume=%v: prompt missing commit/push/PR requirement:\n%s", resume, prompt)
		}
		if !strings.Contains(prompt, "`pr_url`") {
			t.Errorf("resume=%v: prompt missing pr_url requirement:\n%s", resume, prompt)
		}
		if !strings.Contains(prompt, "treated as blocked") {
			t.Errorf("resume=%v: prompt missing the done-without-pr_url-is-blocked statement:\n%s", resume, prompt)
		}
	}
}

func TestBuildPromptResumeStillMentionsTaskFile(t *testing.T) {
	note := &notetask.Note{Frontmatter: notetask.Frontmatter{Status: "ready"}, Prompt: "Do the thing."}
	prompt := buildPrompt(note, "copy.md", true, "main")

	if !strings.Contains(prompt, "copy.md") {
		t.Errorf("resume prompt missing copy file name:\n%s", prompt)
	}
	if !strings.Contains(prompt, "Re-read that file") {
		t.Errorf("resume prompt missing re-read instruction:\n%s", prompt)
	}
}

// TestBuildPromptResumeCarriesReconciliationNotice guards RBG-22's resume
// contract: a resumed session must be told explicitly that origin/<default>
// may have advanced while the task sat blocked, and that reconciling it
// into the task branch (and resolving any conflicts) is its own job -- the
// runner's resume git surface is fetch+checkout only, never merge/rebase.
func TestBuildPromptResumeCarriesReconciliationNotice(t *testing.T) {
	note := &notetask.Note{Frontmatter: notetask.Frontmatter{Status: "ready"}, Prompt: "Do the thing."}
	prompt := buildPrompt(note, "copy.md", true, "main")

	if !strings.Contains(prompt, "origin/main") {
		t.Errorf("resume prompt missing the default branch it must reconcile against:\n%s", prompt)
	}
	if !strings.Contains(prompt, "may have advanced") {
		t.Errorf("resume prompt missing the \"main may have advanced\" notice:\n%s", prompt)
	}
	if !strings.Contains(prompt, "resolve any conflicts") {
		t.Errorf("resume prompt missing the conflict-resolution-is-your-job instruction:\n%s", prompt)
	}
	if !strings.Contains(prompt, "never merges, rebases") {
		t.Errorf("resume prompt missing confirmation that the runner itself never merges/rebases:\n%s", prompt)
	}

	freshPrompt := buildPrompt(note, "copy.md", false, "main")
	if strings.Contains(freshPrompt, "may have advanced") {
		t.Errorf("fresh prompt should not carry the resume-only reconciliation notice:\n%s", freshPrompt)
	}
}

func TestBuildPromptFreshIncludesTaskBody(t *testing.T) {
	note := &notetask.Note{Frontmatter: notetask.Frontmatter{Status: "ready"}, Prompt: "Do the thing."}
	prompt := buildPrompt(note, "copy.md", false, "main")

	if !strings.Contains(prompt, "Do the thing.") {
		t.Errorf("fresh prompt missing task body:\n%s", prompt)
	}
}
