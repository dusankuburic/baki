package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestExtractToken_QueryParamOnlyOnSSEPath is the regression test for the
// token-leak fix: the ?token= query fallback used to apply to every route, so a
// 15-minute access JWT could leak into proxy/browser logs on any /api/ call. It
// must now be accepted ONLY on /api/events (the one endpoint where EventSource
// forces the token into the URL); the Authorization header still works
// everywhere.
func TestExtractToken_QueryParamOnlyOnSSEPath(t *testing.T) {
	cases := []struct {
		name   string
		header string // Authorization header value, "" to omit
		path   string
		query  string // raw query string
		want   string
	}{
		{
			name:   "header accepted everywhere",
			header: "Bearer abc.def.ghi",
			path:   "/api/flow/upload",
			want:   "abc.def.ghi",
		},
		{
			name:  "query accepted on SSE path",
			path:  "/api/events",
			query: "token=abc.def.ghi",
			want:  "abc.def.ghi",
		},
		{
			name:  "query ignored on non-SSE path",
			path:  "/api/flow/upload",
			query: "token=abc.def.ghi",
			want:  "",
		},
		{
			name:  "query ignored on protected route",
			path:  "/api/auth/me",
			query: "token=secret",
			want:  "",
		},
		{
			name:  "query ignored on root api path",
			path:  "/api/",
			query: "token=secret",
			want:  "",
		},
		{
			name:   "header preferred over query on SSE path",
			header: "Bearer from-header",
			path:   "/api/events",
			query:  "token=from-query",
			want:   "from-header",
		},
		{
			name: "no token when nothing provided",
			path: "/api/flow/upload",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path+"?"+tc.query, nil)
			if tc.header != "" {
				req.Header.Set("Authorization", tc.header)
			}
			got := ExtractToken(req)
			if got != tc.want {
				t.Errorf("ExtractToken = %q, want %q", got, tc.want)
			}
		})
	}
}
