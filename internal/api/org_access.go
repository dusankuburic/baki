package api

import (
	"fmt"
	"net/http"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/service"
)

// Organisation role checks of the org-scoped routes that serve what an
// organisation SHARES with its people — its Azure AI Foundry resources and
// models (Part 9). Two levels:
//
//   - orgAccessSharedRead: the roles that run on the organisation's shared
//     credentials, admins and members (service.OrgRoleUsesSharedCredential,
//     the rule the chat turn and the provider listing apply). Viewers,
//     guests and non-members see nothing of them: 403, like a non-member of
//     today's org routes (requireMember) — an unknown organisation included,
//     so the answer is no existence oracle.
//   - orgAccessAdmin: writes, tests and discovery — requireAdmin, today's
//     check of the org credential routes (403 for anyone but an admin, 404
//     for an unknown organisation).
type orgAccess int

const (
	orgAccessSharedRead orgAccess = iota
	orgAccessAdmin
)

// requireOrgAccess checks the caller's role in the organisation of the "id"
// URL parameter against access and returns that organisation's id; on a
// refusal it writes the error and returns false (the caller bails out).
func (h *OrgHandler) requireOrgAccess(w http.ResponseWriter, r *http.Request, access orgAccess) (orgID string, ok bool) {
	if access == orgAccessAdmin {
		org := h.requireAdmin(w, r)
		if org == nil {
			return "", false
		}
		return org.ID, true
	}
	orgID = chi.URLParam(r, "id")
	role, err := h.orgSvc.MemberRole(r.Context(), orgID, h.security.CallerID(r))
	if err != nil || !service.OrgRoleUsesSharedCredential(role) {
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return "", false
	}
	return orgID, true
}
