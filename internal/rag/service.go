package rag

import (
	"context"
	"fmt"
	"strings"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/storage/interfaces"

	"github.com/google/uuid"
)

// SettingsProvider returns the current application settings.
type SettingsProvider interface {
	Get() *models.AppSettings
}

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

func (s *KnowledgeService) embedder(scope string) (ai.Provider, error) {
	if s.factory == nil {
		return nil, fmt.Errorf("embedding provider not configured")
	}

	providerID := "openai" // fallback
	if s.settings != nil {
		if settings := s.settings.Get(); settings != nil && settings.AI.EmbeddingProvider != "" {
			providerID = settings.AI.EmbeddingProvider
		}
	}

	return s.factory.For(scope, providerID)
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

	chunks := chunkText(content, 1000) // ~1000 runes per chunk

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

	if err := s.store.SaveKnowledgeChunks(ctx, knowledgeChunks); err != nil {
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

	emb, err := provider.Embed(ctx, []string{query})
	if err != nil || len(emb) == 0 {
		return "", err
	}

	chunks, err := s.store.SearchKnowledge(ctx, orgID, emb[0], 3)
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

func chunkText(text string, size int) []string {
	var chunks []string
	runes := []rune(text)
	for i := 0; i < len(runes); i += size {
		end := i + size
		if end > len(runes) {
			end = len(runes)
		}
		chunks = append(chunks, string(runes[i:end]))
	}
	return chunks
}
