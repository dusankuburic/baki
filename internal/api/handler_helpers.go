package api

import (
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/service"
)

// resolveFlow loads a flow and verifies the caller has at least minPerm access.
// In local mode (JWTEnabled=false) it returns the currently loaded document.
// All authz logic lives in FlowService.GetAuthorized — do not add policy here.
func resolveFlow(w http.ResponseWriter, r *http.Request, flowSvc *service.FlowService, security *SecurityConfig, flowID, minPerm string) (*models.FlowDocument, bool) {
	if !security.JWTEnabled {
		doc := flowSvc.DocProvider().CurrentDoc()
		if doc == nil {
			render.Error(w, fmt.Errorf("no flow loaded"), http.StatusBadRequest)
			return nil, false
		}
		return doc, true
	}

	userID := security.CallerID(r)
	doc, err := flowSvc.GetAuthorized(r.Context(), flowID, userID, minPerm)
	if err != nil {
		render.Error(w, err, 0)
		return nil, false
	}
	return doc, true
}

// requireFlowPerm verifies the caller has at least minPerm access to the flow,
// writing the HTTP error itself. Like resolveFlow but for handlers that don't
// need the document. Local mode (no JWT) always passes.
func requireFlowPerm(w http.ResponseWriter, r *http.Request, flowSvc *service.FlowService, security *SecurityConfig, flowID, minPerm string) bool {
	if !security.JWTEnabled {
		return true
	}
	if _, err := flowSvc.GetAuthorized(r.Context(), flowID, security.CallerID(r), minPerm); err != nil {
		render.Error(w, err, 0)
		return false
	}
	return true
}

// requireOrgMember verifies the caller is a member of orgID (any role),
// writing a 403 itself when not. Local mode (no JWT) always passes.
// Returns 500 if OrgSvc is not wired (misconfiguration) rather than silently denying.
func requireOrgMember(w http.ResponseWriter, r *http.Request, security *SecurityConfig, orgID string) bool {
	if !security.JWTEnabled {
		return true
	}
	if security.OrgSvc == nil {
		render.Error(w, fmt.Errorf("organization service not available"), http.StatusInternalServerError)
		return false
	}
	if !security.OrgSvc.IsMember(r.Context(), orgID, security.CallerID(r)) {
		render.Error(w, fmt.Errorf("forbidden: not a member of this organization"), http.StatusForbidden)
		return false
	}
	return true
}

// requireOrgAdmin verifies the caller is an admin of orgID, writing a 403
// itself when not. Local mode (no JWT) always passes.
func requireOrgAdmin(w http.ResponseWriter, r *http.Request, security *SecurityConfig, orgID string) bool {
	if !security.JWTEnabled {
		return true
	}
	if security.OrgSvc == nil {
		render.Error(w, fmt.Errorf("organization service not available"), http.StatusInternalServerError)
		return false
	}
	if !security.OrgSvc.IsAdmin(r.Context(), orgID, security.CallerID(r)) {
		render.Error(w, fmt.Errorf("forbidden: organization admin required"), http.StatusForbidden)
		return false
	}
	return true
}
