// Package jev is a client for Jev, TypeSafe's judgment model API. It sends
// a batch of questions alongside arbitrary state and gets back one answer
// per question -- either a "noul" (a yes probability) or a "choice" (a pick
// among options with a confidence and per-option probabilities).
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// defaultBaseURL is used when Client.BaseURL is empty.
const defaultBaseURL = "https://api.typesafe.ai"

// askPath is the systemone endpoint, relative to BaseURL.
const askPath = "/v1/systemone"

// retryDelays are the backoff delays between retry attempts on a 429 or
// 529 response -- exponential, 1s/2s/4s, three retries max.
var retryDelays = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// ErrNoAPIKey is returned by Ask when Client.APIKey is empty -- Ask never
// makes a request in that case.
var ErrNoAPIKey = errors.New("jev: no API key configured")

// Client calls Jev's systemone endpoint.
type Client struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

// Question is one question in an Ask request. Type is "noul" or "choice".
// Criteria is question-type-specific (e.g. a choice's option list) and
// omitted entirely when nil.
type Question struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// Answer is one answer in an Ask response. For a "noul" question, Noul is
// non-nil and is the probability (0-1) of yes. For a "choice" question,
// Choice/Confidence/Probabilities are set instead.
type Answer struct {
	Type          string
	Noul          *float64
	Choice        string
	Confidence    float64
	Probabilities map[string]float64
}

// rawAnswer is the wire shape of one entry in the response's "answers"
// object -- Jev doesn't echo back a "type" field, so Answer.Type is
// inferred from which fields are present.
type rawAnswer struct {
	Noul          *float64           `json:"noul,omitempty"`
	Choice        string             `json:"choice,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
}

type askRequest struct {
	Model     string              `json:"model"`
	State     any                 `json:"state"`
	Questions map[string]Question `json:"questions"`
}

type askResponse struct {
	Answers map[string]rawAnswer `json:"answers"`
}

// Ask sends state and questions to Jev and returns one Answer per question
// ID. It retries a 429 or 529 response up to three times with exponential
// backoff (1s, 2s, 4s), honoring ctx cancellation during the wait; any
// other non-2xx status is returned immediately without retry. The returned
// error never includes the API key or the Authorization header.
func (c *Client) Ask(ctx context.Context, state any, questions map[string]Question) (map[string]Answer, error) {
	if c.APIKey == "" {
		return nil, ErrNoAPIKey
	}

	body, err := json.Marshal(askRequest{Model: c.Model, State: state, Questions: questions})
	if err != nil {
		return nil, fmt.Errorf("jev: encoding request: %w", err)
	}

	var lastErr error
	for attempt := 0; ; attempt++ {
		answers, retryable, err := c.doAsk(ctx, body)
		if err == nil {
			return answers, nil
		}
		lastErr = err
		if !retryable || attempt >= len(retryDelays) {
			return nil, lastErr
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(retryDelays[attempt]):
		}
	}
}

// doAsk makes a single HTTP attempt. retryable is true only for a 429 or
// 529 response.
func (c *Client) doAsk(ctx context.Context, body []byte) (answers map[string]Answer, retryable bool, err error) {
	baseURL := c.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = http.DefaultClient
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+askPath, bytes.NewReader(body))
	if err != nil {
		return nil, false, fmt.Errorf("jev: building request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.APIKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("jev: request failed: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, false, fmt.Errorf("jev: reading response: %w", err)
	}

	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == 529 {
		return nil, true, fmt.Errorf("jev: status %d", resp.StatusCode)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("jev: status %d: %s", resp.StatusCode, data)
	}

	var parsed askResponse
	if err := json.Unmarshal(data, &parsed); err != nil {
		return nil, false, fmt.Errorf("jev: decoding response: %w", err)
	}

	answers = make(map[string]Answer, len(parsed.Answers))
	for id, ra := range parsed.Answers {
		a := Answer{Choice: ra.Choice, Confidence: ra.Confidence, Probabilities: ra.Probabilities}
		if ra.Noul != nil {
			a.Type = "noul"
			a.Noul = ra.Noul
		} else {
			a.Type = "choice"
		}
		answers[id] = a
	}
	return answers, false, nil
}
