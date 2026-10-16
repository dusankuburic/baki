package api

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"strings"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
)

// registerRoutes applies chi's inner middleware layer, mounts the router's own routes and every module's, and
// installs the NotFound fallback; it runs after the outer layers built by BuildHandler (middleware_chain.go — see
// that file's doc for the resolved order). The returned Builder has not been built yet.
func (rt *Router) registerRoutes() *route.Builder {
	r := rt.mux
	// Cross-cutting middleware — applied to every request in order.
	r.Use(rt.securityHeaders)
	r.Use(rt.corsHeaders)
	// Local (no-JWT) mode: every mutating route is unauthenticated behind the
	// loopback bind; refuse browser requests that arrive cross-origin, so no
	// website can drive any of them (now or added later) with a preflight-free
	// "simple" POST. Pass-through in cloud mode and for non-mutating methods.
	r.Use(rt.localOriginGuard)

	// API version alias: the rewrite must stay identical to canonicalRequestPath (middleware_chain.go),
	// which the rate-limit classifier runs outside this mux — the copies drifted once into a bypass.
	// chi routes on RawPath, so strip it there too or an escaped "/" in an id 404s.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			applyVersionAlias(req.URL)
			next.ServeHTTP(w, req)
		})
	})

	r.Use(rt.jwtAuth)
	// Per-user write throttle (cloud mode only; pass-through when the limiter is
	// nil). Runs after jwtAuth so claims are available, and before rlsMiddleware.
	r.Use(rt.perUserRateLimit)
	r.Use(rt.rlsMiddleware)
	// Idempotency-Key (ADR 0007): inside the request transaction, so a key's record commits with the route's writes.
	r.Use(rt.idempotency)

	rb := route.NewBuilder(r)
	rt.Mount(rb)
	for _, m := range rt.modules {
		m.Mount(rb)
	}

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		importPath := r.URL.Path
		if rt.staticDir != "" {
			if strings.HasPrefix(importPath, "/api/") {
				http.NotFound(w, r)
				return
			}
			rt.serveStatic(w, r, importPath)
			return
		}
		http.NotFound(w, r)
	})
	return rb
}

// Mount registers the router's own routes: the Swagger UI, the WebSocket gateway, metrics, pprof and the SSE bus.
func (rt *Router) Mount(rb *route.Builder) {
	// --- Swagger UI ---
	// Always registered; in cloud mode the handler verifies the bearer (a session or an API token) so the schema
	// isn't browsable unauthenticated — it used to check only that an Authorization header was present. In local
	// mode, open access.
	swaggerH := swaggerHandler()
	rb.Get("/swagger", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/index.html", http.StatusMovedPermanently)
	}, route.Public())
	rb.Get("/swagger/*", func(w http.ResponseWriter, r *http.Request) {
		if rt.security.JWTEnabled && !rt.validBearer(r) {
			render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
			return
		}
		swaggerH.ServeHTTP(w, r)
	}, route.SelfAuthenticated())

	// --- WebSocket ---
	// The one-time ticket from /api/ws-ticket authenticates the upgrade; the socket lives as long as the tab.
	rb.Get("/ws", rt.handleWebSocket, route.SelfAuthenticated(), route.LongLived())

	// /metrics: private-IP allowlist alone passes for every request behind a reverse proxy — also set
	// PAD_METRICS_TOKEN and NSG/ACA ingress rules (warnIfMetricsExposed logs that combination at boot).
	rb.Handle("/metrics", middleware.MetricsGuard(rt.trustedProxies, rt.metricsToken)(middleware.MetricsHandler()), route.SelfAuthenticated())
	// /debug/pprof/*: OPT-IN (PAD_PPROF_ENABLED) — it used to sit unconditionally behind the private-IP
	// guard, making /debug/pprof/heap a public heap dump behind a proxy; validation refuses the flag
	// without a token.
	if rt.pprofEnabled {
		// A dedicated ServeMux so the handlers don't leak onto
		// http.DefaultServeMux (which other imports might accidentally expose).
		pprofMux := http.NewServeMux()
		pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
		pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		rb.Handle("/debug/pprof/*", middleware.MetricsGuard(rt.trustedProxies, rt.metricsToken)(pprofMux), route.SelfAuthenticated())
	}
	// The SSE bus lives as long as the tab; a request transaction would pin a pooled connection all that time.
	rb.Get("/api/events", rt.eventManager.HandleEvents, route.LongLived(), route.NoTx())
}
