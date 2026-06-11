package api

import (
	"fmt"
	"net/http"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
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

// isFlowOwner reports whether the caller owns the flow, WITHOUT writing to the
// response. In local mode (no JWT) the single user is always the owner. Use
// this when ownership is only one of several acceptable conditions (e.g. a user
// removing themselves); use requireFlowOwner when ownership is the sole gate,
// since that variant writes the 403/error itself.
func isFlowOwner(r *http.Request, backend storageif.StorageBackend, security *SecurityConfig, flowID string) (bool, error) {
	if !security.JWTEnabled {
		return true, nil
	}
	if backend == nil {
		return false, fmt.Errorf("storage unavailable")
	}
	flow, err := backend.LoadFlow(r.Context(), flowID)
	if err != nil {
		return false, err
	}
	return flow.OwnerID == security.CallerID(r), nil
}

func requireFlowOwner(w http.ResponseWriter, r *http.Request, backend storageif.StorageBackend, security *SecurityConfig, flowID string) bool {
	owner, err := isFlowOwner(r, backend, security, flowID)
	if err != nil {
		render.Error(w, err, 0)
		return false
	}
	if !owner {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	return true
}
