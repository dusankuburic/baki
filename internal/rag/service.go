package rag

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"

	"github.com/google/uuid"
)

// SettingsProvider returns the current application settings.
type SettingsProvider interface {
	Get() *models.AppSettings
}

// maxKnowledgeChunks bounds the number of embedding calls a single document can
// fan out into, so a large upload can't spike cost or trip provider rate limits.
const maxKnowledgeChunks = 500

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
// service scans the settings' enabled providers for the first one that supports
// embeddings. This keeps RAG working on any deployment with at least one
// embedding-capable provider instead of hard-failing on the openai default.
func (s *KnowledgeService) embedder(scope string) (ai.Provider, error) {
	if s.factory == nil {
		return nil, fmt.Errorf("embedding provider not configured")
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
	p, err := s.factory.ForEmbedding(scope, providerID, embeddingModel)
	if err == nil {
		return p, nil
	}

	// Fallback: scan enabled providers for one that can embed. A provider that
	// can't embed returns "not supported" from Embed; we can't know without a key
	// whether it supports embeddings, so we try each configured provider and let
	// the Embed call (later) reject ones that don't. The scan here just needs a
	// constructed provider — i.e. one with a key configured.
	if settings := s.settings; settings != nil {
		if cfg := settings.Get(); cfg != nil {
			for pid, pc := range cfg.AI.Providers {
				if !pc.Enabled || pid == providerID {
					continue
				}
				if p2, err2 := s.factory.ForEmbedding(scope, pid, embeddingModel); err2 == nil {
					return p2, nil
				}
			}
		}
	}
	return nil, fmt.Errorf("embedding provider unavailable (configured %q: %w; no fallback had a key)", providerID, err)
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

	embeddings, err := provider.Embed(ctx, chunks)
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
		sb.WriteString("\n**Relevant Organizational Guidelines:**\n")
		for _, c := range chunks {
			fmt.Fprintf(&sb, "- %s\n", c.Content)
		}
	}

	return sb.String(), nil
}

// chunkText splits text into chunks of at most size runes (not bytes, so
// multi-byte scripts don't blow past the limit), preferring paragraph and
// sentence boundaries.
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

	flush := func() {
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
				return
			}
		}
		current.Reset()
		currentRunes = 0
	}

	for _, para := range paragraphs {
		trimmed := strings.TrimSpace(para)
		if trimmed == "" {
			continue
		}
		paraRunes := utf8.RuneCountInString(trimmed)

		if currentRunes > 0 && currentRunes+paraRunes+2 > size {
			flush()
		}

		if paraRunes > size {
			if currentRunes > 0 {
				flush()
			}
			sentences := splitSentences(trimmed, size)
			for _, sent := range sentences {
				sentRunes := utf8.RuneCountInString(sent)
				if currentRunes > 0 && currentRunes+sentRunes+1 > size {
					flush()
				}
				if currentRunes > 0 {
					current.WriteByte(' ')
					currentRunes++
				}
				current.WriteString(sent)
				currentRunes += sentRunes
			}
		} else {
			if currentRunes > 0 {
				current.WriteString("\n\n")
				currentRunes += 2
			}
			current.WriteString(trimmed)
			currentRunes += paraRunes
		}
	}

	if current.Len() > 0 {
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
	if len(sentences) <= 1 {
		sentences = nil
		runes := []rune(text)
		for len(runes) > 0 {
			end := size
			if end > len(runes) {
				end = len(runes)
			}
			sentences = append(sentences, string(runes[:end]))
			runes = runes[end:]
		}
	}
	return sentences
}
