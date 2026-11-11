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
		prompt := buildPrompt(note, "copy.md", resume, "main", "task/slug", false)

		if !strings.Contains(prompt, "pushed to `origin`") || !strings.Contains(prompt, "pull request") {
			t.Errorf("resume=%v: prompt missing commit/push/PR requirement:\n%s", resume, prompt)
		}
		if !strings.Contains(prompt, "`pr_url`") {
			t.Errorf("resume=%v: prompt missing pr_url requirement:\n%s", resume, prompt)
		}
		if !strings.Contains(prompt, "treated as blocked") {
			t.Errorf("resume=%v: prompt missing the done-without-pr_url-is-blocked statement:\n%s", resume, prompt)
		}
		if !strings.Contains(prompt, "Never merge the pull request") {
			t.Errorf("resume=%v: prompt missing the no-self-merge instruction:\n%s", resume, prompt)
		}
		if !strings.Contains(prompt, "the runner's setting, not an instruction to you") {
			t.Errorf("resume=%v: prompt missing the auto_merge-is-the-runner's-setting clarification:\n%s", resume, prompt)
		}
	}
}

func TestBuildPromptResumeStillMentionsTaskFile(t *testing.T) {
	note := &notetask.Note{Frontmatter: notetask.Frontmatter{Status: "ready"}, Prompt: "Do the thing."}
	prompt := buildPrompt(note, "copy.md", true, "main", "task/slug", false)

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
	prompt := buildPrompt(note, "copy.md", true, "main", "task/slug", false)

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

	freshPrompt := buildPrompt(note, "copy.md", false, "main", "task/slug", false)
	if strings.Contains(freshPrompt, "may have advanced") {
		t.Errorf("fresh prompt should not carry the resume-only reconciliation notice:\n%s", freshPrompt)
	}
}

func TestBuildPromptFreshIncludesTaskBody(t *testing.T) {
	note := &notetask.Note{Frontmatter: notetask.Frontmatter{Status: "ready"}, Prompt: "Do the thing."}
	prompt := buildPrompt(note, "copy.md", false, "main", "task/slug", false)

	if !strings.Contains(prompt, "Do the thing.") {
		t.Errorf("fresh prompt missing task body:\n%s", prompt)
	}
}

// TestBuildPromptResumeLocalOnlyMentionsPushUpstream guards RBG-38: when
// checkoutTaskBranch had to fall back to a branch that was never pushed to
// origin, the resume prompt must tell the session to set an upstream on its
// first push instead of implying the normal (tracked) resume case.
func TestBuildPromptResumeLocalOnlyMentionsPushUpstream(t *testing.T) {
	note := &notetask.Note{Frontmatter: notetask.Frontmatter{Status: "ready"}, Prompt: "Do the thing."}

	localOnlyPrompt := buildPrompt(note, "copy.md", true, "main", "task/rbg-38-test", true)
	if !strings.Contains(localOnlyPrompt, "git push -u origin task/rbg-38-test") {
		t.Errorf("local-only resume prompt missing the git push -u origin instruction:\n%s", localOnlyPrompt)
	}
	if !strings.Contains(localOnlyPrompt, "only locally") {
		t.Errorf("local-only resume prompt missing the only-found-locally notice:\n%s", localOnlyPrompt)
	}

	normalPrompt := buildPrompt(note, "copy.md", true, "main", "task/rbg-38-test", false)
	if strings.Contains(normalPrompt, "git push -u origin") {
		t.Errorf("normal resume prompt should not carry the local-only push instruction:\n%s", normalPrompt)
	}
}

// TestBuildPromptCarriesNumberedBlockerInstructions guards RBG-54: both
// prompts tell the session which blocker number to name in a new Work Log
// entry (the runner opens BlockerRound+1), without letting it touch the
// runner-owned "## Blockers" section. Only a resume with an already-open
// blocker also points at that block's resolution, using BlockerRound
// itself rather than the next number.
func TestBuildPromptCarriesNumberedBlockerInstructions(t *testing.T) {
	tests := []struct {
		name        string
		resume      bool
		round       int
		wantNext    string
		wantCurrent string
	}{
		{name: "fresh with no prior blocker", resume: false, round: 0, wantNext: "`Blocker 1:`"},
		{name: "fresh after a prior blocker", resume: false, round: 2, wantNext: "`Blocker 3:`"},
		{name: "resume with no prior blocker", resume: true, round: 0, wantNext: "`Blocker 1:`"},
		{name: "resume with an open blocker", resume: true, round: 2, wantNext: "`Blocker 3:`", wantCurrent: "Your current blocker is `### Blocker 2` under `## Blockers`. Read its `#### Resolution` first, then continue."},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			note := &notetask.Note{
				Frontmatter: notetask.Frontmatter{Status: "ready", BlockerRound: tt.round},
				Prompt:      "Do the thing.",
			}
			prompt := buildPrompt(note, "copy.md", tt.resume, "main", "task/slug", false)

			if !strings.Contains(prompt, tt.wantNext) {
				t.Errorf("prompt missing next-blocker instruction %q:\n%s", tt.wantNext, prompt)
			}
			if !strings.Contains(prompt, `Do not edit the "## Blockers" section yourself`) {
				t.Errorf("prompt missing the do-not-touch-##-Blockers instruction:\n%s", prompt)
			}

			if tt.wantCurrent == "" {
				if strings.Contains(prompt, "Your current blocker is") {
					t.Errorf("prompt should not point at a current blocker:\n%s", prompt)
				}
				return
			}
			if !strings.Contains(prompt, tt.wantCurrent) {
				t.Errorf("prompt missing current-blocker/resolution instruction %q:\n%s", tt.wantCurrent, prompt)
			}
		})
	}
}
