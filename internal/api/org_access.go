package api

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// Authorization of the org-scoped routes, on the organisation in the "id" URL parameter: AuthzService for the
// caller's actor, so a scoped API token's scopes count too. A refusal is 403 "forbidden". An organisation that does
// not exist, or cannot be read, is 403 for the member actions, which reveal nothing about which organisations exist,
// and 404 for OrgAdmin, as the admin routes have always answered.
//
// Without the server store Can allows every org action, while the organisations still live in memory there: the
// routes that run without the store check with authorizeOrg, which reads the organisation first and so keeps
// answering 403/404 for one that does not exist. canOrg and requireOrg, which decide on the membership row alone,
// serve the routes that need the store.

// orgDenied writes the answer to err, the refusal of action on the organisation.
func orgDenied(w http.ResponseWriter, err error, action service.Action) {
	if action != service.OrgAdmin || errors.Is(err, service.ErrPermissionDenied) {
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return
	}
	render.Error(w, err, http.StatusNotFound)
}

// authorizeOrg reports whether the caller may perform action on the organisation and returns it, roster included,
// from the one read that decided (AuthzService.AuthorizeOrg), or nil once it has written the refusal.
func (h *OrgHandler) authorizeOrg(w http.ResponseWriter, r *http.Request, action service.Action) *storageif.Organisation {
	org, err := h.authz.AuthorizeOrg(r.Context(), auth.ActorFromContext(r.Context()), action, chi.URLParam(r, "id"))
	if err != nil {
		orgDenied(w, err, action)
		return nil
	}
	return org
}

// canOrg is authorizeOrg decided on the caller's membership row (AuthzService.Can, the point lookup) instead of the
// roster, for the routes that need the server store; on a refusal it has written the answer.
func (h *OrgHandler) canOrg(w http.ResponseWriter, r *http.Request, action service.Action) bool {
	err := h.authz.Can(r.Context(), auth.ActorFromContext(r.Context()), action, service.OrgByID(chi.URLParam(r, "id")))
	if err != nil {
		orgDenied(w, err, action)
		return false
	}
	return true
}

// requireOrg is canOrg as route middleware, for the routes that need the server store and whose handler checks
// nothing before it and needs nothing of the organisation but its id.
func (h *OrgHandler) requireOrg(action service.Action) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if h.canOrg(w, r, action) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

// Access levels of the org-scoped routes that serve what an organisation SHARES with its people, its Azure AI
// Foundry resources and models. Their handlers check after the configuration answers (no org keystore, which only
// the database keystore provides; no registry), so the check stays in the handler:
//
//   - orgAccessSharedRead: the roles that run on the organisation's shared credentials, admins and members
//     (service.OrgUse, the rule service.OrgRoleUsesSharedCredential applies to the chat turn and the provider
//     listing). Viewers, guests and non-members see nothing of them: 403, an unknown organisation included.
//   - orgAccessAdmin: writes, tests and discovery (service.OrgAdmin): 403 for anyone but an admin, 404 for an
//     unknown organisation.
type orgAccess int

const (
	orgAccessSharedRead orgAccess = iota
	orgAccessAdmin
)

// requireOrgAccess checks the caller's role in the organisation of the "id"
// URL parameter against access and returns that organisation's id; on a
// refusal it writes the error and returns false (the caller bails out).
func (h *OrgHandler) requireOrgAccess(w http.ResponseWriter, r *http.Request, access orgAccess) (orgID string, ok bool) {
	action := service.OrgUse
	if access == orgAccessAdmin {
		action = service.OrgAdmin
	}
	if !h.canOrg(w, r, action) {
		return "", false
	}
	return chi.URLParam(r, "id"), true
}
