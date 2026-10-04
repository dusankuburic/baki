package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// --- The Azure AI Foundry model registry ---
//
// Personal and org routes share sub-paths and bodies; org reads need an admin or member, writes/tests/discovery an admin (org_access.go);
// no org keystore is 404 ORG_KEYSTORE_UNAVAILABLE, no registry 503. Strict bodies, writes audited; test/discovery run outside the RLS transaction on the chat/analysis buckets.

// Body caps of the registry routes: a resource's settings (or a test's
// draft) leave room for a long Entra ID secret; a model is short fields.
const (
	azureRegistryBodyLimit = 64 << 10
	azureModelBodyLimit    = 16 << 10
)

const azureCodeRegistryUnavailable = "AZURE_REGISTRY_UNAVAILABLE"

var errAzureRegistryUnavailable = errors.New("the Azure AI Foundry model registry is not available in this deployment")

type azureRegistryCall struct {
	svc     *service.AzureModelService
	scope   service.AzureScope
	backend storageif.StorageBackend
	proxies []string
}

func azureModelsOf(p *service.ProviderService) *service.AzureModelService {
	if p == nil {
		return nil
	}
	return p.AzureModels()
}

func renderAzureRegistryUnavailable(w http.ResponseWriter) {
	render.ErrorWithCode(w, errAzureRegistryUnavailable, http.StatusServiceUnavailable, azureCodeRegistryUnavailable)
}

func (h *ProviderHandler) azureRegistry(w http.ResponseWriter, r *http.Request) (azureRegistryCall, bool) {
	svc := azureModelsOf(h.providerSvc)
	if svc == nil {
		renderAzureRegistryUnavailable(w)
		return azureRegistryCall{}, false
	}
	return azureRegistryCall{
		svc:     svc,
		scope:   service.AzureScope{Scope: h.security.KeyScope(r)},
		backend: h.backend,
		proxies: h.security.TrustedProxies,
	}, true
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

// --- Bodies and errors ---

func readAzureRegistryBody(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, error) {
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		return nil, fmt.Errorf("the request body exceeds %d KiB", limit>>10)
	}
	return data, err
}

func decodeAzureRegistryJSON(data []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.EOF) {
			return errors.New("the request body is empty")
		}
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("unexpected data after the JSON object")
	}
	return nil
}

func decodeAzureRegistryBody(w http.ResponseWriter, r *http.Request, limit int64, v any) bool {
	data, err := readAzureRegistryBody(w, r, limit)
	if err == nil {
		err = decodeAzureRegistryJSON(data, v)
	}
	if err != nil {
		renderAzureBodyError(w, err)
		return false
	}
	return true
}

// renderAzureBodyError answers 400 INVALID_CONFIG — except a non-numeric contextWindow or
// maxOutputTokens, which carries its field's code so a form routes it to that field.
func renderAzureBodyError(w http.ResponseWriter, err error) {
	code := ai.AzureCfgErrInvalidConfig
	var typeErr *json.UnmarshalTypeError
	if errors.As(err, &typeErr) {
		switch typeErr.Field {
		case "contextWindow":
			code = ai.AzureCfgErrInvalidContextWindow
		case "maxOutputTokens":
			code = ai.AzureCfgErrInvalidMaxOutput
		}
	}
	render.ErrorWithCode(w, fmt.Errorf("invalid request: %w", err), http.StatusBadRequest, code)
}

func renderAzureRegistryError(w http.ResponseWriter, err error) {
	if status, code, details, ok := service.AzureRegistryHTTPError(err); ok {
		render.ErrorWithCodeAndDetails(w, err, status, code, details)
		return
	}
	render.Error(w, err, http.StatusInternalServerError)
}

func azureQueryFlag(w http.ResponseWriter, r *http.Request, name string) (value, ok bool) {
	raw := r.URL.Query().Get(name)
	if raw == "" {
		return false, true
	}
	v, err := strconv.ParseBool(raw)
	if err != nil {
		render.Error(w, fmt.Errorf("invalid %s=%q: use true or false", name, raw), http.StatusBadRequest)
		return false, false
	}
	return v, true
}

// --- Audit ---

// audit writes one registry row: on the org in org scope, else the resource itself; meta never carries a secret.
func (c azureRegistryCall) audit(r *http.Request, action, resourceType, resourceID string, meta map[string]string) {
	if c.scope.IsOrg() {
		resourceType, resourceID = "org", c.scope.OrgID
	}
	logAudit(r.Context(), c.backend, r, c.proxies, action, resourceType, resourceID, meta)
}

func modelAuditMeta(op string, v service.AzureModelView) map[string]string {
	return map[string]string{
		"op":                   op,
		"modelId":              v.ID,
		"connectionId":         v.ConnectionID,
		"kind":                 v.Kind,
		"deployment":           v.Deployment,
		"isDefault":            strconv.FormatBool(v.IsDefault),
		"isKnowledgeEmbedding": strconv.FormatBool(v.IsKnowledgeEmbedding),
	}
}

func azureTestAuditMeta(in service.AzureTestInput, res service.AzureTestResult) map[string]string {
	meta := map[string]string{"provider": ai.AzureFoundryProviderID, "ok": strconv.FormatBool(res.OK)}
	if id := strings.TrimSpace(in.ConnectionID); id != "" {
		meta["connectionId"] = id
	}
	if in.Config != nil {
		draft := ai.AzureFoundryConfig{Auth: in.Config.Auth}
		draft.Normalize()
		meta["draft"] = "true"
		meta["authMode"] = draft.Auth.Mode
	}
	if m := in.Model; m != nil {
		if id := strings.TrimSpace(m.ID); id != "" {
			meta["modelId"] = id
		} else {
			meta["kind"] = strings.TrimSpace(m.Kind)
			meta["deployment"] = strings.TrimSpace(m.Deployment)
		}
	}
	if res.Code != "" {
		meta["code"] = res.Code
	}
	return meta
}

// --- The operations (shared by the personal and the org routes) ---

func (c azureRegistryCall) listConnections(w http.ResponseWriter, r *http.Request) {
	out, err := c.svc.ListConnections(r.Context(), c.scope)
	if err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	render.JSON(w, out)
}

func (c azureRegistryCall) createConnection(w http.ResponseWriter, r *http.Request) {
	var in service.AzureConnectionInput
	if !decodeAzureRegistryBody(w, r, azureRegistryBodyLimit, &in) {
		return
	}
	v, err := c.svc.CreateConnection(r.Context(), c.scope, in)
	if err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	c.audit(r, AuditActionAzureConnectionSave, "azure_connection", v.ID,
		map[string]string{"op": "create", "connectionId": v.ID, "authMode": v.Config.AuthMode})
	render.JSONStatus(w, http.StatusCreated, service.AzureConnectionEnvelope{Connection: v})
}

func (c azureRegistryCall) getConnection(w http.ResponseWriter, r *http.Request) {
	v, err := c.svc.GetConnection(r.Context(), c.scope, chi.URLParam(r, "connectionId"))
	if err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	render.JSON(w, service.AzureConnectionEnvelope{Connection: v})
}

func (c azureRegistryCall) updateConnection(w http.ResponseWriter, r *http.Request) {
	var in service.AzureConnectionPatch
	if !decodeAzureRegistryBody(w, r, azureRegistryBodyLimit, &in) {
		return
	}
	v, err := c.svc.UpdateConnection(r.Context(), c.scope, chi.URLParam(r, "connectionId"), in)
	if err != nil {
		if id, changed := service.AzureCredentialChanged(err); changed {
			// The new credential went in and could not be rolled back: that change stands and is audited.
			meta := map[string]string{"op": "update", "connectionId": id, "credentialChanged": "true", "outcome": "failed_after_credential_change"}
			if in.Config != nil {
				draft := ai.AzureFoundryConfig{Auth: in.Config.Auth}
				draft.Normalize()
				meta["authMode"] = draft.Auth.Mode
			}
			c.audit(r, AuditActionAzureConnectionSave, "azure_connection", id, meta)
		}
		renderAzureRegistryError(w, err)
		return
	}
	// A successful update is always audited, whatever it changed.
	meta := map[string]string{"op": "update", "connectionId": v.ID}
	if in.Name != nil {
		meta["renamed"] = "true"
	}
	if in.Config != nil {
		meta["credentialChanged"] = "true"
		meta["authMode"] = v.Config.AuthMode
	}
	if in.DismissDiscovery != nil {
		meta["dismissDiscovery"] = strconv.FormatBool(*in.DismissDiscovery)
	}
	if in.Force {
		meta["force"] = "true"
	}
	c.audit(r, AuditActionAzureConnectionSave, "azure_connection", v.ID, meta)
	render.JSON(w, service.AzureConnectionEnvelope{Connection: v})
}

func (c azureRegistryCall) deleteConnection(w http.ResponseWriter, r *http.Request) {
	cascade, ok := azureQueryFlag(w, r, "cascade")
	if !ok {
		return
	}
	force, ok := azureQueryFlag(w, r, "force")
	if !ok {
		return
	}
	id := chi.URLParam(r, "connectionId")
	res, err := c.svc.DeleteConnection(r.Context(), c.scope, id, service.AzureDeleteOptions{Cascade: cascade, Force: force})
	if err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	c.audit(r, AuditActionAzureConnectionDelete, "azure_connection", id, map[string]string{
		"op":                        "delete",
		"connectionId":              id,
		"deletedModels":             strconv.Itoa(res.DeletedModels),
		"clearedKnowledgeEmbedding": strconv.FormatBool(res.ClearedKnowledgeEmbedding),
	})
	render.JSON(w, res)
}

func (c azureRegistryCall) discover(w http.ResponseWriter, r *http.Request) {
	out, err := c.svc.Discover(r.Context(), c.scope, chi.URLParam(r, "connectionId"))
	if err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	render.JSON(w, out)
}

func (c azureRegistryCall) listModels(w http.ResponseWriter, r *http.Request) {
	out, err := c.svc.ListModels(r.Context(), c.scope)
	if err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	render.JSON(w, out)
}

func (c azureRegistryCall) createModel(w http.ResponseWriter, r *http.Request) {
	var in service.AzureModelInput
	if !decodeAzureRegistryBody(w, r, azureModelBodyLimit, &in) {
		return
	}
	v, err := c.svc.CreateModel(r.Context(), c.scope, in)
	if err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	c.audit(r, AuditActionAzureModelSave, "azure_model", v.ID, modelAuditMeta("create", v))
	render.JSONStatus(w, http.StatusCreated, service.AzureModelEnvelope{Model: v})
}

func (c azureRegistryCall) getModel(w http.ResponseWriter, r *http.Request) {
	v, err := c.svc.GetModel(r.Context(), c.scope, chi.URLParam(r, "modelId"))
	if err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	render.JSON(w, service.AzureModelEnvelope{Model: v})
}

func (c azureRegistryCall) updateModel(w http.ResponseWriter, r *http.Request) {
	var in service.AzureModelPatch
	if !decodeAzureRegistryBody(w, r, azureModelBodyLimit, &in) {
		return
	}
	v, err := c.svc.UpdateModel(r.Context(), c.scope, chi.URLParam(r, "modelId"), in)
	if err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	meta := modelAuditMeta("update", v)
	if in.Force {
		meta["force"] = "true"
	}
	c.audit(r, AuditActionAzureModelSave, "azure_model", v.ID, meta)
	render.JSON(w, service.AzureModelEnvelope{Model: v})
}

func (c azureRegistryCall) deleteModel(w http.ResponseWriter, r *http.Request) {
	force, ok := azureQueryFlag(w, r, "force")
	if !ok {
		return
	}
	id := chi.URLParam(r, "modelId")
	if err := c.svc.DeleteModel(r.Context(), c.scope, id, force); err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	meta := map[string]string{"op": "delete", "modelId": id}
	if force {
		meta["force"] = "true"
	}
	c.audit(r, AuditActionAzureModelDelete, "azure_model", id, meta)
	render.JSON(w, statusResponse{Status: "ok"})
}

// test runs one test: 200 with the outcome, whatever it is; an organisation's is audited (providers.test).
func (c azureRegistryCall) test(w http.ResponseWriter, r *http.Request, in service.AzureTestInput) {
	res, err := c.svc.Test(r.Context(), c.scope, in)
	if err != nil {
		renderAzureRegistryError(w, err)
		return
	}
	if c.scope.IsOrg() {
		logAudit(r.Context(), c.backend, r, c.proxies, AuditActionProvidersTest, "org", c.scope.OrgID, azureTestAuditMeta(in, res))
	}
	render.JSON(w, res)
}

// --- Personal routes: /api/providers/azure-foundry/… ---

// @Summary      List Azure AI Foundry resources
// @Description  The caller's Azure AI Foundry resources ("connections"; the UI calls them resources), then — read-only, marked shared with orgId and orgName — the resources of every organisation where the caller is an admin or member (admins change those through the org routes). Each carries its public settings (never a secret, never deployment fields), its status computed at read time (ready, needs_credentials, unreadable, invalid_config, with statusReason) and its last test. registryState is ok, read_only (the desktop list file was written by a newer app version: writes answer 409 REGISTRY_READ_ONLY) or recovered (a corrupt list file was set aside; recoveredCopyPath says where); limits are the per-owner caps; knowledgeEmbeddingEnabled says whether "Use for knowledge base" is offered. Never waits on the network.
// @Tags         providers
// @Produce      json
// @Success      200 {object} service.AzureConnectionsListing "OK"
// @Failure      500 {object} render.ErrorResponse "Internal Server Error"
// @Failure      503 {object} render.ErrorResponse "SECRET_STORAGE_UNAVAILABLE, or AZURE_REGISTRY_UNAVAILABLE (no registry wired)"
// @Router       /api/providers/azure-foundry/connections [get]
func (h *ProviderHandler) handleAzureConnectionList(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.azureRegistry(w, r); ok {
		c.listConnections(w, r)
	}
}

// @Summary      Add an Azure AI Foundry resource
// @Description  Validates and stores a resource: a name (1 to 64 characters; empty takes the endpoint host's first label) and its settings — the endpoint, the credential (an API key, a Microsoft Entra ID service principal or, on the desktop, the Azure CLI sign-in) and an optional Foundry project endpoint (discovery and agents). No deployment fields: models are added with POST /models. The body must be exactly one JSON object with known fields only, at most 64 KiB (400 INVALID_CONFIG otherwise). Audited (azure.connection_save, op create).
// @Tags         providers
// @Accept       json
// @Produce      json
// @Param        request body service.AzureConnectionInput true "The resource"
// @Success      201 {object} service.AzureConnectionEnvelope "Created"
// @Failure      400 {object} render.ErrorResponse "INVALID_CONFIG, INVALID_CONNECTION_NAME, INVALID_ENDPOINT, HOST_NOT_ALLOWED, INVALID_PROJECT_ENDPOINT, CONFIG_TOO_LARGE, INVALID_AUTH or MISSING_SECRET (details.field: mode, key, tenantId, clientId or clientSecret)"
// @Failure      409 {object} render.ErrorResponse "DUPLICATE_CONNECTION_NAME, TOO_MANY_CONNECTIONS (details.limit), CONFLICT or REGISTRY_READ_ONLY"
// @Failure      503 {object} render.ErrorResponse "SECRET_STORAGE_UNAVAILABLE or AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/providers/azure-foundry/connections [post]
func (h *ProviderHandler) handleAzureConnectionCreate(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.azureRegistry(w, r); ok {
		c.createConnection(w, r)
	}
}

// @Summary      Get an Azure AI Foundry resource
// @Description  One of the caller's own resources (see GET /connections).
// @Tags         providers
// @Produce      json
// @Param        connectionId path string true "Resource id: default (the migrated single-document configuration) or ten characters [a-z0-9]"
// @Success      200 {object} service.AzureConnectionEnvelope "OK"
// @Failure      404 {object} render.ErrorResponse "CONNECTION_NOT_FOUND"
// @Failure      503 {object} render.ErrorResponse "SECRET_STORAGE_UNAVAILABLE or AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/providers/azure-foundry/connections/{connectionId} [get]
func (h *ProviderHandler) handleAzureConnectionGet(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.azureRegistry(w, r); ok {
		c.getConnection(w, r)
	}
}

// @Summary      Update an Azure AI Foundry resource
// @Description  Every field optional: rename it, change its settings (an omitted secret is kept only for the same auth mode — the same principal for Entra ID — and while the endpoint and project hosts are unchanged, else MISSING_SECRET says why; the default resource keeps its legacy deployment fields unchanged for a rolled-back binary), and/or hide (dismissDiscovery true) or bring back the "Found in your project" prompt. updatedAt, when sent, is the resource's updatedAt as last read: a stale one is 409 CONFLICT. An endpoint change of the resource that holds the knowledge-base model of a non-empty corpus is 409 REINDEX_REQUIRED unless force is true. Strict body, at most 64 KiB. Audited (azure.connection_save, op update).
// @Tags         providers
// @Accept       json
// @Produce      json
// @Param        connectionId path string true "Resource id"
// @Param        request body service.AzureConnectionPatch true "The changes"
// @Success      200 {object} service.AzureConnectionEnvelope "OK"
// @Failure      400 {object} render.ErrorResponse "INVALID_CONFIG, INVALID_CONNECTION_NAME, INVALID_ENDPOINT, HOST_NOT_ALLOWED, INVALID_PROJECT_ENDPOINT, CONFIG_TOO_LARGE, INVALID_AUTH or MISSING_SECRET (details.field)"
// @Failure      404 {object} render.ErrorResponse "CONNECTION_NOT_FOUND"
// @Failure      409 {object} render.ErrorResponse "DUPLICATE_CONNECTION_NAME, CONFLICT, REINDEX_REQUIRED or REGISTRY_READ_ONLY"
// @Failure      503 {object} render.ErrorResponse "SECRET_STORAGE_UNAVAILABLE or AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/providers/azure-foundry/connections/{connectionId} [put]
func (h *ProviderHandler) handleAzureConnectionUpdate(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.azureRegistry(w, r); ok {
		c.updateConnection(w, r)
	}
}

// @Summary      Remove an Azure AI Foundry resource
// @Description  Deletes the resource and its stored credentials. A resource with models is 409 CONNECTION_IN_USE (details.modelCount) unless cascade=true, which deletes its models too; one holding the knowledge-base model of a non-empty corpus is 409 REINDEX_REQUIRED unless force=true. Nothing changes in Azure. Audited (azure.connection_delete, with the number of models deleted).
// @Tags         providers
// @Produce      json
// @Param        connectionId path string true "Resource id"
// @Param        cascade query bool false "Delete the resource's models too"
// @Param        force query bool false "Confirm the knowledge-base re-index"
// @Success      200 {object} service.AzureConnectionDeleteResult "OK"
// @Failure      400 {object} render.ErrorResponse "A flag that is not true or false"
// @Failure      404 {object} render.ErrorResponse "CONNECTION_NOT_FOUND"
// @Failure      409 {object} render.ErrorResponse "CONNECTION_IN_USE (details.modelCount), REINDEX_REQUIRED or REGISTRY_READ_ONLY"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/providers/azure-foundry/connections/{connectionId} [delete]
func (h *ProviderHandler) handleAzureConnectionDelete(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.azureRegistry(w, r); ok {
		c.deleteConnection(w, r)
	}
}

// @Summary      Discover a resource's deployments and agents
// @Description  Lists what the saved resource's Foundry project offers — its deployments, each with the kind it would be added as, and the agents baki can run — marking what the resource registers already (alreadyAdded, modelId). Needs Microsoft Entra ID (or the Azure CLI) and a project endpoint: 400 DISCOVERY_UNAVAILABLE otherwise, or when the resource's credentials cannot be used. An Azure refusal (a missing role, a wrong project, a sign-in Entra ID refuses) is a 200 with error and code. Rides the analysis rate-limit bucket.
// @Tags         providers
// @Produce      json
// @Param        connectionId path string true "Resource id"
// @Success      200 {object} service.AzureDiscoveryView "OK"
// @Failure      400 {object} render.ErrorResponse "DISCOVERY_UNAVAILABLE"
// @Failure      404 {object} render.ErrorResponse "CONNECTION_NOT_FOUND"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/providers/azure-foundry/connections/{connectionId}/discover [post]
func (h *ProviderHandler) handleAzureConnectionDiscover(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.azureRegistry(w, r); ok {
		c.discover(w, r)
	}
}

// @Summary      List Azure AI Foundry models
// @Description  Every chat, Claude, agent and embedding model of the caller's resources, then — read-only, marked shared with orgId and orgName — those of every organisation where the caller is an admin or member, in display order (purpose, then own before shared, then name), each with its status and the context window the server knows for it (reportedContextWindow). serverDefault is the model ("m/<id>") a new chat starts on when the caller saved no default of their own; omitted when there is none. Never waits on the network.
// @Tags         providers
// @Produce      json
// @Success      200 {object} service.AzureModelsListing "OK"
// @Failure      503 {object} render.ErrorResponse "SECRET_STORAGE_UNAVAILABLE or AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/providers/azure-foundry/models [get]
func (h *ProviderHandler) handleAzureModelList(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.azureRegistry(w, r); ok {
		c.listModels(w, r)
	}
}

// @Summary      Add an Azure AI Foundry model
// @Description  Registers a deployment (kind chat, claude or embedding) or a project agent (kind agent: its resource needs Entra ID and a project endpoint, else 400 KIND_REQUIRES_PROJECT) on one of the caller's resources, with an optional display name (the deployment name by default), purpose (general by default, or custom with purposeCustom), description, context window and output cap. isKnowledgeEmbedding makes an embedding model the knowledge base's (where the knowledge base embeds with Azure); on a non-empty corpus that is 409 REINDEX_REQUIRED unless force is true. isDefault is for organisations only. Strict body, at most 16 KiB. Audited (azure.model_save, op create).
// @Tags         providers
// @Accept       json
// @Produce      json
// @Param        request body service.AzureModelInput true "The model"
// @Success      201 {object} service.AzureModelEnvelope "Created"
// @Failure      400 {object} render.ErrorResponse "INVALID_CONFIG, INVALID_KIND, INVALID_DEPLOYMENT, INVALID_DISPLAY_NAME, INVALID_PURPOSE, INVALID_DESCRIPTION, INVALID_CONTEXT_WINDOW, INVALID_MAX_OUTPUT, INVALID_DESIGNATION or KIND_REQUIRES_PROJECT"
// @Failure      404 {object} render.ErrorResponse "CONNECTION_NOT_FOUND"
// @Failure      409 {object} render.ErrorResponse "DUPLICATE_MODEL (details.existingModelId), TOO_MANY_MODELS (details.limit), CONFLICT, REINDEX_REQUIRED or REGISTRY_READ_ONLY"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/providers/azure-foundry/models [post]
func (h *ProviderHandler) handleAzureModelCreate(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.azureRegistry(w, r); ok {
		c.createModel(w, r)
	}
}

// @Summary      Get an Azure AI Foundry model
// @Description  One of the caller's own models (see GET /models).
// @Tags         providers
// @Produce      json
// @Param        modelId path string true "Model id: ten characters [a-z0-9] (the id, not the m/ chat id)"
// @Success      200 {object} service.AzureModelEnvelope "OK"
// @Failure      404 {object} render.ErrorResponse "MODEL_NOT_FOUND"
// @Failure      503 {object} render.ErrorResponse "SECRET_STORAGE_UNAVAILABLE or AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/providers/azure-foundry/models/{modelId} [get]
func (h *ProviderHandler) handleAzureModelGet(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.azureRegistry(w, r); ok {
		c.getModel(w, r)
	}
}

// @Summary      Update an Azure AI Foundry model
// @Description  An omitted field keeps its value; the id is kept whatever changes — its resource, kind or deployment. contextWindow and maxOutputTokens: null clears the override. description: "" clears it. isKnowledgeEmbedding true moves the knowledge-base designation to this model (409 REINDEX_REQUIRED on a non-empty corpus unless force is true, as for changing the designated model's deployment or resource); false clears it. updatedAt, when sent, is the model's updatedAt as last read (a stale one is 409 CONFLICT). Strict body, at most 16 KiB. Audited (azure.model_save, op update).
// @Tags         providers
// @Accept       json
// @Produce      json
// @Param        modelId path string true "Model id"
// @Param        request body service.AzureModelPatch true "The changes"
// @Success      200 {object} service.AzureModelEnvelope "OK"
// @Failure      400 {object} render.ErrorResponse "INVALID_CONFIG, INVALID_KIND, INVALID_DEPLOYMENT, INVALID_DISPLAY_NAME, INVALID_PURPOSE, INVALID_DESCRIPTION, INVALID_CONTEXT_WINDOW, INVALID_MAX_OUTPUT, INVALID_DESIGNATION or KIND_REQUIRES_PROJECT"
// @Failure      404 {object} render.ErrorResponse "MODEL_NOT_FOUND or CONNECTION_NOT_FOUND"
// @Failure      409 {object} render.ErrorResponse "DUPLICATE_MODEL (details.existingModelId), CONFLICT, REINDEX_REQUIRED or REGISTRY_READ_ONLY"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/providers/azure-foundry/models/{modelId} [put]
func (h *ProviderHandler) handleAzureModelUpdate(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.azureRegistry(w, r); ok {
		c.updateModel(w, r)
	}
}

// @Summary      Remove an Azure AI Foundry model
// @Description  Removes the model from the registry (and the chat model picker); nothing changes in Azure. Removing the knowledge-base model of a non-empty corpus is 409 REINDEX_REQUIRED unless force=true. Audited (azure.model_delete).
// @Tags         providers
// @Produce      json
// @Param        modelId path string true "Model id"
// @Param        force query bool false "Confirm the knowledge-base re-index"
// @Success      200 {object} statusResponse "OK"
// @Failure      400 {object} render.ErrorResponse "A flag that is not true or false"
// @Failure      404 {object} render.ErrorResponse "MODEL_NOT_FOUND"
// @Failure      409 {object} render.ErrorResponse "REINDEX_REQUIRED or REGISTRY_READ_ONLY"
// @Failure      503 {object} render.ErrorResponse "AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/providers/azure-foundry/models/{modelId} [delete]
func (h *ProviderHandler) handleAzureModelDelete(w http.ResponseWriter, r *http.Request) {
	if c, ok := h.azureRegistry(w, r); ok {
		c.deleteModel(w, r)
	}
}

// @Summary      Test an Azure AI Foundry resource or model
// @Description  One route for a saved or draft resource and a saved or draft model: {connectionId} tests a saved resource, {config} a draft one — with connectionId too, the stored secret carries over by the same host rules as a save — and model ({id} of a saved model, or a draft {kind, deployment}) tests a model on that resource. A resource test signs in and probes the endpoint (and the project) with no model call, at no cost; a chat, Claude or embedding model test sends one tiny request Azure bills; an agent test only looks the agent up — it is never run. Drafts write nothing; a saved target records its last test. Every test outcome is a 200 {ok, latencyMs, error?, code?}; a 4xx only means an invalid request. Strict body, at most 64 KiB. Rides the chat rate-limit bucket.
// @Tags         providers
// @Accept       json
// @Produce      json
// @Param        request body service.AzureTestInput true "What to test"
// @Success      200 {object} service.AzureTestResult "The outcome"
// @Failure      400 {object} render.ErrorResponse "INVALID_CONFIG, a draft's settings code (INVALID_ENDPOINT, HOST_NOT_ALLOWED, INVALID_PROJECT_ENDPOINT, INVALID_AUTH or MISSING_SECRET with details.field), or a draft model's INVALID_KIND or INVALID_DEPLOYMENT"
// @Failure      404 {object} render.ErrorResponse "CONNECTION_NOT_FOUND or MODEL_NOT_FOUND"
// @Failure      503 {object} render.ErrorResponse "SECRET_STORAGE_UNAVAILABLE or AZURE_REGISTRY_UNAVAILABLE"
// @Router       /api/providers/azure-foundry/test [post]
func (h *ProviderHandler) handleAzureTest(w http.ResponseWriter, r *http.Request) {
	c, ok := h.azureRegistry(w, r)
	if !ok {
		return
	}
	var in service.AzureTestInput
	if !decodeAzureRegistryBody(w, r, azureRegistryBodyLimit, &in) {
		return
	}
	c.test(w, r, in)
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
