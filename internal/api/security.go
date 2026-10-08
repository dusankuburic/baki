package api

import (
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/platform/reqtx"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type SecurityConfig struct {
	JWTEnabled     bool
	LocalUserID    string
	LocalName      string
	Token          string // local-mode shared bearer
	AuthMgr        *auth.Manager
	Backend        storageif.StorageBackend
	OrgSvc         *collaboration.OrgService
	TrustedProxies []string
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
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		return claims.UserID
	}
	return c.LocalUserID
}

func (c *SecurityConfig) KeyScope(r *http.Request) string {
	if !c.JWTEnabled {
		return ""
	}
	return c.CallerID(r)
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
