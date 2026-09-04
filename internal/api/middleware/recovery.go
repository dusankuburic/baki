package middleware

import (
	"errors"
	"net/http"
	"runtime/debug"

	"pad-analyzer/internal/api/render"
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
				render.Error(w, errors.New("internal server error"), http.StatusInternalServerError)
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
		"path":   redactPath(r.URL.Path),
	}
	if rid := r.Header.Get("X-Request-Id"); rid != "" {
		attrs["request_id"] = rid
	}
	return attrs
}
