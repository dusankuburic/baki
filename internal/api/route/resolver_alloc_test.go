//go:build !race

// The race detector's sync.Pool drops pooled values at random, so allocation counts mean nothing under -race.

package route

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Resolve runs on every request: a flat route, a 405 and a miss cost no allocation (chi's Find allocates only
// while descending into a mounted subrouter, to build the pattern).
func TestResolve_Allocations(t *testing.T) {
	_, _, r := newSampleResolver(t)
	for _, c := range []struct{ method, target string }{
		{http.MethodGet, "/metrics"},
		{http.MethodGet, "/swagger/index.html"},
		{http.MethodPut, "/api/library"},
		{http.MethodOptions, "/api/library"},
		{http.MethodGet, "/api/nothing/here"},
		{http.MethodGet, "/assets/app.js"},
	} {
		u := httptest.NewRequest(c.method, c.target, nil).URL
		r.Resolve(c.method, u) // memoize a 405 first
		if n := testing.AllocsPerRun(100, func() { r.Resolve(c.method, u) }); n != 0 {
			t.Errorf("%s %s: %v allocations per Resolve, want 0", c.method, c.target, n)
		}
	}
}
