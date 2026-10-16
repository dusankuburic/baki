package database

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// seedIdentityCorpus stores one document of org with three equal-width chunks pointing the same way: one
// embedded under identity A, one under B and one legacy row (no identity), through the public write path so
// the column is written the way an upload writes it. Ids carry the prefix so parallel tests do not collide.
func seedIdentityCorpus(t *testing.T, b *PostgresStorageBackend, prefix string) (orgID, docID string) {
	t.Helper()
	ctx := context.Background()
	orgID, docID = prefix+"-org", prefix+"-doc"
	seedOrgRow(t, b.db, orgID)
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE doc_id = $1`, docID)
		_, _ = b.db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, docID)
	})
	if err := b.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: docID, OrgID: orgID, Filename: "identity.md"}); err != nil {
		t.Fatalf("SaveKnowledgeDocument: %v", err)
	}
	vec := []float32{1, 0, 0}
	if err := b.SaveKnowledgeChunks(ctx, "", []interfaces.KnowledgeChunk{
		{ID: prefix + "-a", DocID: docID, OrgID: orgID, Content: "under A", Embedding: vec, EmbeddingIdentity: "A"},
		{ID: prefix + "-b", DocID: docID, OrgID: orgID, Content: "under B", Embedding: vec, EmbeddingIdentity: "B"},
		{ID: prefix + "-legacy", DocID: docID, OrgID: orgID, Content: "legacy", Embedding: vec},
	}); err != nil {
		t.Fatalf("SaveKnowledgeChunks: %v", err)
	}
	return orgID, docID
}

// storedIdentity reads a chunk's embedding_identity; "" for NULL.
func storedIdentity(t *testing.T, b *PostgresStorageBackend, id string) string {
	t.Helper()
	var s sql.NullString
	if err := b.db.QueryRowContext(context.Background(), `SELECT embedding_identity FROM knowledge_chunks WHERE id = $1`, id).Scan(&s); err != nil {
		t.Fatalf("read identity of %s: %v", id, err)
	}
	return s.String
}

func chunkIDs(chunks []interfaces.KnowledgeChunk) string {
	ids := make([]string, len(chunks))
	for i, c := range chunks {
		ids[i] = c.ID
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// TestSearchKnowledge_IdentityFilter_PathParity: both search paths return the chunks embedded under the
// caller's identity plus the legacy rows, never a chunk of another identity, and every row when the caller
// names no identity. The vector path needs pgvector at the fixture's width.
func TestSearchKnowledge_IdentityFilter_PathParity(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	if !b.HasPgvector() {
		t.Skip("pgvector not installed — parity needs both paths live")
	}
	ctx := context.Background()
	orgID, _ := seedIdentityCorpus(t, b, "ident-parity")
	query := []float32{1, 0, 0}
	for _, tc := range []struct{ identity, want string }{
		{"A", "ident-parity-a,ident-parity-legacy"},
		{"B", "ident-parity-b,ident-parity-legacy"},
		{"C", "ident-parity-legacy"},
		{"", "ident-parity-a,ident-parity-b,ident-parity-legacy"},
	} {
		vec, err := b.searchKnowledgeVector(ctx, orgID, query, 10, tc.identity)
		if err != nil {
			t.Fatalf("identity %q: searchKnowledgeVector: %v", tc.identity, err)
		}
		goPath, err := b.searchKnowledgeGo(ctx, orgID, query, 10, tc.identity)
		if err != nil {
			t.Fatalf("identity %q: searchKnowledgeGo: %v", tc.identity, err)
		}
		if got := chunkIDs(vec); got != tc.want {
			t.Errorf("identity %q: vector path = %s, want %s", tc.identity, got, tc.want)
		}
		if got := chunkIDs(goPath); got != tc.want {
			t.Errorf("identity %q: Go path = %s, want %s", tc.identity, got, tc.want)
		}
		public, err := b.SearchKnowledge(ctx, orgID, query, 10, tc.identity)
		if err != nil || chunkIDs(public) != tc.want {
			t.Errorf("identity %q: SearchKnowledge = %s, %v; want %s", tc.identity, chunkIDs(public), err, tc.want)
		}
	}
}

// TestCountKnowledgeChunksByIdentity_ReportsStranded: the counts name the rows at the caller's identity, the
// rows at another (stranded) and, by difference, the legacy rows — with and without the pgvector column.
func TestCountKnowledgeChunksByIdentity_ReportsStranded(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	orgID, _ := seedIdentityCorpus(t, b, "ident-count")
	detected := b.hasPgvector
	t.Cleanup(func() { b.hasPgvector = detected })
	for _, pgvector := range []bool{detected, false} {
		b.hasPgvector = pgvector
		counts, err := b.CountKnowledgeChunksByIdentity(ctx, orgID, "A")
		if err != nil {
			t.Fatalf("pgvector=%v: CountKnowledgeChunksByIdentity: %v", pgvector, err)
		}
		if counts.Total != 3 || counts.Current != 1 || counts.Stranded != 1 || counts.Legacy() != 1 {
			t.Errorf("pgvector=%v: counts for A = %+v, want total 3, current 1, stranded 1, legacy 1", pgvector, counts)
		}
		if pgvector && counts.VectorIndexed != 3 {
			t.Errorf("vector-indexed = %d, want 3 (every row is at the configured width)", counts.VectorIndexed)
		}
		if !pgvector && counts.VectorIndexed != 0 {
			t.Errorf("without pgvector vector-indexed = %d, want 0", counts.VectorIndexed)
		}
		none, err := b.CountKnowledgeChunksByIdentity(ctx, orgID, "")
		if err != nil || none.Total != 3 || none.Current != 0 || none.Stranded != 0 {
			t.Errorf("pgvector=%v: counts without an identity = %+v, %v; want total 3 and nothing current or stranded", pgvector, none, err)
		}
		if empty, err := b.CountKnowledgeChunksByIdentity(ctx, "ident-count-nobody", "A"); err != nil || empty.Total != 0 {
			t.Errorf("counts of an org without chunks = %+v, %v", empty, err)
		}
	}
}

// TestCountStrandedKnowledgeChunksByDocument: per document, the rows at another identity; documents without
// any are absent, and no identity means nothing is stranded.
func TestCountStrandedKnowledgeChunksByDocument(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	orgID, docID := seedIdentityCorpus(t, b, "ident-bydoc")
	got, err := b.CountStrandedKnowledgeChunksByDocument(ctx, orgID, "A")
	if err != nil || len(got) != 1 || got[docID] != 1 {
		t.Errorf("stranded by document for A = %v, %v; want {%s: 1}", got, err, docID)
	}
	if got, err := b.CountStrandedKnowledgeChunksByDocument(ctx, orgID, ""); err != nil || len(got) != 0 {
		t.Errorf("stranded by document without an identity = %v, %v; want none", got, err)
	}
}

// TestStampKnowledgeChunkIdentity_OnlyLegacyRows: stamping from "" rewrites the NULL rows only; a row of
// another identity keeps it until stamped from that identity explicitly.
func TestStampKnowledgeChunkIdentity_OnlyLegacyRows(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	orgID, _ := seedIdentityCorpus(t, b, "ident-stamp")
	n, err := b.StampKnowledgeChunkIdentity(ctx, "", orgID, "", "A")
	if err != nil || n != 1 {
		t.Fatalf("StampKnowledgeChunkIdentity(legacy → A) = %d, %v; want 1", n, err)
	}
	if got := storedIdentity(t, b, "ident-stamp-legacy"); got != "A" {
		t.Errorf("legacy row identity = %q, want A", got)
	}
	if got := storedIdentity(t, b, "ident-stamp-b"); got != "B" {
		t.Errorf("row under B = %q after a legacy stamp, want B", got)
	}
	if n, err := b.StampKnowledgeChunkIdentity(ctx, "", orgID, "", "A"); err != nil || n != 0 {
		t.Errorf("second legacy stamp = %d, %v; want 0 rows", n, err)
	}
	if n, err := b.StampKnowledgeChunkIdentity(ctx, "", "ident-stamp-other-org", "", "A"); err != nil || n != 0 {
		t.Errorf("stamp of another org = %d, %v; want 0 rows", n, err)
	}
	if n, err := b.StampKnowledgeChunkIdentity(ctx, "", orgID, "B", "A"); err != nil || n != 1 {
		t.Errorf("StampKnowledgeChunkIdentity(B → A) = %d, %v; want 1", n, err)
	}
	counts, _ := b.CountKnowledgeChunksByIdentity(ctx, orgID, "A")
	if counts.Current != 3 || counts.Stranded != 0 {
		t.Errorf("counts after stamping = %+v, want every row current", counts)
	}
}

// TestListKnowledgeChunkContents_SkipsCurrentIdentity: a re-index page excludes the rows already at the
// identity it converts to (legacy rows are walked), and walks everything when no identity is named.
func TestListKnowledgeChunkContents_SkipsCurrentIdentity(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	orgID, _ := seedIdentityCorpus(t, b, "ident-walk")
	page, err := b.ListKnowledgeChunkContents(ctx, orgID, interfaces.KnowledgeChunkCursor{}, 0, "A")
	if err != nil || chunkIDs(page) != "ident-walk-b,ident-walk-legacy" {
		t.Errorf("page not at A = %s, %v; want b and legacy", chunkIDs(page), err)
	}
	all, err := b.ListKnowledgeChunkContents(ctx, orgID, interfaces.KnowledgeChunkCursor{}, 0, "")
	if err != nil || chunkIDs(all) != "ident-walk-a,ident-walk-b,ident-walk-legacy" {
		t.Errorf("page without an identity = %s, %v; want every row", chunkIDs(all), err)
	}
}

// TestUpdateKnowledgeChunkEmbeddings_WritesIdentity: a re-index page stamps the identity it embedded under,
// and a provider without one clears the column (the vector's origin is unknown again).
func TestUpdateKnowledgeChunkEmbeddings_WritesIdentity(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	_, docID := seedIdentityCorpus(t, b, "ident-update")
	detected := b.hasPgvector
	t.Cleanup(func() { b.hasPgvector = detected })
	for _, pgvector := range []bool{detected, false} {
		b.hasPgvector = pgvector
		if err := b.UpdateKnowledgeChunkEmbeddings(ctx, "", []interfaces.KnowledgeChunk{
			{ID: "ident-update-legacy", DocID: docID, Embedding: []float32{0, 1, 0}, EmbeddingIdentity: "A"},
			{ID: "ident-update-a", DocID: docID, Embedding: []float32{0, 1, 0}},
		}); err != nil {
			t.Fatalf("pgvector=%v: UpdateKnowledgeChunkEmbeddings: %v", pgvector, err)
		}
		if got := storedIdentity(t, b, "ident-update-legacy"); got != "A" {
			t.Errorf("pgvector=%v: re-embedded legacy row identity = %q, want A", pgvector, got)
		}
		if got := storedIdentity(t, b, "ident-update-a"); got != "" {
			t.Errorf("pgvector=%v: row re-embedded without an identity = %q, want NULL", pgvector, got)
		}
		// Reset for the second round.
		_, _ = b.StampKnowledgeChunkIdentity(ctx, "", "ident-update-org", "A", "")
	}
}

// TestListKnowledgeDocuments_CarriesChunkerVersion: the list reports the version a document was chunked with
// (1 for rows from before the column) and the one SaveKnowledgeDocument wrote.
func TestListKnowledgeDocuments_CarriesChunkerVersion(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	orgID, docID := seedIdentityCorpus(t, b, "ident-version")
	versioned := docID + "-v7"
	t.Cleanup(func() { _, _ = b.db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, versioned) })
	if err := b.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: versioned, OrgID: orgID, Filename: "v7.md", ChunkerVersion: 7}); err != nil {
		t.Fatalf("SaveKnowledgeDocument: %v", err)
	}
	docs, err := b.ListKnowledgeDocuments(ctx, orgID, 0, 0)
	if err != nil || len(docs) != 2 {
		t.Fatalf("ListKnowledgeDocuments = %d docs, %v; want 2", len(docs), err)
	}
	byID := map[string]*interfaces.KnowledgeDocument{}
	for _, d := range docs {
		byID[d.ID] = d
	}
	if byID[docID].ChunkerVersion != 1 || byID[versioned].ChunkerVersion != 7 {
		t.Errorf("chunker versions = %d and %d, want 1 (default) and 7", byID[docID].ChunkerVersion, byID[versioned].ChunkerVersion)
	}
}

// TestKnowledgeChunkWrites_TakeTheOrgAdvisoryLock (R2): the chunk writes of one org serialise across replicas
// through pg_advisory_xact_lock(hashtext(org_id)) in their own transaction — a page write waits while another
// session holds the org's lock in an open transaction, and proceeds once it commits.
func TestKnowledgeChunkWrites_TakeTheOrgAdvisoryLock(t *testing.T) {
	b := newKnowledgeIntegrationBackend(t)
	ctx := context.Background()
	orgID, docID := seedIdentityCorpus(t, b, "ident-lock")
	holder, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	t.Cleanup(func() {
		if !released {
			_ = holder.Rollback()
		}
	})
	if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, orgID); err != nil {
		t.Fatalf("take the org lock: %v", err)
	}
	for name, write := range map[string]func() error{
		"UpdateKnowledgeChunkEmbeddings": func() error {
			return b.UpdateKnowledgeChunkEmbeddings(ctx, "", []interfaces.KnowledgeChunk{{ID: "ident-lock-a", DocID: docID, OrgID: orgID, Embedding: []float32{0, 1, 0}}})
		},
		"SaveKnowledgeChunks": func() error {
			t.Cleanup(func() { _, _ = b.db.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE id = 'ident-lock-new'`) })
			return b.SaveKnowledgeChunks(ctx, "", []interfaces.KnowledgeChunk{{ID: "ident-lock-new", DocID: docID, OrgID: orgID, Content: "new", Embedding: []float32{0, 1, 0}}})
		},
	} {
		done := make(chan error, 1)
		go func() { done <- write() }()
		select {
		case err := <-done:
			t.Fatalf("%s finished (%v) while another session held the org's advisory lock", name, err)
		case <-time.After(300 * time.Millisecond):
		}
		if err := holder.Rollback(); err != nil {
			t.Fatal(err)
		}
		released = true
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("%s after the lock was released: %v", name, err)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s still blocked after the lock was released", name)
		}
		// Re-take for the next write.
		if holder, err = b.db.BeginTx(ctx, nil); err != nil {
			t.Fatal(err)
		}
		released = false
		if _, err := holder.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext($1))`, orgID); err != nil {
			t.Fatalf("re-take the org lock: %v", err)
		}
	}
}

// TestStampKnowledgeChunkIdentity_RLSScoped (Phase 4b review T8): the stamp writes a FORCE RLS table, so it runs
// in its own transaction as the caller like the other chunk writers — a caller outside the org stamps nothing,
// a member stamps the org's legacy rows. On a bare pool connection the policies are inactive and the org
// predicate would be the only guard.
func TestStampKnowledgeChunkIdentity_RLSScoped(t *testing.T) {
	f := newR2Fixture(t)
	skipIfRLSBypassedInternal(t, f.b)
	b := f.b
	b.embeddingDim = 3
	ctx := f.ctx
	member, outsider := f.user("kb-member"), f.user("kb-outsider")
	orgID := f.org("kb-org", member, map[string]string{member: "admin"})
	docID := f.id("kb-doc")
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(ctx, `DELETE FROM knowledge_chunks WHERE doc_id = $1`, docID)
		_, _ = b.db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, docID)
	})
	if err := b.SaveKnowledgeDocument(ctx, member, &interfaces.KnowledgeDocument{ID: docID, OrgID: orgID, Filename: "rls.md"}); err != nil {
		t.Fatalf("SaveKnowledgeDocument: %v", err)
	}
	legacy := docID + "-legacy"
	if err := b.SaveKnowledgeChunks(ctx, member, []interfaces.KnowledgeChunk{{ID: legacy, DocID: docID, OrgID: orgID, Content: "legacy", Embedding: []float32{1, 0, 0}}}); err != nil {
		t.Fatalf("SaveKnowledgeChunks: %v", err)
	}

	if n, err := b.StampKnowledgeChunkIdentity(ctx, outsider, orgID, "", "A"); err != nil || n != 0 {
		t.Errorf("an outsider's stamp = %d, %v; want 0 rows (RLS)", n, err)
	}
	if got := storedIdentity(t, b, legacy); got != "" {
		t.Errorf("legacy row identity after an outsider's stamp = %q, want NULL", got)
	}
	if n, err := b.StampKnowledgeChunkIdentity(ctx, member, orgID, "", "A"); err != nil || n != 1 {
		t.Errorf("a member's stamp = %d, %v; want 1 row", n, err)
	}
	if got := storedIdentity(t, b, legacy); got != "A" {
		t.Errorf("legacy row identity after a member's stamp = %q, want A", got)
	}
}
