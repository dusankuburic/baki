package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/manager"
	"pad-analyzer/internal/migration"
	wshub "pad-analyzer/internal/websocket"
)

type Router struct {
	app            *manager.App
	token          string
	jwtEnabled     bool // true in cloud mode: validate JWT instead of pre-shared token
	allowedOrigins []string
	clients        map[chan Event]bool
	clientsMu      sync.Mutex
	hub            *wshub.Hub
	localUserID    string
	localName      string
	authMgr        *auth.Manager
	orgSvc         *collaboration.OrgService

	migrationMu      sync.Mutex
	migrationRes     *migration.Result
	migrationRunning bool
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
func NewRouter(app *manager.App, token string, jwtEnabled bool, allowedOrigins []string) *Router {
	// Use postgres-backed org storage when available, otherwise fall back to in-memory.
	orgStore := collaboration.NewMemOrgStore()
	if s, ok := app.StorageBackend().(collaboration.OrgStore); ok {
		orgStore = s
	}

	return &Router{
		app:            app,
		token:          token,
		jwtEnabled:     jwtEnabled,
		allowedOrigins: allowedOrigins,
		clients:        make(map[chan Event]bool),
		hub:            wshub.NewHub(),
		localUserID:    "local",
		localName:      "You",
		authMgr:        auth.NewManager(token),
		orgSvc:         collaboration.NewOrgService(orgStore),
	}
}

// isOriginAllowed reports whether the given Origin header value is in the allowlist.
// An empty origin (non-browser, curl, etc.) is always allowed.
func (rt *Router) isOriginAllowed(origin string) bool {
	if origin == "" {
		return true
	}
	for _, o := range rt.allowedOrigins {
		if strings.EqualFold(o, origin) {
			return true
		}
	}
	return false
}

func (rt *Router) Emit(name string, data any) {
	rt.clientsMu.Lock()
	defer rt.clientsMu.Unlock()
	ev := Event{Name: name, Data: data}
	for client := range rt.clients {
		select {
		case client <- ev:
		default:
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
	"/api/health":        true,
}

func (rt *Router) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Limit request body to 10 MB to prevent DoS via large payloads.
	r.Body = http.MaxBytesReader(w, r.Body, 10<<20)

	// Security headers — always set regardless of mode.
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
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

	if rt.jwtEnabled {
		// Cloud mode: use JWT auth except for public auth routes.
		if !publicRoutes[r.URL.Path] {
			tokenStr := auth.ExtractToken(r)
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
		}
	} else {
		// Local/Tauri mode: validate against the pre-shared static token.
		tokenStr := auth.ExtractToken(r)
		if tokenStr != rt.token {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	// SSE endpoint
	if r.URL.Path == "/api/events" {
		rt.handleEvents(w, r)
		return
	}

	// WebSocket collaboration endpoint.
	// In JWT mode resolve the real user from claims; fall back to "local" in desktop mode.
	if r.URL.Path == "/ws" {
		userID, userName := rt.localUserID, rt.localName
		if rt.jwtEnabled {
			if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
				userID = claims.UserID
				userName = claims.Email
			}
		}
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

	ch := make(chan Event, 10)
	rt.clientsMu.Lock()
	rt.clients[ch] = true
	rt.clientsMu.Unlock()

	defer func() {
		rt.clientsMu.Lock()
		delete(rt.clients, ch)
		rt.clientsMu.Unlock()
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
