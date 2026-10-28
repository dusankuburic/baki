package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
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
	"pad-core/ai/scrubber"
)

// maxKnowledgeUploadBytes caps the decoded content (the UI advertises 1MB), enforced server-side so a client cannot reach the paid embeddings API.
const maxKnowledgeUploadBytes = 1 << 20 // 1 MiB

// maxKnowledgeUploadBodyBytes must exceed the content cap: JSON escaping inflates the body, else a tight cap rejects legitimate content before the precise 413.
const maxKnowledgeUploadBodyBytes = 4 << 20 // 4 MiB

// maxKnowledgeFilenameBytes caps an uploaded document's name, the usual file-system name limit.
const maxKnowledgeFilenameBytes = 255

// validKnowledgeFilename refuses a knowledge document name that cannot serve as one (R-security-5). The name is the
// replace key (an upload supersedes every document of the same name), is stored unbounded and is rendered as
// [name] into the guidelines block of the chat prompt, so it must be a bare file name: non-blank, at most
// maxKnowledgeFilenameBytes, valid UTF-8, no control characters and no path separators. U+FFFD is refused with
// invalid UTF-8: encoding/json decodes invalid bytes and lone surrogates to it, so a body's broken name reaches
// the handler as the replacement character.
// invisibleFilenameRune reports a format character (Unicode Cf: bidi overrides and isolates, zero-width marks, the BOM)
// or a line or paragraph separator (Zl, Zp), none of which a file name needs.
func invisibleFilenameRune(r rune) bool {
	return unicode.Is(unicode.Cf, r) || unicode.Is(unicode.Zl, r) || unicode.Is(unicode.Zp, r)
}

func validKnowledgeFilename(name string) error {
	switch {
	case strings.TrimSpace(name) == "":
		return errors.New("filename is required")
	case len(name) > maxKnowledgeFilenameBytes:
		return fmt.Errorf("filename exceeds %d bytes", maxKnowledgeFilenameBytes)
	case !utf8.ValidString(name) || strings.ContainsRune(name, utf8.RuneError):
		return errors.New("filename is not valid UTF-8")
	case strings.IndexFunc(name, unicode.IsControl) >= 0:
		return errors.New("filename contains a control character")
	case strings.IndexFunc(name, invisibleFilenameRune) >= 0:
		// The name is shown to the model as the "[name]" source of every retrieved chunk, and in the admin's list: a
		// bidi override or a zero-width mark there makes it read as something it is not.
		return errors.New("filename contains an invisible or formatting character (a bidi override, a zero-width mark, a line separator)")
	case strings.ContainsAny(name, `/\`):
		return errors.New("filename must be a bare file name, without / or \\")
	}
	return nil
}

// @Summary      List knowledge base docs
// @Description  The org's documents, newest first, each with its state ("ready", or "pending": an upload whose pendingChunks are still to embed — continue it by posting {docId} to the upload route; none of it is searched until it is ready), its chunkerVersion, hasSource (the source text is kept, so a rechunk re-index can cut it again) and its chunk counts: chunkCount, vectorIndexed (in the vector index at the deployment's width) and embeddingStranded (embedded under another embedding model than the caller's, so the caller's searches never return them; re-index converts them). A document with chunks outside the vector index also carries embeddingWidthMismatch {configured, observed, source}: the deployment's PAD_EMBEDDING_DIM against the width of the side source names — "embedder" (what the caller's embedding model produces: set PAD_EMBEDDING_DIM to it and restart) or "stored" (what the stored chunks hold: re-index). On a deployment without a vector index (no pgvector) every document carries noVectorIndex true, vectorIndexed is always 0 and no width is reported: searches run in-process at the embedder's width.
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
		if mismatch != nil && d.ChunkCount-d.PendingChunks > d.VectorIndexed {
			out[i].EmbeddingWidthMismatch = mismatch
		}
	}
	return out
}

// knowledgeUploadRequest is the upload body: a document {filename, content}, or the continuation of an unfinished
// upload {docId, cursor?} — the two shapes are exclusive. cursor is the previous answer's nextCursor (opaque), a hint
// where the walk resumes: absent, it resumes at the first chunk still to embed.
type knowledgeUploadRequest struct {
	Filename string                          `json:"filename,omitempty"`
	Content  string                          `json:"content,omitempty"`
	DocID    string                          `json:"docId,omitempty"`
	Cursor   *storageif.KnowledgeChunkCursor `json:"cursor,omitempty"`
}

// @Summary      Upload knowledge doc
// @Description  Two-phase: stores the document and the text of its chunks first (state "pending"), then embeds the chunks with the caller's embedding provider page by page within the request's budget, each batch committed as it is bought. A document all of whose chunks are embedded turns "ready", joins the corpus and replaces the older version of its filename at once; until then no search returns any of it and the older version keeps answering. While chunks remain (pendingChunks > 0, nextCursor set) post {docId, cursor: nextCursor} to this route to continue the upload (200): it embeds the chunks still to embed with the caller's provider, which must be the model the upload's embedded chunks carry and pass the model guard; nothing is embedded twice, so the cursor is optional, and a ready document answers as it is (embedded 0). A re-upload of the same name supersedes an unfinished upload (the same text resumes it instead: resumed true). Refused with 409 EMBEDDING_MODEL_CHANGED when that provider's model is not the one the org's knowledge base was indexed with (mixing models would make documents unfindable for members on the other model).
// @Tags         org
// @Accept       json
// @Param        id path string true "Org ID"
// @Param        request body knowledgeUploadRequest true "{filename, content} uploads a document; {docId, cursor?} continues an unfinished upload"
// @Produce      json
// @Success      201 {object} rag.UploadResult "Stored: {id, filename, state, chunkerVersion, chunks, embedded, pendingChunks, resumed, truncated, nextCursor}. A request that FAILS after the document was stored answers its error code with details {id, pendingChunks, nextCursor}: continue that document instead of uploading it again"
// @Success      200 {object} rag.UploadResult "A continuation ({docId}): the same shape for the continued document. Also a document whose text is the ready document of its name, already embedded under the caller's embedding model: nothing is stored or embedded, the answer is that document with unchanged: true (an unfinished upload of the name is superseded, as by any re-upload)"
// @Failure      400 {object} map[string]string "Invalid filename (KNOWLEDGE_FILENAME_INVALID: blank, over 255 bytes, not valid UTF-8, or containing a control character or a path separator (slash or backslash); refused before any embedding call), a body mixing a document and a continuation or a cursor naming another document, embedding provider not configured (EMBEDDING_NOT_CONFIGURED; EMBEDDING_ORG_NOT_CONFIGURED when the org's shared Azure credential names no embedding deployment), not allowed here (PROVIDER_NOT_ALLOWED: disabled in the deployment or off the organisation's allowed-provider list), rejected credentials (EMBEDDING_AUTH_FAILED, EMBEDDING_PERMISSION_DENIED) or a request the provider rejected (EMBEDDING_PROVIDER_REJECTED)"
// @Failure      404 {object} map[string]string "A continuation of a document the org does not have (KNOWLEDGE_DOCUMENT_NOT_FOUND)"
// @Failure      409 {object} map[string]string "Embedding model differs from the indexed corpus or, on a continuation, from the upload's (EMBEDDING_MODEL_CHANGED), or another upload/re-index of this org is in flight on any replica, or this replica is at its lease cap (KNOWLEDGE_BUSY)"
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
	var req knowledgeUploadRequest
	if !decodeBody(w, r, &req) {
		return
	}
	orgID := chi.URLParam(r, "id")
	if req.DocID != "" || req.Cursor != nil {
		h.continueKnowledgeUpload(w, r, orgID, req)
		return
	}
	if len(req.Content) > maxKnowledgeUploadBytes {
		render.Error(w, fmt.Errorf("document exceeds %d byte limit", maxKnowledgeUploadBytes), http.StatusRequestEntityTooLarge)
		return
	}
	if err := validKnowledgeFilename(req.Filename); err != nil {
		render.ErrorWithCode(w, err, http.StatusBadRequest, "KNOWLEDGE_FILENAME_INVALID")
		return
	}

	res, err := h.knowledge.UploadDocument(r.Context(), h.security.KeyScope(r), orgID, req.Filename, req.Content)
	// The name as stored: UploadDocument masks it for secret patterns, and the audit row must not keep what it dropped.
	meta := map[string]string{"filename": scrubber.ScrubFreeText(req.Filename)}
	if err != nil {
		if res.ID == "" {
			// Embedding-provider failures are configuration problems, not server faults: 4xx with a machine code so the message survives masking.
			if renderKnowledgeEmbeddingError(w, withRequestContextErr(r, err)) {
				return
			}
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		// Failed after the document was stored: it stays pending with what it bought, and the details say which to continue.
		h.renderKnowledgeUploadFailure(w, r, orgID, res, withRequestContextErr(r, err), meta)
		return
	}
	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionKnowledgeUpload, orgID, knowledgeUploadMeta(meta, res)))
	if res.Unchanged {
		// The text is the ready document of the name, already embedded under the caller's model: nothing was
		// created (R-security-c22).
		render.JSON(w, res)
		return
	}
	render.JSONStatus(w, http.StatusCreated, res)
}

// knowledgeUploadMeta adds what an upload or continuation request did to its audit meta: the document, what is left,
// whether it resumed an unfinished upload.
func knowledgeUploadMeta(meta map[string]string, res rag.UploadResult) map[string]string {
	meta["docId"] = res.ID
	if res.PendingChunks > 0 {
		meta["pendingChunks"] = strconv.Itoa(res.PendingChunks)
	}
	if res.Resumed {
		meta["resumed"] = "true"
	}
	if res.Unchanged {
		meta["unchanged"] = "true"
	}
	return meta
}

// renderKnowledgeUploadFailure answers an upload or continuation that failed after its document was stored: the
// error's code with details {id, pendingChunks, nextCursor} (the document to continue), and an audit row marked failed.
func (h *OrgHandler) renderKnowledgeUploadFailure(w http.ResponseWriter, r *http.Request, orgID string, res rag.UploadResult, err error, meta map[string]string) {
	details := map[string]any{"id": res.ID, "pendingChunks": res.PendingChunks}
	if res.NextCursor != nil {
		details["nextCursor"] = res.NextCursor
	}
	if !renderKnowledgeEmbeddingErrorDetails(w, err, details) {
		render.ErrorWithCodeAndDetails(w, err, http.StatusInternalServerError, "INTERNAL_ERROR", details)
	}
	meta = knowledgeUploadMeta(meta, res)
	meta["failed"] = "true"
	h.audit.Record(r.Context(), orgAuditEntry(audit.ActionKnowledgeUpload, orgID, meta))
}

// continuationCursor validates a continuation's body: no document beside it, and a cursor (optional) naming the
// continued document.
func continuationCursor(req knowledgeUploadRequest) (storageif.KnowledgeChunkCursor, error) {
	switch {
	case req.DocID == "":
		return storageif.KnowledgeChunkCursor{}, fmt.Errorf("a continuation names its docId")
	case req.Filename != "" || req.Content != "":
		return storageif.KnowledgeChunkCursor{}, fmt.Errorf("a body is either a document {filename, content} or a continuation {docId, cursor}")
	case len(req.DocID) > maxReindexCursorField || !utf8.ValidString(req.DocID) || strings.IndexFunc(req.DocID, unicode.IsControl) >= 0:
		return storageif.KnowledgeChunkCursor{}, fmt.Errorf("invalid docId")
	case req.Cursor == nil || req.Cursor.IsZero():
		return storageif.KnowledgeChunkCursor{}, nil
	}
	c := *req.Cursor
	if c.DocID != req.DocID {
		return storageif.KnowledgeChunkCursor{}, fmt.Errorf("the cursor names another document")
	}
	if len(c.ID) > maxReindexCursorField || !utf8.ValidString(c.ID) || strings.IndexFunc(c.ID, unicode.IsControl) >= 0 {
		return storageif.KnowledgeChunkCursor{}, fmt.Errorf("invalid cursor")
	}
	return c, nil
}

// continueKnowledgeUpload is the upload route's continuation branch ({docId, cursor?}): it embeds the rest of an
// unfinished upload (rag.ContinueDocument) and answers 200 with the document; an audit row when it embedded anything.
func (h *OrgHandler) continueKnowledgeUpload(w http.ResponseWriter, r *http.Request, orgID string, req knowledgeUploadRequest) {
	after, err := continuationCursor(req)
	if err != nil {
		render.Error(w, fmt.Errorf("invalid continuation: %w", err), http.StatusBadRequest)
		return
	}
	res, err := h.knowledge.ContinueDocument(r.Context(), h.security.KeyScope(r), orgID, req.DocID, after)
	meta := map[string]string{"continued": "true"}
	if err != nil {
		if res.ID == "" {
			if renderKnowledgeEmbeddingError(w, withRequestContextErr(r, err)) {
				return
			}
			render.Error(w, err, http.StatusInternalServerError)
			return
		}
		h.renderKnowledgeUploadFailure(w, r, orgID, res, withRequestContextErr(r, err), meta)
		return
	}
	if res.Embedded > 0 {
		h.audit.Record(r.Context(), orgAuditEntry(audit.ActionKnowledgeUpload, orgID, knowledgeUploadMeta(meta, res)))
	}
	render.JSON(w, res)
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
	case errors.Is(err, rag.ErrKnowledgeDocumentNotFound):
		render.ErrorWithCodeAndDetails(w, err, http.StatusNotFound, "KNOWLEDGE_DOCUMENT_NOT_FOUND", details)
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
// knowledge_reembed job instead (202), which resumes its own pages, so it takes no cursor. Rechunk runs the rechunk
// re-index instead (rag.RechunkOrg): the documents cut by an older chunker version are cut again and re-embedded; it
// resumes from the corpus itself, so its cursor is only a "more is left" (accepted back, not needed), and it takes
// neither force (switch models with a forced re-index first) nor background.
type knowledgeReindexRequest struct {
	Force      bool                            `json:"force,omitempty"`
	Cursor     *storageif.KnowledgeChunkCursor `json:"cursor,omitempty"`
	Background bool                            `json:"background,omitempty"`
	Rechunk    bool                            `json:"rechunk,omitempty"`
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
	if req.Rechunk && (req.Force || req.Background) {
		return req, fmt.Errorf("a rechunk takes neither force nor background")
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
// @Param        request body knowledgeReindexRequest false "Optional. force=true re-indexes even when the corpus was indexed with a different embedding model (first request of a run only); cursor = the previous answer's nextCursor resumes the run after it; background=true runs the whole re-index as the knowledge_reembed job (202; no cursor), on the organisation's shared embedding credential only; rechunk=true cuts the documents split by an older chunker version again at the current one and re-embeds them (no force, no background): each becomes a new ready version of its name; one stored before its source text was kept is skipped (skippedNoSource) until uploaded again, and one whose name has an unfinished upload is skipped (skippedPending): a rechunk never supersedes an upload"
// @Produce      json
// @Success      200 {object} rag.ReindexResult "Re-indexed this request's pages ({chunks, docs}); nextCursor (with truncated=true) means chunks remain — post again with {force, cursor: nextCursor}. Every page is committed as it completes, so a later failure keeps the earlier pages — a request that FAILS after committing pages answers its error code with details {nextCursor, chunks} to resume from. Unforced, only the chunks not yet embedded under the caller's model are converted; a plain re-index never re-chunks. A rechunk also reports rechunked (this request's), and skippedNoSource, skippedTooLarge and skippedPending, which every request recounts from the start of the corpus (take the last request's, or the largest, never the sum); post {rechunk: true} again while nextCursor is set"
// @Success      202 {object} map[string]string "background=true: the knowledge_reembed job was started ({status: accepted, job}); its progress is the job's run state (pad_job_* metrics, the job_runs row) and the document list's embeddingStranded counts"
// @Failure      400 {object} map[string]string "Embedding provider not configured (EMBEDDING_NOT_CONFIGURED; EMBEDDING_ORG_NOT_CONFIGURED when the org's shared Azure credential names no embedding deployment, or a background run was asked for on a personal key), not allowed here (PROVIDER_NOT_ALLOWED), rejected credentials (EMBEDDING_AUTH_FAILED, EMBEDDING_PERMISSION_DENIED), a request the provider rejected (EMBEDDING_PROVIDER_REJECTED) or malformed body / cursor"
// @Failure      409 {object} map[string]string "Embedding model differs from the indexed corpus (EMBEDDING_MODEL_CHANGED; resend with force — on a resumed request: the caller's model is not the one the run converted to), the caller's embedder is not PAD_EMBEDDING_DIM wide (EMBEDDING_WIDTH_OFF; set the pin to its width and restart, or resend with force), or another upload/re-index of this org — or a background re-index on any replica — is in flight, or this replica is at its lease cap (KNOWLEDGE_BUSY)"
// @Failure      429 {object} map[string]string "Embedding provider throttled the request (EMBEDDING_RATE_LIMITED)"
// @Failure      503 {object} map[string]string "Embedding provider unavailable (EMBEDDING_PROVIDER_UNAVAILABLE), the background job could not be started (KNOWLEDGE_JOB_UNAVAILABLE) or the knowledge base lock could not be taken (KNOWLEDGE_LOCK_UNAVAILABLE)"
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
	var res rag.ReindexResult
	if req.Rechunk {
		res, err = h.knowledge.RechunkOrg(r.Context(), h.security.KeyScope(r), chi.URLParam(r, "id"))
	} else {
		res, err = h.knowledge.ReindexOrg(r.Context(), h.security.KeyScope(r), chi.URLParam(r, "id"), req.Force, after)
	}
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
	if req.Force || req.Cursor != nil || res.Truncated || req.Rechunk {
		details = map[string]string{}
		if req.Force {
			details["force"] = "true"
		}
		if req.Rechunk {
			details["rechunk"] = "true"
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
		// Both walk the embedding provider page by page within one request's budget, every write a short RLS
		// transaction of its own — an upload (or its continuation, the same route) stores its document pending
		// first and commits each embedded batch, a re-index commits each page: the bulk deadline, no request
		// transaction. Both are billed bulk embedding: the embed bucket (R-security-c22), which a paged upload or
		// re-index never exhausts — each continuation follows a request that spent its budget.
		admin.Post("/upload", h.handleKnowledgeUpload, route.RateLimit(route.RateEmbed), route.Billed(), route.NoTx(), route.Bulk())
		admin.Post("/reindex", h.handleKnowledgeReindex, route.RateLimit(route.RateEmbed), route.Billed(), route.NoTx(), route.Bulk())
		admin.Delete("/{docId}", h.handleKnowledgeDelete)
	}, route.Needs(capability.ServerStore))
}
