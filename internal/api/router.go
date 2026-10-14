package api

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/drain"
	"pad-analyzer/internal/platform/secret"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	wshub "pad-analyzer/internal/websocket"

	_ "pad-analyzer/docs"

	"github.com/go-chi/chi/v5"
	"github.com/redis/go-redis/v9"
	httpSwagger "github.com/swaggo/http-swagger"
	"go.uber.org/fx"
)

type Router struct {
	security     *SecurityConfig
	eventManager *EventManager
	// modules are the features whose routes the router mounts, sorted by type.
	modules []Module
	// routes is every route's policy, built from the declarations when the routes were mounted.
	routes *route.Table
	// shares resolves share tokens for the /shared shell; nil when no module serves share links.
	shares sharedFlowLookup

	AllowedOrigins []string
	trustedProxies []string
	metricsToken   secret.Value
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
	// ticketRedis shares the single-use record of tickets across replicas (ws_ticket.go); nil keeps it per replica.
	ticketRedis *redis.Client
	// draining is the server's draining state, consulted before a WebSocket ticket is redeemed; nil is the
	// process's (drain.Process), tests set their own.
	draining *drain.State

	// indexCSPOnce/indexCSPHash cache the sha256 pin of index.html's inline theme-bootstrap script, derived lazily from the file actually served:
	// a hardcoded hash drifted once and silently blocked the bootstrap on every page load — deriving makes that impossible.
	indexCSPOnce sync.Once
	indexCSPHash string

	shutdownCh   chan struct{}
	shutdownOnce sync.Once
	mux          *chi.Mux

	// Route policy (route_policy.go): the resolver over routes (finishRoutes), which policy decides
	// (PAD_ROUTE_POLICY), and what the deployment offers routes that declare Needs.
	resolver         *route.Resolver
	policyMode       config.RoutePolicyMode
	caps             capability.Set
	fallbacksGuarded bool
}

// RouterParams is what NewRouter builds the router from.
type RouterParams struct {
	fx.In

	Lifecycle fx.Lifecycle
	Security  *SecurityConfig
	Events    *EventManager
	// Modules are the features to mount: every Module in fx's "routes" group, in no particular order.
	Modules     []Module `group:"routes"`
	Config      *config.Config
	ShutdownCh  chan struct{}
	FlowChecker wshub.FlowAccessChecker
	Redis       *redis.Client
}

// NewRouter mounts the router's own routes and every module's, and builds the route policy table from their
// declarations. It fails when a route has no declared policy (registered around the Builder).
func NewRouter(p RouterParams) (*Router, error) {
	cfg := p.Config
	rt := &Router{
		security:       p.Security,
		eventManager:   p.Events,
		modules:        sortedModules(p.Modules),
		AllowedOrigins: cfg.Server.AllowedOrigins,
		trustedProxies: cfg.Server.TrustedProxies,
		metricsToken:   cfg.Server.MetricsToken,
		pprofEnabled:   cfg.Server.PprofEnabled,
		staticDir:      cfg.Server.StaticDir,
		// nil client (PAD_REDIS_URL unset) → in-memory hub; otherwise a Redis
		// pub/sub + presence backplane so presence/broadcasts span replicas.
		hub:         wshub.NewHubWithRedis(p.Redis),
		flowChecker: p.FlowChecker,
		usedTickets: make(map[string]time.Time),
		ticketRedis: p.Redis,
		shutdownCh:  p.ShutdownCh,
		mux:         chi.NewRouter(),
		policyMode:  routePolicyMode(cfg),
		caps:        capability.Of(cfg),
	}

	// Release the hub's backplane subscriber on shutdown. SAFETY NET for startServer's OnStop (Router.ShutdownWebSocket → Hub.Shutdown,
	// which already closes); if that hook never fires, this still releases it. Hub.Close is idempotent.
	p.Lifecycle.Append(fx.Hook{OnStop: func(context.Context) error { rt.hub.Close(); return nil }})

	for _, m := range rt.modules {
		if s, ok := m.(sharedFlowLookup); ok && rt.shares == nil {
			rt.shares = s
		}
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

	rb := rt.registerRoutes()
	var err error
	if rt.routes, err = rb.Build(); err != nil {
		return nil, err
	}
	if err := rt.finishRoutes(rb); err != nil {
		return nil, err
	}

	// Reclaim expired single-use WS tickets in the background so consumeTicket
	// stays O(1) per connection instead of scanning the whole map each time.
	go rt.cleanupUsedTickets()

	return rt, nil
}

// sortedModules returns modules in a stable order (by type), so the routes mount the same way whatever order fx
// collected the group in.
func sortedModules(modules []Module) []Module {
	out := slices.Clone(modules)
	slices.SortStableFunc(out, func(a, b Module) int { return strings.Compare(fmt.Sprintf("%T", a), fmt.Sprintf("%T", b)) })
	return out
}

func (rt *Router) Shutdown() {
	rt.shutdownOnce.Do(func() { close(rt.shutdownCh) })
}

// FlowViewers is the WebSocket hub as the FlowChanged subscriber reaches a flow's open viewers (internal/di).
func (rt *Router) FlowViewers() service.FlowViewers {
	return rt.hub
}

// WebSocketHub is the hub itself, for the access-change subscriber that re-checks open sockets (internal/di).
func (rt *Router) WebSocketHub() *wshub.Hub {
	return rt.hub
}

// ShutdownWebSocket gracefully drains all WebSocket clients (CloseGoingAway, conns closed, pumps waited for). Call BEFORE server.Shutdown — http.Server.Shutdown does not close hijacked sockets. Idempotent.
func (rt *Router) ShutdownWebSocket(ctx context.Context) error {
	return rt.hub.Shutdown(ctx)
}

// ServeHTTP is intentionally thin. All cross-cutting concerns (security
// headers, CORS, auth) are handled by chi middleware registered in registerRoutes.
// It resolves the route policy when no outer layer did (tests call it directly);
// resolving before or after the path.Clean gives the same policy.
func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	r = rt.withPolicy(r)
	r.URL.Path = path.Clean(r.URL.Path)
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
	rt.mux.ServeHTTP(w, r)
}

// --- Middleware ---

// BuildVersionHeader carries the running build's version on every response. The frontend compares it with the
// version its bundle was built as and offers a reload when they differ: a tab kept open across a deploy runs old code
// against the new API.
const BuildVersionHeader = "X-Baki-Version"

func (rt *Router) securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(BuildVersionHeader, service.Version)
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
			// A cross-origin client (the desktop webview, a dev server) may read only the headers listed here.
			w.Header().Set("Access-Control-Expose-Headers", BuildVersionHeader+", "+idempotentReplayedHeader)
		}
		w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS, PUT, DELETE")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, Idempotency-Key, Last-Event-ID")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// jwtAuth authenticates the requests whose route policy requires it (authenticates: the declared policy, or the
// legacy publicRoutes table judged on the path chi dispatches on), and refuses a scoped API token the route does
// not allow, before any handler runs.
func (rt *Router) jwtAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pol, resolved := middleware.RequestPolicy(r)
		if !rt.authenticates(r, pol, resolved) {
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
				if claims != nil && claims.TokenScoped() && !rt.tokenAllowed(r, pol, resolved, claims.Scopes) {
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
			// An unset token equals nothing (secret.Equal), so an empty configured token admits no one.
			// Unreachable today (config.Load mints a secret), but fail closed regardless.
			if !rt.security.Token.Equal(tokenStr) {
				render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
				return
			}
			// No claims without accounts: authorization (AuthzService.Can) acts as the single local user.
			r = r.WithContext(auth.WithActor(r.Context(), auth.LocalActor(rt.security.LocalUserID)))
		}
		rt.security.recordCaller(r)
		next.ServeHTTP(w, r)
	})
}

// verifyAPIToken authenticates a machine token: hash → storage lookup → expiry check → the owner's CURRENT user row (deleted/demoted reflected at once). Returns nil on failure (401).
// Deliberately NOT cached: a TTL cache would weaken the revocation-immediacy contract (TestAPIToken_RevokedRejected deletes via storage directly, as retention purges do).
func (rt *Router) verifyAPIToken(ctx context.Context, raw string) *auth.Claims {
	return rt.security.verifyAPIToken(ctx, raw)
}

// verifyAPIToken is the Router's, for the routes that authenticate a token themselves (the CI webhook), so a token
// is verified the same way wherever it is presented.
func (c *SecurityConfig) verifyAPIToken(ctx context.Context, raw string) *auth.Claims {
	if c.Backend == nil {
		return nil
	}
	tok, user := lookupAPITokenOwner(ctx, c.Backend, auth.HashAPIToken(raw))
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

// validBearer reports whether r carries a live session token or API token, verified as jwtAuth verifies them, for
// the routes that authenticate themselves outside jwtAuth (the Swagger UI).
func (rt *Router) validBearer(r *http.Request) bool {
	tokenStr := auth.ExtractToken(r)
	if tokenStr == "" {
		return false
	}
	if auth.IsAPIToken(tokenStr) {
		return rt.verifyAPIToken(r.Context(), tokenStr) != nil
	}
	_, err := rt.security.AuthMgr.Verify(tokenStr)
	return err == nil
}

// lookupAPITokenOwner resolves a PAT hash to the token and its owner's current user row in one round trip, or
// (nil, nil) when either is gone.
func lookupAPITokenOwner(ctx context.Context, tokens storageif.APITokenStore, tokenHash string) (*storageif.APIToken, *storageif.User) {
	tok, user, err := tokens.GetAPITokenWithOwnerByHash(ctx, tokenHash)
	if err != nil || tok == nil || user == nil {
		return nil, nil
	}
	return tok, user
}

// --- WebSocket handler ---

// @Summary      WebSocket gateway
// @Description  Authenticated WebSocket for collaborative presence and live updates; obtain a one-time ticket via /api/ws-ticket. While the server is stopping the upgrade is refused with 503 and code SERVER_RESTARTING (Retry-After) before the ticket is redeemed, so the reconnect may present it again.
// @Tags         realtime
// @Produce      json
// @Success      101 {string} string "Switching Protocols"
// @Failure      503 {object} render.ErrorResponse "The server is restarting (code SERVER_RESTARTING; reconnect after Retry-After)"
// @Router       /ws [get]
func (rt *Router) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	// A stopping server takes no new socket, and refuses before the single-use ticket is spent.
	if rt.draining.Draining() {
		drain.Refuse(w)
		return
	}
	ticket := r.URL.Query().Get("ticket")
	// ConsumeWSTicket atomically verifies + marks the ticket consumed via the shared blacklist (AddIfAbsent), single-use across all replicas;
	// without a blacklist (local mode) it only verifies the JWT — the local map below handles single-use.
	claims, err := rt.security.AuthMgr.ConsumeWSTicket(ticket)
	if err != nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	// Redis (when configured) and the local map: the fallback for local mode and defense-in-depth in cloud (belt-and-suspenders).
	if !rt.consumeTicketEverywhere(r.Context(), claims.ID, claims.ExpiresAt.Time) {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return
	}
	// The socket acts as the credential the ticket was issued from: a PAT's ticket is that token, with its scopes.
	actor := auth.ActorFromWSTicket(claims)
	recordActor(r, actor)
	var isRevoked func(string) bool
	if rt.security.AuthMgr != nil {
		isRevoked = rt.security.AuthMgr.IsRevoked
	}
	// Pass the SOURCE access token's JTI/expiry (SrcJTI/SrcExp), NOT the ticket's own JTI: the ticket JTI is blacklisted only ~30s at consumption.
	// The access-token JTI is what logout/revoke blacklists, so the WS re-authz loop can disconnect the live socket.
	id := wshub.Identity{Actor: actor, DisplayName: claims.Email, AccessJTI: claims.SrcJTI}
	if claims.SrcExp != nil {
		id.AccessExp = claims.SrcExp.Time
	}
	wshub.Handler(rt.hub, id, rt.AllowedOrigins, rt.flowChecker, isRevoked)(w, r)
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
