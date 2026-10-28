package interfaces

import (
	"context"
	"time"
)

// KnowledgeStore covers the org knowledge base (RAG source documents + chunks).
type KnowledgeStore interface {
	// SaveKnowledgeDocument inserts the document row RLS-scoped to userID; the knowledge_documents WITH CHECK policy enforces org membership. An organisation that does not exist is ErrOrgNotFound.
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
	// SaveKnowledgeDocumentWithChunks is SaveKnowledgeDocument and SaveKnowledgeChunks in one transaction, an upload's
	// write: a document is never stored without its chunks. Any refusal (a document or chunk id already stored, the
	// RLS policy) writes nothing.
	SaveKnowledgeDocumentWithChunks(ctx context.Context, userID string, doc *KnowledgeDocument, chunks []KnowledgeChunk) error
	// SearchKnowledge ranks the org's chunks at the query's width by cosine similarity. identity restricts the hits to the chunks embedded under it plus the legacy rows (no identity); "" = any identity.
	SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int, identity string) ([]KnowledgeChunk, error)
	// SearchKnowledgeLexical is the lexical half of hybrid retrieval: up to limit of the org's chunks whose text holds one of phrases at a trigram word similarity (pg_trgm word_similarity: the phrase against its best-matching extent of the chunk) at or above the store's floor, those matching more of the phrases first. identity filters as SearchKnowledge's does, a pending chunk is never a hit; no phrases, no hits.
	SearchKnowledgeLexical(ctx context.Context, orgID string, phrases []string, limit int, identity string) ([]KnowledgeChunk, error)
	// ListKnowledgeChunkContents returns up to limit chunks (no embeddings) strictly after the cursor in (doc_id, id) order — one re-index page; limit <= 0 = backend page cap. notIdentity skips the chunks already embedded under that identity (legacy rows are walked); "" walks every chunk.
	ListKnowledgeChunkContents(ctx context.Context, orgID string, after KnowledgeChunkCursor, limit int, notIdentity string) ([]KnowledgeChunk, error)
	// ListUnindexedKnowledgeChunkContents is ListKnowledgeChunkContents over the chunks OUTSIDE the vector index alone, whatever their identity — the rows a width change left as JSON only; a store without a vector index (HasPgvector false) has nothing outside it and returns every chunk.
	ListUnindexedKnowledgeChunkContents(ctx context.Context, orgID string, after KnowledgeChunkCursor, limit int) ([]KnowledgeChunk, error)
	// UnindexedKnowledgeChunkWidth is the stored embedding width of ONE of the org's chunks outside the vector index (any of them: a corpus is stranded at one width); 0 when every chunk is in the index or the store has none.
	UnindexedKnowledgeChunkWidth(ctx context.Context, orgID string) (int, error)
	// HasPgvector reports whether the store has a vector index at all (the pgvector extension and the embedding_vec column): without one every chunk counts as outside it, which says nothing about searchability — searches run in-process at the query's width.
	HasPgvector() bool
	// SampleKnowledgeChunkEmbeddings returns up to n chunks WITH stored embeddings, one per document, deterministic order — RAG re-embeds their text as an embedding-model mismatch guard.
	SampleKnowledgeChunkEmbeddings(ctx context.Context, orgID string, n int) ([]KnowledgeChunk, error)
	// SampleKnowledgeChunkEmbeddingsUpTo returns up to n chunks at or before the cursor in (doc_id, id) order NEAREST first WITH stored embeddings — a resumed re-index model check.
	SampleKnowledgeChunkEmbeddingsUpTo(ctx context.Context, orgID string, upTo KnowledgeChunkCursor, n int) ([]KnowledgeChunk, error)
	// UpdateKnowledgeChunkEmbeddings replaces chunk embeddings and their embedding identity in place (RLS-scoped like SaveKnowledgeChunks) and refreshes the pgvector column for same-dimension vectors.
	UpdateKnowledgeChunkEmbeddings(ctx context.Context, userID string, chunks []KnowledgeChunk) error
	// CountKnowledgeChunks reports org totals: all chunks and how many are in the pgvector index at the configured dimension (dim-mismatch detection).
	CountKnowledgeChunks(ctx context.Context, orgID string) (total, vectorIndexed int, err error)
	// CountKnowledgeChunksByIdentity is CountKnowledgeChunks split by embedding identity: the chunks at identity (current), at another (stranded) and, by difference, the legacy rows; "" names nothing current or stranded.
	CountKnowledgeChunksByIdentity(ctx context.Context, orgID, identity string) (KnowledgeChunkCounts, error)
	// CountStrandedKnowledgeChunksByDocument counts, per document id, the org's chunks embedded under another identity than identity; documents without any are absent, and "" strands nothing.
	CountStrandedKnowledgeChunksByDocument(ctx context.Context, orgID, identity string) (map[string]int, error)
	// StampKnowledgeChunkIdentity rewrites the embedding identity of the org's chunks at identity from ("" = the legacy rows) to to ("" = none), RLS-scoped to userID like UpdateKnowledgeChunkEmbeddings, and reports how many rows changed.
	StampKnowledgeChunkIdentity(ctx context.Context, userID, orgID, from, to string) (int, error)

	// Two-phase uploads, re-chunking and the per-document identity stamp (R-knowledge-2, -9, -3; v51). A pending
	// chunk (KnowledgeChunk.Pending) belongs to an upload not yet finished: stored without an embedding, embedded page
	// by page (UpdateKnowledgeChunkEmbeddings), it is out of the corpus until FinishKnowledgeDocument — no search
	// returns it, embedded or not, and the census, the samples and the re-index pages leave it out.

	// GetKnowledgeDocument is one of the org's documents with its source text (Content, HasSource: a document stored
	// before v51 kept none), its state and its chunk counts (ChunkCount, PendingChunks); ErrNotFound when the org has
	// no document by that id.
	GetKnowledgeDocument(ctx context.Context, orgID, id string) (*KnowledgeDocument, error)
	// ListPendingKnowledgeChunks returns up to limit (<= 0 = the backend's page cap) of the document's pending chunks
	// still without an embedding, in id order strictly after afterID ("" = from the first) — one page of a two-phase
	// upload's embedding.
	ListPendingKnowledgeChunks(ctx context.Context, orgID, docID, afterID string, limit int) ([]KnowledgeChunk, error)
	// FinishKnowledgeDocument completes a two-phase upload, RLS-scoped to userID: when none of the document's chunks
	// lacks its embedding it marks a pending document ready, puts its chunks into the corpus (Pending cleared) and, in
	// the same transaction, deletes the org's other READY documents of its filename (the versions it supersedes). It
	// reports how many chunks are still to embed (0: the document is ready); ErrNotFound when the org has no document
	// by that id.
	FinishKnowledgeDocument(ctx context.Context, userID, orgID, docID string) (pending int, err error)
	// ListKnowledgeDocumentsBelowChunkerVersion returns up to limit (<= 0 = all) of the org's ready documents split with
	// a chunker version below version, in id order strictly after afterID, without their text (HasSource says whether
	// it is kept) — what a rechunk re-index walks.
	ListKnowledgeDocumentsBelowChunkerVersion(ctx context.Context, orgID string, version int, afterID string, limit int) ([]*KnowledgeDocument, error)
	// SampleLegacyKnowledgeChunkEmbeddings returns, for up to docs of the org's documents holding embedded legacy chunks
	// (no identity, not pending; lowest document ids first), the first and the last such chunk by id WITH stored
	// embeddings, in (doc_id, id) order — a document's proof covers both ends of a re-index that stopped inside it.
	SampleLegacyKnowledgeChunkEmbeddings(ctx context.Context, orgID string, docs int) ([]KnowledgeChunk, error)
	// StampKnowledgeDocumentIdentity stamps to on the legacy chunks (no identity, not pending) of the org's documents
	// docIDs alone — the documents a sample proved (ADR 0027 §3) — RLS-scoped to userID, and reports how many rows changed.
	StampKnowledgeDocumentIdentity(ctx context.Context, userID, orgID string, docIDs []string, to string) (int, error)
}

// The states of a knowledge document (knowledge_documents.state, v51). A pending document has chunks still to embed
// (an upload cut short, resumable by posting its id back to the upload route); a ready one has none.
const (
	KnowledgeDocumentPending = "pending"
	KnowledgeDocumentReady   = "ready"
)

// KnowledgePendingTTL is how long an unfinished upload is kept, counted from when it was first stored: the retention
// sweep then deletes it (PurgeResult.KnowledgeUploads) with its chunks and its kept source text. It never served, and
// the ready version it would have replaced still does. Generous, so an upload left over a holiday can still be
// continued; a re-upload of the same text starts the clock again only when the old one was already swept.
const KnowledgePendingTTL = 30 * 24 * time.Hour

// KnowledgePendingPurgeBatch bounds how many unfinished uploads one retention sweep deletes (each holds up to 2 500
// chunks and 1 MiB of source text, deleted in the same statement); the next sweep takes the rest.
const KnowledgePendingPurgeBatch = 25

// KnowledgeChunkCounts is an org's chunk census relative to one embedding identity (ADR 0027).
type KnowledgeChunkCounts struct {
	// Total is every chunk of the org; VectorIndexed those in the pgvector index at the configured width.
	Total, VectorIndexed int
	// Current are the chunks embedded under the identity asked about, Stranded those under another one.
	Current, Stranded int
}

// Legacy is the number of chunks without an identity: rows from before the column, matched by every search.
func (c KnowledgeChunkCounts) Legacy() int { return c.Total - c.Current - c.Stranded }

type KnowledgeDocument struct {
	ID        string    `json:"id"`
	OrgID     string    `json:"orgId"`
	Filename  string    `json:"filename"`
	CreatedAt time.Time `json:"createdAt"`
	// ChunkCount / VectorIndexed are list-view extras: chunks the doc has vs how many are in the pgvector index; chunks>0 with 0 indexed = stranded by an embedding-provider switch.
	ChunkCount    int `json:"chunkCount,omitempty"`
	VectorIndexed int `json:"vectorIndexed,omitempty"`
	// ChunkerVersion is the chunking geometry the document was split with (rag.chunkerVersion; 1 before the column existed). A document is chunked once, at upload.
	ChunkerVersion int `json:"chunkerVersion,omitempty"`
	// EmbeddingStranded is a list-view extra relative to the viewer's embedding identity: chunks embedded under another identity, which that viewer's searches never return; re-index converts them.
	EmbeddingStranded int `json:"embeddingStranded,omitempty"`
	// State is KnowledgeDocumentPending or KnowledgeDocumentReady; PendingChunks the chunks of a pending document
	// still to embed (a list-view extra, like ChunkCount, which counts them too).
	State         string `json:"state,omitempty"`
	PendingChunks int    `json:"pendingChunks,omitempty"`
	// HasSource reports whether the source text is kept (v51): a document stored before cannot be re-chunked.
	HasSource bool `json:"hasSource,omitempty"`
	// Content is the (masked) source text the chunks were cut from; written by an upload, read by
	// GetKnowledgeDocument only, never listed.
	Content string `json:"-"`
}

type KnowledgeChunk struct {
	ID    string `json:"id"`
	DocID string `json:"docId"`
	// OrgID is denormalised from the parent document (knowledge_chunks.org_id, v28) so org reads and the RLS policy test the chunk row alone; a chunk saved without it inherits its doc's org.
	OrgID     string    `json:"orgId,omitempty"`
	Content   string    `json:"content"`
	Embedding []float32 `json:"embedding"`
	// EmbeddingIdentity names the embedder that produced Embedding (ai.EmbeddingIdentity: the frozen label|baseURL|model|dims form); "" is a legacy row (NULL), matched leniently and stamped once an upload or re-index proves the org's model reproduces it (ADR 0027).
	EmbeddingIdentity string `json:"embeddingIdentity,omitempty"`
	// Filename of the source document (joined at retrieval time) so RAG can attribute each chunk to its source.
	Filename string `json:"filename,omitempty"`
	// ChunkerVersion is the source document's (joined at retrieval time by the vector searches): which overlap the
	// chunk carries (rag.stripOverlapPrefix). 0 = unknown, read as 1.
	ChunkerVersion int `json:"chunkerVersion,omitempty"`
	// Pending marks a chunk of a two-phase upload not yet finished (v51): written by SaveKnowledgeChunks (with no
	// Embedding: one still to embed), cleared for the whole document by FinishKnowledgeDocument.
	Pending bool `json:"pending,omitempty"`
}

// KnowledgeChunkCursor is a keyset position in an org's chunks ordered by (doc_id, id); zero value = start (TEXT columns, "" sorts first); also the re-index resume token — clients treat it as opaque.
type KnowledgeChunkCursor struct {
	DocID string `json:"docId"`
	ID    string `json:"id"`
}

// IsZero reports whether c is the start-of-corpus cursor.
func (c KnowledgeChunkCursor) IsZero() bool { return c.DocID == "" && c.ID == "" }
