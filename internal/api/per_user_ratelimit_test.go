package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
)

func newPerUserTestRouter() *Router {
	return &Router{trustedProxies: nil}
}

func req(method, target string) *http.Request {
	return httptest.NewRequest(method, target, strings.NewReader(""))
}

// Authenticated write → bucket keyed on a hash of the userID (never the raw
// userID), so two requests from the same user share a bucket and the key can't
// be poisoned with delimiters/control-chars.
func TestPerUserKey_AuthedWriteIsHashedUserIDBucket(t *testing.T) {
	rt := newPerUserTestRouter()
	r := req(http.MethodPost, "/api/analysis/analyze")
	r = r.WithContext(auth.WithClaims(context.Background(), &auth.Claims{UserID: "user-42"}))

	key := rt.perUserKey(r)
	if !strings.HasPrefix(key, "ratelimit:peruser:") {
		t.Fatalf("key %q missing peruser prefix", key)
	}
	suffix := strings.TrimPrefix(key, "ratelimit:peruser:")
	if suffix == "" || suffix == "user-42" {
		t.Fatalf("key suffix must be a non-empty hash, not the raw userID; got %q", suffix)
	}
	// Deterministic: same userID → same bucket.
	key2 := rt.perUserKey(req(http.MethodPost, "/api/flow/upload").WithContext(auth.WithClaims(context.Background(), &auth.Claims{UserID: "user-42"})))
	if key != key2 {
		t.Errorf("same userID must hash to the same key: %q vs %q (per-endpoint isolation would defeat the total-write cap)", key, key2)
	}
	// Different user → different bucket.
	key3 := rt.perUserKey(req(http.MethodPost, "/api/analysis/analyze").WithContext(auth.WithClaims(context.Background(), &auth.Claims{UserID: "user-99"})))
	if key == key3 {
		t.Error("different userIDs must hash to different keys")
	}
}

// Reads bypass the per-user limiter (the per-IP limiter still covers them) —
// otherwise a user browsing findings could starve their own write budget.
func TestPerUserKey_ReadsAreSkipped(t *testing.T) {
	rt := newPerUserTestRouter()
	for _, m := range []string{http.MethodGet, http.MethodHead, http.MethodOptions} {
		r := req(m, "/api/analysis/dashboard").WithContext(auth.WithClaims(context.Background(), &auth.Claims{UserID: "u"}))
		if got := rt.perUserKey(r); got != "" {
			t.Errorf("%s returned key %q, want \"\" (reads must skip the per-user limiter)", m, got)
		}
	}
}

// An unauthenticated write (public POST like /api/auth/login) must fall back to a
// PER-IP key — NOT collapse into a shared bucket. Collapsing would lock out
// every legitimate login during a distributed credential-stuffing attack (the
// opposite of the goal).
func TestPerUserKey_UnauthedWriteFallsBackToIP(t *testing.T) {
	rt := newPerUserTestRouter()
	r := req(http.MethodPost, "/api/auth/login") // nil claims → public route
	key := rt.perUserKey(r)
	if !strings.HasPrefix(key, "ratelimit:peruser:ip:") {
		t.Fatalf("unauthed write key = %q, want per-IP fallback prefix", key)
	}
}

// nil claims with empty userID must not key on "" (which would share one bucket
// across all such requests) — it must take the per-IP branch.
func TestPerUserKey_NilClaimsTakesIPBranch(t *testing.T) {
	rt := newPerUserTestRouter()
	r := req(http.MethodPut, "/api/something")
	key := rt.perUserKey(r)
	if !strings.HasPrefix(key, "ratelimit:peruser:ip:") {
		t.Fatalf("nil-claims write key = %q, want per-IP fallback (not a shared empty-userID bucket)", key)
	}
}

// With no limiter wired (local mode), the middleware passes through untouched.
func TestPerUserRateLimit_NilLimiterIsPassthrough(t *testing.T) {
	rt := newPerUserTestRouter() // perUserLimiter is nil
	called := false
	h := rt.perUserRateLimit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req(http.MethodPost, "/api/analysis/analyze"))
	if !called {
		t.Error("nil limiter must pass the request through, not block it")
	}
}

// hashRateLimitKey must be deterministic and produce a hex SHA-256 (64 chars),
// independent of delimiter/control-char concerns in the input.
func TestHashRateLimitKey(t *testing.T) {
	a := hashRateLimitKey("user-1")
	b := hashRateLimitKey("user-1")
	if a != b {
		t.Error("hash must be deterministic")
	}
	if len(a) != 64 {
		t.Errorf("expected 64-char hex SHA-256, got %d chars (%q)", len(a), a)
	}
	// An input that would poison a naive "prefix+raw" key (delimiter/control
	// chars) hashes cleanly here.
	nasty := hashRateLimitKey("a:b\nc\x00d")
	if len(nasty) != 64 || strings.ContainsAny(nasty, ":\n\x00") {
		t.Errorf("hash of a poisoning-prone input must be clean hex, got %q", nasty)
	}
}
