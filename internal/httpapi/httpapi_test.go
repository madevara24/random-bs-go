package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestHealthAlwaysOK(t *testing.T) {
	s := New(make(chan struct{}, 1))
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatalf("GET /health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200", resp.StatusCode)
	}
}

func TestDispatchWakesAndCoalesces(t *testing.T) {
	wake := make(chan struct{}, 1)
	s := New(wake)
	ts := httptest.NewServer(s)
	defer ts.Close()

	post := func() *http.Response {
		resp, err := http.Post(ts.URL+"/dispatch", "", nil)
		if err != nil {
			t.Fatalf("POST /dispatch: %v", err)
		}
		return resp
	}

	// First call: replies immediately and signals wake.
	resp1 := post()
	if resp1.StatusCode != http.StatusAccepted {
		t.Errorf("first POST /dispatch status = %d, want 202", resp1.StatusCode)
	}

	// Second call before anyone drains wake: still replies immediately
	// (non-blocking send), and the extra signal is correctly dropped since
	// the channel is already full.
	done := make(chan struct{})
	go func() {
		resp2 := post()
		if resp2.StatusCode != http.StatusAccepted {
			t.Errorf("second POST /dispatch status = %d, want 202", resp2.StatusCode)
		}
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(1 * time.Second):
		t.Fatal("second POST /dispatch blocked -- handleDispatch must never block on a full wake channel")
	}

	select {
	case <-wake:
	default:
		t.Fatal("wake channel never received a signal from POST /dispatch")
	}
	// Channel should now be empty -- the second POST's signal was
	// correctly coalesced away, not queued.
	select {
	case <-wake:
		t.Fatal("wake channel had a second signal queued -- coalescing failed")
	default:
	}
}

func TestDispatchRejectsNonPost(t *testing.T) {
	s := New(make(chan struct{}, 1))
	ts := httptest.NewServer(s)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/dispatch")
	if err != nil {
		t.Fatalf("GET /dispatch: %v", err)
	}
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /dispatch status = %d, want 405", resp.StatusCode)
	}
}
