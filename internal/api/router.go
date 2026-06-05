package api

import (
	"crypto/subtle"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

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
	Sys      *SystemHandler
	Flow     *FlowHandler
	Library  *LibraryHandler
	Chat     *ChatHandler
	Analysis *AnalysisHandler
	Export   *ExportHandler
	Auth     *AuthHandler
	Admin    *AdminHandler
	Provider *ProviderHandler
	Org      *OrgHandler
	Sharing  *SharingHandler
}

type Router struct {
	security     *SecurityConfig
	eventManager *EventManager
	handlers     Handlers

	AllowedOrigins []string
	trustedProxies []string
	staticDir      string
	hub            *wshub.Hub

	usedTicketsMu sync.Mutex
	usedTickets   map[string]time.Time

	shutdownCh chan struct{}
	mux        *chi.Mux
}

func NewRouter(
	security *SecurityConfig,
	eventManager *EventManager,
	handlers Handlers,
	cfg *config.Config,
	shutdownCh chan struct{},
) *Router {
	rt := &Router{
		security:       security,
		eventManager:   eventManager,
		handlers:       handlers,
		AllowedOrigins: cfg.Server.AllowedOrigins,
		trustedProxies: cfg.Server.TrustedProxies,
		staticDir:      cfg.Server.StaticDir,
		hub:            wshub.NewHub(),
		usedTickets:    make(map[string]time.Time),
		shutdownCh:     shutdownCh,
		mux:            chi.NewRouter(),
	}

	// Wire the CORS allowlist into the SSE EventManager so it respects
	// the same origin policy as every other route (fixes hardcoded "*").
	rt.eventManager.SetOriginChecker(rt.isOriginAllowed)

	// Reclaim expired single-use WS tickets in the background so consumeTicket
	// stays O(1) per connection instead of scanning the whole map each time.
	go rt.cleanupUsedTickets()

	registerRoutes(rt, rt.mux)

	return rt
}

func (rt *Router) Shutdown() {
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
			claims, err := rt.security.AuthMgr.Verify(tokenStr)
			if err != nil {
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

// --- WebSocket handler ---

func (rt *Router) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	ticket := r.URL.Query().Get("ticket")
	claims, err := rt.security.AuthMgr.VerifyWSTicket(ticket)
	if err != nil {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	if !rt.consumeTicket(claims.ID, claims.ExpiresAt.Time) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	wshub.Handler(rt.hub, claims.UserID, claims.Email, rt.AllowedOrigins)(w, r)
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
	"/api/local-config":  true,
	"/healthz":           true,
	"/readyz":            true,
	"/api/health":        true,
	"/metrics":           true,
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
