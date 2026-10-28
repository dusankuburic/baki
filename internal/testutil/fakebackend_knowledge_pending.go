package testutil

import (
	"context"
	"slices"
	"sort"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// The knowledge base's two-phase uploads, source texts and per-document identity stamp (v51), mirroring
// postgres_knowledge_pending.go.

// GetKnowledgeDocument returns a copy of one of the org's documents with its source text and chunk counts.
func (m *FakeBackend) GetKnowledgeDocument(_ context.Context, orgID, id string) (*interfaces.KnowledgeDocument, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	d, ok := m.KnowledgeDocs[id]
	if !ok || d.OrgID != orgID {
		return nil, interfaces.ErrNotFound
	}
	cp := *d
	cp.ChunkCount, cp.VectorIndexed, cp.PendingChunks = 0, 0, 0
	m.countDocumentChunksLocked(&cp)
	return &cp, nil
}

// ListPendingKnowledgeChunks is a page of the document's pending chunks in id order after afterID (limit <= 0 = all).
func (m *FakeBackend) ListPendingKnowledgeChunks(_ context.Context, orgID, docID, afterID string, limit int) ([]interfaces.KnowledgeChunk, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []interfaces.KnowledgeChunk
	for _, c := range m.KnowledgeChunks {
		d, ok := m.KnowledgeDocs[c.DocID]
		if !ok || d.OrgID != orgID || c.DocID != docID || !c.Pending || len(c.Embedding) > 0 || c.ID <= afterID {
			continue
		}
		out = append(out, interfaces.KnowledgeChunk{ID: c.ID, DocID: c.DocID, OrgID: orgID, Content: c.Content, Pending: true})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// FinishKnowledgeDocument turns a document none of whose chunks lacks its embedding ready, puts its chunks into the
// corpus and retires the other ready documents of its filename, all under one lock as the Postgres transaction does.
func (m *FakeBackend) FinishKnowledgeDocument(_ context.Context, _, orgID, docID string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.KnowledgeDocs[docID]
	if !ok || d.OrgID != orgID {
		return 0, interfaces.ErrNotFound
	}
	pending := 0
	for _, c := range m.KnowledgeChunks {
		if c.DocID == docID && c.Pending && len(c.Embedding) == 0 {
			pending++
		}
	}
	if pending > 0 || d.State == interfaces.KnowledgeDocumentReady {
		return pending, nil
	}
	for id, c := range m.KnowledgeChunks {
		if c.DocID == docID && c.Pending {
			c.Pending = false
			m.KnowledgeChunks[id] = c
		}
	}
	d.State = interfaces.KnowledgeDocumentReady
	for id, other := range m.KnowledgeDocs {
		if id != docID && other.OrgID == orgID && other.Filename == d.Filename && other.State == interfaces.KnowledgeDocumentReady {
			m.deleteKnowledgeDocumentLocked(orgID, id)
		}
	}
	return 0, nil
}

// ListKnowledgeDocumentsBelowChunkerVersion pages the org's ready documents below version in id order, without text.
func (m *FakeBackend) ListKnowledgeDocumentsBelowChunkerVersion(_ context.Context, orgID string, version int, afterID string, limit int) ([]*interfaces.KnowledgeDocument, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*interfaces.KnowledgeDocument
	for _, d := range m.KnowledgeDocs {
		if d.OrgID != orgID || d.State != interfaces.KnowledgeDocumentReady || d.ChunkerVersion >= version || d.ID <= afterID {
			continue
		}
		cp := *d
		cp.Content = ""
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// SampleLegacyKnowledgeChunkEmbeddings mirrors the Postgres sample: for the first docs documents (DocID order) holding
// embedded legacy chunks, the first and the last of those by ID, in (DocID, ID) order, embeddings copied.
func (m *FakeBackend) SampleLegacyKnowledgeChunkEmbeddings(_ context.Context, orgID string, docs int) ([]interfaces.KnowledgeChunk, error) {
	if docs <= 0 {
		return nil, nil
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	byDoc := map[string][]interfaces.KnowledgeChunk{}
	for _, c := range m.KnowledgeChunks {
		d, ok := m.KnowledgeDocs[c.DocID]
		if !ok || d.OrgID != orgID || c.EmbeddingIdentity != "" || c.Pending {
			continue
		}
		byDoc[c.DocID] = append(byDoc[c.DocID], c)
	}
	docIDs := make([]string, 0, len(byDoc))
	for id := range byDoc {
		docIDs = append(docIDs, id)
	}
	sort.Strings(docIDs)
	var out []interfaces.KnowledgeChunk
	for _, id := range docIDs[:min(docs, len(docIDs))] {
		chunks := byDoc[id]
		sort.Slice(chunks, func(i, j int) bool { return chunks[i].ID < chunks[j].ID })
		ends := []interfaces.KnowledgeChunk{chunks[0]}
		if len(chunks) > 1 {
			ends = append(ends, chunks[len(chunks)-1])
		}
		for _, c := range ends {
			out = append(out, interfaces.KnowledgeChunk{ID: c.ID, DocID: c.DocID, Content: c.Content, Embedding: slices.Clone(c.Embedding)})
		}
	}
	return out, nil
}

// StampKnowledgeDocumentIdentity stamps to on the legacy, non-pending chunks of the org's documents docIDs.
func (m *FakeBackend) StampKnowledgeDocumentIdentity(_ context.Context, _, orgID string, docIDs []string, to string) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for id, c := range m.KnowledgeChunks {
		d, ok := m.KnowledgeDocs[c.DocID]
		if !ok || d.OrgID != orgID || c.EmbeddingIdentity != "" || c.Pending || !slices.Contains(docIDs, c.DocID) {
			continue
		}
		c.EmbeddingIdentity = to
		m.KnowledgeChunks[id] = c
		n++
	}
	return n, nil
}

// purgeAbandonedKnowledgeUploadsLocked is the retention sweep's knowledge step: the oldest
// KnowledgePendingPurgeBatch unfinished uploads stored more than KnowledgePendingTTL before now go, with their chunks.
// m.mu is held.
func (m *FakeBackend) purgeAbandonedKnowledgeUploadsLocked(now time.Time) int {
	var old []*interfaces.KnowledgeDocument
	for _, d := range m.KnowledgeDocs {
		if d.State == interfaces.KnowledgeDocumentPending && d.CreatedAt.Before(now.Add(-interfaces.KnowledgePendingTTL)) {
			old = append(old, d)
		}
	}
	slices.SortFunc(old, func(a, b *interfaces.KnowledgeDocument) int { return a.CreatedAt.Compare(b.CreatedAt) })
	old = old[:min(len(old), interfaces.KnowledgePendingPurgeBatch)]
	for _, d := range old {
		m.deleteKnowledgeDocumentLocked(d.OrgID, d.ID)
	}
	return len(old)
}
