package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"pad-analyzer/internal/notify"
	"strings"
	"time"

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
// every member may read its public form, and a member's OWN credential
// always wins over it for chat (ProviderFactory.ForOrg). Usage on the shared
// credential is attributed to the org and capped by the org's daily budget
// like every org-scoped turn; the row goes with the organisation.

// @Summary      Get the organisation's shared Azure AI Foundry configuration
// @Description  Returns the org's shared Azure AI Foundry settings (endpoint, auth mode, project endpoint, deployments) without any secret, with who shared it and when. Any member. {configured:false} when the org has none.
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      403 {object} map[string]string "Not a member"
// @Failure      404 {object} map[string]string "Unknown org, or no org keystore in this deployment (ORG_KEYSTORE_UNAVAILABLE)"
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
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	cfg.UpdatedBy = h.memberLabel(r.Context(), cfg.UpdatedByID)
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
// @Description  Validates and stores the org's shared Azure AI Foundry endpoint, credential (API key or Microsoft Entra ID service principal), optional project endpoint and deployment names, for members without a configuration of their own. Org admins only. An omitted secret keeps the stored one.
// @Tags         org
// @Param        id path string true "Org ID"
// @Param        request body object true "request"
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      400 {object} map[string]string "INVALID_CONFIG, INVALID_PROJECT_ENDPOINT, MISSING_DEPLOYMENT or HOST_NOT_ALLOWED"
// @Failure      403 {object} map[string]string "Not an org admin"
// @Failure      404 {object} map[string]string "Unknown org, or no org keystore in this deployment (ORG_KEYSTORE_UNAVAILABLE)"
// @Router       /api/orgs/{id}/providers/azure-foundry/config [post]
func (h *OrgHandler) handleOrgAzureFoundryConfigSave(w http.ResponseWriter, r *http.Request) {
	if !h.orgProviderKeysAvailable(w) {
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	// Same strict decode as the per-user route: the stored document is
	// parsed with DisallowUnknownFields, so a misspelled field must fail
	// here rather than vanish. 64 KiB is far above the 2 KiB document cap.
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
	dec.DisallowUnknownFields()
	var req ai.AzureFoundryConfig
	if err := dec.Decode(&req); err != nil {
		render.ErrorWithCode(w, fmt.Errorf("invalid Azure AI Foundry configuration: %w", err), http.StatusBadRequest, ai.AzureCfgErrInvalidConfig)
		return
	}
	// Normalised before the save AND the audit row: the row must carry the
	// canonical auth mode, not a client's case/whitespace variant.
	req.Normalize()
	if err := h.providers.SaveOrgAzureFoundryConfig(org.ID, h.security.CallerID(r), req); err != nil {
		var cfgErr *ai.AzureConfigError
		if errors.As(err, &cfgErr) {
			render.ErrorWithCode(w, err, http.StatusBadRequest, cfgErr.Code)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Never the secrets — only that the org's provider changed and how it
	// authenticates.
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKeysSave, "org", org.ID,
		map[string]string{"op": "save", "provider": ai.AzureFoundryProviderID, "authMode": req.Auth.Mode})
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Delete the organisation's shared Azure AI Foundry configuration
// @Description  Removes the org's shared Azure AI Foundry credential. Members without a configuration of their own lose the provider. Org admins only.
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} map[string]string "OK"
// @Failure      403 {object} map[string]string "Not an org admin"
// @Failure      404 {object} map[string]string "Unknown org, or no org keystore in this deployment (ORG_KEYSTORE_UNAVAILABLE)"
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
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	logAudit(r.Context(), h.backend, r, h.security.TrustedProxies, AuditActionKeysSave, "org", org.ID,
		map[string]string{"op": "delete", "provider": ai.AzureFoundryProviderID})
	render.JSON(w, map[string]string{"status": "ok"})
}

// @Summary      Test the organisation's shared Azure AI Foundry credential
// @Description  Sends a tiny chat request through the org's shared Azure AI Foundry credential (never the caller's own) and reports whether it works. Org admins only.
// @Tags         org
// @Param        id path string true "Org ID"
// @Produce      json
// @Success      200 {object} map[string]interface{} "ProviderTestResult"
// @Failure      403 {object} map[string]string "Not an org admin"
// @Failure      404 {object} map[string]string "Unknown org, or no org keystore in this deployment (ORG_KEYSTORE_UNAVAILABLE)"
// @Router       /api/orgs/{id}/providers/azure-foundry/test [post]
func (h *OrgHandler) handleOrgAzureFoundryTest(w http.ResponseWriter, r *http.Request) {
	if !h.orgProviderKeysAvailable(w) {
		return
	}
	org := h.requireAdmin(w, r)
	if org == nil {
		return
	}
	res, err := h.providers.TestOrgProviderConnection(r.Context(), h.security.KeyScope(r), org.ID, ai.AzureFoundryProviderID)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	render.JSON(w, res)
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
// @Failure      400 {object} map[string]string "Embedding provider not configured (EMBEDDING_NOT_CONFIGURED), rejected credentials (EMBEDDING_AUTH_FAILED, EMBEDDING_PERMISSION_DENIED) or a request the provider rejected (EMBEDDING_PROVIDER_REJECTED)"
// @Failure      409 {object} map[string]string "Embedding model differs from the indexed corpus (EMBEDDING_MODEL_CHANGED) or another upload/re-index of this org is in flight (KNOWLEDGE_BUSY)"
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
		if renderKnowledgeEmbeddingError(w, err) {
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
// after every specific cause it wraps.
func renderKnowledgeEmbeddingError(w http.ResponseWriter, err error) bool {
	switch {
	case errors.Is(err, rag.ErrEmbeddingNotConfigured), errors.Is(err, rag.ErrEmbeddingUnavailable):
		render.ErrorWithCode(w, err, http.StatusBadRequest, "EMBEDDING_NOT_CONFIGURED")
	case errors.Is(err, rag.ErrEmbeddingModelChanged):
		render.ErrorWithCode(w, err, http.StatusConflict, "EMBEDDING_MODEL_CHANGED")
	case errors.Is(err, rag.ErrKnowledgeBusy):
		render.ErrorWithCode(w, err, http.StatusConflict, "KNOWLEDGE_BUSY")
	case errors.Is(err, ai.ErrRateLimited):
		w.Header().Set("Retry-After", embeddingRetryAfter(err))
		render.ErrorWithCode(w, err, http.StatusTooManyRequests, "EMBEDDING_RATE_LIMITED")
	case errors.Is(err, ai.ErrProviderPermissionDenied): // before ErrApiKeyInvalid, which it also matches
		render.ErrorWithCode(w, err, http.StatusBadRequest, "EMBEDDING_PERMISSION_DENIED")
	case errors.Is(err, ai.ErrApiKeyInvalid):
		render.ErrorWithCode(w, err, http.StatusBadRequest, "EMBEDDING_AUTH_FAILED")
	case errors.Is(err, ai.ErrProviderConfigInvalid), errors.Is(err, ai.ErrKeyNotConfigured):
		render.ErrorWithCode(w, err, http.StatusBadRequest, "EMBEDDING_NOT_CONFIGURED")
	case errors.Is(err, ai.ErrProviderDown), errors.Is(err, ai.ErrCircuitOpen):
		render.ErrorWithCode(w, err, http.StatusServiceUnavailable, "EMBEDDING_PROVIDER_UNAVAILABLE")
	case errors.Is(err, rag.ErrEmbeddingFailed):
		// The provider's own rejection (unknown deployment, invalid input):
		// the admin's configuration, not a server fault — keep the message.
		render.ErrorWithCode(w, err, http.StatusBadRequest, "EMBEDDING_PROVIDER_REJECTED")
	default:
		return false
	}
	return true
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

// maxKnowledgeReindexBodyBytes bounds the optional re-index body ({"force":true}).
const maxKnowledgeReindexBodyBytes = 4 << 10

// @Summary      Re-index org knowledge base
// @Description  Re-embeds every knowledge chunk with the caller's embedding provider — the recovery path after switching embedding provider/model, which strands the existing corpus at the old dimension (searches silently return nothing). When the corpus was indexed with a different embedding model at the same width, the request is refused with 409 EMBEDDING_MODEL_CHANGED unless the body sets "force": true — re-indexing switches every document to the caller's model, and members whose embedding provider uses another model stop receiving the org's guidelines.
// @Tags         org
// @Accept       json
// @Param        id path string true "Org ID"
// @Param        request body object false "Optional JSON object; force=true re-indexes even when the corpus was indexed with a different embedding model"
// @Produce      json
// @Success      200 {object} map[string]interface{} "Re-indexed ({chunks, docs, truncated?}); truncated=true means the per-request ceiling (10 000 chunks) was reached with chunks left unconverted"
// @Failure      400 {object} map[string]string "Embedding provider not configured (EMBEDDING_NOT_CONFIGURED), rejected credentials (EMBEDDING_AUTH_FAILED, EMBEDDING_PERMISSION_DENIED), a request the provider rejected (EMBEDDING_PROVIDER_REJECTED) or malformed body"
// @Failure      409 {object} map[string]string "Embedding model differs from the indexed corpus (EMBEDDING_MODEL_CHANGED; resend with force) or another upload/re-index of this org is in flight (KNOWLEDGE_BUSY)"
// @Failure      429 {object} map[string]string "Embedding provider throttled the request (EMBEDDING_RATE_LIMITED)"
// @Failure      503 {object} map[string]string "Embedding provider unavailable (EMBEDDING_PROVIDER_UNAVAILABLE)"
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
	// Optional body: absent / empty / {} all mean force=false.
	var req struct {
		Force bool `json:"force"`
	}
	if r.Body != nil {
		if err := decodeOptional(http.MaxBytesReader(w, r.Body, maxKnowledgeReindexBodyBytes), &req); err != nil {
			render.Error(w, fmt.Errorf("invalid re-index request: %w", err), http.StatusBadRequest)
			return
		}
	}
	res, err := h.knowledge.ReindexOrg(r.Context(), h.security.KeyScope(r), chi.URLParam(r, "id"), req.Force)
	if err != nil {
		if renderKnowledgeEmbeddingError(w, err) {
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// The audit row records a forced switch and a partial walk (the corpus
	// is larger than one request converts), the two facts a later reader of
	// the log needs.
	var details map[string]string
	if req.Force || res.Truncated {
		details = map[string]string{}
		if req.Force {
			details["force"] = "true"
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
	if err := h.orgSvc.Delete(r.Context(), chi.URLParam(r, "id")); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
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
// @Success      200 {object} map[string]string "OK"
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
	render.JSON(w, map[string]string{"status": "ok"})
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
// @Success      200 {object} map[string]interface{} "Updated"
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
	render.JSON(w, map[string]string{"status": "ok"})
}
