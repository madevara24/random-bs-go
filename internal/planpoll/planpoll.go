// Package planpoll is the human half of the plan gate: every interval, it
// polls each task note waiting at plan_review for a reaction on the plan
// message the gate posted (internal/runner's stopForPlanReview), and
// writes the resulting plan_approved/plan_rejected status -- which
// dispatch then claims exactly like a ready task, handing the decision
// back to the runner (internal/runner's processMultiUnit).
package planpoll

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/notify"
	"github.com/madevara24/random-bs-go/internal/plangate"
	"github.com/madevara24/random-bs-go/internal/taskmeta"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
)

// missingMessageAlertAfter is how many consecutive failed repost attempts
// (see retryMissingPlanMessage) fire one alert -- past this, the note
// would otherwise wait forever with nothing to react to and no signal
// anyone would ever see.
const missingMessageAlertAfter = 10

// Decide maps a plan_review note's Discord reactions to its next status.
// ❌ only counts once this round's own feedback has been written under
// "### Round <n>" in Plan Review (currentRoundReview) -- a reaction can
// land before the human's note edit syncs, and reading that race as an
// instant reject (or worse, falling through to an ✅ also present) would
// replan on nothing. Approval via ✅ needs no feedback. Any other emoji,
// or neither reaction present, is no change.
func Decide(reactions map[string]int, currentRoundReview string) (newStatus string, changed bool) {
	rejected := reactions["❌"] > 0
	approved := reactions["✅"] > 0
	hasFeedback := strings.TrimSpace(currentRoundReview) != ""

	if rejected && hasFeedback {
		return taskmeta.StatusPlanRejected, true
	}
	if rejected {
		return "", false
	}
	if approved {
		return taskmeta.StatusPlanApproved, true
	}
	return "", false
}

// missingMessageTracker counts consecutive failed repost attempts per note
// (see retryMissingPlanMessage), scoped to one Run call's lifetime -- a
// restart resets the count, which only delays the eventual alert, never
// loses correctness.
type missingMessageTracker struct {
	fails map[string]int
}

func newMissingMessageTracker() *missingMessageTracker {
	return &missingMessageTracker{fails: map[string]int{}}
}

// Run polls every interval for plan_review notes whose Discord message has
// a reaction, writing the resulting plan_approved/plan_rejected status and
// calling wake so dispatch claims it immediately rather than waiting for
// its own next trigger. webhookURL is the forum webhook FetchReactions/
// PostToThread call against -- the same one the plan gate itself posts
// through. userID is the human's Discord user ID, threaded down so replies
// posted from this poll can mention them. Blocks until ctx is done.
func Run(ctx context.Context, v *vaultgit.Vault, webhookURL, userID string, wake func(), interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	tracker := newMissingMessageTracker()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			tick(v, webhookURL, userID, wake, tracker)
		}
	}
}

// tick is one full poll pass: sync, scan Tasks/ for plan_review notes, and
// act on each. Exposed as its own step (rather than inlined into Run) so a
// test can drive it directly, once per call, without racing a real
// ticker.
func tick(v *vaultgit.Vault, webhookURL, userID string, wake func(), tracker *missingMessageTracker) {
	if err := v.Sync(); err != nil {
		fmt.Printf("[planpoll] sync: %v\n", err)
		return
	}

	pattern := filepath.Join(v.Path, taskmeta.TasksDir, "*.md")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		fmt.Printf("[planpoll] scanning %s: %v\n", pattern, err)
		return
	}

	for _, m := range matches {
		relPath, err := filepath.Rel(v.Path, m)
		if err != nil {
			continue
		}
		data, err := os.ReadFile(m)
		if err != nil {
			fmt.Printf("[planpoll] reading %s: %v\n", relPath, err)
			continue
		}
		note, err := notetask.Parse(data)
		if err != nil {
			fmt.Printf("[planpoll] parsing %s: %v\n", relPath, err)
			continue
		}
		if note.Frontmatter.Status != taskmeta.StatusPlanReview {
			continue
		}

		processNote(v, webhookURL, userID, wake, tracker, relPath, note)
	}
}

func processNote(v *vaultgit.Vault, webhookURL, userID string, wake func(), tracker *missingMessageTracker, relPath string, note *notetask.Note) {
	threadID := derefStr(note.Frontmatter.DiscordThreadID)
	messageID := derefStr(note.Frontmatter.PlanMessageID)

	if messageID == "" {
		retryMissingPlanMessage(v, webhookURL, userID, relPath, note, threadID, tracker)
		return
	}

	reactions, err := notify.FetchReactions(webhookURL, threadID, messageID)
	if err != nil {
		fmt.Printf("[planpoll] fetching reactions for %s: %v\n", relPath, err)
		return
	}

	round := note.Frontmatter.PlanRound
	if round == 0 {
		round = 1
	}
	review := notetask.CurrentRoundReview(note.PlanReview, round)

	newStatus, changed := Decide(reactions, review)
	if !changed {
		return
	}

	event := "plan_approved (reaction)"
	if newStatus == taskmeta.StatusPlanRejected {
		event = "plan_rejected (reaction)"
	}
	slug := taskmeta.SlugFromPath(relPath)
	werr := v.WriteNote(relPath, fmt.Sprintf("planpoll: %s %s", newStatus, slug), func(n *notetask.Note) error {
		n.Frontmatter.Status = newStatus
		notetask.AppendRunnerLog(n, event, time.Now())
		return nil
	})
	if werr != nil {
		fmt.Printf("[planpoll] writing %s for %s: %v\n", newStatus, relPath, werr)
		return
	}
	// Best effort: the status write above is the durable record and must
	// stand even if Discord is unreachable. A failed reply is only logged
	// -- never retried in a loop, never allowed to undo the write.
	if _, perr := notify.PostToThread(webhookURL, threadID, planDecisionReply(userID, slug, round, newStatus), "", ""); perr != nil {
		fmt.Printf("[planpoll] posting %s reply for %s: %v\n", newStatus, relPath, perr)
	}
	wake()
}

// planDecisionReply is the one-line acknowledgement posted into the task's
// thread once a reaction has been recorded as plan_approved or
// plan_rejected, so the human who reacted sees the poller picked it up and
// what happens next. The mention is omitted when userID is empty, same as
// the plan-review message's mention.
func planDecisionReply(userID, slug string, round int, status string) string {
	mention := ""
	if userID != "" {
		mention = fmt.Sprintf("<@%s> ", userID)
	}
	if status == taskmeta.StatusPlanRejected {
		return fmt.Sprintf("%sPlan for `%s` rejected -- round %d feedback recorded, the planner will replan on the next claim.", mention, slug, round)
	}
	return fmt.Sprintf("%sPlan for `%s` approved -- round %d recorded, the unit loop starts on the next claim.", mention, slug, round)
}

// retryMissingPlanMessage handles a plan_review note whose plan_message_id
// is empty -- stopForPlanReview sets plan_review even when its own Discord
// post failed, so this note would otherwise wait forever with nothing to
// react to. Reposts once per tick through the same plangate renderer
// stopForPlanReview uses, from the persisted plan_gate_reasons when present
// (or, for a note that stopped before that field existed, the latest
// plan_gate Runner Log line as a single Errors bullet), and past
// missingMessageAlertAfter consecutive failures, sends one alert into the
// task's own thread naming it.
func retryMissingPlanMessage(v *vaultgit.Vault, webhookURL, userID, relPath string, note *notetask.Note, threadID string, tracker *missingMessageTracker) {
	round := note.Frontmatter.PlanRound
	if round == 0 {
		round = 1
	}
	slug := taskmeta.SlugFromPath(relPath)

	mention := ""
	if userID != "" {
		mention = fmt.Sprintf("<@%s>", userID)
	}
	message := plangate.RenderPlanReviewMessage(mention, slug, note.Frontmatter.Repo, round, planGateReasons(note))
	attachmentName := fmt.Sprintf("plan-round-%d.md", round)

	messageID, err := notify.PostToThread(webhookURL, threadID, message, attachmentName, note.Plan)
	if err != nil {
		tracker.fails[relPath]++
		fmt.Printf("[planpoll] reposting missing plan message for %s (attempt %d): %v\n", relPath, tracker.fails[relPath], err)
		if tracker.fails[relPath] == missingMessageAlertAfter {
			alertMissingPlanMessage(webhookURL, threadID, slug)
		}
		return
	}
	delete(tracker.fails, relPath)

	werr := v.WriteNote(relPath, fmt.Sprintf("planpoll: plan_posted %s", slug), func(n *notetask.Note) error {
		n.Frontmatter.PlanMessageID = &messageID
		notetask.AppendRunnerLog(n, "plan_posted (retry)", time.Now())
		return nil
	})
	if werr != nil {
		fmt.Printf("[planpoll] writing plan_message_id for %s: %v\n", relPath, werr)
	}
}

// alertMissingPlanMessage sends one best-effort alert into the task's own
// thread after missingMessageAlertAfter failed repost attempts -- if the
// webhook is down enough to cause that, this attempt may well also fail,
// in which case it's just logged, same as every other notify failure in
// this package.
func alertMissingPlanMessage(webhookURL, threadID, slug string) {
	_, err := notify.PostToThread(webhookURL, threadID,
		fmt.Sprintf("Task `%s`'s plan review message could not be posted after %d attempts -- check Discord/webhook health.", slug, missingMessageAlertAfter),
		"", "")
	if err != nil {
		fmt.Printf("[planpoll] alerting about missing plan message for %s also failed: %v\n", slug, err)
	}
}

// planGateReasons rebuilds the renderer's ordered reasons from the note.
// stopForPlanReview persists the grouped plan_gate_reasons, so a normal
// retry renders the exact same text. A note that reached plan_review before
// that field existed has none, and falls back to the latest plan_gate
// Runner Log line -- the only record those notes ever had -- as one Errors
// bullet, matching the retry's pre-grouping message content.
func planGateReasons(note *notetask.Note) []plangate.Reason {
	if len(note.Frontmatter.PlanGateReasons) == 0 {
		return []plangate.Reason{{
			Category: plangate.CategoryErrors,
			Text:     latestPlanGateLine(note.RunnerLog),
		}}
	}

	var reasons []plangate.Reason
	for category, texts := range note.Frontmatter.PlanGateReasons {
		for _, text := range texts {
			reasons = append(reasons, plangate.Reason{Category: plangate.Category(category), Text: text})
		}
	}
	return reasons
}

// latestPlanGateLine returns the most recent Runner Log entry whose event
// starts with "plan_gate" -- the persisted record of why the gate stopped,
// reused verbatim as the repost's stop-reason text since
// plangate.Result.Reasons itself was never persisted anywhere, only this
// line's rendering of it (see runner.gateLogLine).
func latestPlanGateLine(entries []notetask.RunnerLogEntry) string {
	for i := len(entries) - 1; i >= 0; i-- {
		if strings.HasPrefix(entries[i].Event, "plan_gate") {
			return entries[i].Event
		}
	}
	return ""
}

func derefStr(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
