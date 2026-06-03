package middleware

import (
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
)

// Recovery wraps h and recovers from any panic, writing a 500 JSON response.
func Recovery(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic recovered",
					"panic", rec,
					"stack", string(debug.Stack()),
					"method", r.Method,
					"path", r.URL.Path,
				)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				if err := json.NewEncoder(w).Encode(map[string]string{"error": "internal server error"}); err != nil {
					slog.Error("recovery: failed to encode error response", "err", err)
				}
			}
		}()
		h.ServeHTTP(w, r)
	})
}
