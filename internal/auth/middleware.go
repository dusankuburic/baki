package auth

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// Middleware returns an http.Handler that validates JWT access tokens and
// injects the parsed claims into the request context.
//
// Requests that carry a valid token are forwarded to next.
// Requests without a token or with an invalid token receive 401.
//
// The token is expected in the Authorization header as "Bearer <token>",
// or in the ?token= query parameter (for SSE / EventSource clients).
func Middleware(mgr *Manager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tokenStr := ExtractToken(r)
		if tokenStr == "" {
			http.Error(w, "missing token", http.StatusUnauthorized)
			return
		}

		claims, err := mgr.Verify(tokenStr)
		if err != nil {
			http.Error(w, "invalid or expired token", http.StatusUnauthorized)
			return
		}

		next.ServeHTTP(w, r.WithContext(WithClaims(r.Context(), claims)))
	})
}

// StaticTokenMiddleware is the legacy middleware used by the desktop app.
// It compares the raw request token against a pre-shared static secret rather
// than verifying a JWT.  This lets the existing sidecar flow keep working while
// the JWT infrastructure is being rolled out.
func StaticTokenMiddleware(token string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(ExtractToken(r)), []byte(token)) != 1 {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ExtractToken pulls the bearer token from the Authorization header or the
// ?token= query parameter.  Returns the raw token string (without the
// "Bearer " prefix).
func ExtractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		if after, ok := strings.CutPrefix(h, "Bearer "); ok {
			return after
		}
	}
	return r.URL.Query().Get("token")
}
