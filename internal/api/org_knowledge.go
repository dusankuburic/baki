package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

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

// registerKnowledgeRoutes wires the org knowledge-base sub-routes.
func (h *OrgHandler) registerKnowledgeRoutes(r chi.Router) {
	r.Route("/knowledge", func(r chi.Router) {
		r.Get("/", h.handleKnowledgeList)
		r.Post("/upload", h.handleKnowledgeUpload)
		r.Post("/reindex", h.handleKnowledgeReindex)
		r.Delete("/{docId}", h.handleKnowledgeDelete)
	})
}
