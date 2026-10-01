// Package notify is the runner's and the watcher's shared Discord-webhook
// mechanism. Own package specifically because both run modes of the daemon
// need the same Discord-alert mechanism -- the watcher's own down/hung-daemon
// alerts reuse this.
package notify

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
)

// Notifier sends alerts and records the outcome on a task note's Runner
// Log. Vault may be nil for a caller that doesn't want the Runner Log
// append (e.g. a future watcher alert not tied to one specific task note).
type Notifier struct {
	Vault      *vaultgit.Vault
	WebhookURL string

	// ForumWebhookURL, if set, is a forum-channel webhook used to route a
	// task's messages into its own Discord thread. Optional -- empty means
	// every message goes to WebhookURL at the top level, same as before
	// per-task threads existed. Even when set, a given Send call only uses
	// it if that call's own threadID is non-empty (see resolveTarget):
	// a forum webhook can't post without either thread_name or thread_id,
	// so anything not tied to one task's thread must still go to
	// WebhookURL.
	ForumWebhookURL string

	// RunnerLogURL, if set, takes precedence over Vault: instead of
	// writing the Runner Log outcome directly against the vault clone,
	// sendSync POSTs it to the runner daemon's own /runner-log endpoint.
	// Required for a caller running in a different OS process from the one
	// that owns the vault clone -- e.g. the watcher, which found out the
	// hard way (2026-09-17) that two independent vaultgit.Vault instances
	// in two separate processes share no mutex and collide on real git
	// locks. The runner itself leaves this empty and keeps writing via
	// Vault directly, since it already is the process that owns the clone.
	RunnerLogURL string
}

// Send fires the Discord alert asynchronously (fire-and-forget goroutine)
// and returns immediately -- doesn't block the caller's own per-repo
// goroutine from popping its next task. If notePath is non-empty, the
// outcome ("notified" or, after exhausting one retry, "notify_failed") is
// appended to that note's Runner Log once the attempt(s) finish. threadID
// is the task's discord_thread_id, if it has one -- see resolveTarget for
// how it decides where the message actually goes.
func (n *Notifier) Send(notePath, threadID, message, attachmentName, attachmentBody string) {
	go n.sendSync(notePath, threadID, message, attachmentName, attachmentBody)
}

// resolveTarget decides which webhook a message actually goes to: the
// forum webhook, posted into the task's own thread, only when both a
// threadID and a configured ForumWebhookURL are present -- otherwise the
// plain top-level webhook, exactly as before per-task threads existed.
// Returns an empty threadID alongside the top-level URL so callers never
// need a second check.
func (n *Notifier) resolveTarget(threadID string) (url, effectiveThreadID string) {
	if threadID != "" && n.ForumWebhookURL != "" {
		return n.ForumWebhookURL, threadID
	}
	return n.WebhookURL, ""
}

// sendSync is the synchronous core -- exported behavior via Send's
// goroutine wrapper, called directly (not via a goroutine)
// by this package's own tests for deterministic assertions on the retry
// count and the resulting Runner Log line.
func (n *Notifier) sendSync(notePath, threadID, message, attachmentName, attachmentBody string) {
	targetURL, effectiveThreadID := n.resolveTarget(threadID)
	err := postDiscordAlert(targetURL, effectiveThreadID, message, attachmentName, attachmentBody)
	event := "notified"
	if err != nil {
		fmt.Printf("[notify] first attempt failed, retrying once: %v\n", err)
		err = postDiscordAlert(targetURL, effectiveThreadID, message, attachmentName, attachmentBody)
		if err != nil {
			fmt.Printf("[notify] retry also failed, giving up: %v\n", err)
			event = "notify_failed"
		}
	}

	if notePath == "" {
		return
	}
	if n.RunnerLogURL != "" {
		n.postRunnerLog(notePath, event)
		return
	}
	if n.Vault == nil {
		return
	}
	werr := n.Vault.WriteNote(notePath, fmt.Sprintf("runner: %s", event), func(note *notetask.Note) error {
		notetask.AppendRunnerLog(note, event, time.Now())
		return nil
	})
	if werr != nil {
		fmt.Printf("[notify] failed to append %q to Runner Log for %s: %v\n", event, notePath, werr)
		// Called directly, not via Send -- that would attempt
		// another Runner Log append and recurse into this same failure. No
		// attachment, no @-mention: the alert this append was meant to
		// record already carried the mention.
		_ = postDiscordAlert(n.WebhookURL, "",
			fmt.Sprintf("[runner] WARNING: could not append %q to the Runner Log for `%s`: %v", event, notePath, werr),
			"", "")
	}
}

// postRunnerLog asks the runner daemon to record the outcome, since it (not
// this process) owns the vault clone. One attempt, no retry of its own --
// the outer sendSync retry already covers "Discord was down," and if the
// runner daemon itself is unreachable that's a health-check failure the
// watcher's own /health poll surfaces separately, not something worth a
// second retry loop here.
func (n *Notifier) postRunnerLog(notePath, event string) {
	body, err := json.Marshal(map[string]string{"note_path": notePath, "event": event})
	if err != nil {
		fmt.Printf("[notify] failed to marshal runner-log request for %s: %v\n", notePath, err)
		return
	}
	req, err := http.NewRequest(http.MethodPost, n.RunnerLogURL, bytes.NewReader(body))
	if err != nil {
		fmt.Printf("[notify] failed to build runner-log request for %s: %v\n", notePath, err)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Printf("[notify] POST %s for %s: %v\n", n.RunnerLogURL, notePath, err)
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		fmt.Printf("[notify] POST %s for %s returned status %d: %s\n", n.RunnerLogURL, notePath, resp.StatusCode, respBody)
		// Same rule as sendSync's own append-failure warning above: called
		// directly, not via Send, to avoid recursing into
		// another Runner Log append. No attachment, no @-mention.
		_ = postDiscordAlert(n.WebhookURL, "",
			fmt.Sprintf("[runner] WARNING: could not append %q to the Runner Log for `%s`: runner-log endpoint returned status %d: %s", event, notePath, resp.StatusCode, respBody),
			"", "")
	}
}

// postDiscordAlert sends one short message + one .md attachment on a
// single Discord message, via the payload_json + fileN multipart shape.
// threadID, if non-empty, routes the message into that existing Discord
// thread (?thread_id=) instead of posting at the channel's top level.
func postDiscordAlert(webhookURL, threadID, content, attachmentName, attachmentBody string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	payloadJSON, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return fmt.Errorf("marshaling payload_json: %w", err)
	}
	if err := w.WriteField("payload_json", string(payloadJSON)); err != nil {
		return fmt.Errorf("writing payload_json field: %w", err)
	}

	if attachmentName != "" || attachmentBody != "" {
		part, err := w.CreatePart(map[string][]string{
			"Content-Disposition": {fmt.Sprintf(`form-data; name="file1"; filename=%q`, attachmentName)},
			"Content-Type":        {"text/markdown"},
		})
		if err != nil {
			return fmt.Errorf("creating file1 part: %w", err)
		}
		if _, err := part.Write([]byte(attachmentBody)); err != nil {
			return fmt.Errorf("writing attachment body: %w", err)
		}
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("closing multipart writer: %w", err)
	}

	url := webhookURL + "?wait=true"
	if threadID != "" {
		url += "&thread_id=" + threadID
	}
	req, err := http.NewRequest(http.MethodPost, url, &buf)
	if err != nil {
		return fmt.Errorf("building request: %w", err)
	}
	req.Header.Set("Content-Type", w.FormDataContentType())

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("posting to discord: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("discord returned status %d", resp.StatusCode)
	}
	return nil
}

// PostForumClaim creates a new Discord forum post (thread_name in the
// body) via a forum-channel webhook -- the claim message that gives a
// freshly claimed task its own thread. Synchronous, with a short timeout
// and exactly one retry: callers need the returned thread ID (the starter
// message's channel_id) before they can route anything else into it, and
// a setup failure can fire seconds later needing that same ID. content
// carries no @-mentions -- unlike every other alert this package sends,
// a bare claim needs no one's attention yet.
func PostForumClaim(forumWebhookURL, threadName, content string) (string, error) {
	threadID, err := postForumClaimOnce(forumWebhookURL, threadName, content)
	if err != nil {
		fmt.Printf("[notify] forum claim post failed, retrying once: %v\n", err)
		threadID, err = postForumClaimOnce(forumWebhookURL, threadName, content)
	}
	return threadID, err
}

func postForumClaimOnce(forumWebhookURL, threadName, content string) (string, error) {
	body, err := json.Marshal(map[string]string{"thread_name": threadName, "content": content})
	if err != nil {
		return "", fmt.Errorf("marshaling forum claim payload: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, forumWebhookURL+"?wait=true", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("building forum claim request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("posting forum claim to discord: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("discord returned status %d: %s", resp.StatusCode, respBody)
	}

	var decoded struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("decoding forum claim response: %w", err)
	}
	if decoded.ChannelID == "" {
		return "", fmt.Errorf("discord forum claim response missing channel_id")
	}
	return decoded.ChannelID, nil
}

// ReadWebhookURLFromEnvFile is a small convenience for tests/tools that
// need the real webhook URL without going through the full config
// package -- reads a bare KEY=VALUE .env line, same format config.Load
// already parses.
func ReadWebhookURLFromEnvFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	const prefix = "DISCORD_WEBHOOK_URL="
	for scanner.Scan() {
		line := scanner.Text()
		if len(line) > len(prefix) && line[:len(prefix)] == prefix {
			return line[len(prefix):], nil
		}
	}
	return "", fmt.Errorf("DISCORD_WEBHOOK_URL not found in %s", path)
}
