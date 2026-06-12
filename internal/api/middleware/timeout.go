package middleware

import (
	"context"
	"net/http"
	"strings"
	"time"
)

func RequestTimeout(d time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isStreamingPath(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func isStreamingPath(path string) bool {
	switch {
	case path == "/ws",
		path == "/api/events",
		strings.HasSuffix(path, "/stream"):
		return true
	}
	return false
}
