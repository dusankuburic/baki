package rag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"unicode/utf8"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/ai/scrubber"
	"pad-core/logger"
	"pad-core/models"

	"github.com/google/uuid"
)

// Embedding-provider sentinels. Exported so the HTTP layer can map them to a
// distinct machine-readable error code the frontend switches on (a Claude-only
// deployment can't index documents — the UI explains that root cause).
var (
	ErrEmbeddingNotConfigured = errors.New("embedding provider not configured")
	ErrEmbeddingUnavailable   = errors.New("embedding provider unavailable")
	// ErrEmbeddingModelChanged: the caller's embedding model is not the one
	// the org's knowledge base was indexed with (see checkCorpusModel). Upload
	// refuses; re-index refuses unless forced. The HTTP layer maps it to 409
	// EMBEDDING_MODEL_CHANGED.
	ErrEmbeddingModelChanged = errors.New("your embedding model differs from the one this knowledge base was indexed with")
	// ErrEmbeddingFailed: the embedding provider's request failed while
	// indexing (upload, re-index, or the mismatch-guard sample). It wraps the
	// provider's own cause (unwrapped with ai.EmbeddingFailureCause, so a
	// split-cascade failure is not opaque) so the HTTP layer can classify it —
	// invalid key, permission denied, throttling, outage, or the provider's
	// rejection of the request — instead of rendering a masked 500.
	ErrEmbeddingFailed = errors.New("embedding provider request failed")
	// ErrKnowledgeBusy: another upload or re-index of the same org is in
	// flight in this process (see lockOrg). The HTTP layer maps it to 409
	// KNOWLEDGE_BUSY.
	ErrKnowledgeBusy = errors.New("the knowledge base is being updated — try again in a moment")
)

// azureFoundryProviderID is the provider that embeds the knowledge base only
// on the operator's opt-in (PAD_EMBEDDING_PROVIDER; see embedder).
const azureFoundryProviderID = "azure-foundry"

// Mismatch-guard tuning (checkCorpusModel). The same model re-embedding the
// same text scores ~0.999 (float noise, API-side batching); two different
// models' vectors are unrelated (~0). 0.95 separates the two with room for
// a model's own non-determinism. The WORST same-width pair decides, so a
// partially converted (mixed) corpus is caught instead of passing on its
// one matching sample — the sample is one chunk per document (see
// SampleKnowledgeChunkEmbeddings) for the same reason.
const (
	corpusSampleSize   = 3
	sameModelMinCosine = 0.95
)

// Re-index paging (ReindexOrg). One page is five embed batches and commits
// on its own (the re-index route runs outside the request-long RLS
// transaction, so UpdateKnowledgeChunkEmbeddings opens a short RLS
// transaction per page): a failure keeps every page already converted. The
// ceiling bounds one request's embedding bill and duration; a corpus beyond
// it answers with NextCursor and the client resumes from there in a new
// request. The walk is keyset-ordered, so nothing is re-embedded twice.
// Both are vars so tests can lower them.
var (
	reindexPageSize  = 500
	maxReindexChunks = 10000
)

// SettingsProvider returns the current application settings.
type SettingsProvider interface {
	Get() *models.AppSettings
}

// maxKnowledgeChunks bounds the number of embedding calls a single document can
// fan out into, so a large upload can't spike cost or trip provider rate limits.
const maxKnowledgeChunks = 500

// embedBatchSize caps the input items per /embeddings request. Providers cap
// batch size (OpenAI/Azure reject requests with too many inputs); without
// batching, a document with more chunks than the cap fails to index AT ALL
// after chunking succeeded — the whole Embed call errors out. 100 is within
// every observed limit while keeping request count low (500 chunks → 5 calls).
const embedBatchSize = 100

// maxQueryRunes caps the chat query sent to the embeddings API. The request
// body limit alone (10 MiB) would otherwise allow a ~2.5M-token embedding
// request — far above any provider's per-request cap and billed per token, so a
// simple script cycling chat messages could run up a large bill and trip rate
// limits that degrade chat for everyone. 4000 runes is well above any realistic
// natural-language question yet cheap to embed.
const maxQueryRunes = 4000

type KnowledgeService struct {
	store    interfaces.StorageBackend
	factory  *ai.ProviderFactory
	settings SettingsProvider
	// embeddingProvider is the operator's deployment-wide pin
	// (PAD_EMBEDDING_PROVIDER, see WithEmbeddingProvider); "" = none.
	embeddingProvider string
	// embedOverride substitutes the resolved embedder when set — a test hook
	// mirroring ChatService's (applyFixFunc etc.); nil in production.
	embedOverride ai.Provider
	// orgLocks serialises corpus mutations per org (lockOrg): orgID → *sync.Mutex.
	orgLocks sync.Map
	// budget gates the billed embedding runs (upload, re-index) on the daily
	// AI budget; nil = unlimited (local mode, tests). See SetBudgetChecker.
	budget BudgetChecker
}

// BudgetChecker is the daily AI budget gate the knowledge base consults
// before a billed embedding run (the chat service's rule, wired in di):
// an error — matching the chat service's budget sentinels — refuses it.
type BudgetChecker interface {
	CheckBudget(ctx context.Context, scope, orgID string) error
}

// SetBudgetChecker wires the daily budget gate. Set once at boot, before
// requests are served.
func (s *KnowledgeService) SetBudgetChecker(b BudgetChecker) { s.budget = b }

// checkBudget refuses a billed embedding run once the org's (and the
// caller's) daily AI budget is spent; the error is the checker's own.
func (s *KnowledgeService) checkBudget(ctx context.Context, scope, orgID string) error {
	if s.budget == nil {
		return nil
	}
	return s.budget.CheckBudget(ctx, scope, orgID)
}

// Option configures a KnowledgeService at construction.
type Option func(*KnowledgeService)

// WithEmbeddingProvider pins the provider that embeds the org knowledge base
// (PAD_EMBEDDING_PROVIDER, validated by config at boot). It wins over the
// settings' AI.EmbeddingProvider, and it is the only way the knowledge base
// embeds with Azure AI Foundry (see embedder). "" leaves the default path.
func WithEmbeddingProvider(providerID string) Option {
	return func(s *KnowledgeService) { s.embeddingProvider = providerID }
}

// WithEmbedder makes every request embed with p instead of resolving a
// provider per caller — for tests of the knowledge-base flows (other
// packages cannot reach the embedOverride hook) and single-provider tools.
func WithEmbedder(p ai.Provider) Option {
	return func(s *KnowledgeService) { s.embedOverride = p }
}

// NewKnowledgeService takes the provider factory and settings store rather than
// a pre-resolved provider so the embedding provider (and its API key) is
// resolved lazily, per request, in the caller's scope. Resolving once at
// startup with an empty scope would fail in cloud mode (keys are per-user) and
// never pick up keys added later without a restart.
func NewKnowledgeService(store interfaces.StorageBackend, factory *ai.ProviderFactory, settings SettingsProvider, opts ...Option) *KnowledgeService {
	s := &KnowledgeService{store: store, factory: factory, settings: settings}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// embedder resolves the embedding provider in the caller's scope. It honours
// the operator's pin (PAD_EMBEDDING_PROVIDER), else the configured
// EmbeddingProvider (default openai), AND the optional EmbeddingModel override
// (so a deployer can pick e.g. text-embedding-3-large).
//
// Fallback: if the configured embedding provider has no key configured (common
// on a Claude- or Gemini-only deploy that never set EmbeddingProvider), the
// service scans the embedding-CAPABLE providers in a fixed priority order
// (deterministic — a map iteration would pick randomly per call) and uses the
// first one with a key. The capability filter matters: returning the first
// merely-keyed provider (as an earlier version did) let a Claude-only deploy
// construct a chat-only provider that then failed at Embed time with an opaque
// wrapped 500 instead of the machine-readable sentinel the UI explains.
//
// Azure AI Foundry is operator opt-in: it embeds only when the pin is
// "azure-foundry", never through the settings value alone nor the fallback.
// A user's Azure document names an embedding deployment of THEIR choosing
// (the name reveals nothing about the model), so a member with only an Azure
// configuration would otherwise silently add a second embedding model to the
// org's shared corpus — at the same width, where nothing else can tell the
// two apart.
//
// Organisation-shared credentials (G7): for Azure AI Foundry and for the
// operator's pinned provider the org's shared credential is preferred over
// the caller's own (ForEmbeddingOrg) — the corpus is one and must be
// embedded with one resource, whichever member uploads or searches. The
// fallback scan stays per member: an org shares one deliberate credential,
// not a fallback.
func (s *KnowledgeService) embedder(scope, orgID string) (ai.Provider, error) {
	if s.embedOverride != nil {
		return s.embedOverride, nil
	}
	if s.factory == nil {
		return nil, ErrEmbeddingNotConfigured
	}
	forEmbedding := func(pid, model string) (ai.Provider, error) {
		if orgID != "" && (pid == azureFoundryProviderID || (s.embeddingProvider != "" && pid == s.embeddingProvider)) {
			return s.factory.ForEmbeddingOrg(scope, orgID, pid, model)
		}
		return s.factory.ForEmbedding(scope, pid, model)
	}

	providerID := s.embeddingProvider
	embeddingModel := ""
	if s.settings != nil {
		if settings := s.settings.Get(); settings != nil {
			if providerID == "" {
				providerID = settings.AI.EmbeddingProvider
			}
			embeddingModel = settings.AI.EmbeddingModel
		}
	}

	// Try the configured (or default openai) embedding provider first.
	if providerID == "" {
		providerID = "openai"
	}
	if !ai.SupportsEmbeddings(providerID) {
		// Explicitly configured to a chat-only provider (e.g. claude): fail
		// fast with the machine-readable sentinel instead of constructing the
		// provider and failing later at Embed time.
		return nil, fmt.Errorf("%w (%q does not support embeddings)", ErrEmbeddingUnavailable, providerID)
	}
	var err error
	if providerID == azureFoundryProviderID && s.embeddingProvider != azureFoundryProviderID {
		err = fmt.Errorf("%s embeds the knowledge base only when the operator sets PAD_EMBEDDING_PROVIDER=%s", azureFoundryProviderID, azureFoundryProviderID)
	} else {
		var p ai.Provider
		if p, err = forEmbedding(providerID, embeddingModel); err == nil {
			return p, nil
		}
	}

	// Fallback: first keyed, embedding-capable provider in fixed priority
	// order. Having a usable key (which ForEmbedding decides — an Azure
	// document without an embedding deployment is not one) is the whole
	// test. This used to also require settings.AI.Providers[pid].Enabled,
	// but no UI ever sets that flag and the only provider enabled by default
	// (claude) cannot embed — so the fallback was unreachable by construction
	// and a keyed Gemini/GLM deployment without an OpenAI key got
	// ErrEmbeddingUnavailable instead of its own provider. Azure AI Foundry
	// is never a fallback (operator opt-in only, see above).
	for _, pid := range ai.EmbeddingFallbackOrder() {
		if pid == providerID || pid == azureFoundryProviderID {
			continue // already tried above / opt-in only
		}
		if p2, err2 := s.factory.ForEmbedding(scope, pid, embeddingModel); err2 == nil {
			return p2, nil
		}
	}
	return nil, fmt.Errorf("%w (configured %q: %w; no embedding-capable fallback had a key)", ErrEmbeddingUnavailable, providerID, err)
}

// corpusVerdict is what checkCorpusModel learned about the caller's model.
type corpusVerdict int

const (
	corpusEmpty      corpusVerdict = iota // nothing comparable indexed — nothing to disagree with
	corpusSameModel                       // every same-width sample reproduced its stored vector
	corpusOtherModel                      // same width, unrelated vectors: another model
	corpusOtherWidth                      // no same-width pair: the caller's width differs from every sampled row
)

// checkCorpusModel is the embedding-model mismatch guard. Nothing records
// which model produced a stored vector, and equal width does not mean equal
// model (OpenAI text-embedding-3-small and an Azure ada-002 deployment are
// both 1536 wide), so it asks the model itself: up to corpusSampleSize
// indexed chunks (one per document) are re-embedded with provider and
// compared with their stored vectors. The same model reproduces them
// (cosine ~1); another model's vectors are unrelated. The WORST same-width
// pair decides (corpusOtherModel below sameModelMinCosine), so a mixed
// corpus — a partial re-index, a race — refuses everyone until a forced
// re-index repairs it. A sample with no same-width pair is corpusOtherWidth:
// whether that is the stranded-corpus recovery or a switch is the caller's
// call (corpusStranded). All-zero stored vectors have no defined
// similarity and are ignored; an empty corpus is corpusEmpty.
func (s *KnowledgeService) checkCorpusModel(ctx context.Context, provider ai.Provider, orgID string) (corpusVerdict, error) {
	samples, err := s.store.SampleKnowledgeChunkEmbeddings(ctx, orgID, corpusSampleSize)
	if err != nil {
		return corpusEmpty, fmt.Errorf("sample indexed chunks: %w", err)
	}
	return compareSamples(ctx, provider, orgID, samples)
}

// compareSamples re-embeds the sampled chunks' text with provider and
// classifies the result against their stored vectors (checkCorpusModel's
// rule: the worst same-width pair decides; no same-width pair is
// corpusOtherWidth; nothing comparable is corpusEmpty).
func compareSamples(ctx context.Context, provider ai.Provider, orgID string, samples []interfaces.KnowledgeChunk) (corpusVerdict, error) {
	texts := make([]string, 0, len(samples))
	stored := make([][]float32, 0, len(samples))
	for _, c := range samples {
		if strings.TrimSpace(c.Content) == "" || len(c.Embedding) == 0 {
			continue
		}
		texts = append(texts, c.Content)
		stored = append(stored, c.Embedding)
	}
	if len(texts) == 0 {
		return corpusEmpty, nil
	}
	fresh, err := provider.Embed(ctx, texts)
	if err != nil {
		return corpusEmpty, fmt.Errorf("%w: %w", ErrEmbeddingFailed, ai.EmbeddingFailureCause(err))
	}
	if len(fresh) != len(texts) {
		return corpusEmpty, fmt.Errorf("embedding count mismatch: got %d for %d chunks", len(fresh), len(texts))
	}
	worst, sameWidth, compared := 2.0, false, false
	for i := range texts {
		if len(fresh[i]) != len(stored[i]) {
			continue
		}
		sameWidth = true
		sim, ok := cosineSimilarity(stored[i], fresh[i])
		if !ok {
			continue
		}
		compared = true
		worst = min(worst, sim)
	}
	switch {
	case !sameWidth:
		return corpusOtherWidth, nil
	case !compared:
		return corpusEmpty, nil // only undefined (all-zero) vectors at this width
	case worst < sameModelMinCosine:
		logger.Warn("knowledge base: caller's embedding model differs from the indexed corpus",
			"org_id", orgID, "worst_similarity", worst, "width", len(fresh[0]))
		return corpusOtherModel, nil
	}
	return corpusSameModel, nil
}

// corpusStranded: nothing in the org's corpus is in the vector index at the
// deployment's width — the state a width change leaves behind and the only
// case in which a caller at another width is recovering the corpus rather
// than switching it. A store that cannot tell (no pgvector column, so the
// count errors) is treated as stranded, which keeps the pre-guard behaviour.
func (s *KnowledgeService) corpusStranded(ctx context.Context, orgID string) bool {
	_, indexed, err := s.store.CountKnowledgeChunks(ctx, orgID)
	return err != nil || indexed == 0
}

// lockOrg serialises corpus mutations per org in this process: a re-index
// snapshots the corpus, embeds it for seconds to minutes and updates by id,
// so an upload landing in between carries the pre-switch model and is never
// seen by the re-index. Non-blocking, so a second admin gets a 409
// (ErrKnowledgeBusy) instead of a request parked behind a long re-index.
// Search never takes it — readers never wait.
//
// Process-local: a multi-replica deployment needs a database lock instead
// (pg_advisory_xact_lock(hashtext(orgID)) in the request's RLS transaction),
// because two replicas cannot see each other's mutex. sync.Map never evicts;
// it holds one mutex per org ever touched, bounded by the org count.
func (s *KnowledgeService) lockOrg(orgID string) (func(), error) {
	v, _ := s.orgLocks.LoadOrStore(orgID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, ErrKnowledgeBusy
	}
	return mu.Unlock, nil
}

// cosineSimilarity of two equal-length vectors; ok=false when either is all
// zeros (undefined).
func cosineSimilarity(a, b []float32) (float64, bool) {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0, false
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb)), true
}

// AddDocument chunks content, generates embeddings in the caller's scope, then
// persists the document and its chunks. Embedding happens before the document
// row is written so a failure does not leave an orphan "indexed" document; if
// chunk persistence fails the document row is rolled back best-effort.
func (s *KnowledgeService) AddDocument(ctx context.Context, scope, orgID, filename, content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("document is empty")
	}
	if err := s.checkBudget(ctx, scope, orgID); err != nil {
		return err
	}

	provider, err := s.embedder(scope, orgID)
	if err != nil {
		return fmt.Errorf("embedding provider unavailable: %w", err)
	}

	// Defense-in-depth: a service constructed without a store (nil backend,
	// local mode) must fail cleanly here rather than nil-panic at the store
	// dereference below. Placed after input/provider validation so the
	// cheaper checks surface first.
	if s.store == nil {
		return fmt.Errorf("knowledge service has no storage backend (cloud mode required)")
	}

	chunks := chunkText(content, 1000) // ~1000 runes per chunk
	if len(chunks) > maxKnowledgeChunks {
		return fmt.Errorf("document too large: %d chunks exceeds limit of %d", len(chunks), maxKnowledgeChunks)
	}

	// Taken after the cheap validation, so a bad request never waits on (or
	// is refused by) a running re-index.
	unlock, err := s.lockOrg(orgID)
	if err != nil {
		return err
	}
	defer unlock()

	// Replace semantics: re-uploading a filename supersedes the old version
	// (chunks cascade with the document). Before this, both versions were
	// listed AND both were retrieved — stale content kept influencing chat
	// until manually deleted. Superseded IDs are captured up front and
	// deleted BY ID at the end (see the ORDERING comment below), because
	// DeleteKnowledgeDocumentByName would also match the document we are
	// about to write.
	superseded, total, err := s.supersededDocIDs(ctx, orgID, filename)
	if err != nil {
		return fmt.Errorf("replace existing document: %w", err)
	}

	// Never mix embedding models in one corpus: a document embedded with a
	// different model than the rest is unfindable for members on the other
	// model (and vice versa). Checked before the document is embedded, so a
	// refused upload costs one small request. No force option here — an
	// admin who means to switch models re-indexes (with force) first.
	//
	// When the upload replaces every document the org has, nothing survives
	// to be mixed with: the guard has nothing to protect (delete-then-upload
	// reaches the same state unguarded) and would only force a re-index of
	// content about to be deleted.
	if len(superseded) < total {
		verdict, err := s.checkCorpusModel(ctx, provider, orgID)
		if err != nil {
			return err
		}
		// An upload at another width is never a recovery: it always produces
		// a mixed corpus. Re-index first (unforced when the corpus is
		// stranded, see ReindexOrg).
		if verdict == corpusOtherModel || verdict == corpusOtherWidth {
			return ErrEmbeddingModelChanged
		}
	}

	embeddings, err := embedBatched(ctx, provider, chunks)
	if err != nil {
		return fmt.Errorf("%w: %w", ErrEmbeddingFailed, ai.EmbeddingFailureCause(err))
	}
	if len(embeddings) != len(chunks) {
		return fmt.Errorf("embedding count mismatch: got %d for %d chunks", len(embeddings), len(chunks))
	}

	doc := &interfaces.KnowledgeDocument{
		ID:       uuid.NewString(),
		OrgID:    orgID,
		Filename: filename,
	}
	// ORDERING: write the new version FIRST, delete the old one only once the
	// new document AND its chunks have committed. The previous order deleted
	// first, on the reasoning that embedding had already succeeded — true, but
	// the delete still preceded both writes, so a failure in
	// SaveKnowledgeDocument or SaveKnowledgeChunks left the org with NEITHER
	// version, and the rollback below removed the NEW document when the old one
	// was already gone. There is no cross-call transaction available here, so
	// write-new-then-delete-old is what makes a failed re-upload a no-op.
	if err := s.store.SaveKnowledgeDocument(ctx, doc); err != nil {
		return err
	}

	knowledgeChunks := make([]interfaces.KnowledgeChunk, len(chunks))
	for i := range chunks {
		knowledgeChunks[i] = interfaces.KnowledgeChunk{
			ID:        uuid.NewString(),
			DocID:     doc.ID,
			Content:   chunks[i],
			Embedding: embeddings[i],
		}
	}

	// Extract the caller's userID so chunk inserts are RLS-scoped to them
	// (matching the rest of the storage layer). Empty in local mode / unauth.
	userID := ""
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		userID = claims.UserID
	}

	if err := s.store.SaveKnowledgeChunks(ctx, userID, knowledgeChunks); err != nil {
		// Roll back the orphaned document so it doesn't appear "indexed" with no
		// chunks. The previous version is untouched and still serves retrieval.
		_ = s.store.DeleteKnowledgeDocument(ctx, orgID, doc.ID)
		return err
	}

	// The new version is durable; retire the ones it supersedes. A failure here
	// leaves a duplicate rather than a gap — the safe direction, and visible in
	// the document list rather than silent.
	for _, id := range superseded {
		if err := s.store.DeleteKnowledgeDocument(ctx, orgID, id); err != nil {
			logger.Warn("knowledge: failed to remove superseded document version",
				"orgId", orgID, "filename", filename, "docId", id, "error", err)
		}
	}
	return nil
}

// supersededDocIDs returns the IDs of existing documents in orgID sharing
// filename — the versions a re-upload replaces — and how many documents the
// org has in total (so AddDocument can tell "replacing the only document"
// from "adding to a corpus").
func (s *KnowledgeService) supersededDocIDs(ctx context.Context, orgID, filename string) (ids []string, total int, err error) {
	docs, err := s.store.ListKnowledgeDocuments(ctx, orgID, 0)
	if err != nil {
		return nil, 0, err
	}
	for _, d := range docs {
		if d == nil {
			continue
		}
		total++
		if d.Filename == filename {
			ids = append(ids, d.ID)
		}
	}
	return ids, total, nil
}

func (s *KnowledgeService) Search(ctx context.Context, scope, orgID, query string) (string, error) {
	provider, err := s.embedder(scope, orgID)
	if err != nil {
		return "", err
	}

	// Defense-in-depth: nil store (local mode) → return an empty augmentation
	// rather than nil-panic at the store dereference. Chat still works, just
	// without org-guideline context. After the embedder check so the cheaper
	// validation surfaces first.
	if s.store == nil {
		return "", nil
	}

	// Scrub the query before it leaves the process. The chat request only
	// ever carries the scrubbed message (chat_context.go scrubs
	// req.UserMessage before every provider call), but ragGuidelines hands
	// this path the RAW message — without this, the embeddings request was
	// the one place a pasted credential reached a provider verbatim (and,
	// under PAD_EMBEDDING_PROVIDER, possibly a different provider than the
	// chat model's). Scrubbed first so the truncation below cannot cut a
	// secret in half and hide it from the patterns.
	query = scrubber.ScrubText(query)

	// Truncate the query before embedding: an oversized message (up to the 10
	// MiB body limit) would otherwise become a multi-million-token embedding
	// request — billed per token and above every provider's per-request cap.
	if utf8.RuneCountInString(query) > maxQueryRunes {
		query = string([]rune(query)[:maxQueryRunes])
	}

	emb, err := provider.Embed(ctx, []string{query})
	if err != nil || len(emb) == 0 {
		return "", err
	}

	// Top-k retrieval: 5 chunks (was hardcoded 3). More context improves
	// answer grounding without a significant token cost (each chunk is ~250
	// tokens). The pgvector query applies a distance threshold to exclude
	// irrelevant chunks.
	chunks, err := s.store.SearchKnowledge(ctx, orgID, emb[0], 5)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	if len(chunks) > 0 {
		sb.WriteString(assembleGuidelines(chunks))
	} else {
		// Dim-mismatch detection: a corpus stranded by an embedding-provider
		// switch returns empty silently (mismatched rows are excluded before
		// transfer) — log + meter so ops sees WHY guidelines stopped instead
		// of a "healthy" empty result.
		if total, _, cerr := s.store.CountKnowledgeChunks(ctx, orgID); cerr == nil && total > 0 {
			logger.Warn("knowledge search returned nothing but the org has chunks — embedding dimension mismatch? re-index",
				"org_id", orgID, "total_chunks", total, "query_dim", len(emb[0]))
			metrics.RecordRAGDimMismatch()
		}
	}

	return sb.String(), nil
}

// ReindexResult reports what ONE re-index request did. NextCursor is set
// when chunks remain after this request (the per-request ceiling,
// maxReindexChunks, was reached, or the request's deadline stopped the walk
// after a page had committed): the client posts again with it as "cursor"
// and the walk continues after the last committed chunk. Truncated mirrors
// NextCursor != nil for clients that predate the cursor (they tell the
// admin to run it again). Chunks and Docs count this request only.
type ReindexResult struct {
	Chunks     int                              `json:"chunks"`
	Docs       int                              `json:"docs"`
	Truncated  bool                             `json:"truncated,omitempty"`
	NextCursor *interfaces.KnowledgeChunkCursor `json:"nextCursor,omitempty"`
}

// ReindexOrg re-embeds an org's whole corpus with the CURRENT embedding
// provider — the recovery path for an embedding-provider/model switch, which
// strands every existing chunk at the old dimension (searches silently return
// nothing). The chunk TEXT is still the source of truth, so no re-chunking
// happens: contents are walked page by page in (doc_id, id) order starting
// strictly after the cursor `after` (zero = the beginning), each page
// re-embedded in provider-safe batches and its embeddings updated — and
// committed — in place, up to maxReindexChunks per request; NextCursor
// reports a remainder and resumes it. Docs in the result is the number of
// distinct DocIDs among this request's chunks. The per-org lock is held for
// one request, not across a resumed run.
//
// Re-indexing switches the WHOLE org corpus to the caller's embedding model,
// and members whose embedder is another model stop retrieving it. Unless
// force is set, a corpus indexed with a different model at the same width is
// therefore refused with ErrEmbeddingModelChanged (checkCorpusModel), and so
// is one indexed at another width while it is still in the vector index; a
// width change against a corpus with NOTHING in the vector index is the
// stranded-corpus recovery and proceeds unforced. Only the first request
// of a run is judged this way; see reindexGuard for a resumed one.
func (s *KnowledgeService) ReindexOrg(ctx context.Context, scope, orgID string, force bool, after interfaces.KnowledgeChunkCursor) (ReindexResult, error) {
	if s.store == nil {
		return ReindexResult{}, fmt.Errorf("knowledge service has no storage backend (cloud mode required)")
	}
	if err := s.checkBudget(ctx, scope, orgID); err != nil {
		return ReindexResult{}, err
	}
	provider, err := s.embedder(scope, orgID)
	if err != nil {
		return ReindexResult{}, fmt.Errorf("embedding provider unavailable: %w", err)
	}
	unlock, err := s.lockOrg(orgID)
	if err != nil {
		return ReindexResult{}, err
	}
	defer unlock()
	if err := s.reindexGuard(ctx, provider, orgID, force, after); err != nil {
		return ReindexResult{}, err
	}

	userID := ""
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		userID = claims.UserID
	}
	var (
		res    ReindexResult
		docs   = map[string]bool{}
		cursor = after
	)
	// stopped ends the walk on a failure. Pages already committed stay
	// converted either way; when the request's own deadline (or the client
	// going away) stopped it after at least one page, the progress is
	// reported with a NextCursor instead of an error, so the client resumes
	// from the last committed chunk rather than paying for it again.
	stopped := func(err error) (ReindexResult, error) {
		if ctx.Err() == nil || res.Chunks == 0 {
			return ReindexResult{}, err
		}
		logger.Warn("knowledge base re-index stopped by the request deadline; resumable",
			"org_id", orgID, "chunks", res.Chunks, "error", err)
		res.Docs = len(docs)
		next := cursor
		res.NextCursor, res.Truncated = &next, true
		return res, nil
	}
	for res.Chunks < maxReindexChunks {
		want := min(reindexPageSize, maxReindexChunks-res.Chunks)
		page, err := s.store.ListKnowledgeChunkContents(ctx, orgID, cursor, want)
		if err != nil {
			return stopped(fmt.Errorf("load chunk contents: %w", err))
		}
		if len(page) == 0 {
			break
		}
		texts := make([]string, len(page))
		for i := range page {
			texts[i] = page[i].Content
		}
		embeddings, err := embedBatched(ctx, provider, texts)
		if err != nil {
			return stopped(fmt.Errorf("%w: %w", ErrEmbeddingFailed, ai.EmbeddingFailureCause(err)))
		}
		if len(embeddings) != len(page) {
			return ReindexResult{}, fmt.Errorf("embedding count mismatch: got %d for %d chunks", len(embeddings), len(page))
		}
		for i := range page {
			page[i].Embedding = embeddings[i]
		}
		// Commits this page on its own (see reindexPageSize).
		if err := s.store.UpdateKnowledgeChunkEmbeddings(ctx, userID, page); err != nil {
			return stopped(fmt.Errorf("update embeddings: %w", err))
		}
		for i := range page {
			docs[page[i].DocID] = true
		}
		res.Chunks += len(page)
		last := page[len(page)-1]
		cursor = interfaces.KnowledgeChunkCursor{DocID: last.DocID, ID: last.ID}
		if len(page) < want {
			break
		}
	}
	res.Docs = len(docs)
	if res.Chunks >= maxReindexChunks {
		// At the ceiling: resumable only when something is actually left (a
		// corpus of exactly the ceiling is complete). A probe that fails
		// hands out the cursor anyway — the next request finds out.
		if rest, err := s.store.ListKnowledgeChunkContents(ctx, orgID, cursor, 1); err != nil || len(rest) > 0 {
			next := cursor
			res.NextCursor, res.Truncated = &next, true
		}
	}
	if res.Chunks == 0 {
		return ReindexResult{}, nil // nothing (left) to re-index
	}
	logger.Info("knowledge base re-indexed", "org_id", orgID, "chunks", res.Chunks, "docs", res.Docs,
		"resumed", !after.IsZero(), "more", res.NextCursor != nil)
	return res, nil
}

// reindexGuard is the embedding-model mismatch guard of one re-index
// request. The FIRST request (zero cursor) runs checkCorpusModel unless
// forced — the admin's decision to switch the corpus to their model.
//
// A resumed request (non-zero cursor) must not re-run that guard: the corpus
// is now deliberately mixed (converted up to the cursor, the old model
// after it), so the worst-pair rule would refuse every continuation, and
// honouring force alone would let a caller on yet another model continue a
// switch it did not start, leaving the converted part on a model nobody
// uses. Instead it asks the question that matters for a continuation: did
// the caller's model produce the chunks the previous request converted?
// The nearest chunks at or before the cursor are re-embedded and compared
// (compareSamples). Same model: continue — the first request's decision
// (forced or not) carries over. Another model or width: refuse with
// ErrEmbeddingModelChanged whatever force says; a forced switch to that
// model restarts from the top without a cursor. When nothing converted is
// left to compare (the documents were deleted in between, or only
// undefined all-zero vectors remain) the continuation is judged like a
// first request.
func (s *KnowledgeService) reindexGuard(ctx context.Context, provider ai.Provider, orgID string, force bool, after interfaces.KnowledgeChunkCursor) error {
	if !after.IsZero() {
		samples, err := s.store.SampleKnowledgeChunkEmbeddingsUpTo(ctx, orgID, after, corpusSampleSize)
		if err != nil {
			return fmt.Errorf("sample re-indexed chunks: %w", err)
		}
		verdict, err := compareSamples(ctx, provider, orgID, samples)
		if err != nil {
			return err
		}
		switch verdict {
		case corpusSameModel:
			return nil
		case corpusOtherModel, corpusOtherWidth:
			return ErrEmbeddingModelChanged
		}
	}
	if force {
		return nil
	}
	verdict, err := s.checkCorpusModel(ctx, provider, orgID)
	if err != nil {
		return err
	}
	if verdict == corpusOtherModel || (verdict == corpusOtherWidth && !s.corpusStranded(ctx, orgID)) {
		return ErrEmbeddingModelChanged
	}
	return nil
}

// assembleGuidelines renders the retrieved chunks as the guidelines block
// appended to chat context: a grounding instruction (advisory — the flow's
// actual code wins; cite sources), per-chunk source attribution, and the
// chunking overlap de-duplicated (adjacent chunks from one document share the
// carried tail verbatim; emitting both would duplicate that passage).
func assembleGuidelines(chunks []interfaces.KnowledgeChunk) string {
	var sb strings.Builder
	sb.WriteString("\n**Relevant Organizational Guidelines** (apply where relevant; cite the source file when you rely on them):\n")
	for _, c := range chunks {
		content := stripOverlapPrefix(c.Content, c.DocID, chunks)
		if strings.TrimSpace(content) == "" {
			continue // fully duplicated by a neighbour — nothing new to add
		}
		if c.Filename != "" {
			fmt.Fprintf(&sb, "- [%s] %s\n", c.Filename, content)
		} else {
			fmt.Fprintf(&sb, "- %s\n", content)
		}
	}
	return sb.String()
}

// embedBatched embeds items in provider-safe batches, concatenating results
// in order. See embedBatchSize for why single-shot Embed fails on large
// documents.
func embedBatched(ctx context.Context, provider ai.Provider, items []string) ([][]float32, error) {
	var out [][]float32
	for start := 0; start < len(items); start += embedBatchSize {
		end := start + embedBatchSize
		if end > len(items) {
			end = len(items)
		}
		batch, err := provider.Embed(ctx, items[start:end])
		if err != nil {
			return nil, err
		}
		if len(batch) != end-start {
			return nil, fmt.Errorf("embedding batch returned %d results for %d inputs", len(batch), end-start)
		}
		out = append(out, batch...)
	}
	return out, nil
}

// minOverlapRunes is the shortest carried-overlap tail stripOverlapPrefix will
// remove. Shorter matches are as likely to be coincidental repeated phrases
// (common in policy documents: "must", section headers) as genuine chunk
// overlap; the 15% chunk overlap at size 1000 is 150 runes, well above this.
const minOverlapRunes = 60

// stripOverlapPrefix removes the chunking overlap from a retrieved chunk when
// a neighbouring chunk from the SAME document is also in the result set:
// chunk N+1 begins with the exact tail of chunk N (chunkText carries it
// verbatim), so retrieving both duplicates that passage in the assembled
// context. Exact-string matching, same-DocID scoping, longest match first.
// A chunk fully contained in its neighbour trims to "" (the caller skips it).
func stripOverlapPrefix(content, docID string, all []interfaces.KnowledgeChunk) string {
	runes := []rune(content)
	if len(runes) <= minOverlapRunes {
		return content
	}
	for _, other := range all {
		if other.DocID != docID || other.Content == content {
			continue
		}
		otherRunes := []rune(other.Content)
		maxN := len(runes)
		if len(otherRunes) < maxN {
			maxN = len(otherRunes)
		}
		for n := maxN; n >= minOverlapRunes; n-- {
			if string(otherRunes[len(otherRunes)-n:]) == string(runes[:n]) {
				return string(runes[n:])
			}
		}
	}
	return content
}

// chunkText splits text into chunks of at most size runes (not bytes, so
// multi-byte scripts don't blow past the limit), preferring paragraph and
// sentence boundaries. Invariant: every emitted chunk is at most size runes
// (embeddings providers bill per token and cap per-item length, so an
// oversized chunk would fail or distort the whole indexing batch).
func chunkText(text string, size int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	if utf8.RuneCountInString(text) <= size {
		return []string{text}
	}

	overlapRunes := size * 15 / 100 // 15% overlap preserves context across boundaries

	paragraphs := strings.Split(text, "\n\n")
	var chunks []string
	var current strings.Builder
	currentRunes := 0

	// hasNewContent tracks whether anything was appended since the last
	// flush. A "chunk" holding only the carried overlap tail would duplicate
	// the previous chunk's ending verbatim — pure retrieval noise — so flush
	// refuses to emit it and the final append skips it.
	hasNewContent := false

	flush := func() {
		if !hasNewContent {
			// current holds ONLY the carried overlap (or nothing): emitting it
			// would duplicate the previous chunk's tail. Drop the carry — the
			// caller only flushes when the next unit needs the space, and that
			// unit alone fills the chunk.
			current.Reset()
			currentRunes = 0
			return
		}
		content := current.String()
		chunks = append(chunks, content)

		// Carry the tail of this chunk into the next as overlap so a topic
		// straddling the boundary is retrievable from both chunks.
		if overlapRunes > 0 {
			runes := []rune(content)
			if len(runes) > overlapRunes {
				tail := string(runes[len(runes)-overlapRunes:])
				current.Reset()
				current.WriteString(tail)
				currentRunes = overlapRunes
				hasNewContent = false
				return
			}
		}
		current.Reset()
		currentRunes = 0
		hasNewContent = false
	}

	for _, para := range paragraphs {
		trimmed := strings.TrimSpace(para)
		if trimmed == "" {
			continue
		}
		paraRunes := utf8.RuneCountInString(trimmed)

		// Loop (not a single check): the first flush emits + installs a
		// carry; if the incoming unit STILL doesn't fit alongside the carry,
		// a second flush drops the carry so the unit starts fresh — the only
		// alternative would be emitting chunk+unit > size.
		for currentRunes > 0 && currentRunes+paraRunes+2 > size {
			flush()
		}

		if paraRunes > size {
			sentences := splitSentences(trimmed, size)
			for _, sent := range sentences {
				sentRunes := utf8.RuneCountInString(sent)
				for currentRunes > 0 && currentRunes+sentRunes+1 > size {
					flush()
				}
				if currentRunes > 0 {
					current.WriteByte(' ')
					currentRunes++
				}
				current.WriteString(sent)
				currentRunes += sentRunes
				hasNewContent = true
			}
		} else {
			if currentRunes > 0 {
				current.WriteString("\n\n")
				currentRunes += 2
			}
			current.WriteString(trimmed)
			currentRunes += paraRunes
			hasNewContent = true
		}
	}

	if current.Len() > 0 && hasNewContent {
		chunks = append(chunks, current.String())
	}

	return chunks
}

func splitSentences(text string, size int) []string {
	var sentences []string
	var current strings.Builder
	for _, r := range text {
		current.WriteRune(r)
		if r == '.' || r == '!' || r == '?' {
			sentences = append(sentences, strings.TrimSpace(current.String()))
			current.Reset()
		}
	}
	if current.Len() > 0 {
		remainder := strings.TrimSpace(current.String())
		if remainder != "" {
			sentences = append(sentences, remainder)
		}
	}
	// Terminator-less runs (a 5000-rune paragraph with no '.', a minified
	// blob, one long URL) used to be returned whole and then appended
	// verbatim by the caller — blowing the size invariant. Hard-split every
	// oversized piece on rune boundaries instead.
	out := make([]string, 0, len(sentences))
	for _, sent := range sentences {
		runes := []rune(sent)
		for len(runes) > size {
			out = append(out, string(runes[:size]))
			runes = runes[size:]
		}
		if len(runes) > 0 {
			out = append(out, string(runes))
		}
	}
	return out
}
