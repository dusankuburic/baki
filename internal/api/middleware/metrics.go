package middleware

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"pad-analyzer/internal/metrics"
)

// MetricsHandler exposes the Prometheus /metrics endpoint; in production it should be reachable
// only from the metrics scraper — gate at the load balancer or use a private listener if needed.
func MetricsHandler() http.Handler { return metrics.Handler() }

// RecordRateLimitExceeded is wired from ratelimit.go when a request is refused; lives here for
// naming locality.
func RecordRateLimitExceeded(group string) { metrics.RecordRateLimitExceeded(group) }

// Metrics wraps h to record http_requests_total and http_request_duration_seconds. Apply it inside AccessLog so the status label reflects the final response.
// knownRoutes is the set of registered STATIC route patterns bounding the "route" label: an unknown /api/ path is reported as routeOther, not verbatim — pass it from the router's own route tree, never a hand-maintained list. nil is fail-closed (everything unrecognized becomes routeOther), the safe direction for cardinality.
func Metrics(h http.Handler, knownRoutes map[string]struct{}) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Label on the canonical dispatch path, taken before the handler runs: this layer sits outside the mux, and r.URL.Path only became canonical if the router rewrote it — a request refused earlier (429) was labelled on the raw path (/api/v1/library/x as routeOther instead of /api/library/:id).
		// Canonicalizing AFTER the handler would not do either (the router already applied the /api/v1 strip once; a second pass labels /api/v1/v1/... as a route the request never reached). Not dispatchPath: its RawPath branch keeps the escaped, uncleaned spelling, which the collapses below do not match.
		p := canonicalPath(r.URL.Path)
		sr := NewResponseRecorder(w)
		start := time.Now()
		h.ServeHTTP(maybeHijackable(sr), r)
		route := normalizeRoute(p, knownRoutes)
		method := normalizeMethod(r.Method)
		elapsed := time.Since(start).Seconds()
		metrics.HTTPRequestsTotal.WithLabelValues(method, route, strconv.Itoa(sr.status)).Inc()
		metrics.HTTPRequestDuration.WithLabelValues(method, route).Observe(elapsed)
	})
}

// methodOther is the bounded catch-all for any request method outside the
// standard set.
const methodOther = "OTHER"

// normalizeMethod bounds the "method" label: net/http accepts any token, and this layer runs before auth and the rate limiter, so verbatim r.Method let an
// unauthenticated FOO1 /, FOO2 /, … mint a fresh counter and histogram series per request, retained for the process's life.
func normalizeMethod(m string) string {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch,
		http.MethodDelete, http.MethodConnect, http.MethodOptions, http.MethodTrace:
		return m
	}
	return methodOther
}

// routeOther is the bounded catch-all for any /api/ path that is neither a
// registered static route nor one of the explicit dynamic collapses.
const routeOther = "/api/other"

// normalizeRoute strips per-resource identifiers from URL paths so the Prometheus label set has bounded cardinality (one time-series per unique label
// combination — a naive path-per-flow-id label blows up the series count linearly with flows). Adding a case for a new dynamic route is an OPTIMIZATION, not a requirement: an uncased one collapses to routeOther, bounded but coarse.
func normalizeRoute(path string, known map[string]struct{}) string {
	switch {
	case strings.HasPrefix(path, "/api/library/"):
		return "/api/library/:id"
	case strings.HasPrefix(path, "/api/flows/") && strings.Contains(path, "/collaborators"):
		return "/api/flows/:id/collaborators"
	case strings.HasPrefix(path, "/api/admin/users/") && strings.HasSuffix(path, "/role"):
		return "/api/admin/users/:id/role"
	// The four families below used to fall through to the verbatim /api/ case, so each per-request identifier minted its own time series. /api/invites/ was
	// the worst: the path segment is the emailed single-use invite CREDENTIAL, so every accept wrote a live secret into a scraped, retained Prometheus label.
	case strings.HasPrefix(path, "/api/auth/sessions/"):
		return "/api/auth/sessions/:id"
	case strings.HasPrefix(path, "/api/auth/tokens/"):
		return "/api/auth/tokens/:id"
	case strings.HasPrefix(path, "/api/invites/"):
		return "/api/invites/:token/accept"
	case strings.HasPrefix(path, "/api/system/settings/org/"):
		return "/api/system/settings/org/:id"
	case strings.HasPrefix(path, azureRegistryPrefix):
		// The Azure AI Foundry registry: its per-resource and per-model routes collapse to their patterns; anything else under it is no route (the static ones map to themselves).
		if route, ok := azureRegistryRoute(strings.TrimPrefix(path, azureRegistryPrefix)); ok {
			return "/api/providers/azure-foundry" + route
		}
		return routeOther
	case strings.HasPrefix(path, "/api/orgs/"):
		// /api/orgs/:orgId or /api/orgs/:orgId/members[/:userId]
		switch {
		case strings.Contains(path, "/members/"):
			return "/api/orgs/:id/members/:userId"
		case strings.HasSuffix(path, "/members"):
			return "/api/orgs/:id/members"
		}
		// An organisation's Azure AI Foundry registry: the same fixed set of
		// patterns as the caller's own.
		if orgID, tail, ok := strings.Cut(strings.TrimPrefix(path, "/api/orgs/"), "/providers/azure-foundry/"); ok && orgID != "" && !strings.Contains(orgID, "/") {
			if route, ok := azureRegistryRoute(tail); ok {
				return "/api/orgs/:id/providers/azure-foundry" + route
			}
		}
		return "/api/orgs/:id"
	case strings.HasPrefix(path, "/swagger/"):
		return "/swagger/*"
	case path == "/healthz", path == "/readyz", path == "/api/health", path == "/metrics":
		return path
	case path == "/ws":
		// Not under /api/, so without this case it falls through to /static/* — and the Metrics layer observes the handler's whole lifetime, putting every
		// socket's open-to-close duration (minutes to hours) in the asset histogram.
		return path
	case strings.HasPrefix(path, "/api/"):
		// Only REGISTERED static routes are reported verbatim — returning every /api/ path unchanged handed an unauthenticated caller a cardinality bomb:
		// this layer runs outside the mux and before auth, so GET /api/<random> minted a fresh series per request, 404s included, until the process ran out of memory.
		if _, ok := known[path]; ok {
			return path
		}
		return routeOther
	}
	if path == "/" || path == "" {
		return "/"
	}
	return "/static/*"
}

// azureRegistryPrefix starts a caller's own Azure AI Foundry registry routes.
const azureRegistryPrefix = "/api/providers/azure-foundry/"

// azureRegistryRoute collapses the rest of an Azure AI Foundry registry path to its route pattern —
// one of a fixed set, so the label stays bounded whatever the ids; false for a non-route path.
func azureRegistryRoute(tail string) (string, bool) {
	segs := strings.Split(tail, "/")
	switch len(segs) {
	case 1:
		switch segs[0] {
		case "config", "connections", "models", "test":
			return "/" + segs[0], true
		}
	case 2:
		switch {
		case segs[0] == "connections" && segs[1] != "":
			return "/connections/:id", true
		case segs[0] == "models" && segs[1] != "":
			return "/models/:id", true
		}
	case 3:
		if segs[0] == "connections" && segs[1] != "" && segs[2] == "discover" {
			return "/connections/:id/discover", true
		}
	}
	return "", false
}
