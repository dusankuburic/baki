package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/platform/hmacsig"
)

// A rate-limited call (429) is signed again and resent after the server's Retry-After; a call the limit refuses
// past the retries exits 2 like any other refusal.
func TestCLI_ReportRetriesRateLimitedCalls(t *testing.T) {
	var calls atomic.Int32
	limitUntil := int32(2)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		ts, _ := strconv.ParseInt(r.Header.Get(hmacsig.TimestampHeader), 10, 64)
		if !hmacsig.Verify([]byte("topsecret"), ts, body, r.Header.Get(hmacsig.SignatureHeader)) {
			t.Errorf("attempt %d is not signed", calls.Load()+1)
		}
		if calls.Add(1) <= atomic.LoadInt32(&limitUntil) {
			w.Header().Set("Retry-After", "0")
			http.Error(w, `{"error":"rate limit exceeded"}`, http.StatusTooManyRequests)
			return
		}
		_, _ = io.WriteString(w, `{"flowId":"f","passed":true,"gate":"error"}`)
	}))
	t.Cleanup(srv.Close)

	_, stderr, code := runBakicli(t, "report", "-endpoint", srv.URL, "-secret", "topsecret", "-flow-id", "f")
	if code != 0 || calls.Load() != 3 || strings.Count(stderr, "rate limited") != 2 {
		t.Fatalf("exit %d after %d calls, stderr %q; want 0 after 3 calls and two retry notes", code, calls.Load(), stderr)
	}

	calls.Store(0)
	atomic.StoreInt32(&limitUntil, 100)
	_, stderr, code = runBakicli(t, "report", "-endpoint", srv.URL, "-secret", "topsecret", "-flow-id", "f")
	if code != 2 || calls.Load() != ciRetries+1 || !strings.Contains(stderr, "429") {
		t.Errorf("exit %d after %d calls, stderr %q; want 2 after %d calls", code, calls.Load(), stderr, ciRetries+1)
	}
}

func TestCIRetryWait(t *testing.T) {
	for _, c := range []struct {
		header   string
		attempt  int
		min, max time.Duration
	}{
		{"", 0, time.Second, 1500 * time.Millisecond},
		{"", 2, 4 * time.Second, 6 * time.Second},
		{"3", 0, 3 * time.Second, 4500 * time.Millisecond},
		{"0", 3, 0, 0},
		{"3600", 0, ciMaxRetryWait, ciMaxRetryWait},
		{"soon", 1, 2 * time.Second, 3 * time.Second},
	} {
		for range 20 {
			if got := ciRetryWait(c.header, c.attempt); got < c.min || got > c.max {
				t.Errorf("ciRetryWait(%q, %d) = %v, want within [%v, %v]", c.header, c.attempt, got, c.min, c.max)
				break
			}
		}
	}
}
