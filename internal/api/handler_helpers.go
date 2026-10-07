package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	"pad-core/models"
)

// decodeBody decodes the request body into dst, writing a 400 error response and
// returning false on failure. It replaces the decode/400/return boilerplate that
// every mutating handler otherwise repeats. Callers check the bool and return on
// false: `if !decodeBody(w, r, &req) { return }`.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return false
	}
	return true
}

// localDoc is local mode's flow: the document the desktop app has loaded (the
// client's flowId is advisory there), or a 400 when none is.
func localDoc(w http.ResponseWriter, flowSvc *service.FlowService) (*models.FlowDocument, bool) {
	doc := flowSvc.DocProvider().CurrentDoc()
	if doc == nil {
		render.Error(w, fmt.Errorf("no flow loaded"), http.StatusBadRequest)
		return nil, false
	}
	return doc, true
}

// resolveFlowID verifies the caller may perform action on the flow like
// requireFlowPerm and additionally returns the flow ID the request operates on:
// the request's own flowId in cloud mode, the currently loaded document's ID in
// local mode. Header-only — the conversation endpoints need nothing from the
// document but its ID, so they skip the blob download, unmarshal and index
// rebuild that resolving the whole document would cost on every call.
func resolveFlowID(w http.ResponseWriter, r *http.Request, flowSvc *service.FlowService, security *SecurityConfig, flowID string, action service.Action) (string, bool) {
	if !security.JWTEnabled {
		doc, ok := localDoc(w, flowSvc)
		if !ok {
			return "", false
		}
		return doc.ID, true
	}
	if err := flowSvc.CheckFlowPermission(r.Context(), flowID, action); err != nil {
		render.Error(w, err, 0)
		return "", false
	}
	return flowID, true
}

// resolveFlowIDByRole is resolveFlowID decided by the caller's role on the flow
// alone (minPerm), without the token-scope rule Can adds: only for the routes
// kept on their role check, where that rule would refuse a token the route
// admits.
func resolveFlowIDByRole(w http.ResponseWriter, r *http.Request, flowSvc *service.FlowService, security *SecurityConfig, flowID, minPerm string) (string, bool) {
	if !security.JWTEnabled {
		doc, ok := localDoc(w, flowSvc)
		if !ok {
			return "", false
		}
		return doc.ID, true
	}
	if err := flowSvc.CheckFlowRole(r.Context(), flowID, security.CallerID(r), minPerm); err != nil {
		render.Error(w, err, 0)
		return "", false
	}
	return flowID, true
}

// decodeOptional decodes the request body into v when one is present; an
// empty/absent body is not an error (all-zero request). Used by endpoints that
// accept optional JSON payloads (triage filters, toggle flags).
func decodeOptional(r io.Reader, v any) error {
	if r == nil {
		return nil
	}
	if err := json.NewDecoder(r).Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// resolveFlow loads a flow the caller may perform action on (AuthzService.Can
// through FlowService.GetAuthorized, which also applies the action's API-token
// scope rule). In local mode (JWTEnabled=false) it returns the currently loaded
// document. All authz logic lives in the service — do not add policy here.
func resolveFlow(w http.ResponseWriter, r *http.Request, flowSvc *service.FlowService, security *SecurityConfig, flowID string, action service.Action) (*models.FlowDocument, bool) {
	if !security.JWTEnabled {
		return localDoc(w, flowSvc)
	}
	doc, err := flowSvc.GetAuthorized(r.Context(), flowID, action)
	if err != nil {
		render.Error(w, err, 0)
		return nil, false
	}
	return doc, true
}

// resolveFlowByRole is resolveFlow decided by the caller's role on the flow
// alone (minPerm), without the token-scope rule Can adds: only for the routes
// kept on their role check, where that rule would refuse a token the route
// admits.
func resolveFlowByRole(w http.ResponseWriter, r *http.Request, flowSvc *service.FlowService, security *SecurityConfig, flowID, minPerm string) (*models.FlowDocument, bool) {
	if !security.JWTEnabled {
		return localDoc(w, flowSvc)
	}
	doc, err := flowSvc.GetAuthorizedByRole(r.Context(), flowID, security.CallerID(r), minPerm)
	if err != nil {
		render.Error(w, err, 0)
		return nil, false
	}
	return doc, true
}

// requireFlowPerm verifies the caller may perform action on the flow, writing
// the HTTP error itself: 404 for a missing flow, 403 for a refusal. Like
// resolveFlow but for handlers that don't need the document. Header-only: no
// flow content is downloaded for the check. Local mode (no JWT) always passes.
func requireFlowPerm(w http.ResponseWriter, r *http.Request, flowSvc *service.FlowService, security *SecurityConfig, flowID string, action service.Action) bool {
	if !security.JWTEnabled {
		return true
	}
	return flowAllowed(w, flowSvc.CheckFlowPermission(r.Context(), flowID, action))
}

// requireFlow is requireFlowPerm as route middleware (route.Builder.With) for a
// route whose path parameter param names the flow: the handler runs only once
// the caller may perform action on it.
func requireFlow(flowSvc *service.FlowService, security *SecurityConfig, action service.Action, param string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if requireFlowPerm(w, r, flowSvc, security, chi.URLParam(r, param), action) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

// requireLibraryFlow is requireFlow for the library's routes, where
// LibraryService decides: it checks the flow whenever a store holds it, local
// mode included, as the library handlers always have.
func requireLibraryFlow(libSvc *service.LibraryService, action service.Action, param string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if flowAllowed(w, libSvc.CheckFlowPermission(r.Context(), chi.URLParam(r, param), action)) {
				next.ServeHTTP(w, r)
			}
		})
	}
}

// flowAllowed answers a flow check's refusal (404 for a missing flow, 403
// otherwise) and reports whether there was none.
func flowAllowed(w http.ResponseWriter, err error) bool {
	if err != nil {
		render.Error(w, err, 0)
		return false
	}
	return true
}

// securityAuthz is the AuthzService for the organisation and instance decisions
// handlers make. It is built from the storage and organisation service DI builds
// the shared one from and keeps no state of its own (the request's memo rides
// the context), so it decides exactly as the shared one does.
func securityAuthz(security *SecurityConfig) *service.AuthzService {
	return service.NewAuthzService(security.Backend, security.OrgSvc)
}

// callerCan reports whether the request's actor may perform action on res: nil
// when allowed, otherwise why not (AuthzService.Can).
func callerCan(r *http.Request, authz *service.AuthzService, action service.Action, res service.Resource) error {
	return authz.Can(r.Context(), auth.ActorFromContext(r.Context()), action, res)
}

// requireInstancePerm is SecurityConfig.RequireRole decided by Can, inside a
// handler (requireInstance is the route middleware form): the caller's global
// role must allow the instance action (InstanceWrite: admin or member) and a
// scoped API token must hold its scope. A refusal is RequireRole's 403. Local
// mode (no JWT) passes, as RequireRole does.
func requireInstancePerm(w http.ResponseWriter, r *http.Request, security *SecurityConfig, action service.Action) bool {
	if !security.JWTEnabled {
		return true
	}
	if err := callerCan(r, securityAuthz(security), action, service.Instance()); err != nil {
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return false
	}
	return true
}

// requireOrgMember verifies the caller may read orgID (Can's OrgRead: a member
// of any role; a scoped API token needs read or write), writing a 403 itself
// when not. Local mode (no JWT) always passes.
// Returns 500 if OrgSvc is not wired (misconfiguration) rather than silently denying.
func requireOrgMember(w http.ResponseWriter, r *http.Request, security *SecurityConfig, orgID string) bool {
	return requireOrgAction(w, r, security, orgID, service.OrgRead, "forbidden: not a member of this organization")
}

// requireOrgAdmin verifies the caller may administer orgID (Can's OrgAdmin: an
// admin; a scoped API token needs write), writing a 403 itself when not. Local
// mode (no JWT) always passes.
func requireOrgAdmin(w http.ResponseWriter, r *http.Request, security *SecurityConfig, orgID string) bool {
	return requireOrgAction(w, r, security, orgID, service.OrgAdmin, "forbidden: organization admin required")
}

// requireOrgAction is requireOrgMember and requireOrgAdmin: every refusal,
// an unknown organisation included, is the 403 refusal they have always
// answered.
func requireOrgAction(w http.ResponseWriter, r *http.Request, security *SecurityConfig, orgID string, action service.Action, refusal string) bool {
	if !security.JWTEnabled {
		return true
	}
	if security.OrgSvc == nil {
		render.Error(w, fmt.Errorf("organization service not available"), http.StatusInternalServerError)
		return false
	}
	if err := callerCan(r, securityAuthz(security), action, service.OrgByID(orgID)); err != nil {
		render.Error(w, errors.New(refusal), http.StatusForbidden)
		return false
	}
	return true
}
