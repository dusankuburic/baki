package api

import (
	"context"
	"crypto/subtle"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	wshub "pad-analyzer/internal/websocket"

	_ "pad-analyzer/docs"

	"github.com/go-chi/chi/v5"
	httpSwagger "github.com/swaggo/http-swagger"
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
	staticDir      string
	hub            *wshub.Hub
	flowChecker    wshub.FlowAccessChecker

	usedTicketsMu sync.Mutex
	usedTickets   map[string]time.Time

	shutdownCh   chan struct{}
	shutdownOnce sync.Once
	mux          *chi.Mux
}

func NewRouter(
	security *SecurityConfig,
	eventManager *EventManager,
	handlers Handlers,
	cfg *config.Config,
	shutdownCh chan struct{},
	flowChecker wshub.FlowAccessChecker,
) *Router {
	rt := &Router{
		security:       security,
		eventManager:   eventManager,
		handlers:       handlers,
		AllowedOrigins: cfg.Server.AllowedOrigins,
		trustedProxies: cfg.Server.TrustedProxies,
		staticDir:      cfg.Server.StaticDir,
		hub:            wshub.NewHub(),
		flowChecker:    flowChecker,
		usedTickets:    make(map[string]time.Time),
		shutdownCh:     shutdownCh,
		mux:            chi.NewRouter(),
	}

	// Wire the WebSocket hub as a flow-change notifier so that library
	// saves broadcast to all connected viewers.
	if handlers.Library != nil {
		handlers.Library.SetFlowNotifier(rt.hub)
	}

	// Wire the CORS allowlist into the SSE EventManager so it respects
	// the same origin policy as every other route (fixes hardcoded "*").
	rt.eventManager.SetOriginChecker(rt.isOriginAllowed)

	// Wire the per-client connection-limit key so SSE caps don't collapse all
	// users behind a reverse proxy onto one IP bucket.
	rt.eventManager.SetClientKeyFunc(rt.sseClientKey)

	// Wire the auth blacklist so SSE connections are periodically re-validated
	// after the initial upgrade. This disconnects a session once its access
	// token is blacklisted (logout / explicit revoke). Password change and
	// refresh-replay revoke only refresh tokens, so those sessions end when the
	// short-lived access token expires — which the SSE loop enforces directly
	// (it also drops the connection at the access-token expiry).
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
		if rt.security.JWTEnabled {
			w.Header().Set("Strict-Transport-Security", "max-age=31536000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
}

func (rt *Router) corsHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
		next.ServeHTTP(w, r)
	})
}

// jwtAuth enforces authentication on all /api/ routes except publicRoutes.
func (rt *Router) jwtAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/") || publicRoutes[r.URL.Path] {
			next.ServeHTTP(w, r)
			return
		}
		tokenStr := auth.ExtractToken(r)
		if rt.security.JWTEnabled {
			if tokenStr == "" {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			// Machine tokens (PATs) are verified against storage by hash; JWTs are
			// verified locally. Both resolve to the same Claims so downstream authz
			// is identical regardless of credential type.
			var claims *auth.Claims
			if auth.IsAPIToken(tokenStr) {
				claims = rt.verifyAPIToken(r.Context(), tokenStr)
			} else if c, err := rt.security.AuthMgr.Verify(tokenStr); err == nil {
				claims = c
			}
			if claims == nil {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			r = r.WithContext(auth.WithClaims(r.Context(), claims))
		} else {
			if subtle.ConstantTimeCompare([]byte(tokenStr), []byte(rt.security.Token)) != 1 {
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// verifyAPIToken authenticates a machine token: hash → storage lookup → expiry
// check → resolve the owning user (so a deleted/role-changed user is reflected
// immediately). Returns nil on any failure, which the caller maps to 401. The
// owner's *current* role is used, so revoking a token (delete) or demoting the
// user takes effect at once.
func (rt *Router) verifyAPIToken(ctx context.Context, raw string) *auth.Claims {
	if rt.security.Backend == nil {
		return nil
	}
	tok, err := rt.security.Backend.GetAPITokenByHash(ctx, auth.HashAPIToken(raw))
	if err != nil || tok == nil {
		return nil
	}
	if tok.ExpiresAt != nil && time.Now().After(*tok.ExpiresAt) {
		return nil
	}
	user, err := rt.security.Backend.LoadUserByID(ctx, tok.UserID)
	if err != nil || user == nil {
		return nil
	}
	return &auth.Claims{UserID: user.ID, Email: user.Email, Role: user.Role}
}

// --- WebSocket handler ---

func (rt *Router) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("ticket")
	// ConsumeWSTicket atomically verifies + marks the ticket as consumed via
	// the shared blacklist (AddIfAbsent), making it truly single-use across
	// all replicas. When no blacklist is configured (local mode), it only
	// verifies the JWT — the local consumeTicket map handles single-use.
	claims, err := rt.security.AuthMgr.ConsumeWSTicket(ticket)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	// Also check the local map as a fallback for local mode (where
	// ConsumeWSTicket doesn't enforce single-use) and as defense-in-depth
	// in cloud mode (belt-and-suspenders alongside the shared blacklist).
	if !rt.consumeTicket(claims.ID, claims.ExpiresAt.Time) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	var isRevoked func(string) bool
	if rt.security.AuthMgr != nil {
		isRevoked = rt.security.AuthMgr.IsRevoked
	}
	wshub.Handler(rt.hub, claims.UserID, claims.Email, rt.AllowedOrigins, rt.flowChecker, claims.ID, isRevoked)(w, r)
}

// --- Ticket store ---

const maxUsedTickets = 10_000

// consumeTicket records a single-use WS ticket. It is O(1) on the common path;
// expired entries are reclaimed by a background sweep (cleanupUsedTickets) rather
// than scanned on every call. As a safety net, if the cap is hit we do a one-off
// expired-entry sweep before rejecting, so a backlog of expired tickets can't
// lock out new connections.
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

// cleanupUsedTickets periodically evicts expired single-use tickets so the
// usedTickets map doesn't accumulate stale entries between connections. Stops
// when the server's shutdown channel is closed.
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

// sseClientKey returns the key used to bucket SSE connection limits. In JWT
// mode it keys per authenticated user (so users sharing a proxy/NAT egress IP
// are limited independently); otherwise it keys per proxy-aware client IP.
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
		if o == "*" || strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

// localhostHosts are the hostnames the desktop (local, no-JWT) build serves
// from. The Tauri v2 webview origin is "tauri://localhost" on macOS/Linux and
// "http://tauri.localhost" on Windows (WebView2); the Vite dev server is
// localhost:5173. Earlier versions missed "http://tauri.localhost", so on
// Windows every cross-origin request to the sidecar failed CORS (the preflight
// 200'd but carried no Access-Control-Allow-Origin) and nothing loaded.
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
	// SSO endpoints are pre-authentication by definition: the browser hits
	// start/callback before it has any token, and exchange carries its own
	// single-use ticket credential.
	"/api/auth/sso/info":     true,
	"/api/auth/sso/start":    true,
	"/api/auth/sso/callback": true,
	"/api/auth/sso/exchange": true,
	"/api/local-config":      true,
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
