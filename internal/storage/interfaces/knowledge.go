package interfaces

import (
	"context"
	"time"
)

// KnowledgeStore covers the org knowledge base (RAG source documents + chunks).
type KnowledgeStore interface {
	// SaveKnowledgeDocument inserts the document row RLS-scoped to userID; the knowledge_documents WITH CHECK policy enforces org membership.
	SaveKnowledgeDocument(ctx context.Context, userID string, doc *KnowledgeDocument) error
	// DeleteKnowledgeDocument removes a document only when it belongs to orgID (one org cannot delete another's).
	DeleteKnowledgeDocument(ctx context.Context, userID, orgID, id string) error
	// DeleteKnowledgeDocumentByName removes every document with the given filename in the org — re-upload REPLACE semantics (chunks cascade).
	DeleteKnowledgeDocumentByName(ctx context.Context, orgID, filename string) error
	// ListKnowledgeDocuments returns up to limit rows (<=0 = all), newest first, skipping offset rows.
	ListKnowledgeDocuments(ctx context.Context, orgID string, limit, offset int) ([]*KnowledgeDocument, error)
	// ListKnowledgeDocumentIDsByFilename returns the ids of the org's documents named filename (versions a re-upload supersedes) plus the org's total doc count.
	ListKnowledgeDocumentIDsByFilename(ctx context.Context, orgID, filename string) (ids []string, total int, err error)
	// SaveKnowledgeChunks inserts chunk rows RLS-scoped to userID; the knowledge_chunks WITH CHECK policy enforces org membership ("" only in non-authenticated local mode).
	SaveKnowledgeChunks(ctx context.Context, userID string, chunks []KnowledgeChunk) error
	SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]KnowledgeChunk, error)
	// ListKnowledgeChunkContents returns up to limit chunks (no embeddings) strictly after the cursor in (doc_id, id) order — one re-index page; limit <= 0 = backend page cap.
	ListKnowledgeChunkContents(ctx context.Context, orgID string, after KnowledgeChunkCursor, limit int) ([]KnowledgeChunk, error)
	// SampleKnowledgeChunkEmbeddings returns up to n chunks WITH stored embeddings, one per document, deterministic order — RAG re-embeds their text as an embedding-model mismatch guard.
	SampleKnowledgeChunkEmbeddings(ctx context.Context, orgID string, n int) ([]KnowledgeChunk, error)
	// SampleKnowledgeChunkEmbeddingsUpTo returns up to n chunks at or before the cursor in (doc_id, id) order NEAREST first WITH stored embeddings — a resumed re-index model check.
	SampleKnowledgeChunkEmbeddingsUpTo(ctx context.Context, orgID string, upTo KnowledgeChunkCursor, n int) ([]KnowledgeChunk, error)
	// UpdateKnowledgeChunkEmbeddings replaces chunk embeddings in place (RLS-scoped like SaveKnowledgeChunks) and refreshes the pgvector column for same-dimension vectors.
	UpdateKnowledgeChunkEmbeddings(ctx context.Context, userID string, chunks []KnowledgeChunk) error
	// CountKnowledgeChunks reports org totals: all chunks and how many are in the pgvector index at the configured dimension (dim-mismatch detection).
	CountKnowledgeChunks(ctx context.Context, orgID string) (total, vectorIndexed int, err error)
}

type KnowledgeDocument struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"orgId"`
	Filename  string    `json:"filename"`
	CreatedAt time.Time `json:"createdAt"`
	// ChunkCount / VectorIndexed are list-view extras: chunks the doc has vs how many are in the pgvector index; chunks>0 with 0 indexed = stranded by an embedding-provider switch.
	ChunkCount    int `json:"chunkCount,omitempty"`
	VectorIndexed int `json:"vectorIndexed,omitempty"`
}

type KnowledgeChunk struct {
	ID    string `json:"id"`
	DocID string `json:"docId"`
	// OrgID is denormalised from the parent document (knowledge_chunks.org_id, v28) so org reads and the RLS policy test the chunk row alone; a chunk saved without it inherits its doc's org.
	OrgID     string    `json:"orgId,omitempty"`
	Content   string    `json:"content"`
	Embedding []float32 `json:"embedding"`
	// Filename of the source document (joined at retrieval time) so RAG can attribute each chunk to its source.
	Filename string `json:"filename,omitempty"`
}

// KnowledgeChunkCursor is a keyset position in an org's chunks ordered by (doc_id, id); zero value = start (TEXT columns, "" sorts first); also the re-index resume token — clients treat it as opaque.
type KnowledgeChunkCursor struct {
	DocID string `json:"docId"`
	ID    string `json:"id"`
}

// IsZero reports whether c is the start-of-corpus cursor.
func (c KnowledgeChunkCursor) IsZero() bool { return c.DocID == "" && c.ID == "" }
