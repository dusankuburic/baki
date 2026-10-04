package api

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/service"
	"pad-core/models"
)

// --- Organisation-shared Azure AI Foundry credential ---
//
// Trust model: an org admin is NOT a trusted principal — the secret is write-only, only admins
// and members run on it, a member's own credential wins for chat, and it is disabled when its lender stops being an admin.

// orgAzureFoundrySaveRequest is the org save body: config plus the CAS updatedAt the GET returned (omit only when configured:false).
type orgAzureFoundrySaveRequest struct {
	ai.AzureFoundryConfig
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// orgAzureFoundrySaveResponse carries the new updatedAt — the CAS token for the next save.
type orgAzureFoundrySaveResponse struct {
	Status    string `json:"status" example:"ok"`
	UpdatedAt string `json:"updatedAt"`
}

// @Summary      Get the organisation's shared Azure AI Foundry configuration
// @Description  Deprecated (one release): use GET /api/orgs/{id}/providers/azure-foundry/connections and /models. Returns the org's resource "default" (the single-document configuration it migrated to) without any secret: endpoint, auth mode, whether a secret is stored, the Entra ID tenant and client IDs, the project endpoint and the deployment names — built from the registry's entries of that resource, never from the stored document's old fields — plus who shared it (updatedBy: the display name, else email, of the admin who supplied the stored secret — an edit by another admin that kept the secret does not change it) and when it was last saved (updatedAt: RFC 3339 with sub-second precision — send it back verbatim as the save's compare-and-swap token). Any member, viewers included (only admins and members run on it). {configured:false, shared:false} when the org has none; 500 ORG_CREDENTIAL_UNREADABLE when it has one that cannot be read (a deployment key rotation, a database failure).
// @Tags         org
// @Deprecated
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} service.OrgAzureFoundryConfig "OK"
// @Failure      403 {object} render.ErrorResponse "Not a member"
// @Failure      404 {object} render.ErrorResponse "Unknown org, or no org keystore in this deployment (ORG_KEYSTORE_UNAVAILABLE)"
// @Failure      500 {object} render.ErrorResponse "The stored configuration cannot be read (ORG_CREDENTIAL_UNREADABLE)"
// @Router       /api/orgs/{id}/providers/azure-foundry/config [get]
func (h *OrgHandler) handleOrgAzureFoundryConfigGet(w http.ResponseWriter, r *http.Request) {
	if !h.orgProviderKeysAvailable(w) {
		return
	}
	org := h.requireMember(w, r)
	if org == nil {
		return
	}
	cfg, err := h.providers.GetOrgAzureFoundryConfig(org.ID)
	if err != nil {
		if errors.Is(err, service.ErrOrgCredentialUnreadable) {
			render.ErrorWithCode(w, err, http.StatusInternalServerError, "ORG_CREDENTIAL_UNREADABLE")
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// The label names the credential's lender, not its last editor.
	cfg.UpdatedBy = h.memberLabel(r.Context(), cfg.SecretSetByID)
	render.JSON(w, cfg)
}

// @Summary      Save the organisation's shared Azure AI Foundry configuration
// @Description  Deprecated (one release): use the org registry routes (POST/PUT /api/orgs/{id}/providers/azure-foundry/connections, /models). Validates and stores the org's shared Azure AI Foundry endpoint, credential (API key or Microsoft Entra ID service principal), optional project endpoint and deployment names as its resource "default": the credential part is saved (the stored document's old deployment fields carried over unchanged, for a rolled-back binary) and the deployment fields are mapped onto that resource's models — missing ones added, removed ones deleted. Org admins only. An omitted secret keeps the stored one only while the endpoint host (and the project endpoint's host) is unchanged — a changed endpoint needs the secret re-entered (MISSING_SECRET); the admin who supplies the secret becomes the credential's lender (it is disabled when they stop being an admin), an edit that keeps it does not. The body must be exactly one JSON object with known fields only (INVALID_CONFIG otherwise). Compare-and-swap: send back the updatedAt the GET returned (omit it only when the GET said configured:false); when the stored configuration changed since — another admin saved or removed it — nothing is written and the answer is 409 CONFLICT (reload, then re-apply). The reply's updatedAt is the token for the next save.
// @Tags         org
// @Deprecated
// @Param        id path string true "Org ID"
// @Param        request body orgAzureFoundrySaveRequest true "The configuration plus the updatedAt it was loaded with"
// @Accept       json
// @Produce      json
// @Success      200 {object} orgAzureFoundrySaveResponse "OK"
// @Failure      400 {object} render.ErrorResponse "INVALID_CONFIG, INVALID_ENDPOINT, INVALID_PROJECT_ENDPOINT, INVALID_AUTH, MISSING_SECRET, MISSING_DEPLOYMENT, INVALID_DEPLOYMENT, CONFIG_TOO_LARGE or HOST_NOT_ALLOWED"
// @Failure      403 {object} render.ErrorResponse "Not an org admin"
// @Failure      404 {object} render.ErrorResponse "Unknown org, or no org keystore in this deployment (ORG_KEYSTORE_UNAVAILABLE)"
// @Failure      409 {object} render.ErrorResponse "The stored configuration changed since it was loaded (CONFLICT), or the organisation's resource or model limit (TOO_MANY_CONNECTIONS, TOO_MANY_MODELS)"
// @Router       /api/orgs/{id}/providers/azure-foundry/config [post]
func (h *OrgHandler) handleOrgAzureFoundryConfigSave(w http.ResponseWriter, r *http.Request) {
	if !h.orgProviderKeysAvailable(w) {
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	// Same strict decode as the per-user route.
	var req orgAzureFoundrySaveRequest
	if err := decodeAzureConfigBody(w, r, &req); err != nil {
		render.ErrorWithCode(w, fmt.Errorf("invalid Azure AI Foundry configuration: %w", err), http.StatusBadRequest, ai.AzureCfgErrInvalidConfig)
		return
	}
	// Normalised before the save and audit row so the row carries the canonical auth mode.
	req.Normalize()
	updatedAt, err := h.providers.SaveOrgAzureFoundryConfig(org.ID, h.security.CallerID(r), req.UpdatedAt, req.AzureFoundryConfig)
	// Audit carries no secrets: only where the credential goes, whose it is, and whether the stored one carried over.
	meta := azureConfigAuditMeta(req.AzureFoundryConfig, !req.HasSecret())
	meta["provider"] = ai.AzureFoundryProviderID
	if err != nil {
		if _, changed := service.AzureCredentialChanged(err); changed {
			// The credential went in and could not be rolled back: the change stands, audited as failed_after_credential_change.
			meta["outcome"] = "failed_after_credential_change"
			logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKeysSave, "org", org.ID, meta)
		}
		renderAzureRegistryError(w, err)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKeysSave, "org", org.ID, meta)
	render.JSON(w, orgAzureFoundrySaveResponse{Status: "ok", UpdatedAt: updatedAt})
}

// @Summary      Delete the organisation's shared Azure AI Foundry configuration
// @Description  Deprecated (one release): use DELETE /api/orgs/{id}/providers/azure-foundry/connections/{connectionId}. Removes the org's resource "default" — the migrated single-document configuration — with its models and its stored credential (the organisation's other resources stay). Members without models of their own lose the ones it held. Org admins only.
// @Tags         org
// @Deprecated
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} statusResponse "OK"
// @Failure      403 {object} render.ErrorResponse "Not an org admin"
// @Failure      404 {object} render.ErrorResponse "Unknown org, or no org keystore in this deployment (ORG_KEYSTORE_UNAVAILABLE)"
// @Router       /api/orgs/{id}/providers/azure-foundry/config [delete]
func (h *OrgHandler) handleOrgAzureFoundryConfigDelete(w http.ResponseWriter, r *http.Request) {
	if !h.orgProviderKeysAvailable(w) {
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	if err := h.providers.DeleteOrgAzureFoundryConfig(org.ID); err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKeysSave, "org", org.ID,
		map[string]string{"op": "delete", "provider": ai.AzureFoundryProviderID})
	render.JSON(w, statusResponse{Status: "ok"})
}

// @Summary      Test one of the organisation's Azure AI Foundry resources or models
// @Description  POST /api/providers/azure-foundry/test for the organisation's registry, on the organisation's credentials only (never the caller's own): {connectionId}, {config} (a draft; with connectionId the stored secret carries over), optional model ({id} or a draft {kind, deployment}) — every outcome a 200 service.AzureTestResult {ok, latencyMs, error?, code?}, a 4xx only for an invalid request. A resource test makes no model call; a model test one tiny request Azure bills to the organisation's resource (baki's daily budget does not count Azure usage); an agent test only looks the agent up. Deprecated form (one release): an EMPTY body tests the organisation's resource "default" (else its first) as a resource — an authenticated probe, no model call — and answers the legacy models.ProviderTestResult {ok, latencyMs, error?} (no code). Org admins only. Rides the chat rate-limit bucket; audited (providers.test).
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        request body service.AzureTestInput false "What to test (empty: the deprecated org credential test)"
// @Accept       json
// @Produce      json
// @Success      200 {object} service.AzureTestResult "The outcome (the deprecated empty-body form answers models.ProviderTestResult)"
// @Failure      400 {object} render.ErrorResponse "INVALID_CONFIG, a draft's settings code (INVALID_ENDPOINT, HOST_NOT_ALLOWED, INVALID_PROJECT_ENDPOINT, INVALID_AUTH or MISSING_SECRET with details.field), or a draft model's INVALID_KIND or INVALID_DEPLOYMENT"
// @Failure      403 {object} render.ErrorResponse "Not an org admin"
// @Failure      404 {object} render.ErrorResponse "Unknown org, CONNECTION_NOT_FOUND, MODEL_NOT_FOUND, or no org keystore in this deployment (ORG_KEYSTORE_UNAVAILABLE)"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE (a registry test on a process that wired no registry)"
// @Router       /api/orgs/{id}/providers/azure-foundry/test [post]
func (h *OrgHandler) handleOrgAzureFoundryTest(w http.ResponseWriter, r *http.Request) {
	if !h.orgProviderKeysAvailable(w) {
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	body, err := readAzureRegistryBody(w, r, azureRegistryBodyLimit)
	if err != nil {
		renderAzureBodyError(w, err)
		return
	}
	if len(bytes.TrimSpace(body)) == 0 {
		h.legacyOrgAzureTest(w, r, org.ID)
		return
	}
	svc := azureModelsOf(h.providers)
	if svc == nil {
		renderAzureRegistryUnavailable(w)
		return
	}
	if !svc.OrgRegistriesAvailable() {
		render.ErrorWithCode(w, service.ErrOrgKeyStoreUnavailable, http.StatusNotFound, service.AzureCodeOrgKeystoreUnavailable)
		return
	}
	var in service.AzureTestInput
	if err := decodeAzureRegistryJSON(body, &in); err != nil {
		renderAzureBodyError(w, err)
		return
	}
	call := azureRegistryCall{
		svc:     svc,
		scope:   service.AzureScope{Scope: h.security.KeyScope(r), OrgID: org.ID},
		backend: h.backend,
		proxies: h.security.TrustedProxies,
	}
	call.test(w, r, in)
}

// legacyOrgAzureTest is the deprecated body-less form: resource "default" (else the first) probed, legacy ProviderTestResult.
func (h *OrgHandler) legacyOrgAzureTest(w http.ResponseWriter, r *http.Request, orgID string) {
	res, err := h.providers.TestOrgProviderConnection(r.Context(), h.security.KeyScope(r), orgID, ai.AzureFoundryProviderID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionProvidersTest, "org", orgID,
		map[string]string{"provider": ai.AzureFoundryProviderID, "ok": testOutcome(res)})
	render.JSON(w, res)
}

func testOutcome(res *models.ProviderTestResult) string {
	return strconv.FormatBool(res != nil && res.Ok)
}

func (h *OrgHandler) orgAzureRegistry(w http.ResponseWriter, r *http.Request, access orgAccess) (azureRegistryCall, bool) {
	svc := azureModelsOf(h.providers)
	if svc == nil {
		renderAzureRegistryUnavailable(w)
		return azureRegistryCall{}, false
	}
	if !svc.OrgRegistriesAvailable() {
		render.ErrorWithCode(w, service.ErrOrgKeyStoreUnavailable, http.StatusNotFound, service.AzureCodeOrgKeystoreUnavailable)
		return azureRegistryCall{}, false
	}
	orgID, ok := h.requireOrgAccess(w, r, access)
	if !ok {
		return azureRegistryCall{}, false
	}
	return azureRegistryCall{
		svc:     svc,
		scope:   service.AzureScope{Scope: h.security.KeyScope(r), OrgID: orgID},
		backend: h.backend,
		proxies: h.security.TrustedProxies,
	}, true
}

// --- Organisation routes: /api/orgs/{id}/providers/azure-foundry/… ---
//
// The org's curated registry. POST …/test is handleOrgAzureFoundryTest,
// which also serves the deprecated body-less org credential test.

// @Summary      List the organisation's Azure AI Foundry resources
// @Description  The organisation's resources (all marked shared), as GET /api/providers/azure-foundry/connections lists a caller's own — plus, for each, sharedBy (who lent its credential) and credentialUpdatedAt (the compare-and-swap token of a credential change). Org admins and members; 403 for viewers, guests and non-members.
// @Tags         org
// @Produce      json
// @Param        id path string true "Org ID"
// @Success      200 {object} service.AzureConnectionsListing "OK"
// @Failure      403 {object} render.ErrorResponse "Not an admin or member of the org"
// @Failure      404 {object} render.ErrorResponse "No org keystore in this deployment (ORG_KEYSTORE_UNAVAILABLE)"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/orgs/{id}/providers/azure-foundry/connections [get]
func (h *OrgHandler) handleOrgAzureConnectionList(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.orgAzureRegistry(w, r, orgAccessSharedRead); ok {
		c.listConnections(w, r)
	}
}

// @Summary      Add an Azure AI Foundry resource to the organisation
// @Description  POST /api/providers/azure-foundry/connections for the organisation. Org admins only. The admin who supplies a resource's secret becomes its lender: the credential is disabled when they stop being an admin. The Azure CLI sign-in cannot be shared (400 INVALID_AUTH, details.field mode). Audited (azure.connection_save on the org).
// @Tags         org
// @Accept       json
// @Produce      json
// @Param        id path string true "Org ID"
// @Param        request body service.AzureConnectionInput true "The resource"
// @Success      201 {object} service.AzureConnectionEnvelope "Created"
// @Failure      400 {object} render.ErrorResponse "INVALID_CONFIG, INVALID_CONNECTION_NAME, INVALID_ENDPOINT, HOST_NOT_ALLOWED, INVALID_PROJECT_ENDPOINT, CONFIG_TOO_LARGE, INVALID_AUTH or MISSING_SECRET (details.field)"
// @Failure      403 {object} render.ErrorResponse "Not an org admin"
// @Failure      404 {object} render.ErrorResponse "Unknown org, or ORG_KEYSTORE_UNAVAILABLE"
// @Failure      409 {object} render.ErrorResponse "DUPLICATE_CONNECTION_NAME, TOO_MANY_CONNECTIONS (details.limit) or CONFLICT"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/orgs/{id}/providers/azure-foundry/connections [post]
func (h *OrgHandler) handleOrgAzureConnectionCreate(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.orgAzureRegistry(w, r, orgAccessAdmin); ok {
		c.createConnection(w, r)
	}
}

// @Summary      Get one of the organisation's Azure AI Foundry resources
// @Description  Org admins and members.
// @Tags         org
// @Produce      json
// @Param        id path string true "Org ID"
// @Param        connectionId path string true "Resource id"
// @Success      200 {object} service.AzureConnectionEnvelope "OK"
// @Failure      403 {object} render.ErrorResponse "Not an admin or member of the org"
// @Failure      404 {object} render.ErrorResponse "CONNECTION_NOT_FOUND or ORG_KEYSTORE_UNAVAILABLE"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/orgs/{id}/providers/azure-foundry/connections/{connectionId} [get]
func (h *OrgHandler) handleOrgAzureConnectionGet(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.orgAzureRegistry(w, r, orgAccessSharedRead); ok {
		c.getConnection(w, r)
	}
}

// @Summary      Update one of the organisation's Azure AI Foundry resources
// @Description  PUT /api/providers/azure-foundry/connections/{connectionId} for the organisation. Org admins only. A credential change sends credentialUpdatedAt, the token the resource was read with: a stale one (another admin saved in between) is 409 CONFLICT. Audited (azure.connection_save on the org).
// @Tags         org
// @Accept       json
// @Produce      json
// @Param        id path string true "Org ID"
// @Param        connectionId path string true "Resource id"
// @Param        request body service.AzureConnectionPatch true "The changes"
// @Success      200 {object} service.AzureConnectionEnvelope "OK"
// @Failure      400 {object} render.ErrorResponse "INVALID_CONFIG, INVALID_CONNECTION_NAME, INVALID_ENDPOINT, HOST_NOT_ALLOWED, INVALID_PROJECT_ENDPOINT, CONFIG_TOO_LARGE, INVALID_AUTH or MISSING_SECRET (details.field)"
// @Failure      403 {object} render.ErrorResponse "Not an org admin"
// @Failure      404 {object} render.ErrorResponse "Unknown org, CONNECTION_NOT_FOUND or ORG_KEYSTORE_UNAVAILABLE"
// @Failure      409 {object} render.ErrorResponse "DUPLICATE_CONNECTION_NAME, CONFLICT or REINDEX_REQUIRED"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/orgs/{id}/providers/azure-foundry/connections/{connectionId} [put]
func (h *OrgHandler) handleOrgAzureConnectionUpdate(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.orgAzureRegistry(w, r, orgAccessAdmin); ok {
		c.updateConnection(w, r)
	}
}

// @Summary      Remove one of the organisation's Azure AI Foundry resources
// @Description  DELETE /api/providers/azure-foundry/connections/{connectionId} for the organisation. Org admins only. Audited (azure.connection_delete on the org).
// @Tags         org
// @Produce      json
// @Param        id path string true "Org ID"
// @Param        connectionId path string true "Resource id"
// @Param        cascade query bool false "Delete the resource's models too"
// @Param        force query bool false "Confirm the knowledge-base re-index"
// @Success      200 {object} service.AzureConnectionDeleteResult "OK"
// @Failure      400 {object} render.ErrorResponse "A flag that is not true or false"
// @Failure      403 {object} render.ErrorResponse "Not an org admin"
// @Failure      404 {object} render.ErrorResponse "Unknown org, CONNECTION_NOT_FOUND or ORG_KEYSTORE_UNAVAILABLE"
// @Failure      409 {object} render.ErrorResponse "CONNECTION_IN_USE (details.modelCount) or REINDEX_REQUIRED"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/orgs/{id}/providers/azure-foundry/connections/{connectionId} [delete]
func (h *OrgHandler) handleOrgAzureConnectionDelete(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.orgAzureRegistry(w, r, orgAccessAdmin); ok {
		c.deleteConnection(w, r)
	}
}

// @Summary      Discover the deployments and agents of an organisation resource
// @Description  POST /api/providers/azure-foundry/connections/{connectionId}/discover for the organisation. Org admins only.
// @Tags         org
// @Produce      json
// @Param        id path string true "Org ID"
// @Param        connectionId path string true "Resource id"
// @Success      200 {object} service.AzureDiscoveryView "OK"
// @Failure      400 {object} render.ErrorResponse "DISCOVERY_UNAVAILABLE"
// @Failure      403 {object} render.ErrorResponse "Not an org admin"
// @Failure      404 {object} render.ErrorResponse "Unknown org, CONNECTION_NOT_FOUND or ORG_KEYSTORE_UNAVAILABLE"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/orgs/{id}/providers/azure-foundry/connections/{connectionId}/discover [post]
func (h *OrgHandler) handleOrgAzureConnectionDiscover(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.orgAzureRegistry(w, r, orgAccessAdmin); ok {
		c.discover(w, r)
	}
}

// @Summary      List the organisation's Azure AI Foundry models
// @Description  The organisation's models (all marked shared; isDefault marks its "Default for members"), as GET /api/providers/azure-foundry/models lists a caller's own; no serverDefault. Org admins and members; 403 for viewers, guests and non-members.
// @Tags         org
// @Produce      json
// @Param        id path string true "Org ID"
// @Success      200 {object} service.AzureModelsListing "OK"
// @Failure      403 {object} render.ErrorResponse "Not an admin or member of the org"
// @Failure      404 {object} render.ErrorResponse "ORG_KEYSTORE_UNAVAILABLE"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/orgs/{id}/providers/azure-foundry/models [get]
func (h *OrgHandler) handleOrgAzureModelList(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.orgAzureRegistry(w, r, orgAccessSharedRead); ok {
		c.listModels(w, r)
	}
}

// @Summary      Add a model to the organisation
// @Description  POST /api/providers/azure-foundry/models for the organisation. Org admins only. isDefault makes a chat, Claude or agent model the organisation's "Default for members". Audited (azure.model_save on the org).
// @Tags         org
// @Accept       json
// @Produce      json
// @Param        id path string true "Org ID"
// @Param        request body service.AzureModelInput true "The model"
// @Success      201 {object} service.AzureModelEnvelope "Created"
// @Failure      400 {object} render.ErrorResponse "INVALID_CONFIG, INVALID_KIND, INVALID_DEPLOYMENT, INVALID_DISPLAY_NAME, INVALID_PURPOSE, INVALID_DESCRIPTION, INVALID_CONTEXT_WINDOW, INVALID_MAX_OUTPUT, INVALID_DESIGNATION or KIND_REQUIRES_PROJECT"
// @Failure      403 {object} render.ErrorResponse "Not an org admin"
// @Failure      404 {object} render.ErrorResponse "Unknown org, CONNECTION_NOT_FOUND or ORG_KEYSTORE_UNAVAILABLE"
// @Failure      409 {object} render.ErrorResponse "DUPLICATE_MODEL (details.existingModelId), TOO_MANY_MODELS (details.limit), CONFLICT or REINDEX_REQUIRED"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/orgs/{id}/providers/azure-foundry/models [post]
func (h *OrgHandler) handleOrgAzureModelCreate(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.orgAzureRegistry(w, r, orgAccessAdmin); ok {
		c.createModel(w, r)
	}
}

// @Summary      Get one of the organisation's models
// @Description  Org admins and members.
// @Tags         org
// @Produce      json
// @Param        id path string true "Org ID"
// @Param        modelId path string true "Model id"
// @Success      200 {object} service.AzureModelEnvelope "OK"
// @Failure      403 {object} render.ErrorResponse "Not an admin or member of the org"
// @Failure      404 {object} render.ErrorResponse "MODEL_NOT_FOUND or ORG_KEYSTORE_UNAVAILABLE"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/orgs/{id}/providers/azure-foundry/models/{modelId} [get]
func (h *OrgHandler) handleOrgAzureModelGet(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.orgAzureRegistry(w, r, orgAccessSharedRead); ok {
		c.getModel(w, r)
	}
}

// @Summary      Update one of the organisation's models
// @Description  PUT /api/providers/azure-foundry/models/{modelId} for the organisation; isDefault true makes it the "Default for members" (false clears it). Org admins only. Audited (azure.model_save on the org).
// @Tags         org
// @Accept       json
// @Produce      json
// @Param        id path string true "Org ID"
// @Param        modelId path string true "Model id"
// @Param        request body service.AzureModelPatch true "The changes"
// @Success      200 {object} service.AzureModelEnvelope "OK"
// @Failure      400 {object} render.ErrorResponse "INVALID_CONFIG, INVALID_KIND, INVALID_DEPLOYMENT, INVALID_DISPLAY_NAME, INVALID_PURPOSE, INVALID_DESCRIPTION, INVALID_CONTEXT_WINDOW, INVALID_MAX_OUTPUT, INVALID_DESIGNATION or KIND_REQUIRES_PROJECT"
// @Failure      403 {object} render.ErrorResponse "Not an org admin"
// @Failure      404 {object} render.ErrorResponse "Unknown org, MODEL_NOT_FOUND, CONNECTION_NOT_FOUND or ORG_KEYSTORE_UNAVAILABLE"
// @Failure      409 {object} render.ErrorResponse "DUPLICATE_MODEL (details.existingModelId), CONFLICT or REINDEX_REQUIRED"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/orgs/{id}/providers/azure-foundry/models/{modelId} [put]
func (h *OrgHandler) handleOrgAzureModelUpdate(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.orgAzureRegistry(w, r, orgAccessAdmin); ok {
		c.updateModel(w, r)
	}
}

// @Summary      Remove one of the organisation's models
// @Description  DELETE /api/providers/azure-foundry/models/{modelId} for the organisation. Org admins only. Audited (azure.model_delete on the org).
// @Tags         org
// @Produce      json
// @Param        id path string true "Org ID"
// @Param        modelId path string true "Model id"
// @Param        force query bool false "Confirm the knowledge-base re-index"
// @Success      200 {object} statusResponse "OK"
// @Failure      400 {object} render.ErrorResponse "A flag that is not true or false"
// @Failure      403 {object} render.ErrorResponse "Not an org admin"
// @Failure      404 {object} render.ErrorResponse "Unknown org, MODEL_NOT_FOUND or ORG_KEYSTORE_UNAVAILABLE"
// @Failure      409 {object} render.ErrorResponse "REINDEX_REQUIRED"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/orgs/{id}/providers/azure-foundry/models/{modelId} [delete]
func (h *OrgHandler) handleOrgAzureModelDelete(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.orgAzureRegistry(w, r, orgAccessAdmin); ok {
		c.deleteModel(w, r)
	}
}

// registerOrgAzureRoutes wires the organisation-shared Azure AI Foundry
// sub-routes - same sub-paths as the caller's own registry. Read: admin or
// member; write/delete/test/discover: admin. /config is the deprecated
// single-document form; /test also serves its body-less org test.
func (h *OrgHandler) registerOrgAzureRoutes(r chi.Router) {
	r.Route("/providers/azure-foundry", func(r chi.Router) {
		r.Get("/config", h.handleOrgAzureFoundryConfigGet)
		r.Post("/config", h.handleOrgAzureFoundryConfigSave)
		r.Delete("/config", h.handleOrgAzureFoundryConfigDelete)
		r.Post("/test", h.handleOrgAzureFoundryTest)
		r.Get("/connections", h.handleOrgAzureConnectionList)
		r.Post("/connections", h.handleOrgAzureConnectionCreate)
		r.Get("/connections/{connectionId}", h.handleOrgAzureConnectionGet)
		r.Put("/connections/{connectionId}", h.handleOrgAzureConnectionUpdate)
		r.Delete("/connections/{connectionId}", h.handleOrgAzureConnectionDelete)
		r.Post("/connections/{connectionId}/discover", h.handleOrgAzureConnectionDiscover)
		r.Get("/models", h.handleOrgAzureModelList)
		r.Post("/models", h.handleOrgAzureModelCreate)
		r.Get("/models/{modelId}", h.handleOrgAzureModelGet)
		r.Put("/models/{modelId}", h.handleOrgAzureModelUpdate)
		r.Delete("/models/{modelId}", h.handleOrgAzureModelDelete)
	})
}
