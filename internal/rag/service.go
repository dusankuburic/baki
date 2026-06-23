package rag

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"

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

	paragraphs := strings.Split(text, "\n\n")
	var chunks []string
	var current strings.Builder
	currentRunes := 0

	flush := func() {
		chunks = append(chunks, current.String())
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
