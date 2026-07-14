package api

import "testing"

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
