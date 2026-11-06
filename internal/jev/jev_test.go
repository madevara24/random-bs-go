package jev

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestAskSendsBearerAndModel(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		gotBody = string(body)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"answers":{"risky":{"noul":0.12}}}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "k", Model: "jev-latest", BaseURL: srv.URL}
	answers, err := c.Ask(context.Background(), map[string]any{"foo": "bar"}, map[string]Question{
		"risky": {Type: "noul", Instructions: "is it risky?"},
	})
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if gotAuth != "Bearer k" {
		t.Errorf("Authorization = %q, want %q", gotAuth, "Bearer k")
	}
	if !strings.Contains(gotBody, `"model":"jev-latest"`) {
		t.Errorf("body = %s, want it to contain model jev-latest", gotBody)
	}
	if !strings.Contains(gotBody, `"risky"`) || !strings.Contains(gotBody, "is it risky?") {
		t.Errorf("body = %s, want it to contain the risky question", gotBody)
	}
	ans, ok := answers["risky"]
	if !ok || ans.Noul == nil || *ans.Noul != 0.12 {
		t.Errorf("answers[risky] = %+v, want Noul 0.12", ans)
	}
}

func TestAskRetries529ThenSucceeds(t *testing.T) {
	restore := setRetryDelaysForTest(time.Millisecond, time.Millisecond, time.Millisecond)
	defer restore()

	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := count.Add(1)
		if n < 3 {
			w.WriteHeader(529)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"answers":{}}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "k", BaseURL: srv.URL}
	_, err := c.Ask(context.Background(), nil, nil)
	if err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if count.Load() != 3 {
		t.Errorf("request count = %d, want 3", count.Load())
	}
}

func TestAskNoRetryOn400(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"max_tokens_exceeded"}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: "k", BaseURL: srv.URL}
	_, err := c.Ask(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("Ask: want error on 400, got nil")
	}
	if count.Load() != 1 {
		t.Errorf("request count = %d, want 1 (no retry on 400)", count.Load())
	}
}

func TestAskNoKey(t *testing.T) {
	var count atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	c := &Client{APIKey: "", BaseURL: srv.URL}
	_, err := c.Ask(context.Background(), nil, nil)
	if !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("err = %v, want ErrNoAPIKey", err)
	}
	if count.Load() != 0 {
		t.Errorf("request count = %d, want 0", count.Load())
	}
}

func TestAskErrorNeverContainsKey(t *testing.T) {
	const key = "sk-super-secret-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"bad key"}`))
	}))
	defer srv.Close()

	c := &Client{APIKey: key, BaseURL: srv.URL}
	_, err := c.Ask(context.Background(), nil, nil)
	if err == nil {
		t.Fatal("Ask: want error on 401, got nil")
	}
	if strings.Contains(err.Error(), key) {
		t.Errorf("error %q contains the API key", err.Error())
	}
}

// setRetryDelaysForTest swaps the package's retry backoff for the test's
// duration so TestAskRetries529ThenSucceeds doesn't take real seconds to
// run, and returns a func to restore the original delays.
func setRetryDelaysForTest(delays ...time.Duration) (restore func()) {
	orig := retryDelays
	retryDelays = delays
	return func() { retryDelays = orig }
}
