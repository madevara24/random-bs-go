package notify

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// TestPostForumClaimSendsThreadNameAndContent confirms PostForumClaim posts
// the exact shape a forum webhook needs to create a new post (thread_name +
// content, no attachment, ?wait=true so Discord returns the starter
// message), and that the returned thread ID is the response's channel_id --
// the new thread's ID.
func TestPostForumClaimSendsThreadNameAndContent(t *testing.T) {
	var gotBody []byte
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"channel_id":"111222333"}`))
	}))
	defer srv.Close()

	threadID, err := PostForumClaim(srv.URL, "MDC-10 Commit SHA in Health and Deploy Poll", "Task `MDC-10 Commit SHA in Health and Deploy Poll` is claimed")
	if err != nil {
		t.Fatalf("PostForumClaim: %v", err)
	}
	if threadID != "111222333" {
		t.Errorf("threadID = %q, want %q", threadID, "111222333")
	}
	if !strings.Contains(gotQuery, "wait=true") {
		t.Errorf("query = %q, want it to contain wait=true", gotQuery)
	}

	var payload struct {
		ThreadName string `json:"thread_name"`
		Content    string `json:"content"`
	}
	if err := json.Unmarshal(gotBody, &payload); err != nil {
		t.Fatalf("decoding request body: %v", err)
	}
	if payload.ThreadName != "MDC-10 Commit SHA in Health and Deploy Poll" {
		t.Errorf("thread_name = %q, want the task title", payload.ThreadName)
	}
	if payload.Content != "Task `MDC-10 Commit SHA in Health and Deploy Poll` is claimed" {
		t.Errorf("content = %q, want the claim message", payload.Content)
	}
	if strings.Contains(payload.Content, "<@") {
		t.Errorf("content = %q, want no @-mentions on a bare claim", payload.Content)
	}
}

// TestPostForumClaimRetriesExactlyOnce confirms PostForumClaim retries once
// on failure (same contract as sendSync's own retry), not zero and not
// unbounded.
func TestPostForumClaimRetriesExactlyOnce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if n == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"channel_id":"999"}`))
	}))
	defer srv.Close()

	threadID, err := PostForumClaim(srv.URL, "some task", "Task `some task` is claimed")
	if err != nil {
		t.Fatalf("PostForumClaim: %v", err)
	}
	if threadID != "999" {
		t.Errorf("threadID = %q, want %q", threadID, "999")
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("webhook hit %d times, want exactly 2 (1 attempt + 1 retry)", got)
	}
}

// TestPostForumClaimFailsAfterExhaustingRetry confirms a webhook that
// always fails surfaces an error after exactly one retry, not fewer.
func TestPostForumClaimFailsAfterExhaustingRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	if _, err := PostForumClaim(srv.URL, "some task", "Task `some task` is claimed"); err == nil {
		t.Fatal("PostForumClaim returned nil error, want an error after exhausting the retry")
	}
	if got := hits.Load(); got != 2 {
		t.Errorf("webhook hit %d times, want exactly 2 (1 attempt + 1 retry)", got)
	}
}

// TestSendRoutesToForumWebhookWhenThreadIDAndForumURLSet confirms Send
// posts into the task's own thread (via ForumWebhookURL, ?thread_id=) when
// both a threadID and a configured ForumWebhookURL are present, not the
// plain top-level webhook.
func TestSendRoutesToForumWebhookWhenThreadIDAndForumURLSet(t *testing.T) {
	var forumQuery string
	forum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forumQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer forum.Close()

	topLevelHit := false
	topLevel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topLevelHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer topLevel.Close()

	n := &Notifier{WebhookURL: topLevel.URL, ForumWebhookURL: forum.URL}
	n.sendSync("", "777666555", "test message", "", "")

	if topLevelHit {
		t.Error("top-level webhook was hit, want only the forum webhook")
	}
	if !strings.Contains(forumQuery, "thread_id=777666555") {
		t.Errorf("forum query = %q, want it to contain thread_id=777666555", forumQuery)
	}
}

// TestSendFallsBackToTopLevelWhenForumURLUnset confirms that even with a
// non-empty threadID, an unset ForumWebhookURL means every message still
// goes to the plain top-level webhook -- the rollout switch being off must
// behave exactly as before per-task threads existed.
func TestSendFallsBackToTopLevelWhenForumURLUnset(t *testing.T) {
	var gotQuery string
	topLevel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.RawQuery
		w.WriteHeader(http.StatusOK)
	}))
	defer topLevel.Close()

	n := &Notifier{WebhookURL: topLevel.URL}
	n.sendSync("", "777666555", "test message", "", "")

	if strings.Contains(gotQuery, "thread_id") {
		t.Errorf("top-level query = %q, want no thread_id when ForumWebhookURL is unset", gotQuery)
	}
}

// TestSendFallsBackToTopLevelWhenThreadIDEmpty confirms a message not tied
// to a task's thread (empty threadID -- e.g. the watcher's Tier 1 health
// alert) goes to the top-level webhook even when ForumWebhookURL is
// configured.
func TestSendFallsBackToTopLevelWhenThreadIDEmpty(t *testing.T) {
	forumHit := false
	forum := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forumHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer forum.Close()

	topLevelHit := false
	topLevel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		topLevelHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer topLevel.Close()

	n := &Notifier{WebhookURL: topLevel.URL, ForumWebhookURL: forum.URL}
	n.sendSync("", "", "test message", "", "")

	if forumHit {
		t.Error("forum webhook was hit, want the top-level webhook for a message with no threadID")
	}
	if !topLevelHit {
		t.Error("top-level webhook was never hit")
	}
}
