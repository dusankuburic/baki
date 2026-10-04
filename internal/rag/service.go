package rag

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
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

// Embedding-provider sentinels. Exported so the HTTP layer can map them to distinct machine-readable error codes.
var (
	ErrEmbeddingNotConfigured = errors.New("embedding provider not configured")
	ErrEmbeddingUnavailable   = errors.New("embedding provider unavailable")
	// ErrEmbeddingSetupUnavailable: which model embeds the knowledge base could not be decided right
	// now (registry/key store unreadable, legacy import pending). Never a reason to embed with another model.
	ErrEmbeddingSetupUnavailable = errors.New("couldn't load the knowledge base's embedding configuration — try again in a moment")
	// ErrEmbeddingModelChanged: the caller's model is not the one the corpus was indexed with
	// (checkCorpusModel). Upload refuses; re-index refuses unless forced (HTTP 409).
	ErrEmbeddingModelChanged = errors.New("your embedding model differs from the one this knowledge base was indexed with")
	// ErrEmbeddingFailed: the embedding provider's request failed while indexing; wraps the
	// provider's cause (ai.EmbeddingFailureCause) so the HTTP layer can classify it, not mask a 500.
	ErrEmbeddingFailed = errors.New("embedding provider request failed")
	// ErrDocumentTooLarge: the document chunks to more passages than one upload may index
	// (maxKnowledgeChunks) — the byte cap alone cannot track how a text chunks (HTTP 413).
	ErrDocumentTooLarge = errors.New("document too large to index")
	// ErrKnowledgeBusy: another upload or re-index of the same org is in flight in this process
	// (lockOrg; HTTP 409).
	ErrKnowledgeBusy = errors.New("the knowledge base is being updated — try again in a moment")
)

// azureFoundryProviderID is the provider that embeds the knowledge base only on the operator's opt-in (PAD_EMBEDDING_PROVIDER; see embedder).
const azureFoundryProviderID = "azure-foundry"

// Mismatch-guard tuning (checkCorpusModel): the same model re-embedding the same text scores ~0.999,
// two different models' vectors are unrelated (~0); the WORST same-width pair decides, so a mixed corpus is caught.
const (
	corpusSampleSize   = 3
	sameModelMinCosine = 0.95
)

// Re-index paging (ReindexOrg). One page is five embed batches and commits on its own (the re-index
// route runs outside the request-long RLS transaction), so a failure keeps every converted page; beyond
// the chunk ceiling or wall-clock budget the walk answers NextCursor and the client resumes. Vars for tests.
var (
	reindexPageSize  = 500
	maxReindexChunks = 10000
	// reindexRequestBudget bounds one re-index request's WALL CLOCK, checked after each committed
	// page: the Azure Container Apps ingress caps every request at 240 s with no knob; a page starting
	// just under the budget may still spend embed retries.
	reindexRequestBudget = 90 * time.Second
)

// SettingsProvider returns the current application settings.
type SettingsProvider interface {
	Get() *models.AppSettings
}

// maxKnowledgeChunks bounds the passages a single document can fan out into, so a large upload can't
// spike cost or trip rate limits; 2500 covers the advertised 1 MiB upload at its densest chunking.
const maxKnowledgeChunks = 2500

// embedBatchSize caps the input items per /embeddings request: providers cap batch size, and without
// batching a document with more chunks than the cap fails to index after chunking succeeded.
const embedBatchSize = 100

// maxQueryRunes caps the chat query sent to the embeddings API: the request body limit alone would
// allow a multi-million-token billed request that also trips rate limits; 4000 runes is cheap to embed.
const maxQueryRunes = 4000

type KnowledgeService struct {
	store    interfaces.StorageBackend
	factory  *ai.ProviderFactory
	settings SettingsProvider
	// embeddingProvider is the operator's deployment-wide pin (PAD_EMBEDDING_PROVIDER); "" = none.
	embeddingProvider string
	// embedOverride substitutes the resolved embedder when set — a test hook; nil in production.
	embedOverride ai.Provider
	// orgLocks serialises corpus mutations per org (lockOrg): orgID → *sync.Mutex.
	orgLocks sync.Map
	// budget gates billed embedding runs on the daily AI budget; nil = unlimited (local mode, tests).
	budget BudgetChecker
	// azureEmbedder resolves the Azure AI Foundry embedder from the registry's "Use for knowledge
	// base" designation (WithAzureEmbedder); nil keeps the single-document path (ForEmbeddingOrg).
	azureEmbedder AzureEmbedderSource
	// search holds Search's caches (per-org corpus sizes, query embeddings); built on first use.
	search     *searchCaches
	searchOnce sync.Once
}

// AzureEmbedderSource resolves the knowledge base's Azure AI Foundry embedder from the model registry:
// the org's designated embedding model when it holds Azure resources (none designated is an
// ai.ErrOrgEmbeddingDeploymentMissing refusal), else the caller's own; failure to decide is ErrEmbeddingSetupUnavailable.
type AzureEmbedderSource interface {
	KnowledgeEmbedder(ctx context.Context, scope, orgID string) (ai.Provider, error)
}

// BudgetChecker is the daily AI budget gate consulted before a billed embedding run (wired in di).
type BudgetChecker interface {
	CheckBudget(ctx context.Context, scope, orgID string) error
}

// SetBudgetChecker wires the daily budget gate; set once at boot, before requests are served.
func (s *KnowledgeService) SetBudgetChecker(b BudgetChecker) { s.budget = b }

// checkBudget refuses a billed run once the org's (and caller's) daily AI budget is spent. A run on
// Azure AI Foundry (ai.UsesModelAliases) is never refused — its usage is neither priced nor counted.
func (s *KnowledgeService) checkBudget(ctx context.Context, embedder ai.Provider, scope, orgID string) error {
	if s.budget == nil || (embedder != nil && ai.UsesModelAliases(embedder.ID())) {
		return nil
	}
	return s.budget.CheckBudget(ctx, scope, orgID)
}

// Option configures a KnowledgeService at construction.
type Option func(*KnowledgeService)

// WithEmbeddingProvider pins the provider that embeds the org knowledge base (PAD_EMBEDDING_PROVIDER);
// it wins over the settings' AI.EmbeddingProvider and is the only way the knowledge base uses Foundry.
func WithEmbeddingProvider(providerID string) Option {
	return func(s *KnowledgeService) { s.embeddingProvider = providerID }
}

// WithEmbedder makes every request embed with p instead of resolving a provider per caller (tests, single-provider tools).
func WithEmbedder(p ai.Provider) Option {
	return func(s *KnowledgeService) { s.embedOverride = p }
}

// WithAzureEmbedder embeds with Azure AI Foundry through the model registry's designation when the
// operator pins the embedding provider to it; a nil source keeps the single-document path.
func WithAzureEmbedder(source AzureEmbedderSource) Option {
	return func(s *KnowledgeService) { s.azureEmbedder = source }
}

// AzureEmbedderEnabled reports whether the knowledge base embeds with Azure AI Foundry through the
// model registry here (operator pin plus registry).
func (s *KnowledgeService) AzureEmbedderEnabled() bool {
	return s != nil && s.embeddingProvider == azureFoundryProviderID && s.azureEmbedder != nil
}

// NewKnowledgeService takes the factory and settings store rather than a pre-resolved provider so the
// embedding provider (and its key) is resolved lazily, per request, in the caller's scope — keys are per-user in cloud mode.
func NewKnowledgeService(store interfaces.StorageBackend, factory *ai.ProviderFactory, settings SettingsProvider, opts ...Option) *KnowledgeService {
	s := &KnowledgeService{store: store, factory: factory, settings: settings}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// embedder resolves the embedding provider in the caller's scope: the operator's pin (PAD_EMBEDDING_PROVIDER), else the configured provider (default openai) + model override, else the first keyed embedding-capable provider in fixed order.
// Azure AI Foundry is opt-in only, never a fallback (would mix a second model into the org corpus) and uses the org's shared credential (ForEmbeddingOrg); WithAzureEmbedder makes it the registry's designated model.
func (s *KnowledgeService) embedder(scope, orgID string) (ai.Provider, error) {
	return s.embedderFor(context.Background(), scope, orgID)
}

// embedderFor is embedder with the request's context (the registry reads join its transaction).
func (s *KnowledgeService) embedderFor(ctx context.Context, scope, orgID string) (ai.Provider, error) {
	if s.embedOverride != nil {
		return s.embedOverride, nil
	}
	if s.factory == nil && s.azureEmbedder == nil {
		return nil, ErrEmbeddingNotConfigured
	}
	forEmbedding := func(pid, model string) (ai.Provider, error) {
		if pid == azureFoundryProviderID && s.azureEmbedder != nil {
			return s.azureRegistryEmbedder(ctx, scope, orgID)
		}
		if s.factory == nil {
			return nil, ErrEmbeddingNotConfigured
		}
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
		// Chat-only provider: fail fast with the sentinel instead of at Embed time.
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
		// The org's shared credential names no embedding deployment (ai.ErrOrgEmbeddingDeploymentMissing):
		// the fallback scan would silently seed the ORG's corpus from a member's personal key — refuse with the sentinel.
		if errors.Is(err, ai.ErrOrgEmbeddingDeploymentMissing) {
			return nil, fmt.Errorf("%w: %w", ErrEmbeddingUnavailable, err)
		}
		// The registry could not say which model embeds: no other provider may stand in on the corpus.
		if providerID == azureFoundryProviderID && s.azureEmbedder != nil {
			if refusal := azureEmbedderRefusal(err); refusal != nil {
				return nil, refusal
			}
		}
	}
	if s.factory == nil {
		return nil, fmt.Errorf("%w (configured %q: %w)", ErrEmbeddingUnavailable, providerID, err)
	}

	// Fallback: first keyed, embedding-capable provider in fixed priority order; having a usable key
	// (which ForEmbedding decides) is the whole test. Azure AI Foundry is never a fallback.
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

// azureEmbedderRefusal classifies a failure of the registry's embedder source: nil lets the caller look
// further; a designated resource whose settings no longer validate is ErrEmbeddingUnavailable (400);
// anything else (registry/key store unreadable, legacy import pending) is ErrEmbeddingSetupUnavailable.
func azureEmbedderRefusal(err error) error {
	switch {
	case errors.Is(err, ErrEmbeddingSetupUnavailable):
		return err // the source logged its cause and masked it
	case errors.Is(err, ai.ErrEmbeddingDeploymentMissing), errors.Is(err, ai.ErrKeyNotConfigured):
		return nil
	case errors.Is(err, ai.ErrProviderConfigInvalid):
		return fmt.Errorf("%w: %w", ErrEmbeddingUnavailable, err)
	}
	logger.Warn("knowledge: the Azure AI Foundry embedder could not be resolved", "error", err)
	return ErrEmbeddingSetupUnavailable
}

// azureRegistryEmbedder is the pinned Azure AI Foundry embedder from the model registry; no provider
// and no error counts as not configured.
func (s *KnowledgeService) azureRegistryEmbedder(ctx context.Context, scope, orgID string) (ai.Provider, error) {
	p, err := s.azureEmbedder.KnowledgeEmbedder(ctx, scope, orgID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("%s: %w", azureFoundryProviderID, ai.ErrKeyNotConfigured)
	}
	return p, nil
}

// corpusVerdict is what checkCorpusModel learned about the caller's model.
type corpusVerdict int

const (
	corpusEmpty      corpusVerdict = iota // nothing comparable indexed — nothing to disagree with
	corpusSameModel                       // every same-width sample reproduced its stored vector
	corpusOtherModel                      // same width, unrelated vectors: another model
	corpusOtherWidth                      // no same-width pair: the caller's width differs from every sampled row
)

// checkCorpusModel is the embedding-model mismatch guard: nothing records which model produced a stored
// vector, and equal width does not mean equal model (text-embedding-3-small and an Azure ada-002 deployment
// are both 1536 wide) — up to corpusSampleSize indexed chunks (one per document) are re-embedded and compared
// with their stored vectors. The WORST same-width pair decides, so a mixed corpus (partial re-index, race)
// refuses everyone until a forced re-index repairs it; no same-width pair is corpusOtherWidth (corpusStranded).
func (s *KnowledgeService) checkCorpusModel(ctx context.Context, provider ai.Provider, orgID string) (corpusVerdict, error) {
	samples, err := s.store.SampleKnowledgeChunkEmbeddings(ctx, orgID, corpusSampleSize)
	if err != nil {
		return corpusEmpty, fmt.Errorf("sample indexed chunks: %w", err)
	}
	return compareSamples(ctx, provider, orgID, samples)
}

// compareSamples re-embeds the sampled chunks' text with provider and classifies against their
// stored vectors (worst same-width pair decides; no same-width pair = corpusOtherWidth).
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

// corpusStranded: nothing in the org's corpus is in the vector index at the deployment's width — the
// state a width change leaves behind, and the only case where a caller at another width is recovering
// the corpus rather than switching it; a store that cannot tell is treated as stranded.
func (s *KnowledgeService) corpusStranded(ctx context.Context, orgID string) bool {
	_, indexed, err := s.store.CountKnowledgeChunks(ctx, orgID)
	return err != nil || indexed == 0
}

// lockOrg serialises corpus mutations per org in this process: an upload landing mid-re-index would carry
// the pre-switch model and be missed by the re-index. Non-blocking (second admin gets ErrKnowledgeBusy);
// Search never takes it. Process-local only — multi-replica needs a database lock (pg_advisory_xact_lock).
func (s *KnowledgeService) lockOrg(orgID string) (func(), error) {
	v, _ := s.orgLocks.LoadOrStore(orgID, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	if !mu.TryLock() {
		return nil, ErrKnowledgeBusy
	}
	return mu.Unlock, nil
}

// cosineSimilarity of two equal-length vectors; ok=false when either is all zeros (undefined).
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

// AddDocument chunks content, embeds in the caller's scope, then persists document + chunks.
// Embedding happens before the document row is written so a failure leaves no orphan "indexed" document.
func (s *KnowledgeService) AddDocument(ctx context.Context, scope, orgID, filename, content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("document is empty")
	}
	// The embedder is resolved first: whether the budget applies depends on it (Foundry is never gated).
	provider, err := s.embedderFor(ctx, scope, orgID)
	if err != nil {
		return fmt.Errorf("embedding provider unavailable: %w", err)
	}
	if err := s.checkBudget(ctx, provider, scope, orgID); err != nil {
		return err
	}

	// Defense-in-depth: a service without a store (nil backend, local mode) must fail cleanly, not panic.
	if s.store == nil {
		return fmt.Errorf("knowledge service has no storage backend (cloud mode required)")
	}

	chunks := chunkText(content, knowledgeChunkRunes)
	if len(chunks) > maxKnowledgeChunks {
		return fmt.Errorf("%w: %d passages exceed the limit of %d", ErrDocumentTooLarge, len(chunks), maxKnowledgeChunks)
	}

	// Taken after the cheap validation, so a bad request never waits on a running re-index.
	unlock, err := s.lockOrg(orgID)
	if err != nil {
		return err
	}
	defer unlock()
	// Whatever the outcome, the org's cached corpus size is stale once this returns.
	defer s.InvalidateCorpus(orgID)

	// Replace semantics: re-uploading a filename supersedes the old version (chunks cascade). Superseded
	// IDs are captured up front and deleted BY ID, because DeleteKnowledgeDocumentByName would also match the new document.
	superseded, total, err := s.supersededDocIDs(ctx, orgID, filename)
	if err != nil {
		return fmt.Errorf("replace existing document: %w", err)
	}

	// Never mix embedding models in one corpus: a document embedded with a different model is unfindable
	// for members on the other model. Checked before embedding so a refused upload costs one small request;
	// no force option — switch models by re-indexing with force first. Replacing every document skips the guard.
	if len(superseded) < total {
		verdict, err := s.checkCorpusModel(ctx, provider, orgID)
		if err != nil {
			return err
		}
		// An upload at another width is never a recovery: it always produces a mixed corpus.
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

	// Extract the caller's userID once: every storage call below is RLS-scoped to them ("" in local mode).
	userID := ""
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		userID = claims.UserID
	}

	doc := &interfaces.KnowledgeDocument{
		ID:       uuid.NewString(),
		OrgID:    orgID,
		Filename: filename,
	}
	// ORDERING: write the new version FIRST, delete the old only once the new document AND its chunks
	// have committed — deleting first meant a failure left the org with NEITHER version. No cross-call
	// transaction exists, so write-new-then-delete-old makes a failed re-upload a no-op.
	if err := s.store.SaveKnowledgeDocument(ctx, userID, doc); err != nil {
		return err
	}

	knowledgeChunks := make([]interfaces.KnowledgeChunk, len(chunks))
	for i := range chunks {
		knowledgeChunks[i] = interfaces.KnowledgeChunk{
			ID:        uuid.NewString(),
			DocID:     doc.ID,
			OrgID:     orgID,
			Content:   chunks[i],
			Embedding: embeddings[i],
		}
	}

	if err := s.store.SaveKnowledgeChunks(ctx, userID, knowledgeChunks); err != nil {
		// Roll back the orphaned document so it doesn't appear "indexed" with no chunks; the
		// previous version is untouched and still serves retrieval.
		_ = s.store.DeleteKnowledgeDocument(ctx, userID, orgID, doc.ID)
		return err
	}

	// The new version is durable; retire the ones it supersedes. A failure here leaves a duplicate,
	// not a gap — the safe direction.
	for _, id := range superseded {
		if err := s.store.DeleteKnowledgeDocument(ctx, userID, orgID, id); err != nil {
			logger.Warn("knowledge: failed to remove superseded document version",
				"orgId", orgID, "filename", filename, "docId", id, "error", err)
		}
	}
	return nil
}

// supersededDocIDs returns the IDs of existing documents in orgID sharing filename (the versions a
// re-upload replaces) and the org's total document count ("replacing the only document" vs "adding").
func (s *KnowledgeService) supersededDocIDs(ctx context.Context, orgID, filename string) (ids []string, total int, err error) {
	return s.store.ListKnowledgeDocumentIDsByFilename(ctx, orgID, filename)
}

func (s *KnowledgeService) Search(ctx context.Context, scope, orgID, query string) (string, error) {
	// An org without a single indexed chunk has nothing to retrieve: without this check every
	// org-flow chat turn would pay a billed embedding call only to find nothing (count is cached briefly).
	if s.store != nil && s.corpusEmpty(ctx, orgID) {
		return "", nil
	}
	provider, err := s.embedderFor(ctx, scope, orgID)
	if err != nil {
		return "", err
	}

	// Defense-in-depth: nil store (local mode) → empty augmentation instead of a panic; chat works
	// without org-guideline context.
	if s.store == nil {
		return "", nil
	}

	// Scrub the query before it leaves the process: ragGuidelines hands this path the RAW message, so
	// the embeddings request was the one place a pasted credential reached a provider verbatim; scrubbed
	// before truncation so a cut cannot hide half a secret from the patterns.
	query = scrubber.ScrubText(query)

	// Truncate before embedding: an oversized message would become a multi-million-token,
	// billed-per-token request above every provider's per-request cap.
	if utf8.RuneCountInString(query) > maxQueryRunes {
		query = string([]rune(query)[:maxQueryRunes])
	}

	vec, ok, err := s.embedQuery(ctx, provider, scope, orgID, query)
	if err != nil || !ok {
		return "", err
	}

	// Top-k retrieval: 5 chunks — more context improves grounding cheaply; the pgvector query
	// applies a distance threshold to exclude irrelevant chunks.
	chunks, err := s.store.SearchKnowledge(ctx, orgID, vec, 5)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	if len(chunks) > 0 {
		sb.WriteString(assembleGuidelines(chunks))
	} else {
		// Dim-mismatch detection: a corpus stranded by an embedding-provider switch returns empty
		// silently — log + meter so ops sees WHY guidelines stopped instead of a "healthy" empty result.
		if total, _, cerr := s.store.CountKnowledgeChunks(ctx, orgID); cerr == nil && total > 0 {
			logger.Warn("knowledge search returned nothing but the org has chunks — embedding dimension mismatch? re-index",
				"org_id", orgID, "total_chunks", total, "query_dim", len(vec))
			metrics.RecordRAGDimMismatch()
		}
	}

	return sb.String(), nil
}

// ReindexResult reports what ONE re-index request did. NextCursor is set when chunks remain — the
// per-request ceiling (maxReindexChunks) or wall-clock budget (reindexRequestBudget) was reached — and
// the client resumes after the last committed chunk, so converted pages are never re-bought. Truncated
// mirrors NextCursor != nil for older clients; Chunks and Docs count this request only.
type ReindexResult struct {
	Chunks     int                              `json:"chunks"`
	Docs       int                              `json:"docs"`
	Truncated  bool                             `json:"truncated,omitempty"`
	NextCursor *interfaces.KnowledgeChunkCursor `json:"nextCursor,omitempty"`
}

// ReindexOrg re-embeds an org's whole corpus with the CURRENT embedding provider — the recovery path for a
// provider/model switch, which strands every chunk at the old dimension. Chunk TEXT stays the source of truth:
// contents are walked keyset-ordered ((doc_id, id), strictly after cursor `after`; zero = the beginning), each
// page re-embedded and committed in place, up to maxReindexChunks per request and never past reindexRequestBudget
// wall clock; NextCursor reports and resumes a remainder, and a request that fails after committing pages returns
// BOTH the partial result and the error. Unless forced, a corpus indexed with a different model at the same width
// is refused (ErrEmbeddingModelChanged), and so is one at another width while it is still in the vector index; a
// width change against a corpus with NOTHING in the vector index is the stranded-corpus recovery and proceeds
// unforced. Only the first request of a run is judged; see reindexGuard.
func (s *KnowledgeService) ReindexOrg(ctx context.Context, scope, orgID string, force bool, after interfaces.KnowledgeChunkCursor) (ReindexResult, error) {
	if s.store == nil {
		return ReindexResult{}, fmt.Errorf("knowledge service has no storage backend (cloud mode required)")
	}
	provider, err := s.embedderFor(ctx, scope, orgID)
	if err != nil {
		return ReindexResult{}, fmt.Errorf("embedding provider unavailable: %w", err)
	}
	if err := s.checkBudget(ctx, provider, scope, orgID); err != nil {
		return ReindexResult{}, err
	}
	unlock, err := s.lockOrg(orgID)
	if err != nil {
		return ReindexResult{}, err
	}
	defer unlock()
	defer s.InvalidateCorpus(orgID)
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
	// stopped ends the walk on a failure. Pages already committed stay converted either way, so EVERY
	// stop after at least one committed page reports a NextCursor: the request deadline answers
	// 200-with-cursor, any other failure returns BOTH the partial result and the error.
	stopped := func(err error) (ReindexResult, error) {
		if res.Chunks == 0 {
			return ReindexResult{}, err
		}
		res.Docs = len(docs)
		next := cursor
		res.NextCursor, res.Truncated = &next, true
		if ctx.Err() == nil {
			logger.Warn("knowledge base re-index stopped after committed pages; resumable",
				"org_id", orgID, "chunks", res.Chunks, "error", err)
			return res, err
		}
		logger.Warn("knowledge base re-index stopped by the request deadline; resumable",
			"org_id", orgID, "chunks", res.Chunks, "error", err)
		return res, nil
	}
	start := time.Now()
	budgetSpent := false
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
			return stopped(fmt.Errorf("embedding count mismatch: got %d for %d chunks", len(embeddings), len(page)))
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
		// The wall-clock budget (reindexRequestBudget): stop after a COMMITTED page — the next
		// page's embed calls may each wait out retry backoffs past the platform's request cap.
		if time.Since(start) >= reindexRequestBudget {
			budgetSpent = true
			break
		}
	}
	res.Docs = len(docs)
	if budgetSpent || res.Chunks >= maxReindexChunks {
		// At the ceiling or the budget: resumable only when something is actually left. A probe
		// that fails hands out the cursor anyway — the next request finds out.
		if rest, err := s.store.ListKnowledgeChunkContents(ctx, orgID, cursor, 1); err != nil || len(rest) > 0 {
			next := cursor
			res.NextCursor, res.Truncated = &next, true
		}
	}
	if res.Chunks == 0 {
		return ReindexResult{}, nil // nothing (left) to re-index
	}
	logger.Info("knowledge base re-indexed", "org_id", orgID, "chunks", res.Chunks, "docs", res.Docs,
		"resumed", !after.IsZero(), "more", res.NextCursor != nil, "budget_stop", budgetSpent)
	return res, nil
}

// reindexGuard is the mismatch guard of one re-index request. The FIRST request (zero cursor) runs
// checkCorpusModel unless forced — the admin's decision to switch the corpus to their model. A resumed
// request must not re-run it: the corpus is deliberately mixed (converted up to the cursor, the old
// model after it) and the worst-pair rule would refuse every continuation, so it instead asks whether
// the caller's model produced the previously converted chunks (compareSamples): same model continues;
// another model or width refuses with ErrEmbeddingModelChanged whatever force says.
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

// assembleGuidelines renders retrieved chunks as the guidelines block appended to chat context: a grounding
// instruction (advisory — the flow's code wins; cite sources), per-chunk source attribution, overlap de-duplicated.
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

// embedConcurrency is how many embedBatched batches are in flight at once: three overlaps round
// trips without opening a burst a low-TPM deployment would throttle.
const embedConcurrency = 3

// embedBatched embeds items in provider-safe batches, results in item order. For a provider that may
// embed concurrently (ai.EmbedsConcurrently) up to embedConcurrency batches run at once; the first failure
// cancels in-flight batches and launches no more. Any other provider runs one batch at a time.
func embedBatched(ctx context.Context, provider ai.Provider, items []string) ([][]float32, error) {
	if !ai.EmbedsConcurrently(provider) || len(items) <= embedBatchSize {
		var out [][]float32
		for start := 0; start < len(items); start += embedBatchSize {
			batch, err := embedOneBatch(ctx, provider, items[start:min(start+embedBatchSize, len(items))])
			if err != nil {
				return nil, err
			}
			out = append(out, batch...)
		}
		return out, nil
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	out := make([][]float32, len(items))
	var (
		mu       sync.Mutex
		firstErr error
		wg       sync.WaitGroup
	)
	fail := func(err error) {
		mu.Lock()
		if firstErr == nil {
			firstErr = err
			cancel()
		}
		mu.Unlock()
	}
	sem := make(chan struct{}, embedConcurrency)
	launchedAll := true
	for start := 0; start < len(items); start += embedBatchSize {
		if ctx.Err() == nil {
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
			}
		}
		if ctx.Err() != nil {
			launchedAll = false
			break // a batch failed (or the caller went away): launch no more
		}
		end := min(start+embedBatchSize, len(items))
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					fail(fmt.Errorf("embedding batch panicked: %v", r))
				}
			}()
			batch, err := embedOneBatch(ctx, provider, items[start:end])
			if err != nil {
				fail(err)
				return
			}
			copy(out[start:end], batch)
		}(start, end)
	}
	wg.Wait()
	if firstErr != nil {
		return nil, firstErr
	}
	if !launchedAll {
		return nil, ctx.Err() // the caller's context ended before every batch launched
	}
	return out, nil
}

// embedOneBatch embeds one provider-safe batch and checks the vector count.
func embedOneBatch(ctx context.Context, provider ai.Provider, items []string) ([][]float32, error) {
	batch, err := provider.Embed(ctx, items)
	if err != nil {
		return nil, err
	}
	if len(batch) != len(items) {
		return nil, fmt.Errorf("embedding batch returned %d results for %d inputs", len(batch), len(items))
	}
	return batch, nil
}

// Chunking geometry: knowledgeChunkRunes is the chunk size AddDocument hands to chunkText;
// chunkOverlapPercent is the share of each chunk carried verbatim into the next.
const (
	knowledgeChunkRunes = 1000
	chunkOverlapPercent = 15
	// chunkOverlapRunes is the exact length of the tail carried from one chunk into the next
	// (150 runes at the sizes above).
	chunkOverlapRunes = knowledgeChunkRunes * chunkOverlapPercent / 100
	// maxOverlapProbeRunes caps the prefix length stripOverlapPrefix tests: a genuine carried overlap
	// is exactly chunkOverlapRunes long; twice that also strips repeated boilerplate at O(1) per pair. It sizes a stack array — keep it small.
	maxOverlapProbeRunes = 2 * chunkOverlapRunes

	// minOverlapRunes is the shortest tail stripOverlapPrefix removes: shorter matches are as
	// likely coincidental repeated phrases as genuine chunk overlap.
	minOverlapRunes = 60
)

// stripOverlapPrefix removes the chunking overlap from a retrieved chunk when a neighbouring chunk from
// the SAME document is also in the result set: chunk N+1 begins with the exact tail of chunk N, so
// retrieving both duplicates that passage. Exact-string match, same-DocID scoping, longest first; a chunk
// fully contained in its neighbour trims to "" (caller skips it). Runs per retrieved chunk on every RAG
// chat turn — must not allocate: prefixes are byte slices at rune boundaries, tested with HasSuffix.
func stripOverlapPrefix(content, docID string, all []interfaces.KnowledgeChunk) string {
	// offs[k] is the byte length of content's k-rune prefix, up to the probe cap; fixed-size (stack).
	var offs [maxOverlapProbeRunes + 1]int
	k, i := 0, 0
	for i < len(content) && k < maxOverlapProbeRunes {
		_, sz := utf8.DecodeRuneInString(content[i:])
		i += sz
		k++
		offs[k] = i
	}
	// k is content's rune count, capped at the probe length.
	if k <= minOverlapRunes {
		return content
	}
	for _, other := range all {
		if other.DocID != docID || other.Content == content {
			continue
		}
		for n := k; n >= minOverlapRunes; n-- {
			if strings.HasSuffix(other.Content, content[:offs[n]]) {
				return content[offs[n]:]
			}
		}
	}
	return content
}

// chunkText splits text into chunks of at most size runes (not bytes, so multi-byte scripts don't blow
// past the limit), preferring paragraph and sentence boundaries. Invariant: every emitted chunk is at
// most size runes (providers bill per token and cap per-item length).
func chunkText(text string, size int) []string {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	if utf8.RuneCountInString(text) <= size {
		return []string{text}
	}

	overlapRunes := size * chunkOverlapPercent / 100 // 15% overlap preserves context across boundaries

	paragraphs := strings.Split(text, "\n\n")
	var chunks []string
	var current strings.Builder
	currentRunes := 0

	// hasNewContent tracks whether anything was appended since the last flush: a "chunk" holding
	// only the carried overlap tail would duplicate the previous chunk's ending verbatim.
	hasNewContent := false

	flush := func() {
		if !hasNewContent {
			// Only the carried overlap (or nothing): drop the carry rather than emit a duplicate tail.
			current.Reset()
			currentRunes = 0
			return
		}
		content := current.String()
		chunks = append(chunks, content)

		// Carry the tail into the next chunk so a topic straddling the boundary is retrievable from both.
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

		// Loop (not a single check): the first flush emits + installs a carry; if the unit STILL
		// doesn't fit alongside it, a second flush drops the carry so the unit starts fresh.
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
	// Terminator-less runs (a 5000-rune paragraph with no '.', a minified blob, one long URL) used
	// to blow the size invariant; hard-split every oversized piece on rune boundaries instead.
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
