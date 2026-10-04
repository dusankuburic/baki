package api

import (
	"context"
	"crypto/subtle"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	storageif "pad-analyzer/internal/storage/interfaces"
	wshub "pad-analyzer/internal/websocket"

	_ "pad-analyzer/docs"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
	httpSwagger "github.com/swaggo/http-swagger"
	"go.uber.org/fx"
)

// Handlers groups all feature-specific HTTP handlers.
// Pass it as a single value to NewRouter instead of 11 separate parameters.
type Handlers struct {
	Sys       *SystemHandler
	Flow      *FlowHandler
	Library   *LibraryHandler
	Chat      *ChatHandler
	Analysis  *AnalysisHandler
	Dashboard *DashboardHandler
	Export    *ExportHandler
	Auth      *AuthHandler
	Admin     *AdminHandler
	Provider  *ProviderHandler
	Org       *OrgHandler
	Sharing   *SharingHandler
}

type Router struct {
	security     *SecurityConfig
	eventManager *EventManager
	handlers     Handlers

	AllowedOrigins []string
	trustedProxies []string
	metricsToken   string
	// pprofEnabled gates registration of /debug/pprof/* (PAD_PPROF_ENABLED, default false).
	// Config validation guarantees metricsToken is non-empty whenever this is true.
	pprofEnabled bool
	staticDir    string
	hub          *wshub.Hub
	flowChecker  wshub.FlowAccessChecker

	// perUserLimiter caps one authenticated user's total write throughput; nil in local mode (the middleware then passes through).
	// Installed after NewRouter through SetPerUserLimiter, so the middleware loads it per request — at chain construction it is always still nil.
	perUserLimiter atomic.Pointer[middleware.RateLimiter]

	usedTicketsMu sync.Mutex
	usedTickets   map[string]time.Time

	// indexCSPOnce/indexCSPHash cache the sha256 pin of index.html's inline theme-bootstrap script, derived lazily from the file actually served:
	// a hardcoded hash drifted once and silently blocked the bootstrap on every page load — deriving makes that impossible.
	indexCSPOnce sync.Once
	indexCSPHash string

	shutdownCh   chan struct{}
	shutdownOnce sync.Once
	mux          *chi.Mux
}

func NewRouter(
	lc fx.Lifecycle,
	security *SecurityConfig,
	eventManager *EventManager,
	handlers Handlers,
	cfg *config.Config,
	shutdownCh chan struct{},
	flowChecker wshub.FlowAccessChecker,
	redisClient *redis.Client,
) *Router {
	rt := &Router{
		security:       security,
		eventManager:   eventManager,
		handlers:       handlers,
		AllowedOrigins: cfg.Server.AllowedOrigins,
		trustedProxies: cfg.Server.TrustedProxies,
		metricsToken:   cfg.Server.MetricsToken,
		pprofEnabled:   cfg.Server.PprofEnabled,
		staticDir:      cfg.Server.StaticDir,
		// nil client (PAD_REDIS_URL unset) → in-memory hub; otherwise a Redis
		// pub/sub + presence backplane so presence/broadcasts span replicas.
		hub:         wshub.NewHubWithRedis(redisClient),
		flowChecker: flowChecker,
		usedTickets: make(map[string]time.Time),
		shutdownCh:  shutdownCh,
		mux:         chi.NewRouter(),
	}

	// Release the hub's backplane subscriber on shutdown. SAFETY NET for startServer's OnStop (Router.ShutdownWebSocket → Hub.Shutdown,
	// which already closes); if that hook never fires, this still releases it. Hub.Close is idempotent.
	lc.Append(fx.Hook{OnStop: func(context.Context) error { rt.hub.Close(); return nil }})

	// Wire the WebSocket hub as a flow-change notifier so library saves and apply-fix /
	// save-source / suppress edits broadcast to all connected viewers.
	if handlers.Library != nil {
		handlers.Library.SetFlowNotifier(rt.hub)
	}
	if handlers.Flow != nil {
		handlers.Flow.SetFlowNotifier(rt.hub)
	}

	// Wire the CORS allowlist into the SSE EventManager so it respects
	// the same origin policy as every other route (fixes hardcoded "*").
	rt.eventManager.SetOriginChecker(rt.isOriginAllowed)

	// Wire the per-client connection-limit key so SSE caps don't collapse all
	// users behind a reverse proxy onto one IP bucket.
	rt.eventManager.SetClientKeyFunc(rt.sseClientKey)

	// Wire the auth blacklist so SSE connections are periodically re-validated: a blacklisted access token (logout / revoke) disconnects the session.
	// Refresh-only revocations end when the short-lived access token expires, which the SSE loop enforces directly.
	if rt.security.AuthMgr != nil {
		rt.eventManager.SetRevocationChecker(rt.security.AuthMgr.IsRevoked)
	}

	// Reclaim expired single-use WS tickets in the background so consumeTicket
	// stays O(1) per connection instead of scanning the whole map each time.
	go rt.cleanupUsedTickets()

	registerRoutes(rt, rt.mux)

	return rt
}

func (rt *Router) Shutdown() {
	rt.shutdownOnce.Do(func() { close(rt.shutdownCh) })
}

// DrainBackground stops starting best-effort background notifications and waits for the ones in
// flight, or ctx. Call AFTER server.Shutdown, once no request can start another.
func (rt *Router) DrainBackground(ctx context.Context) error {
	if rt.handlers.Analysis == nil {
		return nil
	}
	return rt.handlers.Analysis.DrainBackground(ctx)
}

// ShutdownWebSocket gracefully drains all WebSocket clients (CloseGoingAway, conns closed, pumps waited for). Call BEFORE server.Shutdown — http.Server.Shutdown does not close hijacked sockets. Idempotent.
func (rt *Router) ShutdownWebSocket(ctx context.Context) error {
	return rt.hub.Shutdown(ctx)
}

// ServeHTTP is intentionally thin. All cross-cutting concerns (security
// headers, CORS, auth) are handled by chi middleware registered in registerRoutes.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r.URL.Path = path.Clean(r.URL.Path)
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	rt.mux.ServeHTTP(w, r)
}

// --- Middleware ---

func (rt *Router) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Permissions-Policy", "geolocation=(), microphone=(), camera=(), payment=(), usb=()")
		w.Header().Set("Cross-Origin-Opener-Policy", "same-origin")
		w.Header().Set("Cross-Origin-Resource-Policy", "same-origin")
		// Sensitive JSON (auth/me, sessions, library, admin, account export) must not be retained by a shared/intermediate or browser cache.
		// Intentionally cacheable endpoints override (Set replaces); SSE sets no-cache itself.
		w.Header().Set("Cache-Control", "no-store, private")
		if rt.security.JWTEnabled {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func (rt *Router) corsHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		// Vary: Origin must be set unconditionally: emitted only when the origin is allowed, a CDN/proxy caching by URL could serve the no-ACAO
		// variant (stripping the legitimate ACAO) to a later allowed-origin request, or vice-versa.
		w.Header().Set("Vary", "Origin")
		if rt.isOriginAllowed(origin) && origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// jwtAuth enforces authentication on all /api/ routes except publicRoutes,
// judged on the path chi dispatches on (routePath), not the decoded one.
func (rt *Router) jwtAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := routePath(r)
		if !strings.HasPrefix(p, "/api/") || publicRoutes[p] {
			next.ServeHTTP(w, r)
			return
		}
		tokenStr := auth.ExtractToken(r)
		if rt.security.JWTEnabled {
			if tokenStr == "" {
				render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
				return
			}
			// Machine tokens (PATs) are verified against storage by hash; JWTs locally. Both
			// resolve to the same Claims so downstream authz is identical regardless of type.
			var claims *auth.Claims
			if auth.IsAPIToken(tokenStr) {
				claims = rt.verifyAPIToken(r.Context(), tokenStr)
				// Scope enforcement: a scoped PAT may only touch routes its capabilities allow — checked HERE, before any handler runs, so no route can skip it.
				// Unscoped PATs and interactive JWTs pass through.
				if claims != nil && claims.TokenScoped() && !auth.RouteAllowed(r.Method, p, claims.Scopes) {
					render.Error(w, fmt.Errorf("token is not authorized for this operation (missing scope)"), http.StatusForbidden)
					return
				}
			} else if c, err := rt.security.AuthMgr.Verify(tokenStr); err == nil {
				claims = c
			}
			if claims == nil {
				render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
				return
			}
			r = r.WithContext(auth.WithClaims(r.Context(), claims))
		} else {
			// ConstantTimeCompare("","") == 1 — an empty configured token would admit anyone.
			// Unreachable today (the auth manager backfills a random secret), but fail closed regardless.
			if rt.security.Token == "" || subtle.ConstantTimeCompare([]byte(tokenStr), []byte(rt.security.Token)) != 1 {
				render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// verifyAPIToken authenticates a machine token: hash → storage lookup → expiry check → the owner's CURRENT user row (deleted/demoted reflected at once). Returns nil on failure (401).
// Deliberately NOT cached: a TTL cache would weaken the revocation-immediacy contract (TestAPIToken_RevokedRejected deletes via storage directly, as retention purges do).
func (rt *Router) verifyAPIToken(ctx context.Context, raw string) *auth.Claims {
	if rt.security.Backend == nil {
		return nil
	}
	tok, user := lookupAPITokenOwner(ctx, rt.security.Backend, auth.HashAPIToken(raw))
	if tok == nil || user == nil {
		return nil
	}
	if tok.ExpiresAt != nil && time.Now().After(*tok.ExpiresAt) {
		return nil
	}
	// Populate a stable JTI and the PAT's expiry so a WS ticket issued from this PAT-authenticated request embeds a real SrcJTI/SrcExp —
	// else the re-authz loop never arms the expiry timer and a revoked/expired PAT's live socket would stay open indefinitely.
	return auth.ClaimsForPAT(user.ID, user.Email, user.Role, tok.ID, tok.ExpiresAt, tok.Scopes)
}

// patBackend is what PAT authentication needs from storage.
type patBackend interface {
	GetAPITokenByHash(ctx context.Context, tokenHash string) (*storageif.APIToken, error)
	LoadUserByID(ctx context.Context, id string) (*storageif.User, error)
}

// lookupAPITokenOwner resolves a PAT hash to the token and its owner's current user row, or (nil, nil). Backends that can join the two
// (storageif.APITokenOwnerLookup — Postgres) answer in one round trip; the fallback is the token read then the user read.
func lookupAPITokenOwner(ctx context.Context, b patBackend, tokenHash string) (*storageif.APIToken, *storageif.User) {
	if j, ok := b.(storageif.APITokenOwnerLookup); ok {
		tok, user, err := j.GetAPITokenWithOwnerByHash(ctx, tokenHash)
		if err != nil || tok == nil || user == nil {
			return nil, nil
		}
		return tok, user
	}
	tok, err := b.GetAPITokenByHash(ctx, tokenHash)
	if err != nil || tok == nil {
		return nil, nil
	}
	user, err := b.LoadUserByID(ctx, tok.UserID)
	if err != nil || user == nil {
		return nil, nil
	}
	return tok, user
}

// --- WebSocket handler ---

// @Summary      WebSocket gateway
// @Description  Authenticated WebSocket for collaborative presence and live updates; obtain a one-time ticket via /api/ws-ticket.
// @Tags         realtime
// @Produce      json
// @Success      101 {string} string "Switching Protocols"
// @Router       /ws [get]
func (rt *Router) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("ticket")
	// ConsumeWSTicket atomically verifies + marks the ticket consumed via the shared blacklist (AddIfAbsent), single-use across all replicas;
	// without a blacklist (local mode) it only verifies the JWT — the local map below handles single-use.
	claims, err := rt.security.AuthMgr.ConsumeWSTicket(ticket)
	if err != nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	// Local map fallback for local mode and defense-in-depth in cloud (belt-and-suspenders).
	if !rt.consumeTicket(claims.ID, claims.ExpiresAt.Time) {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	var isRevoked func(string) bool
	if rt.security.AuthMgr != nil {
		isRevoked = rt.security.AuthMgr.IsRevoked
	}
	// Pass the SOURCE access token's JTI/expiry (SrcJTI/SrcExp), NOT the ticket's own JTI: the ticket JTI is blacklisted only ~30s at consumption.
	// The access-token JTI is what logout/revoke blacklists, so the WS re-authz loop can disconnect the live socket.
	accessJTI := claims.SrcJTI
	var accessExp time.Time
	if claims.SrcExp != nil {
		accessExp = claims.SrcExp.Time
	}
	wshub.Handler(rt.hub, claims.UserID, claims.Email, rt.AllowedOrigins, rt.flowChecker, accessJTI, accessExp, isRevoked)(w, r)
}

// --- Ticket store ---

const maxUsedTickets = 10_000

// consumeTicket records a single-use WS ticket, O(1) on the common path (a background sweep
// reclaims expired entries); at the cap a one-off expired sweep runs before rejecting.
func (rt *Router) consumeTicket(jti string, exp time.Time) bool {
	if jti == "" {
		return false
	}
	rt.usedTicketsMu.Lock()
	defer rt.usedTicketsMu.Unlock()

	if _, seen := rt.usedTickets[jti]; seen {
		return false
	}
	if len(rt.usedTickets) >= maxUsedTickets {
		now := time.Now()
		for id, t := range rt.usedTickets {
			if t.Before(now) {
				delete(rt.usedTickets, id)
			}
		}
		if len(rt.usedTickets) >= maxUsedTickets {
			return false
		}
	}
	rt.usedTickets[jti] = exp
	return true
}

// cleanupUsedTickets periodically evicts expired single-use tickets so the map doesn't
// accumulate stale entries; stops when the shutdown channel closes.
func (rt *Router) cleanupUsedTickets() {
	defer func() {
		if r := recover(); r != nil {
			slog.Warn("cleanupUsedTickets goroutine panicked", "err", r)
		}
	}()
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-rt.shutdownCh:
			return
		case <-t.C:
			now := time.Now()
			rt.usedTicketsMu.Lock()
			for id, exp := range rt.usedTickets {
				if exp.Before(now) {
					delete(rt.usedTickets, id)
				}
			}
			rt.usedTicketsMu.Unlock()
		}
	}
}

// sseClientKey buckets SSE connection limits: per authenticated user in JWT mode (users sharing
// a proxy/NAT egress IP are limited independently), else per proxy-aware client IP.
func (rt *Router) sseClientKey(r *http.Request) string {
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		return "user:" + claims.UserID
	}
	return "ip:" + middleware.ClientIP(r, rt.trustedProxies)
}

// --- Origin checks ---

func (rt *Router) isOriginAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	if !rt.security.JWTEnabled && isLocalhostOrigin(origin) {
		return true
	}
	for _, o := range rt.AllowedOrigins {
		// A "*" wildcard is only honored when auth is disabled: in cloud mode requests are credentialed, and reflecting an arbitrary origin alongside
		// credentials defeats the same-origin policy — an explicit allowlist is required.
		if o == "*" {
			if !rt.security.JWTEnabled {
				return true
			}
			continue
		}
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// localhostHosts are the hostnames the desktop (local, no-JWT) build serves from: the Tauri v2 webview origin is "tauri://localhost" on
// macOS/Linux and "http://tauri.localhost" on Windows (WebView2). Earlier versions missed the Windows form, so on Windows nothing loaded (CORS).
var localhostHosts = map[string]bool{
	"localhost":       true,
	"127.0.0.1":       true,
	"tauri.localhost": true,
}

// isLocalhostOrigin matches on the parsed scheme+hostname rather than a string
// prefix, so look-alikes like http://localhost.evil.com are not accepted.
func isLocalhostOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	switch u.Scheme {
	case "http", "https", "tauri":
		return localhostHosts[u.Hostname()]
	default:
		return false
	}
}

var publicRoutes = map[string]bool{
	"/api/auth/register": true,
	"/api/auth/login":    true,
	"/api/auth/refresh":  true,
	"/api/auth/logout":   true,
	// Password recovery and email verification are pre-authentication: the user
	// either has no session or is acting on a one-time emailed token.
	"/api/auth/forgot-password":     true,
	"/api/auth/reset-password":      true,
	"/api/auth/verify-email":        true,
	"/api/auth/resend-verification": true,
	// SSO endpoints are pre-authentication by definition: the browser hits start/callback before it has any token, and exchange carries its own single-use ticket credential.
	"/api/auth/sso/info":     true,
	"/api/auth/sso/start":    true,
	"/api/auth/sso/callback": true,
	"/api/auth/sso/exchange": true,
	"/api/local-config":      true,
	"/api/system/features":   true, // pre-auth: login page reads flags to hide the register button
	"/api/shared":            true, // unauthenticated share-link viewer (?token=...)
	"/api/integrations/ci":   true, // HMAC-authenticated inbound CI webhook (X-Baki-Signature)
	"/healthz":               true,
	"/readyz":                true,
	"/api/health":            true,
	"/metrics":               true,
}

// swagger handler (chi-compatible, not a method on Router)
func swaggerHandler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/swagger" || r.URL.Path == "/swagger/" {
			http.Redirect(w, r, "/swagger/index.html", http.StatusMovedPermanently)
			return
		}
		httpSwagger.WrapHandler.ServeHTTP(w, r)
	}
}
