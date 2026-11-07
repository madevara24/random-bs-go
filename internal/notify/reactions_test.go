package notify

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestFetchReactionsAbsentField confirms a message with no reactions at
// all -- Discord omits the "reactions" key entirely, rather than returning
// an empty list -- comes back as an empty, non-nil map and no error.
func TestFetchReactionsAbsentField(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"42","content":"plan review"}`))
	}))
	defer srv.Close()

	reactions, err := FetchReactions(srv.URL, "thread-1", "42")
	if err != nil {
		t.Fatalf("FetchReactions: %v", err)
	}
	if reactions == nil {
		t.Fatal("reactions = nil, want a non-nil empty map")
	}
	if len(reactions) != 0 {
		t.Errorf("reactions = %+v, want empty", reactions)
	}
	if !strings.Contains(gotPath, "/messages/42") {
		t.Errorf("path = %q, want it to contain %q", gotPath, "/messages/42")
	}
	if !strings.Contains(gotQuery, "thread_id=thread-1") {
		t.Errorf("query = %q, want it to contain %q", gotQuery, "thread_id=thread-1")
	}
}

// TestFetchReactionsCounts confirms a message with reactions comes back as
// an emoji-name-to-count map, built from Discord's own reactions list
// shape.
func TestFetchReactionsCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"42","reactions":[{"emoji":{"name":"✅"},"count":1},{"emoji":{"name":"❌"},"count":2}]}`))
	}))
	defer srv.Close()

	reactions, err := FetchReactions(srv.URL, "thread-1", "42")
	if err != nil {
		t.Fatalf("FetchReactions: %v", err)
	}
	if reactions["✅"] != 1 {
		t.Errorf("reactions[✅] = %d, want 1", reactions["✅"])
	}
	if reactions["❌"] != 2 {
		t.Errorf("reactions[❌] = %d, want 2", reactions["❌"])
	}
}

// TestFetchReactionsErrorStatus confirms a non-2xx response is surfaced as
// an error, not silently treated as "no reactions."
func TestFetchReactionsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("unknown message"))
	}))
	defer srv.Close()

	if _, err := FetchReactions(srv.URL, "thread-1", "42"); err == nil {
		t.Fatal("FetchReactions returned nil error, want one on a 404")
	}
}
