package ai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// TestIsTransportError pins the classifier's two-sided rule: network causes
// underneath a *url.Error (what client.Do returns) are transport failures;
// the CALLER ending its own request — cancelled or past its deadline — is not,
// even though that also surfaces through *url.Error.
func TestIsTransportError(t *testing.T) {
	refused := &url.Error{Op: "Post", URL: "http://127.0.0.1:1/x", Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}}
	dnsFailure := &url.Error{Op: "Post", URL: "http://no-such-host.invalid/x", Err: &net.OpError{
		Op: "dial", Net: "tcp", Err: &net.DNSError{Err: "no such host", Name: "no-such-host.invalid", IsNotFound: true}}}
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"connection refused", refused, true},
		{"dns failure", dnsFailure, true},
		{"wrapped by the provider layer", fmt.Errorf("openai API request: %w", refused), true},
		{"caller cancelled", &url.Error{Op: "Post", URL: "http://127.0.0.1:1/x", Err: context.Canceled}, false},
		{"caller deadline", &url.Error{Op: "Post", URL: "http://127.0.0.1:1/x", Err: context.DeadlineExceeded}, false},
		{"bare context.Canceled", context.Canceled, false},
		{"ordinary error", errors.New("boom"), false},
		{"nil", nil, false},
	} {
		if got := isTransportError(tc.err); got != tc.want {
			t.Errorf("%s: isTransportError = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// countingTransport counts every round trip so tests can assert how many
// attempts reached the (dead) endpoint.
type countingTransport struct {
	inner http.RoundTripper
	n     *atomic.Int32
}

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.n.Add(1)
	return c.inner.RoundTrip(r)
}

// refusedEndpointProvider points a compat provider at a CLOSED local listener:
// every attempt gets connection refused, through a counting transport.
func refusedEndpointProvider(t *testing.T) (*OpenAIProvider, *atomic.Int32) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	base := srv.URL + "/chat/completions"
	srv.Close()
	var attempts atomic.Int32
	client := &http.Client{Transport: &countingTransport{inner: http.DefaultTransport, n: &attempts}}
	return compatErrProvider(client, base), &attempts
}

// A connection-refused endpoint must be retried within the existing budget
// and then surface as ErrProviderDown with the network error's own text
// still visible ("dial tcp …", the part the user can act on).
func TestRetry_ConnectionRefusedRetriedAsProviderDown(t *testing.T) {
	p, attempts := refusedEndpointProvider(t)
	rp := NewRetryingProviderWithConfig(p, 3, time.Millisecond)

	_, err := rp.Chat(context.Background(), compatChatReq())
	if !errors.Is(err, ErrProviderDown) {
		t.Fatalf("want ErrProviderDown-wrapped error, got %v", err)
	}
	if !strings.Contains(err.Error(), "dial tcp") {
		t.Errorf("the transport error's text must stay visible, got %q", err.Error())
	}
	if attempts.Load() != 3 {
		t.Errorf("attempts = %d, want 3 (retried within the budget)", attempts.Load())
	}
}

// The caller cancelling its request is NOT a provider outage: no retry, no
// ErrProviderDown wrapping — the cancellation surfaces unchanged.
func TestRetry_CallerCancelNotRetriedNorWrapped(t *testing.T) {
	stub := &retryStub{chatErrs: []error{
		&url.Error{Op: "Post", URL: "http://127.0.0.1:1/x", Err: context.Canceled}}}
	rp := NewRetryingProviderWithConfig(stub, 3, time.Millisecond)

	_, err := rp.Chat(context.Background(), Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if errors.Is(err, ErrProviderDown) {
		t.Errorf("a caller cancellation must not read as ErrProviderDown: %v", err)
	}
	if stub.chatCalls != 1 {
		t.Errorf("calls = %d, want 1 (cancellation is not retryable)", stub.chatCalls)
	}
}

// Transport failures count toward the circuit-open threshold like any other
// provider-down condition: five connection-refused calls open the circuit.
func TestCircuitBreaker_TransportFailuresTrip(t *testing.T) {
	resetBreakerRegistry()
	t.Cleanup(resetBreakerRegistry)
	p, _ := refusedEndpointProvider(t)
	cb := NewCircuitBreakerProvider(p)

	for range cbFailureThreshold {
		if _, err := cb.Chat(context.Background(), compatChatReq()); !isTransportError(err) {
			t.Fatalf("want a transport error per call, got %v", err)
		}
	}
	if _, err := cb.Chat(context.Background(), compatChatReq()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("want ErrCircuitOpen after %d transport failures, got %v", cbFailureThreshold, err)
	}
}
