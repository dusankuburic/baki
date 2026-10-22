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
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// maxKnowledgeUploadBytes caps the decoded content (the UI advertises 1MB), enforced server-side so a client cannot reach the paid embeddings API.
const maxKnowledgeUploadBytes = 1 << 20 // 1 MiB

// maxKnowledgeUploadBodyBytes must exceed the content cap: JSON escaping inflates the body, else a tight cap rejects legitimate content before the precise 413.
const maxKnowledgeUploadBodyBytes = 4 << 20 // 4 MiB

// @Summary      List knowledge base docs
// @Description  The org's documents, newest first, each with its chunk counts: chunkCount, vectorIndexed (in the vector index at the deployment's width) and embeddingStranded (embedded under another embedding model than the caller's, so the caller's searches never return them; re-index converts them). A document with chunks outside the vector index also carries embeddingWidthMismatch {configured, observed, source}: the deployment's PAD_EMBEDDING_DIM against the width of the side source names — "embedder" (what the caller's embedding model produces: set PAD_EMBEDDING_DIM to it and restart) or "stored" (what the stored chunks hold: re-index). On a deployment without a vector index (no pgvector) every document carries noVectorIndex true, vectorIndexed is always 0 and no width is reported: searches run in-process at the embedder's width.
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
	limit, ok := clampListLimit(w, r.URL.Query().Get("limit"))
	if !ok {
		return
	}
	// Offset pages past the newest-first page so the older tail stays reachable.
	offset, ok := clampListOffset(w, r.URL.Query().Get("offset"))
	if !ok {
		return
	}
	docs, err := h.knowledge.ListDocuments(r.Context(), h.security.KeyScope(r), chi.URLParam(r, "id"), limit, offset)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	orgID := chi.URLParam(r, "id")
	render.JSON(w, knowledgeListView(docs, h.knowledge.EmbeddingWidthMismatch(r.Context(), orgID, docs), h.knowledge.HasVectorIndex()))
}

// knowledgeDocumentView is a listed document plus, on one with chunks outside the vector index, the knowledge
// base's embedding width against the deployment's (embeddingWidthMismatch: {configured, observed, source}) — which
// of PAD_EMBEDDING_DIM and the model to change before re-indexing, source naming the side that is off; absent when
// the widths agree or are unknown. On a deployment without a vector index noVectorIndex says so: no chunk is ever
// in one there, which says nothing about searchability, so the panel leaves "not searchable" out.
type knowledgeDocumentView struct {
	*storageif.KnowledgeDocument
	EmbeddingWidthMismatch *rag.EmbeddingWidthMismatch `json:"embeddingWidthMismatch,omitempty"`
	NoVectorIndex          bool                        `json:"noVectorIndex,omitempty"`
}

// knowledgeListView attaches mismatch to every document of docs with chunks outside the vector index, and marks
// every document when the deployment has no vector index (the listing is a bare array; the fact rides on each row).
func knowledgeListView(docs []*storageif.KnowledgeDocument, mismatch *rag.EmbeddingWidthMismatch, vectorIndex bool) []knowledgeDocumentView {
	out := make([]knowledgeDocumentView, len(docs))
	for i, d := range docs {
		out[i].KnowledgeDocument = d
		out[i].NoVectorIndex = !vectorIndex
		if mismatch != nil && d.ChunkCount > d.VectorIndexed {
			out[i].EmbeddingWidthMismatch = mismatch
		}
	}
	return out
}

// @Summary      Upload knowledge doc
// @Description  Chunks and embeds the document with the caller's embedding provider. Refused with 409 EMBEDDING_MODEL_CHANGED when that provider's model is not the one the org's knowledge base was indexed with (mixing models would make documents unfindable for members on the other model).
// @Tags         org
// @Param        id path string true "Org ID"
// @Accept       multipart/form-data
// @Produce      json
// @Success      201 {object} map[string]interface{} "Uploaded"
// @Failure      400 {object} map[string]string "Embedding provider not configured (EMBEDDING_NOT_CONFIGURED; EMBEDDING_ORG_NOT_CONFIGURED when the org's shared Azure credential names no embedding deployment), not allowed here (PROVIDER_NOT_ALLOWED: disabled in the deployment or off the organisation's allowed-provider list), rejected credentials (EMBEDDING_AUTH_FAILED, EMBEDDING_PERMISSION_DENIED) or a request the provider rejected (EMBEDDING_PROVIDER_REJECTED)"
// @Failure      409 {object} map[string]string "Embedding model differs from the indexed corpus (EMBEDDING_MODEL_CHANGED) or another upload/re-index of this org is in flight on any replica, or this replica is at its lease cap (KNOWLEDGE_BUSY)"
// @Failure      413 {object} map[string]string "The document chunks to more passages than one upload may index (KNOWLEDGE_DOCUMENT_TOO_LARGE)"
// @Failure      429 {object} map[string]string "Embedding provider throttled the request (EMBEDDING_RATE_LIMITED)"
// @Failure      503 {object} map[string]string "Embedding provider unavailable (EMBEDDING_PROVIDER_UNAVAILABLE) or the knowledge base lock could not be taken (KNOWLEDGE_LOCK_UNAVAILABLE)"
// @Router       /api/orgs/{id}/knowledge/upload [post]
func (h *OrgHandler) handleKnowledgeUpload(w http.ResponseWriter, r *http.Request) {
	if h.knowledge == nil {
		render.Error(w, fmt.Errorf("knowledge service not configured"), http.StatusServiceUnavailable)
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
	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionKnowledgeUpload, chi.URLParam(r, "id"), map[string]string{"filename": req.Filename}))
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
	docID := chi.URLParam(r, "docId")
	if err := h.knowledge.DeleteDocument(r.Context(), h.security.CallerID(r), chi.URLParam(r, "id"), docID); err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionKnowledgeDelete, chi.URLParam(r, "id"), map[string]string{"docId": docID}))
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
	case errors.Is(err, ai.ErrOrgEmbeddingDeploymentMissing), errors.Is(err, rag.ErrEmbeddingOrgCredentialRequired):
		// Before ErrEmbeddingUnavailable, which wraps it: the org's shared credential names no embedding deployment,
		// or a background re-index was asked for on a personal key.
		render.ErrorWithCodeAndDetails(w, err, http.StatusBadRequest, "EMBEDDING_ORG_NOT_CONFIGURED", details)
	case errors.Is(err, ai.ErrProviderNotAllowed):
		// Also before ErrEmbeddingUnavailable: the embedding provider is disabled here or off the org's list.
		render.ErrorWithCodeAndDetails(w, err, http.StatusBadRequest, service.ProviderCodeNotAllowed, details)
	case errors.Is(err, ai.ErrProviderPolicyUnavailable):
		// The org's policy could not be read: the operator's problem (logged there), the sentinel's copy only.
		render.ErrorWithCodeAndDetails(w, ai.ErrProviderPolicyUnavailable, http.StatusServiceUnavailable, service.ProviderCodePolicyUnavailable, details)
	case errors.Is(err, rag.ErrEmbeddingNotConfigured), errors.Is(err, rag.ErrEmbeddingUnavailable):
		render.ErrorWithCodeAndDetails(w, err, http.StatusBadRequest, "EMBEDDING_NOT_CONFIGURED", details)
	case errors.Is(err, rag.ErrEmbeddingModelChanged):
		render.ErrorWithCodeAndDetails(w, err, http.StatusConflict, "EMBEDDING_MODEL_CHANGED", details)
	case errors.Is(err, rag.ErrEmbeddingWidthOff):
		render.ErrorWithCodeAndDetails(w, err, http.StatusConflict, "EMBEDDING_WIDTH_OFF", details)
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
	case errors.Is(err, rag.ErrReembedUnavailable):
		render.ErrorWithCodeAndDetails(w, err, http.StatusServiceUnavailable, "KNOWLEDGE_JOB_UNAVAILABLE", details)
	case errors.Is(err, rag.ErrKnowledgeLockUnavailable):
		// The cross-replica lock of the org could not be asked for (the database did not answer): nothing was started.
		render.ErrorWithCodeAndDetails(w, err, http.StatusServiceUnavailable, "KNOWLEDGE_LOCK_UNAVAILABLE", details)
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
// (opaque to clients); absent (or zero) starts from the beginning. Background runs the whole re-index as the
// knowledge_reembed job instead (202), which resumes its own pages, so it takes no cursor.
type knowledgeReindexRequest struct {
	Force      bool                            `json:"force,omitempty"`
	Cursor     *storageif.KnowledgeChunkCursor `json:"cursor,omitempty"`
	Background bool                            `json:"background,omitempty"`
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
	if req.Background {
		return req, fmt.Errorf("a background re-index resumes on its own; it takes no cursor")
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
// @Description  Re-embeds every knowledge chunk with the caller's embedding provider — the recovery path after switching embedding provider/model, which strands the existing corpus at the old dimension (searches silently return nothing). When the corpus was indexed with a different embedding model at the same width, the request is refused with 409 EMBEDDING_MODEL_CHANGED unless the body sets "force": true — re-indexing switches every document to the caller's model, and members whose embedding provider uses another model stop receiving the org's guidelines. An embedder whose vectors are not PAD_EMBEDDING_DIM wide is refused with 409 EMBEDDING_WIDTH_OFF unless forced: nothing it embeds can enter the vector index, so set PAD_EMBEDDING_DIM to its width and restart instead.
// @Tags         org
// @Accept       json
// @Param        id path string true "Org ID"
// @Param        request body knowledgeReindexRequest false "Optional. force=true re-indexes even when the corpus was indexed with a different embedding model (first request of a run only); cursor = the previous answer's nextCursor resumes the run after it; background=true runs the whole re-index as the knowledge_reembed job (202; no cursor), on the organisation's shared embedding credential only"
// @Produce      json
// @Success      200 {object} rag.ReindexResult "Re-indexed this request's pages ({chunks, docs}); nextCursor (with truncated=true) means chunks remain — post again with {force, cursor: nextCursor}. Every page is committed as it completes, so a later failure keeps the earlier pages — a request that FAILS after committing pages answers its error code with details {nextCursor, chunks} to resume from. Unforced, only the chunks not yet embedded under the caller's model are converted"
// @Success      202 {object} map[string]string "background=true: the knowledge_reembed job was started ({status: accepted, job}); its progress is the job's run state (pad_job_* metrics, the job_runs row) and the document list's embeddingStranded counts"
// @Failure      400 {object} map[string]string "Embedding provider not configured (EMBEDDING_NOT_CONFIGURED; EMBEDDING_ORG_NOT_CONFIGURED when the org's shared Azure credential names no embedding deployment, or a background run was asked for on a personal key), not allowed here (PROVIDER_NOT_ALLOWED), rejected credentials (EMBEDDING_AUTH_FAILED, EMBEDDING_PERMISSION_DENIED), a request the provider rejected (EMBEDDING_PROVIDER_REJECTED) or malformed body / cursor"
// @Failure      409 {object} map[string]string "Embedding model differs from the indexed corpus (EMBEDDING_MODEL_CHANGED; resend with force — on a resumed request: the caller's model is not the one the run converted to), the caller's embedder is not PAD_EMBEDDING_DIM wide (EMBEDDING_WIDTH_OFF; set the pin to its width and restart, or resend with force), or another upload/re-index of this org — or a background re-index on any replica — is in flight, or this replica is at its lease cap (KNOWLEDGE_BUSY)"
// @Failure      429 {object} map[string]string "Embedding provider throttled the request (EMBEDDING_RATE_LIMITED) or the daily AI budget is spent (BUDGET_EXCEEDED)"
// @Failure      503 {object} map[string]string "Embedding provider unavailable (EMBEDDING_PROVIDER_UNAVAILABLE), the budget could not be checked (BUDGET_UNAVAILABLE) or the background job could not be started (KNOWLEDGE_JOB_UNAVAILABLE) or the knowledge base lock could not be taken (KNOWLEDGE_LOCK_UNAVAILABLE)"
// @Failure      504 {object} map[string]string "The request's deadline ran out before a page was committed (KNOWLEDGE_TIMEOUT)"
// @Failure      500 {object} map[string]string "Internal Server Error"
// @Router       /api/orgs/{id}/knowledge/reindex [post]
func (h *OrgHandler) handleKnowledgeReindex(w http.ResponseWriter, r *http.Request) {
	if h.knowledge == nil {
		render.Error(w, fmt.Errorf("knowledge service not configured"), http.StatusServiceUnavailable)
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
	if req.Background {
		h.handleKnowledgeReindexBackground(w, r, req.Force)
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
			h.audit.Record(r.Context(), orgAuditEntry(audit.ActionKnowledgeReindex, chi.URLParam(r, "id"),
				map[string]string{"truncated": "true", "failed": "true"}))
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
	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionKnowledgeReindex, chi.URLParam(r, "id"), details))
	render.JSON(w, res)
}

// handleKnowledgeReindexBackground is the reindex route's {"background": true} branch: the knowledge_reembed job
// takes the run, the request answers 202 with the job's name, and the audit row says the run went to the
// background.
func (h *OrgHandler) handleKnowledgeReindexBackground(w http.ResponseWriter, r *http.Request, force bool) {
	job, err := h.knowledge.ReindexInBackground(r.Context(), h.security.KeyScope(r), chi.URLParam(r, "id"), force)
	if err != nil {
		if renderKnowledgeEmbeddingError(w, withRequestContextErr(r, err)) {
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	details := map[string]string{"background": "true"}
	if force {
		details["force"] = "true"
	}
	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionKnowledgeReindex, chi.URLParam(r, "id"), details))
	render.JSONStatus(w, http.StatusAccepted, map[string]string{"status": "accepted", "job": job})
}

// registerKnowledgeRoutes wires the org knowledge-base sub-routes, which live in the server store. Members read,
// admins write.
func (h *OrgHandler) registerKnowledgeRoutes(rb *route.Builder) {
	rb.Route("/knowledge", func(rb *route.Builder) {
		rb.With(h.requireOrg(service.OrgRead)).Get("/", h.handleKnowledgeList)
		admin := rb.With(h.requireOrg(service.OrgAdmin))
		// Both walk the embedding provider for minutes and commit page by page,
		// each write in a short RLS transaction of its own: the bulk deadline,
		// no request transaction.
		admin.Post("/upload", h.handleKnowledgeUpload, route.NoTx(), route.Bulk())
		admin.Post("/reindex", h.handleKnowledgeReindex, route.NoTx(), route.Bulk())
		admin.Delete("/{docId}", h.handleKnowledgeDelete)
	}, route.Needs(capability.ServerStore))
}
