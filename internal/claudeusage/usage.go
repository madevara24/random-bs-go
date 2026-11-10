// Package claudeusage fetches the current Claude subscription usage from
// Anthropic's OAuth usage endpoint -- the same endpoint Claude Code
// statusline tools use. The endpoint is undocumented, so response parsing
// is deliberately tolerant: either usage window may be missing.
//
// The OAuth access token is read from the credentials file on every Fetch
// call. It is never cached in memory across calls, never logged, and is
// scrubbed from any error this package returns.
package claudeusage

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// DefaultBaseURL is Anthropic's API base URL. Client.BaseURL overrides it
// (tests point this at an httptest.Server).
const DefaultBaseURL = "https://api.anthropic.com"

const (
	usagePath      = "/api/oauth/usage"
	betaHeader     = "anthropic-beta"
	betaValue      = "oauth-2025-04-20"
	defaultTimeout = 5 * time.Second
	maxBodyBytes   = 1 << 20
)

// Window is one rate-limit window (five_hour or seven_day). Utilization is
// a percent. ResetsAt is the zero time when the endpoint omitted
// resets_at.
type Window struct {
	Utilization float64
	ResetsAt    time.Time
}

// Usage is the subset of Anthropic's usage response this runner cares
// about. A window absent from the response (or null) is left nil rather
// than treated as an error.
type Usage struct {
	FiveHour *Window
	SevenDay *Window
}

// Client fetches Claude usage. BaseURL and CredentialsPath are injectable so
// tests can use an httptest.Server and a temporary credentials file. The
// zero value is usable except for CredentialsPath: Fetch then returns an
// error rather than silently reading the wrong file.
type Client struct {
	BaseURL         string
	CredentialsPath string

	// HTTPClient and Timeout are optional. NewClient sets a client and a
	// 5s timeout; a caller that supplies HTTPClient can also shorten
	// Timeout (tests do) without waiting five seconds.
	HTTPClient *http.Client
	Timeout    time.Duration
}

// NewClient returns a Client for baseURL and credentialsPath. Empty
// arguments fall back to DefaultBaseURL and DefaultCredentialsPath
// respectively, and a nil/empty base is never carried into a request.
func NewClient(baseURL, credentialsPath string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if credentialsPath == "" {
		credentialsPath, _ = DefaultCredentialsPath()
	}
	return &Client{
		BaseURL:         strings.TrimRight(baseURL, "/"),
		CredentialsPath: credentialsPath,
		HTTPClient:      &http.Client{Timeout: defaultTimeout},
		Timeout:         defaultTimeout,
	}
}

// NewDefaultClient returns a Client pointed at Anthropic's real endpoint and
// the current user's ~/.claude/.credentials.json.
func NewDefaultClient() (*Client, error) {
	path, err := DefaultCredentialsPath()
	if err != nil {
		return nil, fmt.Errorf("claude usage: locating credentials file: %w", err)
	}
	return NewClient(DefaultBaseURL, path), nil
}

// DefaultCredentialsPath returns ~/.claude/.credentials.json for the user
// this process runs as.
func DefaultCredentialsPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".claude", ".credentials.json"), nil
}

// Fetch reads the OAuth access token fresh from the credentials file, then
// calls GET <base>/api/oauth/usage. Every failure returns a short error whose
// text has the token scrubbed even if an underlying transport error somehow
// contained it.
func (c *Client) Fetch(ctx context.Context) (Usage, error) {
	token, err := readToken(c.CredentialsPath)
	if err != nil {
		return Usage{}, err
	}
	usage, err := c.fetch(ctx, token)
	if err != nil {
		return Usage{}, redactError(err, token)
	}
	return usage, nil
}

// Fetch is the package-level convenience call using the default endpoint and
// credentials path. It reads the token fresh on every call.
func Fetch(ctx context.Context) (Usage, error) {
	client, err := NewDefaultClient()
	if err != nil {
		return Usage{}, err
	}
	return client.Fetch(ctx)
}

func readToken(path string) (string, error) {
	if strings.TrimSpace(path) == "" {
		return "", fmt.Errorf("claude usage: credentials path is empty")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("claude usage: reading credentials: %w", err)
	}

	var creds struct {
		ClaudeAiOauth struct {
			AccessToken string `json:"accessToken"`
		} `json:"claudeAiOauth"`
	}
	if err := json.Unmarshal(data, &creds); err != nil {
		return "", fmt.Errorf("claude usage: parsing credentials: %w", err)
	}
	token := strings.TrimSpace(creds.ClaudeAiOauth.AccessToken)
	if token == "" {
		return "", fmt.Errorf("claude usage: credentials file has no claudeAiOauth.accessToken")
	}
	return token, nil
}

func (c *Client) fetch(ctx context.Context, token string) (Usage, error) {
	baseURL := strings.TrimRight(c.BaseURL, "/")
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}

	timeout := c.Timeout
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, baseURL+usagePath, nil)
	if err != nil {
		return Usage{}, fmt.Errorf("claude usage: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set(betaHeader, betaValue)

	httpClient := c.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return Usage{}, fmt.Errorf("claude usage: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Deliberately status-only: never carry a response body (which
		// could echo request details) into an error the runner may log.
		return Usage{}, fmt.Errorf("claude usage: unexpected HTTP status %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes))
	if err != nil {
		return Usage{}, fmt.Errorf("claude usage: reading response: %w", err)
	}
	return parseUsage(body)
}

type responseWindow struct {
	Utilization *float64 `json:"utilization"`
	ResetsAt    *string  `json:"resets_at"`
}

type usageResponse struct {
	FiveHour *responseWindow `json:"five_hour"`
	SevenDay *responseWindow `json:"seven_day"`
}

func parseUsage(body []byte) (Usage, error) {
	var raw usageResponse
	if err := json.Unmarshal(body, &raw); err != nil {
		return Usage{}, fmt.Errorf("claude usage: parsing response: %w", err)
	}

	var usage Usage
	var err error
	if usage.FiveHour, err = convertWindow("five_hour", raw.FiveHour); err != nil {
		return Usage{}, err
	}
	if usage.SevenDay, err = convertWindow("seven_day", raw.SevenDay); err != nil {
		return Usage{}, err
	}
	return usage, nil
}

func convertWindow(name string, raw *responseWindow) (*Window, error) {
	if raw == nil {
		return nil, nil
	}
	w := &Window{}
	if raw.Utilization != nil {
		w.Utilization = *raw.Utilization
	}
	if raw.ResetsAt != nil && strings.TrimSpace(*raw.ResetsAt) != "" {
		resetsAt, err := time.Parse(time.RFC3339, strings.TrimSpace(*raw.ResetsAt))
		if err != nil {
			return nil, fmt.Errorf("claude usage: parsing %s.resets_at: %w", name, err)
		}
		w.ResetsAt = resetsAt
	}
	return w, nil
}

func redactError(err error, token string) error {
	if err == nil {
		return nil
	}
	if token == "" || !strings.Contains(err.Error(), token) {
		return err
	}
	return fmt.Errorf("%s", strings.ReplaceAll(err.Error(), token, "[redacted]"))
}
