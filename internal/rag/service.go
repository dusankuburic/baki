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
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/lock"
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
	// (lockOrg; HTTP 409), or a background re-embed runs on some replica (ReindexInBackground).
	ErrKnowledgeBusy = errors.New("the knowledge base is being updated — try again in a moment")
	// ErrEmbeddingOrgCredentialRequired: a background re-embed runs unattended under the organisation's
	// shared embedding credential only; the caller's embedder is their own key (HTTP 400, the org admin's
	// EMBEDDING_ORG_NOT_CONFIGURED guidance). The synchronous re-index stays open to them.
	ErrEmbeddingOrgCredentialRequired = errors.New("a background re-index needs the organisation's own embedding credential — re-index now instead, or ask an org admin to configure a shared embedding provider")
	// ErrReembedUnavailable: the background re-embed could not start — its cross-replica lock could not be
	// asked for, or the server is shutting down (HTTP 503).
	ErrReembedUnavailable = errors.New("the background re-index could not be started — try again in a moment")
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
	// bus announces corpus changes (KnowledgeDocChanged); the corpus count cache drops on them. nil publishes nothing.
	bus *eventbus.Bus
	// jobLeaser is the deployment's cross-replica job lock (WithJobLeaser); nil runs the background re-embed
	// without one, in this process alone.
	jobLeaser lock.Leaser
	// reembed runs the on-demand background re-embed (reembed_job.go); built on first use, under reembedMu
	// with the request the next run takes.
	reembed        *lifecycle.Singleton
	reembedMu      sync.Mutex
	reembedPending *reembedRequest
}

// AzureEmbedderSource resolves the knowledge base's Azure AI Foundry embedder from the model registry:
// the org's designated embedding model when it holds Azure resources (none designated is an
// ai.ErrOrgEmbeddingDeploymentMissing refusal), else the caller's own; failure to decide is ErrEmbeddingSetupUnavailable.
type AzureEmbedderSource interface {
	KnowledgeEmbedder(ctx context.Context, scope, orgID string) (ai.Provider, error)
}

// BudgetChecker is the daily AI budget gate consulted before a billed embedding run (service.AIBudget,
// the one chat turns use).
type BudgetChecker interface {
	CheckBudget(ctx context.Context, scope, orgID string) error
}

// checkBudget refuses a billed run once the org's (and caller's) daily AI budget is spent. A run on
// Azure AI Foundry (ai.UsesModelAliases) is never refused — its usage is neither priced nor counted.
func (s *KnowledgeService) checkBudget(ctx context.Context, embedder ai.Provider, scope, orgID string) error {
	if s.budget == nil || (embedder != nil && ai.UsesModelAliases(embedder.ID())) {
		return nil
	}
	return s.budget.CheckBudget(ctx, scope, orgID)
}

// budgetRun names a billed run's spender on ctx and gives it one usage memo: every embedding call of the run
// (N batches, the wrapper's pre-call hook before each) is charged to the caller IN the org — whichever key the
// chain resolved to, the org's corpus bills the org — and reads today's spend once. A memo already on ctx (a
// chat turn's) is kept.
func budgetRun(ctx context.Context, scope, orgID string) context.Context {
	return ai.WithBudgetScope(ai.WithBudgetMemo(ctx), scope, orgID)
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

// WithBudgetChecker gates billed embedding runs (upload, re-index) on the daily AI budget; without it they
// are unlimited.
func WithBudgetChecker(b BudgetChecker) Option {
	return func(s *KnowledgeService) { s.budget = b }
}

// WithEventBus announces every change of an org's corpus (upload, delete, re-index) on bus.
func WithEventBus(bus *eventbus.Bus) Option {
	return func(s *KnowledgeService) { s.bus = bus }
}

// WithJobLeaser runs the background re-embed (ReindexInBackground) under the deployment's job lease, one run
// across every replica (ADR 0023); without it the process runs it alone.
func WithJobLeaser(l lock.Leaser) Option {
	return func(s *KnowledgeService) { s.jobLeaser = l }
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

// registryEmbedded reports whether pid's embedding model is a designated deployment alias (ai.CapModelAliases,
// Azure AI Foundry): such a provider embeds the corpus only through its registry's designation and only on the
// operator's opt-in, never as a fallback — a second model must never mix into the org corpus.
func registryEmbedded(pid string) bool {
	spec, ok := ai.SpecOf(pid)
	return ok && spec.Caps.Has(ai.CapModelAliases)
}

// defaultEmbeddingProvider is the provider the corpus embeds with when nothing names one: the first
// embedding-capable spec in display order (openai).
func defaultEmbeddingProvider() string {
	if order := ai.EmbeddingFallbackOrder(); len(order) > 0 {
		return order[0]
	}
	return ""
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
		if registryEmbedded(pid) && s.azureEmbedder != nil {
			return s.azureRegistryEmbedder(ctx, scope, orgID)
		}
		if s.factory == nil {
			return nil, ErrEmbeddingNotConfigured
		}
		if orgID != "" && (registryEmbedded(pid) || (s.embeddingProvider != "" && pid == s.embeddingProvider)) {
			return s.factory.ForEmbeddingOrg(ctx, scope, orgID, pid, model)
		}
		return s.factory.ForEmbedding(ctx, scope, pid, model)
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

	// Try the configured embedding provider first, else the first embedding-capable one (openai).
	if providerID == "" {
		providerID = defaultEmbeddingProvider()
	}
	if !ai.SupportsEmbeddings(providerID) {
		// Chat-only provider: fail fast with the sentinel instead of at Embed time.
		return nil, fmt.Errorf("%w (%q does not support embeddings)", ErrEmbeddingUnavailable, providerID)
	}
	var err error
	if registryEmbedded(providerID) && s.embeddingProvider != providerID {
		err = fmt.Errorf("%s embeds the knowledge base only when the operator sets PAD_EMBEDDING_PROVIDER=%s", providerID, providerID)
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
		// The policy could not be read: not a "no key" case, so no other provider may stand in; the sentinel's
		// copy is fixed and the cause was logged where it failed.
		if errors.Is(err, ai.ErrProviderPolicyUnavailable) {
			return nil, err
		}
		// The registry could not say which model embeds: no other provider may stand in on the corpus.
		if registryEmbedded(providerID) && s.azureEmbedder != nil {
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
		if pid == providerID || registryEmbedded(pid) {
			continue // already tried above / opt-in only
		}
		if p2, err2 := s.factory.ForEmbedding(ctx, scope, pid, embeddingModel); err2 == nil {
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
	case errors.Is(err, ai.ErrProviderNotAllowed), errors.Is(err, ai.ErrProviderPolicyUnavailable):
		return err // the policy's answer: 400 PROVIDER_NOT_ALLOWED, or its own fixed copy
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

// checkCorpusModel is the embedding-model mismatch guard. Every chunk records the identity of the embedder
// that produced its vector (ADR 0027), so the guard is a column compare first: a corpus whose every row carries
// the caller's identity is accepted without a provider call, and rows stamped with another identity are another
// model whatever a sample would say. Equal width does not mean equal model (text-embedding-3-small and an Azure
// ada-002 deployment are both 1536 wide), so the rows without an identity — legacy rows from before the column,
// and every row for a provider that names none — still take the sample proof: up to corpusSampleSize indexed
// chunks (one per document) are re-embedded and compared with their stored vectors; the WORST same-width pair
// decides, so a mixed corpus (partial re-index, race) refuses everyone until a forced re-index repairs it; no
// same-width pair is corpusOtherWidth (corpusStranded). A proof over legacy rows stamps them with the caller's
// identity: the lenient match ends for the org, and the next upload is a column compare.
func (s *KnowledgeService) checkCorpusModel(ctx context.Context, provider ai.Provider, orgID string) (corpusVerdict, error) {
	identity := ai.EmbeddingIdentity(provider)
	var counts interfaces.KnowledgeChunkCounts
	if identity != "" {
		var err error
		if counts, err = s.store.CountKnowledgeChunksByIdentity(ctx, orgID, identity); err != nil {
			return corpusEmpty, fmt.Errorf("count indexed chunks: %w", err)
		}
		switch {
		case counts.Total == 0:
			return corpusEmpty, nil
		case counts.Stranded == 0 && counts.Legacy() == 0:
			return corpusSameModel, nil
		}
	}
	samples, err := s.store.SampleKnowledgeChunkEmbeddings(ctx, orgID, corpusSampleSize)
	if err != nil {
		return corpusEmpty, fmt.Errorf("sample indexed chunks: %w", err)
	}
	verdict, err := compareSamples(ctx, provider, orgID, samples)
	if err != nil || identity == "" {
		return verdict, err
	}
	if counts.Stranded > 0 {
		// The sample only says whether the other identity is another WIDTH (the stranded-corpus recovery);
		// at the same width it is another model, also when the sample happened to pick the matching rows.
		if verdict == corpusOtherWidth {
			return corpusOtherWidth, nil
		}
		logger.Warn("knowledge base: chunks were indexed under another embedding identity than the caller's",
			"org_id", orgID, "stranded_chunks", counts.Stranded, "total_chunks", counts.Total)
		return corpusOtherModel, nil
	}
	if verdict == corpusSameModel {
		userID := ""
		if claims := auth.ClaimsFromContext(ctx); claims != nil {
			userID = claims.UserID
		}
		if n, err := s.store.StampKnowledgeChunkIdentity(ctx, userID, orgID, "", identity); err != nil {
			logger.Warn("knowledge base: could not stamp the legacy chunks with the proven identity", "org_id", orgID, "error", err)
		} else if n > 0 {
			logger.Info("knowledge base: legacy chunks stamped with the proven embedding identity", "org_id", orgID, "chunks", n)
		}
	}
	return verdict, nil
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
	ctx = budgetRun(ctx, scope, orgID)
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
	var docID string
	defer func() {
		s.bus.Publish(ctx, events.KnowledgeDocChanged{OrgID: orgID, DocID: docID, Kind: events.KnowledgeDocAdded})
	}()

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

	identity := ai.EmbeddingIdentity(provider)
	doc := &interfaces.KnowledgeDocument{
		ID:             uuid.NewString(),
		OrgID:          orgID,
		Filename:       filename,
		ChunkerVersion: chunkerVersion,
	}
	docID = doc.ID
	// ORDERING: write the new version FIRST, delete the old only once the new document AND its chunks
	// have committed — deleting first meant a failure left the org with NEITHER version. No cross-call
	// transaction exists, so write-new-then-delete-old makes a failed re-upload a no-op.
	if err := s.store.SaveKnowledgeDocument(ctx, userID, doc); err != nil {
		return err
	}

	knowledgeChunks := make([]interfaces.KnowledgeChunk, len(chunks))
	for i := range chunks {
		knowledgeChunks[i] = interfaces.KnowledgeChunk{
			ID:                uuid.NewString(),
			DocID:             doc.ID,
			OrgID:             orgID,
			Content:           chunks[i],
			Embedding:         embeddings[i],
			EmbeddingIdentity: identity,
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

// ListDocuments returns up to limit of the org's documents (<=0 = all), newest first, skipping offset, each with
// the number of its chunks the caller's searches would never return (EmbeddingStranded: embedded under another
// identity than the caller's embedder's). A caller without an embedder has nothing to compare with and sees none.
func (s *KnowledgeService) ListDocuments(ctx context.Context, scope, orgID string, limit, offset int) ([]*interfaces.KnowledgeDocument, error) {
	docs, err := s.store.ListKnowledgeDocuments(ctx, orgID, limit, offset)
	if err != nil || len(docs) == 0 {
		return docs, err
	}
	identity := s.viewerIdentity(ctx, scope, orgID)
	if identity == "" {
		return docs, nil
	}
	stranded, err := s.store.CountStrandedKnowledgeChunksByDocument(ctx, orgID, identity)
	if err != nil {
		logger.Warn("knowledge: could not count the documents' stranded chunks", "org_id", orgID, "error", err)
		return docs, nil
	}
	for _, d := range docs {
		d.EmbeddingStranded = stranded[d.ID]
	}
	return docs, nil
}

// viewerIdentity is the embedding identity of the caller's embedder, "" when none resolves (no key, a provider
// that names none): resolving one reads credentials and settings, never the provider.
func (s *KnowledgeService) viewerIdentity(ctx context.Context, scope, orgID string) string {
	if s.embedOverride == nil && s.factory == nil && s.azureEmbedder == nil {
		return ""
	}
	// Outside a run nothing seeds the budget scope: name the org for the policy's reads here.
	provider, err := s.embedderFor(ai.WithBudgetScope(ctx, scope, orgID), scope, orgID)
	if err != nil {
		return ""
	}
	return ai.EmbeddingIdentity(provider)
}

// DeleteDocument removes one of the org's documents (its chunks cascade), RLS-scoped to userID, then announces
// it, so the next chat turn re-checks the org's chunk count rather than trust the cached "non-empty".
func (s *KnowledgeService) DeleteDocument(ctx context.Context, userID, orgID, docID string) error {
	if err := s.store.DeleteKnowledgeDocument(ctx, userID, orgID, docID); err != nil {
		return err
	}
	s.bus.Publish(ctx, events.KnowledgeDocChanged{OrgID: orgID, DocID: docID, Kind: events.KnowledgeDocDeleted})
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
	ctx = budgetRun(ctx, scope, orgID)
	provider, err := s.embedderFor(ctx, scope, orgID)
	if err != nil {
		return "", err
	}
	// The query embed is billed like any other: gated here, explicitly, not only by the chat turn's own check
	// (inside a chat turn the read is the turn's memoised one).
	if err := s.checkBudget(ctx, provider, scope, orgID); err != nil {
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
	// applies a distance threshold to exclude irrelevant chunks. Only chunks this embedder produced
	// (or legacy rows of unknown origin) are candidates: another model's vectors at the same width
	// are noise cosine cannot tell from a miss.
	identity := ai.EmbeddingIdentity(provider)
	chunks, err := s.store.SearchKnowledge(ctx, orgID, vec, 5, identity)
	if err != nil {
		return "", err
	}

	var sb strings.Builder
	if len(chunks) > 0 {
		sb.WriteString(assembleGuidelines(chunks))
	} else {
		s.explainEmptySearch(ctx, orgID, identity, len(vec))
	}

	return sb.String(), nil
}

// explainEmptySearch tells an empty result's reasons apart for ops, since a corpus stranded by an embedding
// switch returns empty silently: chunks the caller's identity cannot see (a model switch at the same width)
// and a corpus with nothing in the vector index at the configured width (a width switch) are each logged and
// metered on their own counter; a corpus that simply has nothing above the relevance floor is a miss, not a
// signal (R4).
func (s *KnowledgeService) explainEmptySearch(ctx context.Context, orgID, identity string, queryDim int) {
	counts, err := s.store.CountKnowledgeChunksByIdentity(ctx, orgID, identity)
	if err != nil || counts.Total == 0 {
		return
	}
	switch {
	case counts.Stranded > 0:
		logger.Warn("knowledge search returned nothing: chunks were indexed under another embedding model than the caller's — re-index",
			"org_id", orgID, "stranded_chunks", counts.Stranded, "total_chunks", counts.Total)
		recordRAGStrandedSearch()
	case counts.VectorIndexed == 0:
		logger.Warn("knowledge search returned nothing but the org has chunks — embedding dimension mismatch? re-index",
			"org_id", orgID, "total_chunks", counts.Total, "query_dim", queryDim)
		recordRAGDimMismatch()
	}
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
// unforced. Only the first request of a run is judged; see reindexGuard. Unforced, the walk skips the chunks
// already embedded under the caller's identity (ADR 0027): a repeated or restarted run never re-buys a converted
// page, and a corpus the proof just stamped has nothing left to convert; forced, every chunk is rewritten — the
// admin's word that the vectors must change even where the identity would not (an Azure deployment re-pointed
// behind the same name). Every converted chunk is stamped with the identity it was embedded under.
func (s *KnowledgeService) ReindexOrg(ctx context.Context, scope, orgID string, force bool, after interfaces.KnowledgeChunkCursor) (ReindexResult, error) {
	return s.reindexOrg(ctx, scope, orgID, force, after, false)
}

// reindexOrg is ReindexOrg; with orgCredential the page runs only on the organisation's shared credential
// (ErrEmbeddingOrgCredentialRequired otherwise) — the background job's rule, re-checked on every page because
// every page resolves the embedder anew and the member-first fallback would otherwise continue on the starting
// admin's personal key once the org's is gone.
func (s *KnowledgeService) reindexOrg(ctx context.Context, scope, orgID string, force bool, after interfaces.KnowledgeChunkCursor, orgCredential bool) (ReindexResult, error) {
	if s.store == nil {
		return ReindexResult{}, fmt.Errorf("knowledge service has no storage backend (cloud mode required)")
	}
	ctx = budgetRun(ctx, scope, orgID)
	provider, err := s.embedderFor(ctx, scope, orgID)
	if err != nil {
		return ReindexResult{}, fmt.Errorf("embedding provider unavailable: %w", err)
	}
	if orgCredential && !s.onOrgCredential(provider) {
		return ReindexResult{}, ErrEmbeddingOrgCredentialRequired
	}
	if err := s.checkBudget(ctx, provider, scope, orgID); err != nil {
		return ReindexResult{}, err
	}
	unlock, err := s.lockOrg(orgID)
	if err != nil {
		return ReindexResult{}, err
	}
	defer unlock()
	defer s.bus.Publish(ctx, events.KnowledgeDocChanged{OrgID: orgID, Kind: events.KnowledgeDocReindexed})
	if err := s.reindexGuard(ctx, provider, orgID, force, after); err != nil {
		return ReindexResult{}, err
	}

	userID := ""
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		userID = claims.UserID
	}
	identity := ai.EmbeddingIdentity(provider)
	skip := identity
	if force {
		skip = ""
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
		page, err := s.store.ListKnowledgeChunkContents(ctx, orgID, cursor, want, skip)
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
			page[i].EmbeddingIdentity = identity
			page[i].OrgID = orgID
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
		if rest, err := s.store.ListKnowledgeChunkContents(ctx, orgID, cursor, 1, skip); err != nil || len(rest) > 0 {
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
	// chunkerVersion names this geometry on every document it splits (knowledge_documents.chunker_version,
	// ADR 0027). A document is chunked once — the original text is not kept — so a change to the geometry
	// below cannot be applied to stored documents; it must bump the version (TestChunkText_GoldenGeometry pins
	// the split) so that stripOverlapPrefix can tell which overlap a retrieved chunk carries.
	chunkerVersion      = 1
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
