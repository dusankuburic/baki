package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/platform/reqtx"
	"pad-analyzer/internal/platform/secret"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type SecurityConfig struct {
	JWTEnabled     bool
	LocalUserID    string
	LocalName      string
	Token          secret.Value // local-mode shared bearer
	AuthMgr        *auth.Manager
	Backend        storageif.StorageBackend
	OrgSvc         *collaboration.OrgService
	TrustedProxies []string
	// AllowedOrigins mirrors server.allowed_origins for rejectForeignOrigin;
	// the CORS middleware keeps its own copy for response headers.
	AllowedOrigins []string
	// RequestTx opens each authenticated request's row-level-security transaction and Idempotency keeps the
	// Idempotency-Key responses inside it; both are nil without row-level security (desktop, auth off).
	RequestTx   reqtx.Opener
	Idempotency storageif.IdempotencyStore
	// Features gates optional product behaviour (read-only, env-sourced).
	Features FeatureFlags
}

// FeatureFlags mirrors config.FeatureFlags for the API layer so handlers can
// gate on flags without importing config.
type FeatureFlags struct {
	DisableSignUp bool
	// LocalConfigEndpoint allows GET /api/local-config to return the session
	// token. Off unless a browser-based local dev server explicitly enables it —
	// see config.ServerConfig.LocalConfigEndpoint for why the desktop app must
	// never turn it on.
	LocalConfigEndpoint bool
}

func (c *SecurityConfig) CallerID(r *http.Request) string {
	return c.CallerIDFromContext(r.Context())
}

// rejectForeignOrigin guards local-mode MUTATING requests against drive-by
// cross-origin "simple" POSTs. Local mode runs unauthenticated behind the
// loopback bind by design; CORS already stops a foreign page from READING the
// response (no Access-Control-Allow-Origin is reflected), but a text/plain
// cross-origin POST executes without a preflight — a malicious page could make
// the app parse an arbitrary local file into the session, overwrite the loaded
// flow's source, or write an export to any path. When the browser reports an
// Origin this deployment does not serve — not a localhost/Tauri webview
// origin, not in allowed_origins, not this server's own origin — refuse the
// request; non-browser callers (curl, tests) send no Origin and pass.
func (c *SecurityConfig) rejectForeignOrigin(w http.ResponseWriter, r *http.Request) bool {
	if c.JWTEnabled {
		return false // authenticated routes carry their own CSRF posture
	}
	origin := r.Header.Get("Origin")
	if origin == "" || isLocalhostOrigin(origin) {
		return false
	}
	// Same-origin requests (the SPA this server itself served, whatever
	// hostname it was reached under): the Origin's host:port equals the Host
	// the browser connected to. A cross-site fetch cannot forge that equality —
	// its Origin names the attacker's site while Host names this server.
	if o, err := url.Parse(origin); err == nil && o.Host != "" && o.Host == r.Host {
		return false
	}
	for _, o := range c.AllowedOrigins {
		if strings.EqualFold(o, origin) {
			return false
		}
	}
	render.Error(w, fmt.Errorf("origin not allowed"), http.StatusForbidden)
	return true
}

// CallerIDFromContext is CallerID for a typed endpoint, which has the request's context rather than the request.
func (c *SecurityConfig) CallerIDFromContext(ctx context.Context) string {
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		return claims.UserID
	}
	return c.LocalUserID
}

func (c *SecurityConfig) KeyScope(r *http.Request) string {
	return c.KeyScopeFromContext(r.Context())
}

// KeyScopeFromContext is KeyScope for a typed endpoint.
func (c *SecurityConfig) KeyScopeFromContext(ctx context.Context) string {
	if !c.JWTEnabled {
		return ""
	}
	return c.CallerIDFromContext(ctx)
}

var roleRank = map[auth.Role]int{
	auth.RoleAdmin:  40,
	auth.RoleMember: 30,
	auth.RoleViewer: 20,
	auth.RoleGuest:  10,
}

func (c *SecurityConfig) RequireRole(w http.ResponseWriter, r *http.Request, minRole auth.Role) bool {
	if !c.JWTEnabled {
		return true
	}
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
		return false
	}
	if roleRank[claims.Role] < roleRank[minRole] {
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return false
	}
	return true
}
