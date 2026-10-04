package api

import (
	"net/http"
	"testing"

	"pad-analyzer/internal/config"
)

// TestPerUserRateLimit_IsEnforcedThroughBuildHandler is the regression test
// for the per-user write throttle being a silent no-op. registerRoutes adds
// perUserRateLimit via r.Use inside NewRouter, and chi evaluates every Use
// middleware once, when the first route is registered — so a construction-time
// nil check saw the limiter BuildHandler installs only afterwards as nil and
// returned next for the life of the process. The request has to go through the
// real path (NewRouter, then BuildHandler in cloud mode) for the bug to be
// reachable at all.
func TestPerUserRateLimit_IsEnforcedThroughBuildHandler(t *testing.T) {
	rt := newJWTTestRouter(t)
	var served int
	rt.mux.Post("/api/probe/write", func(w http.ResponseWriter, _ *http.Request) {
		served++
		w.WriteHeader(http.StatusNoContent)
	})

	const burst = 3
	h := fullChain(t, rt, func(cfg *config.Config) {
		cfg.Mode = config.ModeCloud
		// Only the per-user bucket may refuse: the per-IP general bucket
		// (which this write also rides) gets more than enough headroom.
		cfg.Runtime.RateLimitGeneralRPS = 1000
		cfg.Runtime.RateLimitGeneralBurst = 1000
		cfg.Runtime.RateLimitPerUserRPS = 0.001
		cfg.Runtime.RateLimitPerUserBurst = burst
	})
	bearer := jwtBearer(t, rt, "u-peruser", "u@example.com")

	for i := 1; i <= burst; i++ {
		if rr := serve(h, http.MethodPost, "/api/probe/write", bearer); rr.Code != http.StatusNoContent {
			t.Fatalf("write %d of %d: got %d, want 204 (body %s)", i, burst, rr.Code, rr.Body.String())
		}
	}
	rr := serve(h, http.MethodPost, "/api/probe/write", bearer)
	if rr.Code != http.StatusTooManyRequests {
		t.Fatalf("write %d (bucket of %d exhausted): got %d, want 429 — the per-user limiter is not applied to requests", burst+1, burst, rr.Code)
	}
	if served != burst {
		t.Errorf("handler ran %d times, want exactly %d (the refused write must not reach it)", served, burst)
	}

	// Reads are never subject to the per-user bucket, exhausted or not.
	rt.mux.Get("/api/probe/read", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	if rr := serve(h, http.MethodGet, "/api/probe/read", bearer); rr.Code != http.StatusNoContent {
		t.Errorf("read after the write bucket is exhausted: got %d, want 204", rr.Code)
	}
}

// Local mode installs no per-user limiter, and the per-request load must then
// be a pure pass-through: the same burst of writes all succeed.
func TestPerUserRateLimit_LocalModeIsPassthroughThroughBuildHandler(t *testing.T) {
	rt := newJWTTestRouter(t)
	rt.mux.Post("/api/probe/write", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	h := fullChain(t, rt, func(cfg *config.Config) {
		cfg.Runtime.RateLimitPerUserBurst = 1
	})
	bearer := jwtBearer(t, rt, "u-local", "u@example.com")
	for i := 0; i < 5; i++ {
		if rr := serve(h, http.MethodPost, "/api/probe/write", bearer); rr.Code != http.StatusNoContent {
			t.Fatalf("local-mode write %d: got %d, want 204", i+1, rr.Code)
		}
	}
}
