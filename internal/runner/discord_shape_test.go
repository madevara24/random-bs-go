package runner

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"os"
	"strings"
	"testing"
)

// TestAlertPayloadReachesDiscordInVerifiedShape is the other half of Phase
// 9's test gate: "Verify the attachment actually reaches Discord in the
// same shape confirmed live in Design - Runner.md's notify.sh section" --
// a short message + a real .md file attachment on one message, via
// `payload_json` + `fileN` multipart fields against the real webhook.
//
// Gated behind an env var, not run as part of a normal `go test ./...`,
// since it fires a real message into the real #dev channel -- opt in
// explicitly: PMRUNNER_SEND_REAL_DISCORD_TEST=1 go test ./internal/runner/
// -run TestAlertPayloadReachesDiscordInVerifiedShape
func TestAlertPayloadReachesDiscordInVerifiedShape(t *testing.T) {
	if os.Getenv("PMRUNNER_SEND_REAL_DISCORD_TEST") == "" {
		t.Skip("set PMRUNNER_SEND_REAL_DISCORD_TEST=1 to actually fire this against the real Discord webhook")
	}

	webhookURL := readDiscordWebhookURLFromEnvFile(t, "/home/obsidian/pm-runner-go/.env")

	payload := AlertPayload{
		Slug:       "phase9-discord-shape-check",
		Repo:       "phase6-test-repo",
		Scenario:   ScenarioNoCopy,
		Stage:      "result read-back",
		ExitCode:   1,
		StderrTail: "(no real stderr -- this is a Phase 9 shape-verification test, not a real task alert)",
		PRState:    "unknown (test)",
		LogPointer: "(no real log -- test)",
	}

	content := fmt.Sprintf("[PM RUNNER GO REWRITE -- TEST FIRE, NOT A REAL TASK ALERT] %s", payload.DiscordMessage("0"))

	if err := postDiscordAlert(webhookURL, content, payload.Slug+".md", payload.AttachmentMarkdown()); err != nil {
		t.Fatalf("posting alert to real Discord webhook: %v", err)
	}
}

func readDiscordWebhookURLFromEnvFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening %s: %v", path, err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "DISCORD_WEBHOOK_URL=") {
			return strings.TrimSpace(strings.TrimPrefix(line, "DISCORD_WEBHOOK_URL="))
		}
	}
	t.Fatalf("DISCORD_WEBHOOK_URL not found in %s", path)
	return ""
}

// postDiscordAlert sends a one-shot (no retry -- that's Phase 10's job)
// short message + .md attachment, matching the exact shape Design -
// Runner.md's notify.sh section verified live: `payload_json` + `fileN`
// multipart fields against `$DISCORD_WEBHOOK_URL?wait=true`.
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

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("posting to discord: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("discord returned status %d", resp.StatusCode)
	}
	return nil
}
