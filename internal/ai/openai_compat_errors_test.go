package ai

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// compatErrProvider builds an OpenAI-compatible provider pointed at an
// arbitrary base URL, for exercising openaiBase's error classification with
// bodies a gateway (not the model API) produces.
func compatErrProvider(client *http.Client, base string) *OpenAIProvider {
	return &OpenAIProvider{openaiBase: openaiBase{
		apiKey: "sk-test", client: client, baseURL: &base, providerLabel: "openai",
	}}
}

// compatChatReq is one minimal chat request against the compat provider.
func compatChatReq() Request {
	return Request{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hi"}}}
}

// A 429 with an EMPTY body (APIM/gateway throttling) must still classify as
// ErrRateLimited — before the status-first rework it fell through to a plain
// "API error (status 429)" that neither the retry wrapper nor the circuit
// breaker recognised.
func TestChatError_EmptyBody429IsRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")

	if _, err := p.Chat(context.Background(), compatChatReq()); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("empty-body 429: want ErrRateLimited, got %v", err)
	}
}

// A 502 with an HTML body (gateway/proxy error page) must classify as
// ErrProviderDown so it retries and counts toward the breaker.
func TestChatError_HTML502IsProviderDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("<html><body><h1>502 Bad Gateway</h1></body></html>"))
	}))
	defer srv.Close()
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")

	if _, err := p.Chat(context.Background(), compatChatReq()); !errors.Is(err, ErrProviderDown) {
		t.Fatalf("HTML-body 502: want ErrProviderDown, got %v", err)
	}
}

// A 401 with an empty body keeps the invalid-key mapping — the status says
// the credential was rejected even when the body explains nothing.
func TestChatError_EmptyBody401IsInvalidKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")

	if _, err := p.Chat(context.Background(), compatChatReq()); !errors.Is(err, ErrApiKeyInvalid) {
		t.Fatalf("empty-body 401: want ErrApiKeyInvalid, got %v", err)
	}
}

// 403 is classified like 401: forbidden is a credential problem, not a
// transient condition, whatever the body says.
func TestChatError_403MapsToInvalidKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":{"message":"access forbidden","code":"403"}}`))
	}))
	defer srv.Close()
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")

	if _, err := p.Chat(context.Background(), compatChatReq()); !errors.Is(err, ErrApiKeyInvalid) {
		t.Fatalf("403: want ErrApiKeyInvalid, got %v", err)
	}
}

// The stream handler classifies by status first too: empty-body 429/HTML 502
// must retry instead of surfacing as an unclassified stream error.
func TestStreamError_StatusFirstWithoutJSONEnvelope(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		check  func(*testing.T, error)
	}{
		{
			name:   "empty-body 429",
			status: http.StatusTooManyRequests,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrRateLimited) {
					t.Fatalf("want ErrRateLimited, got %v", err)
				}
			},
		},
		{
			name:   "JSON 429 without error.message",
			status: http.StatusTooManyRequests,
			body:   `{"error":{"code":"429"}}`,
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrRateLimited) {
					t.Fatalf("want ErrRateLimited, got %v", err)
				}
			},
		},
		{
			name:   "HTML 502",
			status: http.StatusBadGateway,
			body:   "<html>502 Bad Gateway</html>",
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrProviderDown) {
					t.Fatalf("want ErrProviderDown, got %v", err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")

			err := p.Stream(context.Background(), compatChatReq(), func(Chunk) {})
			if err == nil {
				t.Fatal("expected the non-200 to surface as a stream error")
			}
			tc.check(t, err)
		})
	}
}

// The Retry-After hint on a body-less 429 must still pace the retry wrapper:
// rateLimitErr (which parses retry-after-ms) is now built regardless of the
// body, so backoff waits at least the hint instead of the base delay.
func TestRetry_Bare429RetryAfterHonored(t *testing.T) {
	var attempts atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		w.Header().Set("retry-after-ms", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	rp := NewRetryingProviderWithConfig(compatErrProvider(srv.Client(), srv.URL+"/chat/completions"), 2, time.Millisecond)

	start := time.Now()
	_, err := rp.Chat(context.Background(), compatChatReq())
	elapsed := time.Since(start)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited after exhausting retries, got %v", err)
	}
	if attempts.Load() != 2 {
		t.Errorf("attempts = %d, want 2", attempts.Load())
	}
	if elapsed < 60*time.Millisecond {
		t.Errorf("elapsed %v < the 60ms retry-after-ms hint (hint not honored)", elapsed)
	}
}
