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
	// ruleProfiles is invalidated when an org's custom rules change so the next
	// analysis re-resolves instead of serving a cached profile. Nil in local
	// mode and in tests that don't exercise the rule endpoints.
	ruleProfiles *service.RuleProfileResolver
	// providers serves the organisation's shared provider credentials (G7);
	// the routes report ORG_KEYSTORE_UNAVAILABLE when it is nil or has no
	// org secret store (desktop, local mode).
	providers *service.ProviderService

	// notifyPolicy decides whether an org notification channel may point at
	// loopback. Resolved once at construction from JWTEnabled — the same "is this
	// a multi-user deployment" gate the other handlers use — because that is the
	// property that matters: the risk is a TENANT aiming a webhook at the
	// server's own network. Config validation rejects cloud mode without auth, so
	// cloud always lands on the strict policy, while a single-user desktop run
	// keeps the localhost hatch that makes local webhooks useful.
	notifyPolicy notify.TargetPolicy
	// channelClient is the one guarded HTTP client shared by every channel
	// test (D-25): building a client per call cloned a transport whose idle
	// connections nobody closed.
	channelClient *http.Client
}

// channelPolicy maps the deployment's auth posture onto the egress policy.
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

// knowledgeBackendAvailable gates the Knowledge Base routes on a real storage
// backend. In local/desktop mode the backend is nil (the feature is cloud-only),
// and the filesystem backend's knowledge methods are unreachable stubs — so
// without this guard the handlers nil-panic on h.backend. Returns 503 (mirrors
// the policy handlers' policyAvailable) rather than crashing the request.
func (h *OrgHandler) knowledgeBackendAvailable(w http.ResponseWriter) bool {
	if h.backend == nil {
		render.Error(w, fmt.Errorf("knowledge base requires a storage backend (cloud mode)"), http.StatusServiceUnavailable)
		return false
	}
	return true
}

// orgProviderKeysAvailable gates the organisation credential routes (G7) on
// an org secret store: the desktop and local mode have no organisations and
// no database keystore, so the routes answer 404 ORG_KEYSTORE_UNAVAILABLE
// there instead of nil-panicking.
func (h *OrgHandler) orgProviderKeysAvailable(w http.ResponseWriter) bool {
	if h.providers == nil || !h.providers.OrgKeysAvailable() {
		render.ErrorWithCode(w, service.ErrOrgKeyStoreUnavailable, http.StatusNotFound, "ORG_KEYSTORE_UNAVAILABLE")
		return false
	}
	return true
}

// --- Organisation-shared Azure AI Foundry credential (G7) ---
//
// Trust model. An org admin is NOT a trusted principal — anyone can create an
// organisation and be its admin — so what these routes hold is only what
// that admin chose to lend the org's members: the secret is write-only
// (never in any response, GET included), only admins write or delete it,
// every member may read its public form, only admins and members (never
// viewers, guests or outside collaborators) may run on it, and a member's
// OWN credential always wins over it for chat (ProviderFactory.ForOrg).
// Usage on the shared credential is attributed to the org (token counts; Azure
// AI Foundry is not priced by baki, so the org's daily budget neither counts
// nor gates it — the cost and the quotas are the Azure resource's); the row
// goes with the organisation, and it is disabled when the admin who supplied
// its secret (the lender, secret_set_by — an edit that carried the stored
// secret does not make its editor a lender) is demoted, removed or deletes
// their account (disableLentCredential).

// orgAzureFoundrySaveRequest is the org save body: the Azure AI Foundry
// document plus the compare-and-swap token. UpdatedAt is the updatedAt the
// caller's GET returned, sent back verbatim; omit it (or send "") only when
// that GET said configured:false.
type orgAzureFoundrySaveRequest struct {
	ai.AzureFoundryConfig
	UpdatedAt string `json:"updatedAt,omitempty"`
}

// orgAzureFoundrySaveResponse is the org save's reply: the new updatedAt is
// the token for the next save without a reload.
type orgAzureFoundrySaveResponse struct {
	Status    string `json:"status" example:"ok"`
	UpdatedAt string `json:"updatedAt"`
}

// statusResponse is the {"status":"ok"} reply of a mutation.
type statusResponse struct {
	Status string `json:"status" example:"ok"`
}

// orgMemberMutationResponse is the {"status":"ok"} reply of a membership
// change. sharedCredentialDisabled is true when removing or demoting that
// member disabled the org's shared Azure AI Foundry credential (they were
// its lender — disableLentCredential), so the panel explains why the shared
// provider vanished and offers to re-enter it instead of leaving a support
// ticket; omitted when nothing was disabled.
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
	// The label names the credential's lender — whose secret members run on
	// — not the admin who last edited a deployment name.
	cfg.UpdatedBy = h.memberLabel(r.Context(), cfg.SecretSetByID)
	render.JSON(w, cfg)
}

// memberLabel renders a user id as the name members see: the display name,
// else the email, else the id itself (a departed account).
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
	// Same strict decode as the per-user route: unknown fields and trailing
	// data are refused.
	var req orgAzureFoundrySaveRequest
	if err := decodeAzureConfigBody(w, r, &req); err != nil {
		render.ErrorWithCode(w, fmt.Errorf("invalid Azure AI Foundry configuration: %w", err), http.StatusBadRequest, ai.AzureCfgErrInvalidConfig)
		return
	}
	// Normalised before the save AND the audit row: the row must carry the
	// canonical auth mode, not a client's case/whitespace variant.
	req.Normalize()
	updatedAt, err := h.providers.SaveOrgAzureFoundryConfig(org.ID, h.security.CallerID(r), req.UpdatedAt, req.AzureFoundryConfig)
	// Never the secrets: where the org's credential now goes, whose it is,
	// and whether the stored secret was carried over (a saved document
	// always holds one, so one the request omitted was kept).
	meta := azureConfigAuditMeta(req.AzureFoundryConfig, !req.HasSecret())
	meta["provider"] = ai.AzureFoundryProviderID
	if err != nil {
		if _, changed := service.AzureCredentialChanged(err); changed {
			// The save failed after the new credential (and its lender) went
			// in, and the previous one could not be put back: the change
			// stands, so it is audited like a successful one — marked so.
			meta["outcome"] = "failed_after_credential_change"
			logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKeysSave, "org", org.ID, meta)
		}
		// A settings code (400), CONFLICT (409), and — the save now writes
		// the registry's resource "default" and its models — the registry's
		// own refusals (TOO_MANY_*, REGISTRY_READ_ONLY, …).
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

// legacyOrgAzureTest is the deprecated body-less org credential test: the
// organisation's resource "default" (else its first), probed as a resource
// when the registry is wired; the legacy ProviderTestResult answer.
func (h *OrgHandler) legacyOrgAzureTest(w http.ResponseWriter, r *http.Request, orgID string) {
	res, err := h.providers.TestOrgProviderConnection(r.Context(), h.security.KeyScope(r), orgID, ai.AzureFoundryProviderID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// A call on the org's credential: who ran it, and the outcome.
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionProvidersTest, "org", orgID,
		map[string]string{"provider": ai.AzureFoundryProviderID, "ok": testOutcome(res)})
	render.JSON(w, res)
}

// testOutcome is a connection test's audit value: "true" when it passed.
func testOutcome(res *models.ProviderTestResult) string {
	return strconv.FormatBool(res != nil && res.Ok)
}

// Reasons recorded on the audit row of a disabled shared credential.
const (
	lenderReasonDemoted        = "lender_demoted"
	lenderReasonRemoved        = "lender_removed"
	lenderReasonAccountDeleted = "lender_account_deleted"
)

// disableLentCredential disables the org's shared Azure AI Foundry
// credentials — every resource's (the key family: "azure-foundry" for the
// resource "default", "azure-foundry/<id>" for the others) — whose secret
// userID supplied, when userID just stopped being an admin of orgID (its
// lender, even after another admin edited a resource without re-entering the
// secret; that edit does not make its editor a lender), and audits each one
// (keys.save, op "disable", provider the key, like the other org credential
// rows, with the departed user's id). The registry rows stay: their
// resources read needs_credentials until an admin enters the credentials
// again. Reports whether any credential was disabled, so the membership
// handlers can tell the admin what disappeared. Best effort: the membership
// change already happened, so a failure is logged, not reported to the
// caller.
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

// disableLentCredentials is disableLentCredential for every org whose shared
// credentials userID lent (account erasure): one audit row per disabled key.
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

// auditLentCredentialDisabled records one disabled shared credential: the
// organisation, the credential's key (provider) and — when the key names a
// registry resource — its connectionId. The meta carries the departed
// user's id: the reasons name the relationship (lender_demoted /
// lender_removed / lender_account_deleted) but not WHO left, and an auditor
// otherwise has to correlate timestamps with the member-remove / role /
// account-delete rows.
func auditLentCredentialDisabled(r *http.Request, backend storageif.StorageBackend, trustedProxies []string, orgID, key, userID, reason string) {
	meta := map[string]string{"op": "disable", "provider": key, "reason": reason, "user": userID}
	if connID, ok := ai.ParseAzureConnectionKey(key); ok {
		meta["connectionId"] = connID
	}
	logAudit(r.Context(), backend, r, trustedProxies, AuditActionKeysSave, "org", orgID, meta)
}

// requireMember verifies the caller is a member of the org identified by the
// "id" URL parameter. On success it returns the org; on failure it writes the
// appropriate error response and returns nil (the caller must bail out).
func (h *OrgHandler) requireMember(w http.ResponseWriter, r *http.Request) *storageif.Organisation {
	id := chi.URLParam(r, "id")
	if !h.orgSvc.IsMember(r.Context(), id, h.security.CallerID(r)) {
		render.Error(w, fmt.Errorf("forbidden"), http.StatusForbidden)
		return nil
	}
	org, err := h.orgSvc.Get(r.Context(), id)
	if err != nil {
		render.Error(w, err, http.StatusNotFound)
		return nil
	}
	return org
}

// requireAdmin verifies the caller is an admin of the org identified by the
// "id" URL parameter. On success it returns the org; on failure it writes the
// appropriate error response and returns nil (the caller must bail out).
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

// maxKnowledgeUploadBytes caps the decoded document content. The UI advertises
// 1MB; enforce it server-side so a client can't ingest arbitrary content (which
// would be chunked and sent to the paid embeddings API).
const maxKnowledgeUploadBytes = 1 << 20 // 1 MiB

// maxKnowledgeUploadBodyBytes bounds the raw request body. It must be larger
// than maxKnowledgeUploadBytes because JSON-encoding the content escapes
// newlines/quotes/control chars, inflating the body well past the content size
// — so a tight cap would reject legitimate ~1MiB markdown before the precise
// content-length check below could return a clean 413.
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
	docs, err := h.backend.ListKnowledgeDocuments(r.Context(), chi.URLParam(r, "id"), limit)
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
	if h.requireAdmin(w, r) == nil {
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
		// Embedding-provider failures are a configuration problem, not a
		// server fault: render as 4xx with a distinct machine code so the
		// frontend can explain the root cause (and so the message survives
		// 5xx masking).
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
	if h.requireAdmin(w, r) == nil {
		return
	}
	if err := h.backend.DeleteKnowledgeDocument(r.Context(), chi.URLParam(r, "id"), docID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKnowledgeDelete, "org", chi.URLParam(r, "id"), map[string]string{"docId": docID})
	render.JSON(w, map[string]string{"status": "ok"})
}

// renderKnowledgeEmbeddingError renders the knowledge base's embedding
// failures with a distinct machine code, and reports whether it did. None of
// these is a server fault: a missing embedding provider, rejected
// credentials, a missing data-plane role, a mistyped deployment or a
// throttled resource are the admin's configuration (4xx keeps the provider's
// message, which names the cause), a provider outage or open breaker is a
// 503 (the message is masked; the code is what the frontend keys on), and a
// caller whose embedding model is not the corpus's is a 409 the frontend
// turns into an explanation (upload) or a second confirm (re-index).
//
// Order matters: the permission-denied error also unwraps to
// ai.ErrApiKeyInvalid, and the rag.ErrEmbeddingFailed catch-all must come
// after every specific cause it wraps. The request's own deadline (or the
// client going away — withRequestContextErr folds it in) is a 504
// KNOWLEDGE_TIMEOUT and comes before the provider causes: a provider call
// cut off by the deadline surfaces as a transport failure that would
// otherwise read as an outage or as the provider's rejection.
func renderKnowledgeEmbeddingError(w http.ResponseWriter, err error) bool {
	return renderKnowledgeEmbeddingErrorDetails(w, err, nil)
}

// renderKnowledgeEmbeddingErrorDetails is renderKnowledgeEmbeddingError
// with structured details carried on the error envelope: a re-index that
// failed after committing pages reports its resume cursor this way
// ({nextCursor, chunks}), so the client continues instead of restarting
// from a zero cursor (F-13).
func renderKnowledgeEmbeddingErrorDetails(w http.ResponseWriter, err error, details map[string]any) bool {
	switch {
	case errors.Is(err, service.ErrAIBudgetExceeded):
		// The daily AI budget gate (rag.BudgetChecker) refused the billed
		// embedding run before it started.
		render.ErrorWithCodeAndDetails(w, err, http.StatusTooManyRequests, "BUDGET_EXCEEDED", details)
	case errors.Is(err, service.ErrAIBudgetUnavailable):
		render.ErrorWithCodeAndDetails(w, err, http.StatusServiceUnavailable, "BUDGET_UNAVAILABLE", details)
	case errors.Is(err, ai.ErrOrgEmbeddingDeploymentMissing):
		// Before ErrEmbeddingUnavailable, which the rag embedder wraps it
		// with: the org's shared Azure credential names no embedding
		// deployment — the org admin's to fix, not the member's settings,
		// and the personal fallback was deliberately refused (F-33).
		render.ErrorWithCodeAndDetails(w, err, http.StatusBadRequest, "EMBEDDING_ORG_NOT_CONFIGURED", details)
	case errors.Is(err, rag.ErrEmbeddingNotConfigured), errors.Is(err, rag.ErrEmbeddingUnavailable):
		render.ErrorWithCodeAndDetails(w, err, http.StatusBadRequest, "EMBEDDING_NOT_CONFIGURED", details)
	case errors.Is(err, rag.ErrEmbeddingModelChanged):
		render.ErrorWithCodeAndDetails(w, err, http.StatusConflict, "EMBEDDING_MODEL_CHANGED", details)
	case errors.Is(err, rag.ErrKnowledgeBusy):
		render.ErrorWithCodeAndDetails(w, err, http.StatusConflict, "KNOWLEDGE_BUSY", details)
	case errors.Is(err, rag.ErrDocumentTooLarge):
		// More passages than one upload may index; the byte cap alone cannot
		// track how a text chunks (F-14).
		render.ErrorWithCodeAndDetails(w, err, http.StatusRequestEntityTooLarge, "KNOWLEDGE_DOCUMENT_TOO_LARGE", details)
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		render.ErrorWithCodeAndDetails(w, err, http.StatusGatewayTimeout, "KNOWLEDGE_TIMEOUT", details)
	case errors.Is(err, rag.ErrEmbeddingSetupUnavailable):
		// Which model embeds could not be decided right now (the Azure AI
		// Foundry model registry or the key store could not be read, a
		// legacy import is still pending): no configuration gap, so not
		// EMBEDDING_NOT_CONFIGURED — a 503 whose message is masked (the
		// cause, which may name database hosts and roles, was logged) under
		// the code the frontend shows its "try again" copy for.
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
		// The provider's own rejection (unknown deployment, invalid input):
		// the admin's configuration, not a server fault — keep the message.
		render.ErrorWithCodeAndDetails(w, err, http.StatusBadRequest, "EMBEDDING_PROVIDER_REJECTED", details)
	default:
		return false
	}
	return true
}

// withRequestContextErr joins the request context's error (deadline
// exceeded, canceled) to err when the context is done, so the knowledge
// ladder maps a failure the deadline caused to KNOWLEDGE_TIMEOUT even when
// the provider layer reported it as something else.
func withRequestContextErr(r *http.Request, err error) error {
	if ctxErr := r.Context().Err(); ctxErr != nil && !errors.Is(err, ctxErr) {
		return errors.Join(err, ctxErr)
	}
	return err
}

// embeddingRetryAfter is the Retry-After value (delta-seconds) for a throttled
// embedding request: the provider's own hint when it carried one, else 30 s.
func embeddingRetryAfter(err error) string {
	var rl *ai.RateLimitError
	if errors.As(err, &rl) && rl.RetryAfter > 0 {
		return fmt.Sprintf("%d", int((rl.RetryAfter+time.Second-1)/time.Second))
	}
	return "30"
}

// maxKnowledgeReindexBodyBytes bounds the optional re-index body
// ({"force":true,"cursor":{"docId":…,"id":…}}).
const maxKnowledgeReindexBodyBytes = 4 << 10

// maxReindexCursorField bounds each re-index cursor component. Document and
// chunk ids are uuids (36 bytes); the cap keeps a hand-made cursor small.
const maxReindexCursorField = 128

// knowledgeReindexRequest is the optional re-index body. Cursor is the
// previous answer's nextCursor (opaque to clients): the walk resumes after
// it. Absent (or both fields empty) starts from the beginning.
type knowledgeReindexRequest struct {
	Force  bool                            `json:"force,omitempty"`
	Cursor *storageif.KnowledgeChunkCursor `json:"cursor,omitempty"`
}

// decodeKnowledgeReindexRequest decodes the optional body strictly: an
// empty body is {} (force=false, no cursor); unknown fields, trailing data,
// a half-filled cursor, an oversized or non-printable component are
// refused.
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
	if h.requireAdmin(w, r) == nil {
		return
	}
	// Optional body: absent / empty / {} all mean force=false from the start.
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
	// The route runs outside the request-long RLS transaction
	// (rlsExemptPath): every page commits on its own.
	res, err := h.knowledge.ReindexOrg(r.Context(), h.security.KeyScope(r), chi.URLParam(r, "id"), req.Force, after)
	if err != nil {
		werr := withRequestContextErr(r, err)
		if res.NextCursor != nil {
			// The request failed after committing at least one page: the same
			// status/code mapping, plus the resume cursor in the envelope's
			// details so the client continues from the last committed chunk
			// instead of restarting and re-buying the converted pages (F-13).
			details := map[string]any{"nextCursor": res.NextCursor, "chunks": res.Chunks}
			if !renderKnowledgeEmbeddingErrorDetails(w, werr, details) {
				render.ErrorWithCodeAndDetails(w, werr, http.StatusInternalServerError, "INTERNAL_ERROR", details)
			}
			// The audit row of a partial run records that it failed midway
			// with pages committed — the facts a later reader needs.
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
	// The audit row (one per request of a resumable run) records a forced
	// switch, a resumed request and a partial walk (more pages follow), the
	// facts a later reader of the log needs.
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

// ── Org notification channels (R2-3) ─────────────────────────────────────────
//
// Org admins configure their own webhook/Teams/Slack destinations; governance
// events for the org's flows are delivered there IN ADDITION to the
// deployment-global channels (scanner.dispatchOrgChannels).

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
	// Fail fast on a bad URL (the notify builder validates HTTPS + localhost
	// carve-outs) so a typo never silently disables the channel.
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

// handleChannelTest delivers a synthetic test event to ONE channel so an
// admin verifies the destination without waiting for a real governance
// signal. Synchronous (the 4xx/5xx is the point).
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
		// A panicking notifier must not take the process down. notify.deliver
		// has this recovery, but this goroutine calls n.Notify directly so the
		// handler can surface the outcome — convert the panic into the error
		// the waiting handler reports.
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
// NOTE ON THE PREVIOUS DOCUMENTATION. This block used to read "Only available to
// system admins" and declare a 403 the handler cannot produce. No such check
// existed, and adding one would have broken the shipped product: SettingsModal
// renders OrganizationsPanel for every authenticated cloud user, and its create
// form is ungated (the isAdmin flag in that panel is ORG-scoped membership, not
// a system role). The documentation was wrong, not the code — but a comment that
// asserts an authorization boundary is worse than no comment, because a reviewer
// reads it as protection that is already in place.
//
// The self-serve model does leave an unmetered surface: creation is unlimited
// (OrgService.Create applies no quota), and each org unlocks invites,
// notification channels, custom rules and knowledge uploads — the last of which
// spends real money on embeddings. That is a quota/billing decision, not an
// authorization one, and it is deliberately left to the product owner.
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

	if h.requireAdmin(w, r) == nil {
		return
	}
	// Create refuses an empty name; rename must too, or an org can be renamed
	// into one nobody can pick out of the switcher.
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
	if h.requireAdmin(w, r) == nil {
		return
	}
	// The shared credential's row cascades with the organisation; what the
	// process keeps for it (token source, cached listings) is read now and
	// released once the delete succeeded.
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
	id := chi.URLParam(r, "id")
	if h.requireMember(w, r) == nil {
		return
	}
	members, err := h.orgSvc.ListMembers(r.Context(), id)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, members)
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
	// A removed admin who lent the org its shared credential takes it along:
	// the reply says so, so the panel explains the disappearance (F-48).
	disabled := disableLentCredential(r, h.backend, h.security.TrustedProxies, h.providers, id, userID, lenderReasonRemoved)
	render.JSON(w, orgMemberMutationResponse{Status: "ok", SharedCredentialDisabled: disabled})
}

// handleOrgInviteList returns all pending and resolved invites for an
// organisation. Only org admins may view the invite list.
// @Summary      List pending invites for an organization
// @Tags         orgs
// @Produce      json
// @Success      200 {object} object "OK"
// @Failure      401 {object} object "Unauthorized"
// @Router       /api/orgs/{id}/invites [get]
func (h *OrgHandler) handleOrgInviteList(w http.ResponseWriter, r *http.Request) {
	id := chi.URLParam(r, "id")
	if h.requireAdmin(w, r) == nil {
		return
	}
	invites, err := h.orgSvc.ListInvites(r.Context(), id)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, invites)
}

// handleOrgInviteCreate creates a pending invite for an email to join the
// organisation. Only org admins may create invites. The raw invite token is
// returned exactly once in this response — only its hash is persisted, so it
// cannot be recovered later. The caller is responsible for delivering it to
// the invitee (e.g. via an emailed accept link).
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

	// Email the invitee their link. Best-effort: the raw token is still returned
	// so an admin can share it manually if SMTP isn't configured or delivery fails.
	if h.email != nil {
		if err := h.email.SendOrgInvite(r.Context(), email, org.Name, token); err != nil {
			logger.Error("sending org invite email failed", "error", err, "org", id, "email", email)
		}
	}

	render.JSON(w, map[string]any{
		"invite": invite,
		"token":  token,
	})
}

// handleOrgInviteRevoke deletes a pending invite so it can no longer be
// accepted. Only org admins may revoke invites.
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
	if h.requireAdmin(w, r) == nil {
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

// handleInviteAccept redeems an invite token, adding the calling user to the
// invite's organisation with the role specified in the invite.
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
	// H12: re-verify email-verified status at invite-accept time, rather than
	// trusting the JWT (which has no EmailVerified claim). The JWT only proves
	// the user authenticated; it does NOT prove their email is currently
	// verified. Without this lookup, a shadow account created with
	// `victim@example.com` (never verified) could accept invites destined to
	// the victim.
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

	if h.requireAdmin(w, r) == nil {
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
		// A demoted admin can no longer manage the credential they lent;
		// the reply says when that took the shared credential with it (F-48).
		disabled = disableLentCredential(r, h.backend, h.security.TrustedProxies, h.providers, id, userID, lenderReasonDemoted)
	}
	render.JSON(w, orgMemberMutationResponse{Status: "ok", SharedCredentialDisabled: disabled})
}
