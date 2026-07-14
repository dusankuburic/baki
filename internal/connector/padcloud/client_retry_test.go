package padcloud

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"
)

// errThenOKTransport returns a transport-level error for the first `fails`
// round-trips, then a 200. A transport error yields a nil *http.Response, which
// is the path that previously panicked in retryAfter.
type errThenOKTransport struct {
	fails int
	calls int
}

func (t *errThenOKTransport) RoundTrip(*http.Request) (*http.Response, error) {
	t.calls++
	if t.calls <= t.fails {
		return nil, fmt.Errorf("simulated network error")
	}
	return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
}

// TestRetryAfterNilResponse guards the regression where retryAfter dereferenced
// a nil *http.Response (passed on the network-error retry path) and panicked.
func TestRetryAfterNilResponse(t *testing.T) {
	// Must not panic and must return a non-negative backoff.
	if d := retryAfter(nil, 0); d < 0 {
		t.Fatalf("retryAfter(nil, 0) = %v, want >= 0", d)
	}
}

// TestDoWithRetry_RetriesTransportError verifies a transient network error is
// retried (not panicked on) and the eventual success is returned.
func TestDoWithRetry_RetriesTransportError(t *testing.T) {
	tr := &errThenOKTransport{fails: 1}
	c := &HTTPClient{
		http:       &http.Client{Transport: tr, Timeout: 5 * time.Second},
		maxRetries: 3,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	resp, err := c.doWithRetry(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, "http://example.invalid/x", nil)
	})
	if err != nil {
		t.Fatalf("doWithRetry after one transport error = %v, want success", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if tr.calls != 2 {
		t.Fatalf("transport calls = %d, want 2 (1 failure + 1 success)", tr.calls)
	}
}
