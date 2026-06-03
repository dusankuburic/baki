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

func requireFlowOwner(w http.ResponseWriter, r *http.Request, backend storageif.StorageBackend, security *SecurityConfig, flowID string) bool {
	if !security.JWTEnabled {
		return true
	}
	if backend == nil {
		http.Error(w, "storage unavailable", http.StatusInternalServerError)
		return false
	}
	flow, err := backend.LoadFlow(r.Context(), flowID)
	if err != nil {
		render.Error(w, err, 0)
		return false
	}
	if flow.OwnerID != security.CallerID(r) {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return false
	}
	return true
}
