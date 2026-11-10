package claudeusage

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testToken = "sk-ant-oat01-test-token-DO-NOT-LEAK"

func writeCredentials(t *testing.T, token string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".credentials.json")
	body, err := json.Marshal(map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken": token,
			"scopes":      []string{"user:profile"},
		},
	})
	if err != nil {
		t.Fatalf("marshal credentials: %v", err)
	}
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}
	return path
}

func assertNoToken(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("error leaks the access token: %v", err)
	}
}

func TestFetchParsesUsageAndSendsOAuthHeaders(t *testing.T) {
	const body = `{
		"five_hour": {"utilization": 42.5, "resets_at": "2026-11-10T12:34:56Z"},
		"seven_day": {"utilization": 7, "resets_at": "2026-11-17T01:02:03.123Z"},
		"extra": "ignored"
	}`

	var gotPath, gotAuth, gotBeta string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotBeta = r.Header.Get("anthropic-beta")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()

	creds := writeCredentials(t, testToken)
	client := NewClient(server.URL, creds)

	usage, err := client.Fetch(context.Background())
	if err != nil {
		t.Fatalf("Fetch: %v", err)
	}

	if gotPath != "/api/oauth/usage" {
		t.Errorf("path = %q, want /api/oauth/usage", gotPath)
	}
	if gotAuth != "Bearer "+testToken {
		t.Errorf("Authorization header = %q, want Bearer <token>", gotAuth)
	}
	if gotBeta != "oauth-2025-04-20" {
		t.Errorf("anthropic-beta = %q, want oauth-2025-04-20", gotBeta)
	}

	if usage.FiveHour == nil {
		t.Fatal("five_hour was not parsed")
	}
	if usage.FiveHour.Utilization != 42.5 {
		t.Errorf("five_hour utilization = %v, want 42.5", usage.FiveHour.Utilization)
	}
	wantFive := time.Date(2026, 11, 10, 12, 34, 56, 0, time.UTC)
	if !usage.FiveHour.ResetsAt.Equal(wantFive) {
		t.Errorf("five_hour resets_at = %v, want %v", usage.FiveHour.ResetsAt, wantFive)
	}

	if usage.SevenDay == nil {
		t.Fatal("seven_day was not parsed")
	}
	if usage.SevenDay.Utilization != 7 {
		t.Errorf("seven_day utilization = %v, want 7", usage.SevenDay.Utilization)
	}
	wantSeven := time.Date(2026, 11, 17, 1, 2, 3, 123000000, time.UTC)
	if !usage.SevenDay.ResetsAt.Equal(wantSeven) {
		t.Errorf("seven_day resets_at = %v, want %v", usage.SevenDay.ResetsAt, wantSeven)
	}
}

func TestFetchToleratesMissingAndNullWindows(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantFive bool
		wantSev  bool
	}{
		{name: "only five_hour", body: `{"five_hour":{"utilization":1,"resets_at":"2026-11-10T12:00:00Z"}}`, wantFive: true},
		{name: "only seven_day", body: `{"seven_day":{"utilization":2,"resets_at":"2026-11-17T12:00:00Z"}}`, wantSev: true},
		{name: "null window", body: `{"five_hour":null,"seven_day":{"utilization":3}}`, wantSev: true},
		{name: "empty object", body: `{}`},
		{name: "zero utilization is kept", body: `{"five_hour":{"utilization":0,"resets_at":"2026-11-10T12:00:00Z"}}`, wantFive: true},
		{name: "missing resets_at", body: `{"seven_day":{"utilization":9}}`, wantSev: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer server.Close()

			client := NewClient(server.URL, writeCredentials(t, testToken))
			usage, err := client.Fetch(context.Background())
			if err != nil {
				t.Fatalf("Fetch: %v", err)
			}
			if (usage.FiveHour != nil) != tt.wantFive {
				t.Errorf("FiveHour present = %t, want %t", usage.FiveHour != nil, tt.wantFive)
			}
			if (usage.SevenDay != nil) != tt.wantSev {
				t.Errorf("SevenDay present = %t, want %t", usage.SevenDay != nil, tt.wantSev)
			}
		})
	}
}

func TestFetchReadsTokenFreshOnEveryCall(t *testing.T) {
	var auths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auths = append(auths, r.Header.Get("Authorization"))
		_, _ = w.Write([]byte(`{"five_hour":{"utilization":1}}`))
	}))
	defer server.Close()

	path := writeCredentials(t, "first-token")
	client := NewClient(server.URL, path)

	if _, err := client.Fetch(context.Background()); err != nil {
		t.Fatalf("first Fetch: %v", err)
	}

	// Rewrite the credentials file between calls: Fetch must re-read it.
	body := `{"claudeAiOauth":{"accessToken":"second-token"}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("rewrite credentials: %v", err)
	}
	if _, err := client.Fetch(context.Background()); err != nil {
		t.Fatalf("second Fetch: %v", err)
	}

	if len(auths) != 2 {
		t.Fatalf("got %d requests, want 2", len(auths))
	}
	if auths[0] != "Bearer first-token" {
		t.Errorf("first Authorization = %q", auths[0])
	}
	if auths[1] != "Bearer second-token" {
		t.Errorf("second Authorization = %q (token was cached)", auths[1])
	}
}

func TestFetchFailureModes(t *testing.T) {
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		credsPath func(t *testing.T) string
		timeout   time.Duration
		wantSub   string
	}{
		{
			name:      "non-2xx",
			handler:   func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) },
			credsPath: func(t *testing.T) string { return writeCredentials(t, testToken) },
			wantSub:   "401",
		},
		{
			name: "malformed json",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"five_hour":`))
			},
			credsPath: func(t *testing.T) string { return writeCredentials(t, testToken) },
			wantSub:   "parsing response",
		},
		{
			name: "malformed resets_at",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = w.Write([]byte(`{"five_hour":{"utilization":1,"resets_at":"not-a-time"}}`))
			},
			credsPath: func(t *testing.T) string { return writeCredentials(t, testToken) },
			wantSub:   "resets_at",
		},
		{
			name:      "missing credentials file",
			handler:   func(w http.ResponseWriter, r *http.Request) {},
			credsPath: func(t *testing.T) string { return filepath.Join(t.TempDir(), "nope.json") },
			wantSub:   "reading credentials",
		},
		{
			name:      "missing token",
			handler:   func(w http.ResponseWriter, r *http.Request) {},
			credsPath: func(t *testing.T) string { return writeCredentials(t, "") },
			wantSub:   "accessToken",
		},
		{
			name: "timeout",
			handler: func(w http.ResponseWriter, r *http.Request) {
				time.Sleep(300 * time.Millisecond)
			},
			credsPath: func(t *testing.T) string { return writeCredentials(t, testToken) },
			timeout:   25 * time.Millisecond,
			wantSub:   "request failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(tt.handler)
			defer server.Close()

			client := NewClient(server.URL, tt.credsPath(t))
			if tt.timeout > 0 {
				client.Timeout = tt.timeout
				client.HTTPClient = &http.Client{Timeout: tt.timeout}
			}

			_, err := client.Fetch(context.Background())
			assertNoToken(t, err)
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Errorf("error = %q, want it to contain %q", err, tt.wantSub)
			}
		})
	}
}

func TestFetchMissingTokenWithBlankJSONValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".credentials.json")
	if err := os.WriteFile(path, []byte(`{"claudeAiOauth":{"accessToken":"   "}}`), 0o600); err != nil {
		t.Fatalf("write credentials: %v", err)
	}

	client := NewClient("http://127.0.0.1:1", path)
	_, err := client.Fetch(context.Background())
	assertNoToken(t, err)
	if err == nil || !strings.Contains(err.Error(), "accessToken") {
		t.Fatalf("error = %v, want a missing-token error", err)
	}
}

func TestFetchErrorRedactsTokenIfItEverAppears(t *testing.T) {
	// Defensive path: a transport error that somehow contains the token is
	// scrubbed before it can reach a log or Discord message.
	err := redactError(os.ErrNotExist, testToken)
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("unrelated error handling leaked the token: %v", err)
	}

	err = redactError(&tokenBearingError{token: testToken}, testToken)
	if strings.Contains(err.Error(), testToken) {
		t.Fatalf("redactError leaked the token: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("redactError = %q, want [redacted]", err)
	}
}

type tokenBearingError struct{ token string }

func (e *tokenBearingError) Error() string { return "transport failed for " + e.token }

func TestNewClientDefaultsAndTrimsBaseURL(t *testing.T) {
	client := NewClient("https://example.test/", "/tmp/creds.json")
	if client.BaseURL != "https://example.test" {
		t.Errorf("BaseURL = %q, want trailing slash trimmed", client.BaseURL)
	}
	if client.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", client.Timeout)
	}

	path, err := DefaultCredentialsPath()
	if err != nil {
		t.Fatalf("DefaultCredentialsPath: %v", err)
	}
	if !strings.HasSuffix(path, filepath.Join(".claude", ".credentials.json")) {
		t.Errorf("DefaultCredentialsPath = %q, want ~/.claude/.credentials.json", path)
	}
}

func TestFetchRejectsBadBaseURL(t *testing.T) {
	client := NewClient("://bad-url", writeCredentials(t, testToken))
	_, err := client.Fetch(context.Background())
	assertNoToken(t, err)
	if err == nil || !strings.Contains(err.Error(), "building request") {
		t.Fatalf("error = %v, want a building-request error", err)
	}
}
