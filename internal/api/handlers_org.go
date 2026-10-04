package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"io"
	"net/http"
	"pad-analyzer/internal/notify"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

type OrgHandler struct {
	orgSvc    *collaboration.OrgService
	backend   storageif.StorageBackend
	knowledge *rag.KnowledgeService
	security  *SecurityConfig
	email     *mailer.Service
	// ruleProfiles is invalidated when org custom rules change so the next
	// analysis re-resolves; nil in local mode.
	ruleProfiles *service.RuleProfileResolver
	// providers serves org-shared credentials; routes answer
	// ORG_KEYSTORE_UNAVAILABLE without an org secret store.
	providers *service.ProviderService

	// notifyPolicy: strict whenever JWTEnabled (multi-user) so a tenant
	// cannot aim a webhook at the server's own network; desktop allows loopback.
	notifyPolicy notify.TargetPolicy
	// channelClient is the one guarded client shared by every channel test
	// (D-25): per-call clients leaked cloned transports' idle conns.
	channelClient *http.Client
}

func channelPolicy(security *SecurityConfig) notify.TargetPolicy {
	if security != nil && security.JWTEnabled {
		return notify.BlockInternalTargets
	}
	return notify.AllowLoopbackTargets
}

func NewOrgHandler(orgSvc *collaboration.OrgService, backend storageif.StorageBackend, knowledge *rag.KnowledgeService, security *SecurityConfig, email *mailer.Service, ruleProfiles *service.RuleProfileResolver, providers *service.ProviderService) *OrgHandler {
	return &OrgHandler{
		orgSvc:        orgSvc,
		backend:       backend,
		knowledge:     knowledge,
		security:      security,
		email:         email,
		ruleProfiles:  ruleProfiles,
		providers:     providers,
		notifyPolicy:  channelPolicy(security),
		channelClient: notify.NewChannelClient(channelPolicy(security)),
	}
}

func (h *OrgHandler) knowledgeBackendAvailable(w http.ResponseWriter) bool {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("knowledge base requires a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return false
	}
	return true
}

func (h *OrgHandler) orgProviderKeysAvailable(w http.ResponseWriter) bool {
	if h.providers == nil || !h.providers.OrgKeysAvailable() {
		render.ErrorWithCode(w, service.ErrOrgKeyStoreUnavailable, http.StatusNotFound, "ORG_KEYSTORE_UNAVAILABLE")
		return false
	}
	return true
}

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

type statusResponse struct {
	Status string `json:"status" example:"ok"`
}

// orgMemberMutationResponse is a membership change reply; sharedCredentialDisabled is true when the change disabled the org's shared Azure credential.
type orgMemberMutationResponse struct {
	Status                   string `json:"status" example:"ok"`
	SharedCredentialDisabled bool   `json:"sharedCredentialDisabled,omitempty"`
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

func (h *OrgHandler) memberLabel(ctx context.Context, userID string) string {
	if userID == "" || h.backend == nil {
		return userID
	}
	u, err := h.backend.LoadUserByID(ctx, userID)
	if err != nil || u == nil {
		return userID
	}
	if u.DisplayName != "" {
		return u.DisplayName
	}
	if u.Email != "" {
		return u.Email
	}
	return userID
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

const (
	lenderReasonDemoted        = "lender_demoted"
	lenderReasonRemoved        = "lender_removed"
	lenderReasonAccountDeleted = "lender_account_deleted"
)

// disableLentCredential disables the org's shared Azure credentials userID lent when userID stops
// being an admin of orgID; registry rows stay (needs_credentials). Best effort: failures are logged.
func disableLentCredential(r *http.Request, backend storageif.StorageBackend, trustedProxies []string, providers *service.ProviderService, orgID, userID, reason string) bool {
	if providers == nil || !providers.OrgKeysAvailable() {
		return false
	}
	keys, err := providers.DisableOrgAzureCredentialsSavedBy(orgID, userID)
	if err != nil {
		logger.Warn("org: disabling a departed admin's shared credentials failed", "org", orgID, "error", err)
		return false
	}
	for _, key := range keys {
		auditLentCredentialDisabled(r, backend, trustedProxies, orgID, key, userID, reason)
	}
	return len(keys) > 0
}

// disableLentCredentials is disableLentCredential for every org userID lent to (account erasure).
func disableLentCredentials(r *http.Request, backend storageif.StorageBackend, trustedProxies []string, providers *service.ProviderService, userID, reason string) {
	if providers == nil || !providers.OrgKeysAvailable() {
		return
	}
	refs, err := providers.DisableOrgAzureCredentialsLentBy(userID)
	if err != nil {
		logger.Warn("account: disabling the user's shared org credentials failed", "error", err)
	}
	for _, ref := range refs {
		auditLentCredentialDisabled(r, backend, trustedProxies, ref.OrgID, ref.Provider, userID, reason)
	}
}

// auditLentCredentialDisabled records one disabled credential; meta carries the user id because the reason names only the relationship.
func auditLentCredentialDisabled(r *http.Request, backend storageif.StorageBackend, trustedProxies []string, orgID, key, userID, reason string) {
	meta := map[string]string{"op": "disable", "provider": key, "reason": reason, "user": userID}
	if connID, ok := ai.ParseAzureConnectionKey(key); ok {
		meta["connectionId"] = connID
	}
	logAudit(r.Context(), backend, r, trustedProxies, AuditActionKeysSave, "org", orgID, meta)
}

// requireMember checks membership in the org in "id"; non-member and unknown org both get 403 (no existence oracle).
func (h *OrgHandler) requireMember(w http.ResponseWriter, r *http.Request) *storageif.Organisation {
	id := chi.URLParam(r, "id")
	org, err := h.orgSvc.GetAsMember(r.Context(), id, h.security.CallerID(r))
	if err != nil {
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return nil
	}
	return org
}

// requireAdminRole is requireAdmin needing only the decision: reads the membership row, not the org with its whole roster.
func (h *OrgHandler) requireAdminRole(w http.ResponseWriter, r *http.Request) bool {
	id := chi.URLParam(r, "id")
	role, err := h.orgSvc.MemberRole(r.Context(), id, h.security.CallerID(r))
	switch {
	case errors.Is(err, collaboration.ErrMemberNotFound):
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return false
	case err != nil:
		render.Error(w, err, http.StatusNotFound)
		return false
	case role != auth.RoleAdmin:
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return false
	}
	return true
}

func (h *OrgHandler) requireAdmin(w http.ResponseWriter, r *http.Request) *storageif.Organisation {
	id := chi.URLParam(r, "id")
	org, err := h.orgSvc.GetAndCheckAdmin(r.Context(), id, h.security.CallerID(r))
	if err != nil {
		if errors.Is(err, collaboration.ErrNotOrgAdmin) {
			render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
			return nil
		}
		render.Error(w, err, http.StatusNotFound)
		return nil
	}
	return org
}

// maxKnowledgeUploadBytes caps the decoded content (the UI advertises 1MB), enforced server-side so a client cannot reach the paid embeddings API.
const maxKnowledgeUploadBytes = 1 << 20 // 1 MiB

// maxKnowledgeUploadBodyBytes must exceed the content cap: JSON escaping inflates the body, else a tight cap rejects legitimate content before the precise 413.
const maxKnowledgeUploadBodyBytes = 4 << 20 // 4 MiB

// @Summary      List knowledge base docs
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Documents"
// @Router       /api/orgs/{id}/knowledge [get]
func (h *OrgHandler) handleKnowledgeList(w http.ResponseWriter, r *http.Request) {
	if h.knowledge == nil {
		render.Error(w, fmt.Errorf("knowledge service not configured"), http.StatusServiceUnavailable)
		return
	}
	if !h.knowledgeBackendAvailable(w) {
		return
	}
	if h.requireMember(w, r) == nil {
		return
	}
	limit, ok := clampListLimit(w, r.URL.Query().Get("limit"))
	if !ok {
		return
	}
	// Offset pages past the newest-first page so the older tail stays reachable.
	offset, ok := clampListOffset(w, r.URL.Query().Get("offset"))
	if !ok {
		return
	}
	docs, err := h.backend.ListKnowledgeDocuments(r.Context(), chi.URLParam(r, "id"), limit, offset)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, docs)
}

// @Summary      Upload knowledge doc
// @Description  Chunks and embeds the document with the caller's embedding provider. Refused with 409 EMBEDDING_MODEL_CHANGED when that provider's model is not the one the org's knowledge base was indexed with (mixing models would make documents unfindable for members on the other model).
// @Tags         org
// @Param        id path string true "Org ID"
// @Accept       multipart/form-data
// @Produce      json
// @Success      201 {object} map[string]interface{} "Uploaded"
// @Failure      400 {object} map[string]string "Embedding provider not configured (EMBEDDING_NOT_CONFIGURED; EMBEDDING_ORG_NOT_CONFIGURED when the org's shared Azure credential names no embedding deployment), rejected credentials (EMBEDDING_AUTH_FAILED, EMBEDDING_PERMISSION_DENIED) or a request the provider rejected (EMBEDDING_PROVIDER_REJECTED)"
// @Failure      409 {object} map[string]string "Embedding model differs from the indexed corpus (EMBEDDING_MODEL_CHANGED) or another upload/re-index of this org is in flight (KNOWLEDGE_BUSY)"
// @Failure      413 {object} map[string]string "The document chunks to more passages than one upload may index (KNOWLEDGE_DOCUMENT_TOO_LARGE)"
// @Failure      429 {object} map[string]string "Embedding provider throttled the request (EMBEDDING_RATE_LIMITED)"
// @Failure      503 {object} map[string]string "Embedding provider unavailable (EMBEDDING_PROVIDER_UNAVAILABLE)"
// @Router       /api/orgs/{id}/knowledge/upload [post]
func (h *OrgHandler) handleKnowledgeUpload(w http.ResponseWriter, r *http.Request) {
	if h.knowledge == nil {
		render.Error(w, fmt.Errorf("knowledge service not configured"), http.StatusServiceUnavailable)
		return
	}
	if !h.knowledgeBackendAvailable(w) {
		return
	}
	if !h.requireAdminRole(w, r) {
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxKnowledgeUploadBodyBytes)
	var req struct {
		Filename string `json:"filename"`
		Content  string `json:"content"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if len(req.Content) > maxKnowledgeUploadBytes {
		render.Error(w, fmt.Errorf("document exceeds %d byte limit", maxKnowledgeUploadBytes), http.StatusRequestEntityTooLarge)
		return
	}

	if err := h.knowledge.AddDocument(r.Context(), h.security.KeyScope(r), chi.URLParam(r, "id"), req.Filename, req.Content); err != nil {
		// Embedding-provider failures are configuration problems, not server faults: 4xx with a machine code so the message survives masking.
		if renderKnowledgeEmbeddingError(w, withRequestContextErr(r, err)) {
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKnowledgeUpload, "org", chi.URLParam(r, "id"), map[string]string{"filename": req.Filename})
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Delete knowledge doc
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        docId path string true "Document ID"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Deleted"
// @Router       /api/orgs/{id}/knowledge/{docId} [delete]
func (h *OrgHandler) handleKnowledgeDelete(w http.ResponseWriter, r *http.Request) {
	if h.knowledge == nil {
		render.Error(w, fmt.Errorf("knowledge service not configured"), http.StatusServiceUnavailable)
		return
	}
	if !h.knowledgeBackendAvailable(w) {
		return
	}
	docID := chi.URLParam(r, "docId")
	if !h.requireAdminRole(w, r) {
		return
	}
	if err := h.backend.DeleteKnowledgeDocument(r.Context(), h.security.CallerID(r), chi.URLParam(r, "id"), docID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Re-check the org's chunk count next chat turn, not the cached "non-empty".
	h.knowledge.InvalidateCorpus(chi.URLParam(r, "id"))
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKnowledgeDelete, "org", chi.URLParam(r, "id"), map[string]string{"docId": docID})
	render.JSON(w, map[string]string{"status": "ok"})
}

// renderKnowledgeEmbeddingError maps knowledge embedding failures to 4xx/5xx with a machine code the frontend keys on.
// Order matters: permission-denied also matches ErrApiKeyInvalid, and the rag.ErrEmbeddingFailed catch-all must come last.
func renderKnowledgeEmbeddingError(w http.ResponseWriter, err error) bool {
	return renderKnowledgeEmbeddingErrorDetails(w, err, nil)
}

// renderKnowledgeEmbeddingErrorDetails adds structured details: a re-index failed after committing pages reports its resume cursor.
func renderKnowledgeEmbeddingErrorDetails(w http.ResponseWriter, err error, details map[string]any) bool {
	switch {
	case errors.Is(err, service.ErrAIBudgetExceeded):
		render.ErrorWithCodeAndDetails(w, err, http.StatusTooManyRequests, "BUDGET_EXCEEDED", details)
	case errors.Is(err, service.ErrAIBudgetUnavailable):
		render.ErrorWithCodeAndDetails(w, err, http.StatusServiceUnavailable, "BUDGET_UNAVAILABLE", details)
	case errors.Is(err, ai.ErrOrgEmbeddingDeploymentMissing):
		// Before ErrEmbeddingUnavailable, which wraps it: the org's shared credential names no embedding deployment.
		render.ErrorWithCodeAndDetails(w, err, http.StatusBadRequest, "EMBEDDING_ORG_NOT_CONFIGURED", details)
	case errors.Is(err, rag.ErrEmbeddingNotConfigured), errors.Is(err, rag.ErrEmbeddingUnavailable):
		render.ErrorWithCodeAndDetails(w, err, http.StatusBadRequest, "EMBEDDING_NOT_CONFIGURED", details)
	case errors.Is(err, rag.ErrEmbeddingModelChanged):
		render.ErrorWithCodeAndDetails(w, err, http.StatusConflict, "EMBEDDING_MODEL_CHANGED", details)
	case errors.Is(err, rag.ErrKnowledgeBusy):
		render.ErrorWithCodeAndDetails(w, err, http.StatusConflict, "KNOWLEDGE_BUSY", details)
	case errors.Is(err, rag.ErrDocumentTooLarge):
		// More passages than one upload may index; the byte cap cannot track how a text chunks.
		render.ErrorWithCodeAndDetails(w, err, http.StatusRequestEntityTooLarge, "KNOWLEDGE_DOCUMENT_TOO_LARGE", details)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		render.ErrorWithCodeAndDetails(w, err, http.StatusGatewayTimeout, "KNOWLEDGE_TIMEOUT", details)
	case errors.Is(err, rag.ErrEmbeddingSetupUnavailable):
		// Which model embeds could not be decided (registry or key store unreadable, import pending): 503, masked message.
		render.ErrorWithCodeAndDetails(w, err, http.StatusServiceUnavailable, "EMBEDDING_PROVIDER_UNAVAILABLE", details)
	case errors.Is(err, ai.ErrRateLimited):
		w.Header().Set("Retry-After", embeddingRetryAfter(err))
		render.ErrorWithCodeAndDetails(w, err, http.StatusTooManyRequests, "EMBEDDING_RATE_LIMITED", details)
	case errors.Is(err, ai.ErrProviderPermissionDenied): // before ErrApiKeyInvalid, which it also matches
		render.ErrorWithCodeAndDetails(w, err, http.StatusBadRequest, "EMBEDDING_PERMISSION_DENIED", details)
	case errors.Is(err, ai.ErrApiKeyInvalid):
		render.ErrorWithCodeAndDetails(w, err, http.StatusBadRequest, "EMBEDDING_AUTH_FAILED", details)
	case errors.Is(err, ai.ErrProviderConfigInvalid), errors.Is(err, ai.ErrKeyNotConfigured):
		render.ErrorWithCodeAndDetails(w, err, http.StatusBadRequest, "EMBEDDING_NOT_CONFIGURED", details)
	case errors.Is(err, ai.ErrProviderDown), errors.Is(err, ai.ErrCircuitOpen):
		render.ErrorWithCodeAndDetails(w, err, http.StatusServiceUnavailable, "EMBEDDING_PROVIDER_UNAVAILABLE", details)
	case errors.Is(err, rag.ErrEmbeddingFailed):
		// The provider rejected the request itself: the admin's configuration — keep the message.
		render.ErrorWithCodeAndDetails(w, err, http.StatusBadRequest, "EMBEDDING_PROVIDER_REJECTED", details)
	default:
		return false
	}
	return true
}

// withRequestContextErr joins the request context's error to err when the context is done, so deadline failures map to KNOWLEDGE_TIMEOUT.
func withRequestContextErr(r *http.Request, err error) error {
	if ctxErr := r.Context().Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return errors.Join(err, ctxErr)
	}
	return err
}

// embeddingRetryAfter is the Retry-After delta-seconds: the provider's hint, else 30 s.
func embeddingRetryAfter(err error) string {
	var rl *ai.RateLimitError
	if errors.As(err, &rl) && rl.RetryAfter > 0 {
		return fmt.Sprintf("%d", int((rl.RetryAfter+time.Second-1)/time.Second))
	}
	return "30"
}

// maxKnowledgeReindexBodyBytes bounds the optional re-index body.
const maxKnowledgeReindexBodyBytes = 4 << 10

// maxReindexCursorField bounds each re-index cursor component (ids are uuids, 36 bytes).
const maxReindexCursorField = 128

// knowledgeReindexRequest is the optional re-index body. Cursor is the previous answer's nextCursor
// (opaque to clients); absent (or zero) starts from the beginning.
type knowledgeReindexRequest struct {
	Force  bool                            `json:"force,omitempty"`
	Cursor *storageif.KnowledgeChunkCursor `json:"cursor,omitempty"`
}

// decodeKnowledgeReindexRequest decodes the optional body strictly: empty is {}; unknown fields and bad cursors refused.
func decodeKnowledgeReindexRequest(body io.Reader) (knowledgeReindexRequest, error) {
	var req knowledgeReindexRequest
	if body == nil {
		return req, nil
	}
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		if errors.Is(err, io.EOF) {
			return knowledgeReindexRequest{}, nil
		}
		return req, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return req, fmt.Errorf("unexpected data after the JSON object")
	}
	if req.Cursor == nil {
		return req, nil
	}
	c := *req.Cursor
	if c.IsZero() {
		req.Cursor = nil
		return req, nil
	}
	if c.DocID == "" || c.ID == "" {
		return req, fmt.Errorf("cursor needs both docId and id")
	}
	for _, v := range []string{c.DocID, c.ID} {
		if len(v) > maxReindexCursorField || !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0 {
			return req, fmt.Errorf("invalid cursor")
		}
	}
	return req, nil
}

// @Summary      Re-index org knowledge base
// @Description  Re-embeds every knowledge chunk with the caller's embedding provider — the recovery path after switching embedding provider/model, which strands the existing corpus at the old dimension (searches silently return nothing). When the corpus was indexed with a different embedding model at the same width, the request is refused with 409 EMBEDDING_MODEL_CHANGED unless the body sets "force": true — re-indexing switches every document to the caller's model, and members whose embedding provider uses another model stop receiving the org's guidelines.
// @Tags         org
// @Accept       json
// @Param        id path string true "Org ID"
// @Param        request body knowledgeReindexRequest false "Optional. force=true re-indexes even when the corpus was indexed with a different embedding model (first request of a run only); cursor = the previous answer's nextCursor resumes the run after it"
// @Produce      json
// @Success      200 {object} rag.ReindexResult "Re-indexed this request's pages ({chunks, docs}); nextCursor (with truncated=true) means chunks remain — post again with {force, cursor: nextCursor}. Every page is committed as it completes, so a later failure keeps the earlier pages — a request that FAILS after committing pages answers its error code with details {nextCursor, chunks} to resume from"
// @Failure      400 {object} map[string]string "Embedding provider not configured (EMBEDDING_NOT_CONFIGURED; EMBEDDING_ORG_NOT_CONFIGURED when the org's shared Azure credential names no embedding deployment), rejected credentials (EMBEDDING_AUTH_FAILED, EMBEDDING_PERMISSION_DENIED), a request the provider rejected (EMBEDDING_PROVIDER_REJECTED) or malformed body / cursor"
// @Failure      409 {object} map[string]string "Embedding model differs from the indexed corpus (EMBEDDING_MODEL_CHANGED; resend with force — on a resumed request: the caller's model is not the one the run converted to) or another upload/re-index of this org is in flight (KNOWLEDGE_BUSY)"
// @Failure      429 {object} map[string]string "Embedding provider throttled the request (EMBEDDING_RATE_LIMITED) or the daily AI budget is spent (BUDGET_EXCEEDED)"
// @Failure      503 {object} map[string]string "Embedding provider unavailable (EMBEDDING_PROVIDER_UNAVAILABLE) or the budget could not be checked (BUDGET_UNAVAILABLE)"
// @Failure      504 {object} map[string]string "The request's deadline ran out before a page was committed (KNOWLEDGE_TIMEOUT)"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/orgs/{id}/knowledge/reindex [post]
func (h *OrgHandler) handleKnowledgeReindex(w http.ResponseWriter, r *http.Request) {
	if h.knowledge == nil {
		render.Error(w, fmt.Errorf("knowledge service not configured"), http.StatusServiceUnavailable)
		return
	}
	if !h.knowledgeBackendAvailable(w) {
		return
	}
	if !h.requireAdminRole(w, r) {
		return
	}
	var body io.Reader
	if r.Body != nil {
		body = http.MaxBytesReader(w, r.Body, maxKnowledgeReindexBodyBytes)
	}
	req, err := decodeKnowledgeReindexRequest(body)
	if err != nil {
		render.Error(w, fmt.Errorf("invalid re-index request: %w", err), http.StatusBadRequest)
		return
	}
	var after storageif.KnowledgeChunkCursor
	if req.Cursor != nil {
		after = *req.Cursor
	}
	// Runs outside the request-long RLS transaction (rlsExemptPath): every page commits on its own.
	res, err := h.knowledge.ReindexOrg(r.Context(), h.security.KeyScope(r), chi.URLParam(r, "id"), req.Force, after)
	if err != nil {
		werr := withRequestContextErr(r, err)
		if res.NextCursor != nil {
			// Failed after committing pages: the resume cursor rides the details so the client does not re-buy converted pages.
			details := map[string]any{"nextCursor": res.NextCursor, "chunks": res.Chunks}
			if !renderKnowledgeEmbeddingErrorDetails(w, werr, details) {
				render.ErrorWithCodeAndDetails(w, werr, http.StatusInternalServerError, "INTERNAL_ERROR", details)
			}
			logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKnowledgeReindex, "org", chi.URLParam(r, "id"),
				map[string]string{"truncated": "true", "failed": "true"})
			return
		}
		if renderKnowledgeEmbeddingError(w, werr) {
			return
		}
		render.Error(w, werr, http.StatusInternalServerError)
		return
	}
	var details map[string]string
	if req.Force || req.Cursor != nil || res.Truncated {
		details = map[string]string{}
		if req.Force {
			details["force"] = "true"
		}
		if req.Cursor != nil {
			details["resumed"] = "true"
		}
		if res.Truncated {
			details["truncated"] = "true"
		}
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKnowledgeReindex, "org", chi.URLParam(r, "id"), details)
	render.JSON(w, res)
}

// ── Org notification channels ─────────────────────────────────────────
//
// Admin-configured webhook/Teams/Slack destinations; org governance events are
// delivered there in addition to the deployment-global channels (scanner.dispatchOrgChannels).

// channelOut is the client shape — the HMAC secret never leaves the server.
type channelOut struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	URL       string    `json:"url"`
	Enabled   bool      `json:"enabled"`
	CreatedAt time.Time `json:"createdAt"`
}

// @Summary      List org notification channels
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Channels"
// @Router       /api/orgs/{id}/channels [get]
func (h *OrgHandler) handleChannelList(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.JSON(w, []channelOut{})
		return
	}
	if h.requireMember(w, r) == nil {
		return
	}
	channels, err := h.backend.ListOrgChannels(r.Context(), chi.URLParam(r, "id"), false)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	out := make([]channelOut, len(channels))
	for i, c := range channels {
		out[i] = channelOut{ID: c.ID, Name: c.Name, Kind: c.Kind, URL: c.URL, Enabled: c.Enabled, CreatedAt: c.CreatedAt}
	}
	render.JSON(w, out)
}

// @Summary      Create/update an org notification channel
// @Tags         org
// @Param        id path string true "Org ID"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Channel"
// @Router       /api/orgs/{id}/channels [post]
func (h *OrgHandler) handleChannelSave(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("org channels require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	var req struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Kind    string `json:"kind"`
		URL     string `json:"url"`
		Secret  string `json:"secret"`
		Enabled *bool  `json:"enabled"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	if !notify.ValidChannelKind(req.Kind) {
		render.Error(w, fmt.Errorf("kind must be one of webhook, teams, slack"), http.StatusBadRequest)
		return
	}
	// Fail fast on a bad URL so a typo never silently disables the channel.
	if err := notify.ValidateChannel(r.Context(), req.Kind, req.URL, h.notifyPolicy); err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	id := req.ID
	if id == "" {
		id = uuid.NewString()
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	ch := &storageif.OrgChannel{
		ID: id, OrgID: org.ID, Name: req.Name, Kind: req.Kind,
		URL: req.URL, Secret: req.Secret, Enabled: enabled,
		CreatedAt: time.Now().UTC(),
	}
	if err := h.backend.SaveOrgChannel(r.Context(), ch); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgChannelSave, "org", org.ID,
		map[string]string{"kind": req.Kind})
	render.JSON(w, channelOut{ID: ch.ID, Name: ch.Name, Kind: ch.Kind, URL: ch.URL, Enabled: ch.Enabled, CreatedAt: ch.CreatedAt})
}

// @Summary      Delete an org notification channel
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        channelId path string true "Channel ID"
// @Produce      json
// @Success      200 {object} map[string]string "Deleted"
// @Router       /api/orgs/{id}/channels/{channelId} [delete]
func (h *OrgHandler) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("org channels require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	if err := h.backend.DeleteOrgChannel(r.Context(), org.ID, chi.URLParam(r, "channelId")); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgChannelDelete, "org", org.ID,
		map[string]string{"channelId": chi.URLParam(r, "channelId")})
	render.JSON(w, map[string]string{"status": "ok"})
}

// handleChannelTest delivers a synthetic event to ONE channel so an admin
// verifies the destination; synchronous (the 4xx/5xx is the point).
// @Summary      Test an org notification channel
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        channelId path string true "Channel ID"
// @Produce      json
// @Success      200 {object} map[string]string "Delivered"
// @Router       /api/orgs/{id}/channels/{channelId}/test [post]
func (h *OrgHandler) handleChannelTest(w http.ResponseWriter, r *http.Request) {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("org channels require a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	channelID := chi.URLParam(r, "channelId")
	channels, err := h.backend.ListOrgChannels(r.Context(), org.ID, false)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	var target *storageif.OrgChannel
	for _, c := range channels {
		if c.ID == channelID {
			target = c
			break
		}
	}
	if target == nil {
		render.Error(w, fmt.Errorf("channel not found"), http.StatusNotFound)
		return
	}
	n, err := notify.BuildChannelNotifierWith(r.Context(), h.channelClient, target.Kind, target.URL, target.Secret, h.notifyPolicy)
	if err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	done := make(chan error, 1)
	// #nosec G118 -- a detached, explicitly bounded context is correct here. The
	// channel test dials an operator-supplied endpoint; binding it to r.Context()
	// would let a client abort mid-dial and leave the delivery in an unknown
	// state, and the handler below already waits on `done` with its own deadline.
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		// A panicking notifier must not take the process down: convert the panic into the reported error.
		defer func() {
			if r := recover(); r != nil {
				logger.Warn("org channel test panicked", "channel", target.Name, "kind", target.Kind, "recover", r)
				done <- fmt.Errorf("delivery panicked: %v", r)
			}
		}()
		done <- n.Notify(ctx, notify.Event{
			Type:    notify.EventTest,
			Title:   "baki channel test",
			Message: fmt.Sprintf("This is a test delivery to %q (%s). If you can read this, the channel works.", target.Name, target.Kind),
			At:      time.Now().UTC(),
		})
	}()
	if err := <-done; err != nil {
		render.Error(w, fmt.Errorf("delivery failed: %w", err), http.StatusBadGateway)
		return
	}
	render.JSON(w, map[string]string{"status": "delivered"})
}

// @Summary      List user's organizations
// @Description  Returns a list of organizations the currently authenticated user belongs to.
// @Tags         organization
// @Produce      json
// @Success      200 {object} []map[string]interface{} "OK"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/orgs [get]
func (h *OrgHandler) handleOrgList(w http.ResponseWriter, r *http.Request) {
	userID := h.security.CallerID(r)
	orgs, err := h.orgSvc.ListForUser(r.Context(), userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, orgs)
}

// @Summary      Create organization
// @Description  Creates a new organization owned by the caller, who becomes its
// @Description  first admin. Available to ANY authenticated user: org creation is
// @Description  self-serve, and the settings UI offers it to every cloud user.
// @Tags         organization
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Router       /api/orgs [post]
//
// NOTE: org creation is deliberately self-serve (any authenticated cloud user) and
// unmetered (OrgService.Create applies no quota) — a billing decision left to the product owner.
func (h *OrgHandler) handleOrgCreate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	userID := h.security.CallerID(r)
	org, err := h.orgSvc.Create(r.Context(), req.Name, userID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgCreate, "org", org.ID, map[string]string{"name": org.Name})
	render.JSON(w, org)
}

// @Summary      Update organization
// @Tags         org
// @Param        id path string true "Org ID"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Updated"
// @Router       /api/orgs/{id} [put]
func (h *OrgHandler) handleOrgUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	if !h.requireAdminRole(w, r) {
		return
	}
	// Create refuses an empty name; rename must too.
	name := strings.TrimSpace(req.Name)
	if name == "" {
		render.Error(w, fmt.Errorf("organisation name is required"), http.StatusBadRequest)
		return
	}
	updated, err := h.orgSvc.Update(r.Context(), chi.URLParam(r, "id"), name)
	if err != nil {
		render.Error(w, err, 0)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgUpdate, "org", updated.ID, map[string]string{"name": updated.Name})
	render.JSON(w, updated)
}

// @Summary      Delete organization
// @Description  Deletes an existing organization. Only organization admins can delete.
// @Tags         organization
// @Param        id path string true "id"
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/orgs/{id} [delete]
func (h *OrgHandler) handleOrgDelete(w http.ResponseWriter, r *http.Request) {
	if !h.requireAdminRole(w, r) {
		return
	}
	// The shared credential's row cascades with the org; the cached state is read now, released after the delete.
	release := func() {}
	if h.providers != nil {
		release = h.providers.PrepareOrgDeletion(chi.URLParam(r, "id"))
	}
	if err := h.orgSvc.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	release()
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgDelete, "org", chi.URLParam(r, "id"), nil)
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      List org members
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Members"
// @Router       /api/orgs/{id}/members [get]
func (h *OrgHandler) handleOrgMemberList(w http.ResponseWriter, r *http.Request) {
	// The membership check already loaded the org with its roster.
	org := h.requireMember(w, r)
	if org == nil {
		return
	}
	render.JSON(w, org.Members)
}

// @Summary      Remove organization member
// @Description  Removes a user from an organization. Only organization admins can remove members.
// @Tags         organization
// @Param        id path string true "id"
// @Param        userId path string true "userId"
// @Produce      json
// @Success      200 {object} orgMemberMutationResponse "OK; sharedCredentialDisabled is true when the removed member was the lender of the org's shared Azure AI Foundry credential and it was disabled"
// @Failure      403 {object} map[string]string "Forbidden"
// @Failure      404 {object} map[string]string "Not Found"
// @Failure      409 {object} map[string]string "Conflict"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/orgs/{id}/members/{userId} [delete]
func (h *OrgHandler) handleOrgMemberRemove(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := chi.URLParam(r, "userId")

	callerID := h.security.CallerID(r)
	_, adminErr := h.orgSvc.GetAndCheckAdmin(r.Context(), id, callerID)
	if adminErr != nil {
		if !errors.Is(adminErr, collaboration.ErrNotOrgAdmin) {
			render.Error(w, adminErr, http.StatusNotFound)
			return
		}
		if callerID != userID {
			render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
			return
		}
	}

	if err := h.orgSvc.RemoveMember(r.Context(), id, userID); err != nil {
		if errors.Is(err, collaboration.ErrLastAdmin) {
			render.Error(w, err, http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgMemberRemove, "org", id, map[string]string{"targetUser": userID})
	// A removed admin who lent the shared credential takes it along.
	disabled := disableLentCredential(r, h.backend, h.security.TrustedProxies, h.providers, id, userID, lenderReasonRemoved)
	render.JSON(w, orgMemberMutationResponse{Status: "ok", SharedCredentialDisabled: disabled})
}

// @Summary      List pending invites for an organization
// @Tags         orgs
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      401 {object} object "Unauthorized"
// @Router       /api/orgs/{id}/invites [get]
func (h *OrgHandler) handleOrgInviteList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if !h.requireAdminRole(w, r) {
		return
	}
	invites, err := h.orgSvc.ListInvites(r.Context(), id)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, invites)
}

// handleOrgInviteCreate creates a pending invite; the raw token is returned
// exactly once in this response — only its hash is persisted.
// @Summary      Create org invite
// @Description  handleOrgInviteCreate creates a pending invite for an email to join the organisation. Only org admins may create invites. The raw invite token is returned exactly once in this response — only its hash is persisted, so it cannot be recovered later. The caller is responsible for delivering it to the invitee (e.g. via an emailed accept link).
// @Tags         org
// @Param        id path string true "Org ID"
// @Accept       json
// @Produce      json
// @Success      201 {object} map[string]interface{} "Invite"
// @Router       /api/orgs/{id}/invites [post]
func (h *OrgHandler) handleOrgInviteCreate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := h.security.CallerID(r)
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}

	var req struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	email, err := validateEmail(req.Email)
	if err != nil {
		render.Error(w, err, http.StatusBadRequest)
		return
	}
	role := auth.Role(req.Role)
	if role == "" {
		role = auth.RoleMember
	}
	if !role.IsValid() {
		render.Error(w, fmt.Errorf("invalid role %q", req.Role), http.StatusBadRequest)
		return
	}

	invite, token, err := h.orgSvc.CreateInvite(r.Context(), id, email, role, userID, collaboration.DefaultInviteTTL)
	if err != nil {
		if errors.Is(err, storageif.ErrOrgInviteExists) {
			render.Error(w, err, http.StatusConflict)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgInviteCreate, "org", id, map[string]string{"email": email, "role": string(role)})

	// Best-effort invite email, registered post-commit: SendOrgInvite is a 30 s SMTP round trip;
	// running it inside the request held the RLS transaction's pooled connection for the whole dial.
	if h.email != nil {
		storagedb.RegisterPostCommit(r.Context(), func() {
			if err := h.email.SendOrgInvite(context.Background(), email, org.Name, token); err != nil {
				logger.Error("sending org invite email failed", "error", err, "org", id, "email", email)
			}
		})
	}

	render.JSON(w, map[string]any{
		"invite": invite,
		"token":  token,
	})
}

// @Summary      Revoke a pending invite
// @Tags         orgs
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      401 {object} object "Unauthorized"
// @Failure      403 {object} object "Forbidden"
// @Router       /api/orgs/{id}/invites/{inviteId} [delete]
func (h *OrgHandler) handleOrgInviteRevoke(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	inviteID := chi.URLParam(r, "inviteId")
	if !h.requireAdminRole(w, r) {
		return
	}

	if err := h.orgSvc.RevokeInvite(r.Context(), id, inviteID); err != nil {
		if errors.Is(err, collaboration.ErrInviteNotFound) {
			render.Error(w, err, http.StatusNotFound)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgInviteRevoke, "org", id, map[string]string{"inviteId": inviteID})
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Accept an invite
// @Description  handleInviteAccept redeems an invite token, adding the calling user to the invite's organisation with the role specified in the invite.
// @Tags         org
// @Param        token path string true "Invite token"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "Accepted"
// @Router       /api/invites/{token}/accept [post]
func (h *OrgHandler) handleInviteAccept(w http.ResponseWriter, r *http.Request) {
	token := chi.URLParam(r, "token")
	userID := h.security.CallerID(r)
	userEmail := ""
	if claims := auth.ClaimsFromContext(r.Context()); claims != nil {
		userEmail = claims.Email
	}
	// H12: re-verify email-verified status at accept time — the JWT proves authentication, not
	// verification; an unverified account must not accept the victim's invites.
	emailVerified := false
	if h.backend != nil && userID != "" {
		if u, err := h.backend.LoadUserByID(r.Context(), userID); err == nil && u != nil {
			emailVerified = u.EmailVerified
		}
	}

	org, err := h.orgSvc.AcceptInvite(r.Context(), token, userID, userEmail, emailVerified)
	if err != nil {
		switch {
		case errors.Is(err, collaboration.ErrInviteNotFound):
			render.Error(w, err, http.StatusNotFound)
		case errors.Is(err, collaboration.ErrInviteExpired), errors.Is(err, collaboration.ErrInviteAlreadyAccepted):
			render.Error(w, err, http.StatusGone)
		default:
			render.Error(w, err, http.StatusInternalServerError)
		}
		return
	}

	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgInviteAccept, "org", org.ID, nil)
	render.JSON(w, org)
}

// @Summary      Change member role
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        userId path string true "User ID"
// @Accept       json
// @Produce      json
// @Success      200 {object} orgMemberMutationResponse "Updated; sharedCredentialDisabled is true when the demoted member was the lender of the org's shared Azure AI Foundry credential and it was disabled"
// @Router       /api/orgs/{id}/members/{userId}/role [put]
func (h *OrgHandler) handleOrgMemberRoleUpdate(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	userID := chi.URLParam(r, "userId")

	var req struct {
		Role string `json:"role"`
	}
	if !decodeBody(w, r, &req) {
		return
	}

	if !h.requireAdminRole(w, r) {
		return
	}
	role := auth.Role(req.Role)
	if !role.IsValid() {
		render.Error(w, fmt.Errorf("invalid role %q", req.Role), http.StatusBadRequest)
		return
	}
	if err := h.orgSvc.SetRole(r.Context(), id, userID, role); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionOrgMemberRole, "org", id, map[string]string{"targetUser": userID, "role": req.Role})
	disabled := false
	if role != auth.RoleAdmin {
		// A demoted admin can no longer manage the credential they lent.
		disabled = disableLentCredential(r, h.backend, h.security.TrustedProxies, h.providers, id, userID, lenderReasonDemoted)
	}
	render.JSON(w, orgMemberMutationResponse{Status: "ok", SharedCredentialDisabled: disabled})
}
