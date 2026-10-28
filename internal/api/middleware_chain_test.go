package api

import (
	"net/http"
	"net/url"
	"testing"

	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/config"
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
			if got := rateLimitGroup(m, &url.URL{Path: p}); got != rlGroupAuth {
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
		// The billed routes ride the cost buckets (R-security-c22): the context preview embeds the query (chat
		// bucket); the knowledge base's upload and re-index embed in bulk (embed bucket).
		{"POST", "/api/chat/preview-context", rlGroupChat},
		{"POST", "/api/v1/chat/preview-context", rlGroupChat},
		{"POST", "/api/orgs/o1/knowledge/upload", route.RateEmbed},
		{"POST", "/api/v1/orgs/o1/knowledge/reindex", route.RateEmbed},
		{"GET", "/api/orgs/o1/knowledge", rlGroupGeneral},
		{"DELETE", "/api/orgs/o1/knowledge/d1", rlGroupGeneral},
		{"POST", "/api/orgs//knowledge/upload", rlGroupGeneral},
		{"POST", "/api/flow/upload", rlGroupUpload},
		{"GET", "/api/flow/upload", rlGroupGeneral}, // upload bucket is POST-only
		{"POST", "/api/auth/profile/avatar", route.RateAvatar},
		{"POST", "/api/v1/auth/profile/avatar", route.RateAvatar},
		{"DELETE", "/api/auth/profile/avatar", rlGroupGeneral}, // the removal decodes nothing
		{"GET", "/api/system/info", rlGroupGeneral},
		{"POST", "/api/flow/list", rlGroupGeneral},
		// The connection test is billed spend (up to three real chat
		// completions per call) and rides the tight chat bucket; the free
		// provider routes stay on general.
		{"POST", "/api/providers/test", rlGroupChat},
		{"POST", "/api/v1/providers/test", rlGroupChat},
		{"GET", "/api/providers/list", rlGroupGeneral},
		{"POST", "/api/providers/github/poll", rlGroupGeneral},
		// The Azure AI Foundry registry (azureOutboundRoute): a test is a
		// probe or one tiny billed call (chat bucket), a discovery the
		// project's listings (analysis bucket) — the caller's own and an
		// organisation's; its CRUD routes stay on general.
		{"POST", "/api/providers/azure-foundry/test", rlGroupChat},
		{"POST", "/api/orgs/o1/providers/azure-foundry/test", rlGroupChat},
		{"POST", "/api/providers/azure-foundry/connections/abcdefghij/discover", rlGroupAnalysis},
		{"POST", "/api/orgs/o1/providers/azure-foundry/connections/default/discover", rlGroupAnalysis},
		{"POST", "/api/providers/azure-foundry/connections", rlGroupGeneral},
		{"POST", "/api/providers/azure-foundry/models", rlGroupGeneral},
		// The channel test dials an admin-chosen URL: its own bucket; the channel CRUD stays on general.
		{"POST", "/api/orgs/o1/channels/c1/test", route.RateChannelTest},
		{"POST", "/api/v1/orgs/o1/channels/c1/test", route.RateChannelTest},
		{"POST", "/api/orgs/o1/channels", rlGroupGeneral},
		{"GET", "/api/orgs/o1/channels/c1/test", rlGroupGeneral},
	}
	for _, c := range cases {
		if got := rateLimitGroup(c.method, &url.URL{Path: c.path}); got != c.want {
			t.Errorf("rateLimitGroup(%s,%s) = %q, want %q", c.method, c.path, got, c.want)
		}
	}
}

// TestRateLimit_AvatarUploadHasItsOwnBucket: the avatar upload shared the upload bucket with flow uploads, so a
// client syncing flows refused its user's avatar change, and a burst of avatar picks refused the next flow upload.
// Each must leave the other's bucket untouched, in every route-policy mode (the legacy classifier decides in two, the
// declared policy in the third). The refusal is the limiter's 429, ahead of authentication.
func TestRateLimit_AvatarUploadHasItsOwnBucket(t *testing.T) {
	const flowUpload, avatar = "/api/flow/upload", "/api/auth/profile/avatar"
	for _, mode := range allRoutePolicyModes {
		for _, c := range []struct{ name, drained, other string }{
			{"flow uploads drained", flowUpload, avatar},
			{"avatar uploads drained", avatar, flowUpload},
		} {
			t.Run(string(mode)+" "+c.name, func(t *testing.T) {
				h := fullChain(t, newModeTestRouter(t, mode), func(cfg *config.Config) {
					cfg.Mode = config.ModeCloud
					cfg.Runtime.RateLimitGeneralRPS, cfg.Runtime.RateLimitGeneralBurst = 1000, 1000
					cfg.Runtime.RateLimitPerUserRPS, cfg.Runtime.RateLimitPerUserBurst = 1000, 1000
					cfg.Runtime.RateLimitUploadRPS, cfg.Runtime.RateLimitUploadBurst = 0.001, 1
					cfg.Runtime.RateLimitAvatarRPS, cfg.Runtime.RateLimitAvatarBurst = 0.001, 1
				})
				if rr := serve(h, http.MethodPost, c.drained, ""); rr.Code == http.StatusTooManyRequests {
					t.Fatalf("first POST %s refused", c.drained)
				}
				if rr := serve(h, http.MethodPost, c.drained, ""); rr.Code != http.StatusTooManyRequests {
					t.Fatalf("second POST %s = %d, want 429 (its bucket holds one)", c.drained, rr.Code)
				}
				if rr := serve(h, http.MethodPost, c.other, ""); rr.Code == http.StatusTooManyRequests {
					t.Errorf("POST %s after POST %s emptied its bucket = 429, want a bucket of its own", c.other, c.drained)
				}
			})
		}
	}
}

// B1.9: the unauthenticated share-link report rides the tighter analysis
// bucket — it resolves a flow and runs the analyzer per fresh flow.
func TestRateLimitGroup_SharedReportInAnalysisBucket(t *testing.T) {
	if got := rateLimitGroup("GET", &url.URL{Path: "/api/shared"}); got != rlGroupAnalysis {
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
		{"v1 azure test", "POST", "/api/v1/providers/azure-foundry/test", rlGroupChat},
		{"dot segment azure discover", "POST", "/api/providers/azure-foundry/connections/./abcdefghij/discover", rlGroupAnalysis},
		{"v1 org azure discover", "POST", "/api/v1/orgs/o1/providers/azure-foundry/connections/abcdefghij/discover", rlGroupAnalysis},
		// Near-misses must NOT be rewritten: /api/v1foo is a distinct path.
		{"v1 prefix not a segment", "POST", "/api/v1foo/auth/login", rlGroupGeneral},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := rateLimitGroup(c.method, &url.URL{Path: c.path}); got != c.want {
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
