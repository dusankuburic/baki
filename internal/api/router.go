package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"net/http"
	"path"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/manager"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/migration"
	wshub "pad-analyzer/internal/websocket"

	_ "pad-analyzer/docs"

	httpSwagger "github.com/swaggo/http-swagger"
)

// refreshTokenStore tracks issued refresh tokens so they can be rotated and
// revoked (cloud mode). The Postgres backend implements it; in local mode the
// router runs without it (stateless refresh).
type refreshTokenStore interface {
	StoreRefreshToken(ctx context.Context, jti, userID string, expiresAt time.Time) error
	IsRefreshTokenValid(ctx context.Context, jti string) (bool, error)
	RevokeRefreshToken(ctx context.Context, jti string) error
	RevokeUserRefreshTokens(ctx context.Context, userID string) error
}

type Router struct {
	app            *manager.App
	token          string
	jwtEnabled     bool // true in cloud mode: validate JWT instead of pre-shared token
	allowedOrigins []string
	staticDir      string
	clients        map[chan Event]bool
	clientsMu      sync.Mutex
	hub            *wshub.Hub
	localUserID    string
	localName      string
	authMgr        *auth.Manager
	orgSvc         *collaboration.OrgService
	tokenStore     refreshTokenStore // non-nil in cloud mode with a DB backend

	migrationMu      sync.Mutex
	migrationRes     *migration.Result
	migrationRunning bool

	// usedTickets records consumed WebSocket connect tickets (by jti) so a
	// ticket can be redeemed at most once. Entries are pruned as they expire.
	usedTicketsMu sync.Mutex
	usedTickets   map[string]time.Time
}

type Event struct {
	Name string `json:"name"`
	Data any    `json:"data"`
}

// NewRouter creates the HTTP router.
//
// token is the pre-shared secret (local mode) or the JWT signing key (cloud mode).
// jwtEnabled should be true for cloud deployments where each request carries a JWT.
// allowedOrigins lists origins permitted for CORS and WebSocket upgrades.
// Pass nil or empty to allow only same-origin (localhost in local mode).
func NewRouter(app *manager.App, token string, jwtEnabled bool, allowedOrigins []string, staticDir string) *Router {
	// Use postgres-backed org storage when available, otherwise fall back to in-memory.
	orgStore := collaboration.NewMemOrgStore()
	if s, ok := app.StorageBackend().(collaboration.OrgStore); ok {
		orgStore = s
	}

	// Refresh-token rotation requires a durable store; only enabled when the
	// backend (Postgres) implements it.
	var tokenStore refreshTokenStore
	if ts, ok := app.StorageBackend().(refreshTokenStore); ok {
		tokenStore = ts
	}

	return &Router{
		app:            app,
		token:          token,
		jwtEnabled:     jwtEnabled,
		allowedOrigins: allowedOrigins,
		staticDir:      staticDir,
		clients:        make(map[chan Event]bool),
		hub:            wshub.NewHub(),
		localUserID:    "local",
		localName:      "You",
		authMgr:        auth.NewManager(token),
		orgSvc:         collaboration.NewOrgService(orgStore),
		tokenStore:     tokenStore,
		usedTickets:    make(map[string]time.Time),
	}
}

// maxUsedTickets caps the replay-protection map so a flood of issued tickets
// cannot grow the map unboundedly. Tickets are tiny (jti + expiry), so a
// 10k cap is generous (~hundreds of KB) while still bounding worst-case
// memory. When full, consumeTicket refuses new entries rather than evicting
// arbitrary ones — that would create a window where a previously-used ticket
// could be replayed.
const maxUsedTickets = 10_000

// consumeTicket records a ticket jti as used and reports whether the caller may
// proceed. Returns false on replay (already-seen jti) or when the bounded
// store is full. Expired entries are pruned at every insert so the cap is
// only reached if the issuance rate exceeds the ticket TTL × throughput.
func (rt *Router) consumeTicket(jti string, exp time.Time) bool {
	if jti == "" {
		return false
	}
	rt.usedTicketsMu.Lock()
	defer rt.usedTicketsMu.Unlock()

	now := time.Now()
	// Prune expired entries first — this is the dominant size control.
	for id, t := range rt.usedTickets {
		if t.Before(now) {
			delete(rt.usedTickets, id)
		}
	}
	if _, seen := rt.usedTickets[jti]; seen {
		return false
	}
	if len(rt.usedTickets) >= maxUsedTickets {
		// Refuse rather than evict: evicting an arbitrary entry would let
		// the just-evicted ticket be replayed before its real expiry.
		logger.Warn("consumeTicket: usedTickets cap reached, refusing ticket",
			"cap", maxUsedTickets, "jti", jti)
		return false
	}
	rt.usedTickets[jti] = exp
	return true
}

// isOriginAllowed reports whether the given Origin header value is in the allowlist.
// An empty origin (non-browser, curl, etc.) is always allowed.
// In local mode (no JWT) any localhost or tauri:// origin is implicitly allowed
// so that the Tauri WebView — whether served from the dev Vite server or the
// production tauri:// protocol — can reach the loopback-only sidecar.
func (rt *Router) isOriginAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	if !rt.jwtEnabled && isLocalhostOrigin(origin) {
		return true
	}
	for _, o := range rt.allowedOrigins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// isLocalhostOrigin returns true for origins that are unambiguously local:
// http(s)://localhost:*, http(s)://127.0.0.1:*, and the Tauri custom protocol.
func isLocalhostOrigin(origin string) bool {
	for _, prefix := range []string{
		"http://localhost", "https://localhost",
		"http://127.0.0.1", "https://127.0.0.1",
		"tauri://localhost",
	} {
		if strings.HasPrefix(origin, prefix) {
			return true
		}
	}
	return false
}

// maxSSEClients caps concurrent SSE connections in cloud mode to prevent
// unbounded memory growth from clients that connect and never disconnect.
const maxSSEClients = 500

func (rt *Router) Emit(name string, data any) {
	rt.clientsMu.Lock()
	defer rt.clientsMu.Unlock()
	ev := Event{Name: name, Data: data}
	for client := range rt.clients {
		select {
		case client <- ev:
		default:
			// Channel buffer full — client is too slow. Log so the issue is
			// visible in server logs rather than silently disappearing.
			logger.Warn("SSE client dropped event: send buffer full", "event", name)
		}
	}
}

// publicRoutes are paths that must remain accessible without authentication
// in cloud/JWT mode (login, token refresh, registration).
var publicRoutes = map[string]bool{
	"/api/auth/register": true,
	"/api/auth/login":    true,
	"/api/auth/refresh":  true,
	"/healthz":           true,
	"/readyz":            true,
	"/api/health":        true,
	"/metrics":           true, // Prometheus scrape; gate via network policy, not auth.
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Clean the path to handle double slashes and trailing slashes.
	// This ensures that routes in dispatch() match correctly.
	r.URL.Path = path.Clean(r.URL.Path)

	// Limit request body to 10 MB to prevent DoS via large payloads.
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)

	// Strip the default Go server header. Identifying the runtime + version
	// helps targeted-CVE attackers more than it helps any legitimate
	// operator (`Server` is not part of any contract).
	w.Header().Set("Server", "")

	// Security headers — applied to every response. Some (CSP) are only
	// meaningful for HTML responses; we set them on the SPA-fallback path
	// in dispatch() rather than here so JSON API responses don't carry
	// CSP unnecessarily.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
	// Disable powerful browser APIs for any HTML this app might serve.
	// The PAD Analyzer SPA needs none of these; an XSS that tries to use
	// them is blocked at the browser policy layer.
	w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=(), usb=()")
	// Cross-origin isolation: prevent other-origin documents from getting
	// a reference to our window (mitigates a class of Spectre-style and
	// cross-origin information disclosure attacks).
	w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
	w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
	if rt.jwtEnabled {
		// HSTS only makes sense when TLS is present (cloud mode).
		w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
	}

	// CORS: echo the request origin only if it is in the allowlist.
	// In local/Tauri mode the allowedOrigins list is empty, so only
	// non-browser callers (empty Origin) are accepted without a check.
	origin := r.Header.Get("Origin")
	if rt.isOriginAllowed(origin) && origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
		w.Header().Set("Vary", "Origin")
	}
	w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusOK)
		return
	}

	// Swagger UI
	if strings.HasPrefix(r.URL.Path, "/swagger/") || r.URL.Path == "/swagger" {
		if r.URL.Path == "/swagger" {
			http.Redirect(w, r, "/swagger/", http.StatusMovedPermanently)
			return
		}
		httpSwagger.WrapHandler.ServeHTTP(w, r)
		return
	}

	// Authentication.
	//
	// /ws is intentionally excluded here: browsers cannot set an Authorization
	// header on a WebSocket handshake, so the only header-free options are the
	// token in the URL (which leaks into logs/history) or a short-lived ticket.
	// The /ws branch below authenticates via a single-use ticket instead.
	isAPI := strings.HasPrefix(r.URL.Path, "/api/")
	if isAPI && !publicRoutes[r.URL.Path] {
		tokenStr := auth.ExtractToken(r)
		if rt.jwtEnabled {
			// Cloud mode: validate JWT
			if tokenStr == "" {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			claims, err := rt.authMgr.Verify(tokenStr)
			if err != nil {
				http.Error(w, "invalid or expired token", http.StatusUnauthorized)
				return
			}
			r = r.WithContext(auth.WithClaims(r.Context(), claims))
		} else {
			// Local/Tauri mode: validate against the pre-shared static token.
			// Constant-time compare so the check can't be brute-forced via timing.
			if subtle.ConstantTimeCompare([]byte(tokenStr), []byte(rt.token)) != 1 {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
	}

	// SSE endpoint
	if r.URL.Path == "/api/events" {
		rt.handleEvents(w, r)
		return
	}

	// WebSocket collaboration endpoint.
	//
	// Authenticated via a short-lived, single-use ?ticket= (obtained from
	// POST /api/ws-ticket with the normal access token). This keeps the
	// long-lived access token out of the WS URL. The same flow is used in
	// local mode so the desktop client never puts its token in the URL either.
	if r.URL.Path == "/ws" {
		ticket := r.URL.Query().Get("ticket")
		claims, err := rt.authMgr.VerifyWSTicket(ticket)
		if err != nil {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if !rt.consumeTicket(claims.ID, claims.ExpiresAt.Time) {
			// Replayed or malformed ticket.
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		userID, userName := claims.UserID, claims.Email
		wshub.Handler(rt.hub, userID, userName, rt.allowedOrigins)(w, r)
		return
	}

	// Dispatch other routes
	rt.dispatch(w, r)
}

func (rt *Router) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Enforce a cap in cloud mode to prevent unbounded memory growth from
	// clients that connect and never disconnect.
	if rt.jwtEnabled {
		rt.clientsMu.Lock()
		count := len(rt.clients)
		rt.clientsMu.Unlock()
		if count >= maxSSEClients {
			http.Error(w, "too many SSE connections", http.StatusServiceUnavailable)
			return
		}
	}

	ch := make(chan Event, 10)
	rt.clientsMu.Lock()
	rt.clients[ch] = true
	rt.clientsMu.Unlock()
	metrics.SSEClientStart()

	defer func() {
		rt.clientsMu.Lock()
		delete(rt.clients, ch)
		rt.clientsMu.Unlock()
		metrics.SSEClientEnd()
	}()

	ctx := r.Context()
	for {
		select {
		case <-ctx.Done():
			return
		case ev := <-ch:
			data, _ := json.Marshal(ev)
			fmt.Fprintf(w, "data: %s\n\n", data)
			flusher.Flush()
		}
	}
}
