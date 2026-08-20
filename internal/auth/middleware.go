package auth

import (
	"encoding/json"
	"net/http"
	"strings"
)

// writeError emits the API-standard error envelope ({code,message,requestId}).
// Duplicated in miniature from api/render because importing it from here would
// cycle (render → service → …, api → auth). Keep the field names in sync.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Code      string `json:"code"`
		Message   string `json:"message"`
		RequestID string `json:"requestId,omitempty"`
	}{Code: codeFor(status), Message: msg})
}

func codeFor(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "UNAUTHORIZED"
	case http.StatusForbidden:
		return "FORBIDDEN"
	case http.StatusBadRequest:
		return "BAD_REQUEST"
	default:
		return "ERROR"
	}
}

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
			writeError(w, http.StatusUnauthorized, "missing token")
			return
		}

		claims, err := mgr.Verify(tokenStr)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}

		next.ServeHTTP(w, r.WithContext(WithClaims(r.Context(), claims)))
	})
}

// StaticTokenMiddleware was the legacy pre-JWT middleware; removed with its
// callers when the desktop sidecar moved to the shared token pipeline.

// ExtractToken pulls the bearer token from the Authorization header. As a
// narrow exception, it also reads ?token= from the query string, but ONLY for
// the /api/events SSE endpoint: browsers cannot set headers on EventSource
// requests, so the access token has to ride in the URL there. Accepting ?token=
// on every /api/ route would leak 15-minute access JWTs into proxy/WAF/browser
// logs and Referer headers; restricting it to the SSE path confines that
// exposure to the one endpoint that needs it. Returns the raw token string
// (without the "Bearer " prefix).
//
// The auth scheme is case-insensitive per RFC 7235 §2.1, so "bearer <jwt>",
// "BEARER <jwt>", etc. are all accepted (some client libraries emit lowercase).
func ExtractToken(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		// Match the scheme case-insensitively, then take the remainder of the
		// original header (preserving the token's exact casing) after a single
		// space separator.
		if idx := strings.IndexByte(h, ' '); idx == len("bearer") &&
			strings.EqualFold(h[:idx], "bearer") {
			return strings.TrimSpace(h[idx+1:])
		}
	}
	if r.URL.Path == sseTokenPath {
		return r.URL.Query().Get("token")
	}
	return ""
}

// sseTokenPath is the only route permitted to carry the access token in the
// query string (see ExtractToken).
const sseTokenPath = "/api/events"
