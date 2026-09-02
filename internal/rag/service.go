package rag

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/storage/interfaces"
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
	// embedOverride substitutes the resolved embedder when set — a test hook
	// mirroring ChatService's (applyFixFunc etc.); nil in production.
	embedOverride ai.Provider
}

// NewKnowledgeService takes the provider factory and settings store rather than
// a pre-resolved provider so the embedding provider (and its API key) is
// resolved lazily, per request, in the caller's scope. Resolving once at
// startup with an empty scope would fail in cloud mode (keys are per-user) and
// never pick up keys added later without a restart.
func NewKnowledgeService(store interfaces.StorageBackend, factory *ai.ProviderFactory, settings SettingsProvider) *KnowledgeService {
	return &KnowledgeService{store: store, factory: factory, settings: settings}
}

// embedder resolves the embedding provider in the caller's scope. It honours
// the configured EmbeddingProvider (default openai) AND the optional
// EmbeddingModel override (so a deployer can pick e.g. text-embedding-3-large).
//
// Fallback: if the configured embedding provider has no key configured (common
// on a Claude- or Gemini-only deploy that never set EmbeddingProvider), the
// service scans the embedding-CAPABLE providers in a fixed priority order
// (deterministic — a map iteration would pick randomly per call), restricted to
// providers enabled in settings, and uses the first one with a key. The
// capability filter matters: returning the first merely-keyed provider (as an
// earlier version did) let a Claude-only deploy construct a chat-only provider
// that then failed at Embed time with an opaque wrapped 500 instead of the
// machine-readable sentinel the UI explains.
func (s *KnowledgeService) embedder(scope string) (ai.Provider, error) {
	if s.embedOverride != nil {
		return s.embedOverride, nil
	}
	if s.factory == nil {
		return nil, ErrEmbeddingNotConfigured
	}

	providerID := ""
	embeddingModel := ""
	if s.settings != nil {
		if settings := s.settings.Get(); settings != nil {
			providerID = settings.AI.EmbeddingProvider
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
	p, err := s.factory.ForEmbedding(scope, providerID, embeddingModel)
	if err == nil {
		return p, nil
	}

	// Fallback: first enabled, keyed, embedding-capable provider in fixed
	// priority order.
	if s.settings != nil {
		if cfg := s.settings.Get(); cfg != nil {
			for _, pid := range ai.EmbeddingFallbackOrder() {
				if pid == providerID {
					continue // already tried above
				}
				if pc, ok := cfg.AI.Providers[pid]; !ok || !pc.Enabled {
					continue
				}
				if p2, err2 := s.factory.ForEmbedding(scope, pid, embeddingModel); err2 == nil {
					return p2, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("%w (configured %q: %w; no enabled embedding-capable fallback had a key)", ErrEmbeddingUnavailable, providerID, err)
}

// AddDocument chunks content, generates embeddings in the caller's scope, then
// persists the document and its chunks. Embedding happens before the document
// row is written so a failure does not leave an orphan "indexed" document; if
// chunk persistence fails the document row is rolled back best-effort.
func (s *KnowledgeService) AddDocument(ctx context.Context, scope, orgID, filename, content string) error {
	if strings.TrimSpace(content) == "" {
		return fmt.Errorf("document is empty")
	}

	provider, err := s.embedder(scope)
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

	embeddings, err := embedBatched(ctx, provider, chunks)
	if err != nil {
		return fmt.Errorf("generate embeddings: %w", err)
	}
	if len(embeddings) != len(chunks) {
		return fmt.Errorf("embedding count mismatch: got %d for %d chunks", len(embeddings), len(chunks))
	}

	doc := &interfaces.KnowledgeDocument{
		ID:       uuid.NewString(),
		OrgID:    orgID,
		Filename: filename,
	}
	// Replace semantics: re-uploading a filename supersedes the old version
	// (chunks cascade with the document). Before this, both versions were
	// listed AND both were retrieved — stale content kept influencing chat
	// until manually deleted. Deleted AFTER embedding succeeded so an
	// embedding failure never destroys the existing index.
	if err := s.store.DeleteKnowledgeDocumentByName(ctx, orgID, filename); err != nil {
		return fmt.Errorf("replace existing document: %w", err)
	}
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
		// Roll back the orphaned document so it doesn't appear "indexed" with no chunks.
		_ = s.store.DeleteKnowledgeDocument(ctx, orgID, doc.ID)
		return err
	}
	return nil
}

func (s *KnowledgeService) Search(ctx context.Context, scope, orgID, query string) (string, error) {
	provider, err := s.embedder(scope)
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

// ReindexResult reports what a re-index did.
type ReindexResult struct {
	Chunks    int  `json:"chunks"`
	Docs      int  `json:"docs"`
	Truncated bool `json:"truncated,omitempty"`
}

// ReindexOrg re-embeds an org's whole corpus with the CURRENT embedding
// provider — the recovery path for an embedding-provider/model switch, which
// strands every existing chunk at the old dimension (searches silently return
// nothing). The chunk TEXT is still the source of truth, so no re-chunking
// happens: contents are loaded, re-embedded in provider-safe batches, and the
// embeddings updated in place. DocCount in the result is derived from distinct
// DocIDs among the loaded chunks.
func (s *KnowledgeService) ReindexOrg(ctx context.Context, scope, orgID string) (ReindexResult, error) {
	if s.store == nil {
		return ReindexResult{}, fmt.Errorf("knowledge service has no storage backend (cloud mode required)")
	}
	provider, err := s.embedder(scope)
	if err != nil {
		return ReindexResult{}, fmt.Errorf("embedding provider unavailable: %w", err)
	}

	chunks, err := s.store.ListKnowledgeChunkContents(ctx, orgID)
	if err != nil {
		return ReindexResult{}, fmt.Errorf("load chunk contents: %w", err)
	}
	if len(chunks) == 0 {
		return ReindexResult{}, nil // nothing indexed — nothing to re-index
	}
	res := ReindexResult{Chunks: len(chunks)}
	docs := map[string]bool{}
	for _, c := range chunks {
		docs[c.DocID] = true
	}
	res.Docs = len(docs)

	// Detect the store's cap: a second page attempt isn't worth the API —
	// flag via the same bound the loader uses. (The loader's LIMIT equals
	// the constant; a full page means possible truncation.)
	res.Truncated = len(chunks) >= 2000

	texts := make([]string, len(chunks))
	for i := range chunks {
		texts[i] = chunks[i].Content
	}
	embeddings, err := embedBatched(ctx, provider, texts)
	if err != nil {
		return ReindexResult{}, fmt.Errorf("generate embeddings: %w", err)
	}
	if len(embeddings) != len(chunks) {
		return ReindexResult{}, fmt.Errorf("embedding count mismatch: got %d for %d chunks", len(embeddings), len(chunks))
	}
	for i := range chunks {
		chunks[i].Embedding = embeddings[i]
	}

	userID := ""
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		userID = claims.UserID
	}
	if err := s.store.UpdateKnowledgeChunkEmbeddings(ctx, userID, chunks); err != nil {
		return ReindexResult{}, fmt.Errorf("update embeddings: %w", err)
	}
	logger.Info("knowledge base re-indexed", "org_id", orgID, "chunks", res.Chunks, "docs", res.Docs)
	return res, nil
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
