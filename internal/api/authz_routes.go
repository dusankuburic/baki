package api

import (
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
)

// requireInstance is route middleware for a deployment-wide action (service.InstanceAdmin): AuthzService.Can on
// service.Instance decides on the caller's global role and token scopes before the handler runs. It answers as
// SecurityConfig.RequireRole does, 401 without a caller and 403 "forbidden" for a refusal, and like it lets every
// request through without accounts, where the caller is the single local user.
func requireInstance(authz *service.AuthzService, security *SecurityConfig, action service.Action) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if security.JWTEnabled {
				actor := auth.ActorFromContext(r.Context())
				if actor == nil {
					render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
					return
				}
				if err := authz.Can(r.Context(), actor, action, service.Instance()); err != nil {
					render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
					return
				}
			}
			next.ServeHTTP(w, r)
		})
	}
}

// mountAdmin registers fn's routes under /api/admin, the deployment's administration, which AdminHandler and
// SystemHandler share: the global admin role decides (service.InstanceAdmin), and a scoped API token needs the
// admin scope.
func mountAdmin(rb *route.Builder, authz *service.AuthzService, security *SecurityConfig, fn func(rb *route.Builder)) {
	rb.Route("/api/admin", func(rb *route.Builder) {
		fn(rb.With(requireInstance(authz, security, service.InstanceAdmin)))
	}, route.Scopes(auth.ScopeAdmin))
}
