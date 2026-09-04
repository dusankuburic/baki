package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestRateLimitGroup_AuthPathsInclusive is the regression test for the
// forgot-password / reset-password rate-limit fix: those two endpoints used to
// fall through to the looser "general" bucket, enabling email-flooding and SMTP
// cost amplification. They must now resolve to the tighter "auth" group, the
// same as login/register/refresh/change-password.
func TestRateLimitGroup_AuthPathsInclusive(t *testing.T) {
	authPaths := []string{
		"/api/auth/login",
		"/api/auth/refresh",
		"/api/auth/register",
		"/api/auth/change-password",
		"/api/auth/forgot-password",
		"/api/auth/reset-password",
		"/api/auth/verify-email",
		"/api/auth/sso/exchange",
	}
	for _, p := range authPaths {
		// Auth-grouping is path-based and method-independent (a GET probe of a
		// reset endpoint should still be throttled as auth).
		for _, m := range []string{"GET", "POST"} {
			if got := rateLimitGroup(m, p); got != rlGroupAuth {
				t.Errorf("rateLimitGroup(%s,%s) = %q, want %q", m, p, got, rlGroupAuth)
			}
		}
	}
}

func TestRateLimitGroup_OtherBuckets(t *testing.T) {
	cases := []struct {
		method, path, want string
	}{
		{"POST", "/api/analysis/run", rlGroupAnalysis},
		{"GET", "/api/analysis/run", rlGroupGeneral}, // analysis bucket is POST-only
		{"POST", "/api/chat/stream", rlGroupChat},
		{"POST", "/api/flow/upload", rlGroupUpload},
		{"GET", "/api/flow/upload", rlGroupGeneral}, // upload bucket is POST-only
		{"GET", "/api/system/info", rlGroupGeneral},
		{"POST", "/api/flow/list", rlGroupGeneral},
	}
	for _, c := range cases {
		if got := rateLimitGroup(c.method, c.path); got != c.want {
			t.Errorf("rateLimitGroup(%s,%s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// B1.9: the unauthenticated share-link report rides the tighter analysis
// bucket — it resolves a flow and runs the analyzer per fresh flow.
func TestRateLimitGroup_SharedReportInAnalysisBucket(t *testing.T) {
	if got := rateLimitGroup("GET", "/api/shared"); got != rlGroupAnalysis {
		t.Errorf("GET /api/shared group = %q, want %q", got, rlGroupAnalysis)
	}
}

// TestRateLimitGroup_NormalizesPath is the regression test for the rate-limit
// bypass: rateLimitGroup runs in BuildHandler's layer 7, which is OUTSIDE the
// chi mux, so it saw the RAW request path. Two normalizations happen after it
// and before routing — Router.ServeHTTP's path.Clean, and registerRoutes's
// /api/v1 → /api version rewrite — so any request spelled non-canonically was
// classified on a string that never reaches a handler.
//
// Both spellings below route to the real endpoint but used to classify as
// "general" (60/min by default) instead of "auth" (5/min): a 12x brute-force
// and password-reset-email amplification, and the same trick moved chat spend
// and uploads off their dedicated buckets.
func TestRateLimitGroup_NormalizesPath(t *testing.T) {
	cases := []struct {
		name, method, path, want string
	}{
		// /api/v1 alias — a documented, fully-supported client entrypoint.
		{"v1 login", "POST", "/api/v1/auth/login", rlGroupAuth},
		{"v1 forgot-password", "POST", "/api/v1/auth/forgot-password", rlGroupAuth},
		{"v1 chat stream", "POST", "/api/v1/chat/stream", rlGroupChat},
		{"v1 upload", "POST", "/api/v1/flow/upload", rlGroupUpload},
		{"v1 analysis", "POST", "/api/v1/analysis/run", rlGroupAnalysis},
		{"v1 shared", "GET", "/api/v1/shared", rlGroupAnalysis},
		// Non-canonical spellings collapsed by path.Clean.
		{"dot segment", "POST", "/api/auth/./login", rlGroupAuth},
		{"double slash", "POST", "/api//auth/login", rlGroupAuth},
		{"parent segment", "POST", "/api/auth/../auth/login", rlGroupAuth},
		{"trailing dot", "POST", "/api/chat/./stream", rlGroupChat},
		// Near-misses must NOT be rewritten: /api/v1foo is a distinct path.
		{"v1 prefix not a segment", "POST", "/api/v1foo/auth/login", rlGroupGeneral},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rateLimitGroup(c.method, c.path); got != c.want {
				t.Errorf("rateLimitGroup(%s,%s) = %q, want %q", c.method, c.path, got, c.want)
			}
		})
	}
}

// TestPathAliases_ReachTheSameHandler is the other half of the rate-limit
// bypass regression: it proves the alternate spellings are REAL, working
// entrypoints, not theoretical strings. If they 404'd, misclassifying them
// would not matter. They don't — each reaches the identical handler with
// identical auth semantics, which is exactly why classifying them as "general"
// let a caller pick their own rate-limit bucket.
//
// The /api/v1 alias in particular shipped with no routing test at all.
func TestPathAliases_ReachTheSameHandler(t *testing.T) {
	rt := newJWTTestRouter(t)
	bearer := jwtBearer(t, rt, "user1", "user1@example.com")

	base := "/api/auth/me"
	aliases := []string{
		"/api/v1/auth/me",      // documented version alias
		"/api/auth/./me",       // dot segment
		"/api//auth/me",        // duplicate slash
		"/api/auth/../auth/me", // parent segment
	}

	wantAuthed := doRequestWithAuth(t, rt, http.MethodGet, base, bearer, nil).Code
	wantAnon := doRequestWithAuth(t, rt, http.MethodGet, base, "", nil).Code
	if wantAuthed != http.StatusOK || wantAnon != http.StatusUnauthorized {
		t.Fatalf("baseline %s: authed=%d anon=%d, want 200/401", base, wantAuthed, wantAnon)
	}

	for _, p := range aliases {
		if got := doRequestWithAuth(t, rt, http.MethodGet, p, bearer, nil).Code; got != wantAuthed {
			t.Errorf("authed GET %s = %d, want %d (same as %s)", p, got, wantAuthed, base)
		}
		if got := doRequestWithAuth(t, rt, http.MethodGet, p, "", nil).Code; got != wantAnon {
			t.Errorf("anon GET %s = %d, want %d (same as %s)", p, got, wantAnon, base)
		}
	}
}

// TestStaticRoutePatterns_CoversRealRoutes guards the other failure mode of the
// bounded metrics label: if the walk silently produced an empty or tiny set,
// every real route would collapse to "/api/other" and the HTTP metrics would
// quietly go blind. Nothing else would fail.
func TestStaticRoutePatterns_CoversRealRoutes(t *testing.T) {
	got := staticRoutePatterns(newJWTTestRouter(t).mux)

	if len(got) < 100 {
		t.Fatalf("staticRoutePatterns returned %d routes, want >=100 — the walk is probably broken", len(got))
	}
	// A spread across the route groups, including the buckets rate limiting
	// cares about.
	for _, want := range []string{
		"/api/auth/login",
		"/api/auth/me",
		"/api/chat/stream",
		"/api/flow/upload",
		"/api/system/settings",
		"/api/admin/users/list",
		"/healthz",
		"/readyz",
	} {
		if _, ok := got[want]; !ok {
			t.Errorf("expected %q in the static route set", want)
		}
	}
	// Dynamic patterns must NOT be in the set — they are collapsed by
	// normalizeRoute instead, and a literal "{id}" would never match a path.
	for k := range got {
		if strings.ContainsAny(k, "{*") {
			t.Errorf("dynamic pattern %q leaked into the static route set", k)
		}
	}
}
