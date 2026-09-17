// Package notify is the runner's and the watcher's shared Discord-webhook
// + hermes -z mechanism. Own package specifically because both run modes
// of the daemon need the same Discord-alert mechanism -- the watcher's own
// down/hung-daemon alerts reuse this.
package notify

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/madevara24/random-bs-go/internal/notetask"
	"github.com/madevara24/random-bs-go/internal/vaultgit"
)

// DefaultHermesCmd is the real invocation -- no bare `hermes` binary
// exists on PATH, this is the venv python module invocation found via the
// hermes-gateway.service systemd unit's actual ExecStart. Callers needing
// something else (tests, a future path change) override Notifier.HermesCmd.
var DefaultHermesCmd = []string{
	"/home/obsidian/.hermes/hermes-agent/venv/bin/python", "-m", "hermes_cli.main", "-z",
}

// Notifier sends alerts and records the outcome on a task note's Runner
// Log. Vault may be nil for a caller that doesn't want the Runner Log
// append (e.g. a future watcher alert not tied to one specific task note).
type Notifier struct {
	Vault      *vaultgit.Vault
	WebhookURL string

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

	// HermesCmd is the full argv minus the final message argument, e.g.
	// {"/path/to/python", "-m", "hermes_cli.main", "-z"} -- the message
	// text is appended as the last arg. Defaults to DefaultHermesCmd.
	HermesCmd []string
}

func (n *Notifier) hermesCmd() []string {
	if len(n.HermesCmd) > 0 {
		return n.HermesCmd
	}
	return DefaultHermesCmd
}

// Send fires the Discord alert asynchronously (fire-and-forget goroutine)
// and returns immediately -- doesn't block the caller's own per-repo
// goroutine from popping its next task. If notePath is non-empty, the
// outcome ("notified" or, after exhausting one retry, "notify_failed") is
// appended to that note's Runner Log once the attempt(s) finish.
func (n *Notifier) Send(notePath, message, attachmentName, attachmentBody string) {
	go n.sendSync(notePath, message, attachmentName, attachmentBody)
}

// SendBlocked does everything Send does, and additionally invokes hermes
// -z as a real first-responder call -- a `blocked`-specific
// two-notification pattern (most Discord bots, Hermes likely included,
// filter out webhook/bot-authored messages, so the Discord mention alone
// probably wouldn't register as input for Hermes).
func (n *Notifier) SendBlocked(notePath, slug, reason, message, attachmentName, attachmentBody string) {
	go func() {
		n.sendSync(notePath, message, attachmentName, attachmentBody)
		n.invokeHermes(slug, reason)
	}()
}

// sendSync is the synchronous core -- exported behavior via Send/
// SendBlocked's goroutine wrapper, called directly (not via a goroutine)
// by this package's own tests for deterministic assertions on the retry
// count and the resulting Runner Log line.
func (n *Notifier) sendSync(notePath, message, attachmentName, attachmentBody string) {
	err := postDiscordAlert(n.WebhookURL, message, attachmentName, attachmentBody)
	event := "notified"
	if err != nil {
		fmt.Printf("[notify] first attempt failed, retrying once: %v\n", err)
		err = postDiscordAlert(n.WebhookURL, message, attachmentName, attachmentBody)
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
		fmt.Printf("[notify] POST %s for %s returned status %d\n", n.RunnerLogURL, notePath, resp.StatusCode)
	}
}

func (n *Notifier) invokeHermes(slug, reason string) {
	argv := n.hermesCmd()
	text := fmt.Sprintf("Task %s is blocked: %s. Post an update in #dev and help if you can before escalating.", slug, reason)
	args := append(append([]string{}, argv[1:]...), text)
	cmd := exec.Command(argv[0], args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		fmt.Printf("[notify] hermes -z invocation for %s failed: %v\n", slug, err)
	}
}

// postDiscordAlert sends one short message + one .md attachment on a
// single Discord message, via the payload_json + fileN multipart shape.
func postDiscordAlert(webhookURL, content, attachmentName, attachmentBody string) error {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	payloadJSON, err := json.Marshal(map[string]string{"content": content})
	if err != nil {
		return fmt.Errorf("marshaling payload_json: %w", err)
	}
	if err := w.WriteField("payload_json", string(payloadJSON)); err != nil {
		return fmt.Errorf("writing payload_json field: %w", err)
	}

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
	if err := w.Close(); err != nil {
		return fmt.Errorf("closing multipart writer: %w", err)
	}

	req, err := http.NewRequest(http.MethodPost, webhookURL+"?wait=true", &buf)
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
