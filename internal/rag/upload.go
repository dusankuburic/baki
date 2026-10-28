package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"

	"github.com/google/uuid"
)

// ErrKnowledgeDocumentNotFound: the organisation has no knowledge document by that id (HTTP 404).
var ErrKnowledgeDocumentNotFound = errors.New("knowledge document not found")

// commitTimeout bounds the write of a batch the provider already returned when the request's context has ended: what
// was bought is kept (a continuation would otherwise buy it again), but not waited on forever.
const commitTimeout = 10 * time.Second

// UploadResult reports what ONE upload or continuation request did to a document (R-knowledge-2). The upload is
// two-phase: the document and the text of every chunk are stored first (State pending), then the chunks are embedded
// page by page, each batch committing on its own, until the request budget (reindexRequestBudget) or the end. A
// document whose chunks are all embedded turns ready, joins the corpus and retires the older versions of its
// filename at once; until then no search returns any of its chunks and the version it replaces keeps answering.
// NextCursor is set while chunks remain (PendingChunks > 0): post {docId, cursor} back to the upload route. The cursor
// is a hint only — the chunks still to embed are the resume state, so a client that never received it resumes with
// none, and one past every chunk still to embed (stale or forged) walks from the start. Truncated mirrors
// NextCursor != nil, as on a re-index.
type UploadResult struct {
	ID             string `json:"id"`
	Filename       string `json:"filename"`
	State          string `json:"state"`
	ChunkerVersion int    `json:"chunkerVersion"`
	// Chunks is the document's chunk count; Embedded the chunks this request embedded; PendingChunks those still to
	// embed.
	Chunks        int `json:"chunks"`
	Embedded      int `json:"embedded"`
	PendingChunks int `json:"pendingChunks"`
	// Resumed: the upload found an unfinished upload of the same name and text and continued it instead of storing
	// (and paying for) it again.
	Resumed bool `json:"resumed,omitempty"`
	// Unchanged: the upload's text is the ready document of the name, its chunks embedded under the caller's
	// embedding identity: nothing was stored or embedded, and the result is that document (R-security-c22).
	Unchanged  bool                             `json:"unchanged,omitempty"`
	Truncated  bool                             `json:"truncated,omitempty"`
	NextCursor *interfaces.KnowledgeChunkCursor `json:"nextCursor,omitempty"`
}

// callerUserID is the user every storage call of a request is RLS-scoped to ("" in local mode).
func callerUserID(ctx context.Context) string {
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		return claims.UserID
	}
	return ""
}

// AddDocument uploads a document and embeds it within one request's budget (UploadDocument), for callers that need
// no progress report; a document too large for one request stays pending, resumable by ContinueDocument.
func (s *KnowledgeService) AddDocument(ctx context.Context, scope, orgID, filename, content string) error {
	_, err := s.UploadDocument(ctx, scope, orgID, filename, content)
	return err
}

// UploadDocument is the two-phase upload (UploadResult). Secrets are masked in the document BEFORE it is chunked,
// never chunk by chunk: the mask is then in the text every chunk is cut from, so neighbouring chunks carry an
// identical overlap and stripOverlapPrefix's exact match keeps de-duplicating it — scrubbed on its own, a chunk whose
// carried tail begins inside a secret keeps a key-less fragment of the value while its neighbour masks it. The
// provider and the store see the masked text only (the stored source text too); the render-time scrub of retrieved
// guidelines stays as defence in depth. The filename is masked too: it is stored, listed and rendered beside the
// chunk (ADR 0027 §10).
//
// The org's lease (lockOrg) is held over this request alone — the model guard, the pending write and this request's
// pages — never across requests: between them the document is pending, which no other writer touches (a re-index
// walks no pending chunk), and a continuation takes the lease again and re-checks the model. A failure after the
// document is stored returns BOTH the result (its id, what is left) and the error; the request's deadline or the
// client going away is no failure: the pages bought are kept and the result says what is left.
func (s *KnowledgeService) UploadDocument(ctx context.Context, scope, orgID, filename, content string) (res UploadResult, err error) {
	if strings.TrimSpace(content) == "" {
		return res, fmt.Errorf("document is empty")
	}
	ctx = usageRun(ctx, scope, orgID)
	provider, err := s.embedderFor(ctx, scope, orgID)
	if err != nil {
		return res, fmt.Errorf("embedding provider unavailable: %w", err)
	}
	// Defense-in-depth: a service without a store (nil backend, local mode) must fail cleanly, not panic.
	if s.store == nil {
		return res, fmt.Errorf("knowledge service has no storage backend (cloud mode required)")
	}
	scrubbed := scrubText(content)
	filename = scrubText(filename)
	chunks := chunkDocument(chunkerVersion, scrubbed, filename)
	if len(chunks) > maxKnowledgeChunks {
		return res, fmt.Errorf("%w: %d passages exceed the limit of %d", ErrDocumentTooLarge, len(chunks), maxKnowledgeChunks)
	}

	// Taken after the cheap validation, so a bad request never waits on a running re-index; ctx is cancelled if the
	// lease is lost.
	callerCtx := ctx
	ctx, unlock, err := s.lockOrg(ctx, orgID)
	if err != nil {
		return res, err
	}
	defer unlock()
	defer func() { err = lostOr(ctx, err) }()
	// Whatever the outcome, the org's cached corpus size is stale once this returns.
	defer func() {
		s.bus.Publish(callerCtx, events.KnowledgeDocChanged{OrgID: orgID, DocID: res.ID, Kind: events.KnowledgeDocAdded})
	}()
	res, err = s.ingestLocked(ctx, provider, orgID, filename, scrubbed, chunks, time.Now(), false)
	if scrubbed != content && res.ID != "" && !res.Resumed && !res.Unchanged {
		recordRAGScrubbedChunks("upload", maskedChunks(chunks))
	}
	return res, err
}

// ContinueDocument embeds the rest of an unfinished upload (UploadResult) within one request's budget, after the
// cursor's chunk when the cursor names this document (a hint: the walk takes the chunks still to embed wherever they
// are; a cursor after which none is left restarts the walk from the first). The model guard runs again, and the
// caller's model must be the one the upload's embedded chunks carry. A
// ready document has nothing left; one the org does not have is ErrKnowledgeDocumentNotFound.
func (s *KnowledgeService) ContinueDocument(ctx context.Context, scope, orgID, docID string, after interfaces.KnowledgeChunkCursor) (res UploadResult, err error) {
	if s.store == nil {
		return res, fmt.Errorf("knowledge service has no storage backend (cloud mode required)")
	}
	ctx = usageRun(ctx, scope, orgID)
	provider, err := s.embedderFor(ctx, scope, orgID)
	if err != nil {
		return res, fmt.Errorf("embedding provider unavailable: %w", err)
	}
	callerCtx := ctx
	ctx, unlock, err := s.lockOrg(ctx, orgID)
	if err != nil {
		return res, err
	}
	defer unlock()
	defer func() { err = lostOr(ctx, err) }()
	start := time.Now()
	doc, err := s.store.GetKnowledgeDocument(ctx, orgID, docID)
	if errors.Is(err, interfaces.ErrNotFound) {
		return res, ErrKnowledgeDocumentNotFound
	}
	if err != nil {
		return res, fmt.Errorf("load document: %w", err)
	}
	if doc.State != interfaces.KnowledgeDocumentPending {
		return resultOf(doc, 0), nil
	}
	defer func() {
		s.bus.Publish(callerCtx, events.KnowledgeDocChanged{OrgID: orgID, DocID: docID, Kind: events.KnowledgeDocAdded})
	}()
	superseded, total, err := s.supersededDocIDs(ctx, orgID, doc.Filename)
	if err != nil {
		return res, fmt.Errorf("replace existing document: %w", err)
	}
	if len(superseded) < total {
		if err := s.uploadGuard(ctx, provider, orgID); err != nil {
			return res, err
		}
	}
	if err := s.pendingDocumentGuard(ctx, provider, orgID, docID); err != nil {
		return res, err
	}
	afterID := ""
	if after.DocID == docID {
		afterID = after.ID
	}
	return s.embedPendingLocked(ctx, provider, orgID, docID, afterID, start)
}

// resultOf is the result of a request that embedded embedded chunks of doc.
func resultOf(doc *interfaces.KnowledgeDocument, embedded int) UploadResult {
	return UploadResult{ID: doc.ID, Filename: doc.Filename, State: doc.State, ChunkerVersion: doc.ChunkerVersion,
		Chunks: doc.ChunkCount, Embedded: embedded, PendingChunks: doc.PendingChunks}
}

// errPendingUploadOfName: keepPending was asked and the name has an unfinished upload of other text (RechunkOrg skips
// the document).
var errPendingUploadOfName = errors.New("the name has an unfinished upload of other text")

// embeddedUnder reports whether every chunk of doc, a ready document, is embedded under provider's embedding
// identity, in the vector index: a re-upload of its text would buy the same vectors again. Only a provider that names
// its identity can be matched; a document with legacy chunks (no identity), or an org with any, is not matched —
// the stored vectors' model is unproven — nor is an org with chunks outside the vector index (a width change in
// flight), conservatively.
func (s *KnowledgeService) embeddedUnder(ctx context.Context, provider ai.Provider, orgID string, doc *interfaces.KnowledgeDocument) (bool, error) {
	identity := ai.EmbeddingIdentity(provider)
	if identity == "" || doc.State != interfaces.KnowledgeDocumentReady || doc.ChunkCount == 0 || doc.PendingChunks > 0 {
		return false, nil
	}
	stranded, err := s.store.CountStrandedKnowledgeChunksByDocument(ctx, orgID, identity)
	if err != nil {
		return false, fmt.Errorf("count the stored document's chunks: %w", err)
	}
	if stranded[doc.ID] > 0 {
		return false, nil
	}
	counts, err := s.store.CountKnowledgeChunksByIdentity(ctx, orgID, identity)
	if err != nil {
		return false, fmt.Errorf("count the org's chunks: %w", err)
	}
	if counts.Legacy() > 0 || (s.HasVectorIndex() && counts.VectorIndexed < counts.Total) {
		return false, nil
	}
	return true, nil
}

// ingestLocked stores a document (already masked and chunked) as a pending upload under the org's lease and embeds it
// within the request's budget, start being when the request began. An unfinished upload of the same name and the
// same text at this chunker version is resumed instead; one with another text is superseded — deleted, as it never
// served — before the new upload is stored, and a delete that fails fails the request with nothing stored: two
// unfinished uploads of one name never coexist (the older one, continued and finished later, would retire the newer
// one's ready version). With keepPending (a rechunk) an unfinished upload of other text is never superseded:
// errPendingUploadOfName, nothing written. Ready versions of the name stay until the new one is finished.
//
// An upload (not a rechunk) whose text is the ready version of the name, at this chunker version, with every chunk
// embedded under the caller's identity (embeddedUnder), is unchanged (R-security-c22): no embedding call, nothing
// stored, the result is that version with Unchanged set. The unfinished uploads of the name are superseded as by any
// re-upload — the one of the same text too, which would only buy the ready version's vectors again — so the newest
// upload keeps deciding what the name holds. The stored source text is compared directly: every candidate is read
// with it here anyway (the resume check does the same), and equality has no collision to reason about; a document
// stored before its text was kept (v51) never matches and is embedded again once.
func (s *KnowledgeService) ingestLocked(ctx context.Context, provider ai.Provider, orgID, filename, scrubbed string, chunks []string, start time.Time, keepPending bool) (UploadResult, error) {
	// The versions a re-upload replaces are captured up front and handled BY ID: a delete by name would also match
	// the new document.
	superseded, total, err := s.supersededDocIDs(ctx, orgID, filename)
	if err != nil {
		return UploadResult{}, fmt.Errorf("replace existing document: %w", err)
	}
	var (
		resume *interfaces.KnowledgeDocument
		stale  []string
		same   *interfaces.KnowledgeDocument // the ready version of the same text
	)
	for _, id := range superseded {
		d, err := s.store.GetKnowledgeDocument(ctx, orgID, id)
		if errors.Is(err, interfaces.ErrNotFound) {
			continue
		}
		if err != nil {
			return UploadResult{}, fmt.Errorf("replace existing document: %w", err)
		}
		if d.State != interfaces.KnowledgeDocumentPending {
			if same == nil && !keepPending && d.ChunkerVersion == chunkerVersion && d.HasSource && d.Content == scrubbed {
				same = d
			}
			continue
		}
		if resume == nil && d.ChunkerVersion == chunkerVersion && d.Content == scrubbed {
			resume = d
			continue
		}
		stale = append(stale, id)
	}
	if keepPending && len(stale) > 0 {
		return UploadResult{}, errPendingUploadOfName
	}
	if same != nil {
		unchanged, err := s.embeddedUnder(ctx, provider, orgID, same)
		if err != nil {
			return UploadResult{}, err
		}
		if unchanged {
			return s.unchangedLocked(ctx, orgID, same, resume, stale)
		}
	}

	// Never mix embedding models in one corpus: a document embedded with a different model is unfindable for
	// members on the other model. Checked before embedding so a refused upload costs one small request; no force
	// option — switch models by re-indexing with force first. Replacing every document skips the guard.
	if len(superseded) < total {
		if err := s.uploadGuard(ctx, provider, orgID); err != nil {
			return UploadResult{}, err
		}
	}

	userID := callerUserID(ctx)
	// The unfinished uploads this one supersedes go first, before anything is stored or bought: a failure stops the
	// request with the org as it was, and the ready version answers meanwhile.
	for _, id := range stale {
		if err := s.store.DeleteKnowledgeDocument(ctx, userID, orgID, id); err != nil && !errors.Is(err, interfaces.ErrNotFound) {
			return UploadResult{}, fmt.Errorf("remove the unfinished upload this one replaces: %w", err)
		}
	}
	var docID string
	if resume != nil {
		if err := s.pendingDocumentGuard(ctx, provider, orgID, resume.ID); err != nil {
			return UploadResult{}, err
		}
		docID = resume.ID
	} else {
		doc := &interfaces.KnowledgeDocument{
			ID:             uuid.NewString(),
			OrgID:          orgID,
			Filename:       filename,
			ChunkerVersion: chunkerVersion,
			State:          interfaces.KnowledgeDocumentPending,
			Content:        scrubbed,
		}
		knowledgeChunks := make([]interfaces.KnowledgeChunk, len(chunks))
		for i := range chunks {
			knowledgeChunks[i] = interfaces.KnowledgeChunk{ID: uuid.NewString(), DocID: doc.ID, OrgID: orgID, Content: chunks[i], Pending: true}
		}
		// The document and its chunks are one transaction: written apart, a crash between them left a document
		// with no chunks. The versions it replaces are untouched and still serve.
		if err := s.store.SaveKnowledgeDocumentWithChunks(ctx, userID, doc, knowledgeChunks); err != nil {
			return UploadResult{}, err
		}
		docID = doc.ID
	}
	res, err := s.embedPendingLocked(ctx, provider, orgID, docID, "", start)
	res.Resumed = resume != nil
	return res, err
}

// unchangedLocked answers an upload whose text is doc, the ready version of its name, already embedded under the
// caller's identity: the unfinished uploads of the name (resume, stale) are superseded, nothing is embedded.
func (s *KnowledgeService) unchangedLocked(ctx context.Context, orgID string, doc, resume *interfaces.KnowledgeDocument, stale []string) (UploadResult, error) {
	userID := callerUserID(ctx)
	if resume != nil {
		stale = append(stale, resume.ID)
	}
	for _, id := range stale {
		if err := s.store.DeleteKnowledgeDocument(ctx, userID, orgID, id); err != nil && !errors.Is(err, interfaces.ErrNotFound) {
			return UploadResult{}, fmt.Errorf("remove the unfinished upload this one replaces: %w", err)
		}
	}
	logger.Info("knowledge document unchanged; not embedded again", "org_id", orgID, "doc_id", doc.ID,
		"chunks", doc.ChunkCount, "superseded_pending", len(stale))
	res := resultOf(doc, 0)
	res.Unchanged = true
	return res, nil
}

// uploadGuard is the model guard of an upload (checkCorpusModel): another model at the same width is refused, and so
// is another width unless the rows at it are the caller's own, stranded outside the vector index.
func (s *KnowledgeService) uploadGuard(ctx context.Context, provider ai.Provider, orgID string) error {
	verdict, _, err := s.checkCorpusModel(ctx, provider, orgID)
	if err != nil {
		return err
	}
	switch verdict {
	case corpusOtherModel:
		return ErrEmbeddingModelChanged
	case corpusOtherWidth:
		// Another width is a recovery only when the rows at it are the caller's own, outside the index (a
		// PAD_EMBEDDING_DIM change under one identity): no second model enters the corpus and the re-index
		// converts them. Anyone else's rows at another width would make a mixed corpus.
		own, err := s.ownRowsStranded(ctx, orgID, ai.EmbeddingIdentity(provider))
		if err != nil {
			return err
		}
		if !own {
			return ErrEmbeddingModelChanged
		}
	}
	return nil
}

// pendingDocumentGuard refuses to continue an unfinished upload with another model than the one its embedded chunks
// carry: the census leaves a pending document out, so only its own chunks can tell. A provider that names no
// identity is matched leniently, as legacy rows are.
func (s *KnowledgeService) pendingDocumentGuard(ctx context.Context, provider ai.Provider, orgID, docID string) error {
	identity := ai.EmbeddingIdentity(provider)
	if identity == "" {
		return nil
	}
	stranded, err := s.store.CountStrandedKnowledgeChunksByDocument(ctx, orgID, identity)
	if err != nil {
		return fmt.Errorf("count the upload's chunks: %w", err)
	}
	if stranded[docID] > 0 {
		return fmt.Errorf("%w: this upload was started with another embedding model — delete it and upload the document again", ErrEmbeddingModelChanged)
	}
	return nil
}

// embedPendingLocked embeds a pending document's chunks still to embed, page by page (reindexPageSize) after afterID,
// committing every batch as the provider returns it, until none is left or the request's budget is spent (checked
// after each page, as a re-index does), then finishes the document when nothing is left. A batch the provider
// returned after the request's context ended is still committed (commitTimeout) — it was bought — unless the
// org's lease was lost, when another replica may be writing the org.
func (s *KnowledgeService) embedPendingLocked(ctx context.Context, provider ai.Provider, orgID, docID, afterID string, start time.Time) (UploadResult, error) {
	identity := ai.EmbeddingIdentity(provider)
	userID := callerUserID(ctx)
	embedded := 0
	cursor := afterID
	var walkErr error
	for {
		page, err := s.store.ListPendingKnowledgeChunks(ctx, orgID, docID, cursor, reindexPageSize)
		if err != nil {
			walkErr = fmt.Errorf("load the chunks to embed: %w", err)
			break
		}
		if len(page) == 0 && cursor != "" && afterID != "" {
			// Nothing after the cursor: it was stale or forged (the cursor is a hint), or the chunks before it are
			// still to embed. Walk once more from the start rather than answer nothing with the same cursor, which a
			// client following it would post forever.
			cursor, afterID = "", ""
			continue
		}
		if len(page) == 0 {
			break
		}
		n, prefix, err := s.embedPendingPage(ctx, provider, identity, userID, orgID, page)
		embedded += n
		if prefix > 0 {
			cursor = page[prefix-1].ID
		}
		if err != nil {
			walkErr = err
			break
		}
		if len(page) < reindexPageSize || time.Since(start) >= reindexRequestBudget {
			break
		}
	}

	// The finish (and the report) runs whatever stopped the walk: a request cut off after its last page still turns
	// the document ready.
	fctx, cancel := commitContext(ctx)
	defer cancel()
	var res UploadResult
	pending, ferr := s.store.FinishKnowledgeDocument(fctx, userID, orgID, docID)
	doc, gerr := s.store.GetKnowledgeDocument(fctx, orgID, docID)
	switch {
	case gerr == nil:
		res = resultOf(doc, embedded)
	case ferr == nil:
		ferr = gerr
	}
	res.ID, res.Embedded = docID, embedded
	if ferr == nil {
		res.PendingChunks = pending
	}
	if res.PendingChunks > 0 || ferr != nil {
		res.NextCursor, res.Truncated = &interfaces.KnowledgeChunkCursor{DocID: docID, ID: cursor}, true
	}
	if walkErr != nil {
		walkErr = lostOr(ctx, walkErr)
		if ctx.Err() != nil && !lockLost(ctx) {
			// The request's deadline, or the client gone: what it bought is kept, the rest resumable.
			logger.Warn("knowledge upload stopped by the request's end; resumable", "org_id", orgID, "doc_id", docID,
				"embedded", embedded, "pending", res.PendingChunks, "error", walkErr)
			return res, nil
		}
		logger.Warn("knowledge upload stopped; resumable", "org_id", orgID, "doc_id", docID, "embedded", embedded, "error", walkErr)
		return res, walkErr
	}
	if ferr != nil {
		return res, fmt.Errorf("finish the document: %w", ferr)
	}
	if res.State == interfaces.KnowledgeDocumentReady {
		logger.Info("knowledge document indexed", "org_id", orgID, "doc_id", docID, "chunks", res.Chunks, "embedded", embedded)
	}
	return res, nil
}

// commitContext is the context a write of what was already bought runs under: ctx while it lives (or when its lease
// was lost — then the write must fail), else a short context free of the request's end.
func commitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx.Err() == nil || lockLost(ctx) {
		return ctx, func() {}
	}
	return context.WithTimeout(context.WithoutCancel(ctx), commitTimeout)
}

// embedPendingPage embeds one page of a pending document, committing each batch on its own as the provider returns
// it (embedBatchedEach). It reports how many chunks were committed and prefix, the length of the longest run of the
// page from its start that is committed (the cursor's place; batches may land out of order).
func (s *KnowledgeService) embedPendingPage(ctx context.Context, provider ai.Provider, identity, userID, orgID string, page []interfaces.KnowledgeChunk) (committed, prefix int, err error) {
	texts := make([]string, len(page))
	for i := range page {
		texts[i] = page[i].Content
	}
	var (
		mu    sync.Mutex
		done  = make([]bool, len(page))
		noted bool
	)
	err = embedBatchedEach(ctx, provider, texts, func(start, end int, vectors [][]float32) error {
		batch := make([]interfaces.KnowledgeChunk, end-start)
		for i := range batch {
			c := page[start+i]
			batch[i] = interfaces.KnowledgeChunk{ID: c.ID, DocID: c.DocID, OrgID: orgID, Content: c.Content, Embedding: vectors[i], EmbeddingIdentity: identity}
		}
		cctx, cancel := commitContext(ctx)
		defer cancel()
		if err := s.store.UpdateKnowledgeChunkEmbeddings(cctx, userID, batch); err != nil {
			return storeWriteError{fmt.Errorf("update embeddings: %w", err)}
		}
		mu.Lock()
		defer mu.Unlock()
		if !noted {
			noted = true
			s.noteEmbeddingWidth(orgID, provider, vectors)
		}
		for i := start; i < end; i++ {
			done[i] = true
		}
		committed += end - start
		return nil
	})
	mu.Lock()
	defer mu.Unlock()
	for prefix < len(done) && done[prefix] {
		prefix++
	}
	var write storeWriteError
	switch {
	case errors.As(err, &write):
		err = write.err
	case err != nil:
		err = fmt.Errorf("%w: %w", ErrEmbeddingFailed, ai.EmbeddingFailureCause(err))
	}
	return committed, prefix, err
}

// storeWriteError marks a failure of embedPendingPage's commit, which is the store's, not the provider's.
type storeWriteError struct{ err error }

func (e storeWriteError) Error() string { return e.err.Error() }
func (e storeWriteError) Unwrap() error { return e.err }
