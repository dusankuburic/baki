package api

import (
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type SecurityConfig struct {
	JWTEnabled  bool
	LocalUserID string
	LocalName   string
	Token       string
	AuthMgr     *auth.Manager
	Backend     storageif.StorageBackend
	OrgSvc      *collaboration.OrgService
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
