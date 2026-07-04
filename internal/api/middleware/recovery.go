package middleware

import (
	"encoding/json"
	"net/http"
	"runtime/debug"

	"pad-analyzer/internal/errreport"
)

// Recovery wraps h and recovers from any panic, writing a 500 JSON response.
// The recovered panic is forwarded to the errreport sink so it surfaces in the
// exception metrics (pad_panics_total) and any registered aggregation backend.
func Recovery(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				stack := debug.Stack()
				errreport.CapturePanic(r.Context(), "http", rec, stack, AttrsFromRequest(r))
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				if err := json.NewEncoder(w).Encode(map[string]string{"error": "internal server error"}); err != nil {
					_ = err // body write failed; nothing useful to do
				}
			}
		}()
		h.ServeHTTP(w, r)
	})
}

// AttrsFromRequest builds the structured context attached to a forwarded HTTP
// panic: method, path, and request id (when present) so the exception backend
// can group/route by endpoint and correlate with access logs.
func AttrsFromRequest(r *http.Request) errreport.Attrs {
	attrs := errreport.Attrs{
		"method": r.Method,
		"path":   r.URL.Path,
	}
	if rid := r.Header.Get("X-Request-Id"); rid != "" {
		attrs["request_id"] = rid
	}
	return attrs
}
