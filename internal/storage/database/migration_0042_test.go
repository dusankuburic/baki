package database

import (
	"context"
	"database/sql"
	"strings"
	"testing"
)

// TestMigration0042_Shape pins the v42 step without a database: one nullable TEXT column on the chunks, one
// NOT NULL INT DEFAULT 1 column on the documents, both IF NOT EXISTS, expand-compatible, no index and no
// CONCURRENTLY — and a down step that drops both.
func TestMigration0042_Shape(t *testing.T) {
	m, ok := migrationByVersion(42)
	if !ok {
		t.Fatal("migration v42 not registered")
	}
	if m.name != "knowledge_embedding_identity" {
		t.Errorf("v42 name = %q, want knowledge_embedding_identity", m.name)
	}
	if m.compat != compatExpand {
		t.Errorf("v42 compat = %v, want expand (the release before ignores both columns)", m.compat)
	}
	up := oneLine(m.sql)
	for _, want := range []string{
		"ALTER TABLE knowledge_chunks ADD COLUMN IF NOT EXISTS embedding_identity TEXT;",
		"ALTER TABLE knowledge_documents ADD COLUMN IF NOT EXISTS chunker_version INT NOT NULL DEFAULT 1;",
	} {
		if !strings.Contains(up, want) {
			t.Errorf("v42 lacks %q", want)
		}
	}
	for _, forbidden := range []string{"CREATE INDEX", "CONCURRENTLY"} {
		if strings.Contains(strings.ToUpper(m.sql), forbidden) {
			t.Errorf("v42 must not contain %s", forbidden)
		}
	}
	down := oneLine(m.downSQL)
	for _, want := range []string{
		"ALTER TABLE knowledge_chunks DROP COLUMN IF EXISTS embedding_identity;",
		"ALTER TABLE knowledge_documents DROP COLUMN IF EXISTS chunker_version;",
	} {
		if !strings.Contains(down, want) {
			t.Errorf("v42 down lacks %q", want)
		}
	}
}

// TestMigration0042_ColumnsAndDefaults checks the catalog after boot: embedding_identity is a nullable text
// column without a default (NULL = legacy), chunker_version a NOT NULL integer defaulting to 1, and a document
// row inserted without the column reads 1.
func TestMigration0042_ColumnsAndDefaults(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	if v, _ := b.CurrentSchemaVersion(ctx); v < 42 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	column := func(table, name string) (dataType, nullable string, dflt sql.NullString) {
		t.Helper()
		err := b.db.QueryRowContext(ctx, `
			SELECT data_type, is_nullable, column_default FROM information_schema.columns
			WHERE table_schema = current_schema() AND table_name = $1 AND column_name = $2`, table, name).
			Scan(&dataType, &nullable, &dflt)
		if err != nil {
			t.Fatalf("%s.%s: %v", table, name, err)
		}
		return dataType, nullable, dflt
	}
	if dt, null, dflt := column("knowledge_chunks", "embedding_identity"); dt != "text" || null != "YES" || dflt.Valid {
		t.Errorf("knowledge_chunks.embedding_identity = %s nullable=%s default=%v; want nullable text without a default", dt, null, dflt)
	}
	if dt, null, dflt := column("knowledge_documents", "chunker_version"); dt != "integer" || null != "NO" || !dflt.Valid || dflt.String != "1" {
		t.Errorf("knowledge_documents.chunker_version = %s nullable=%s default=%v; want integer NOT NULL DEFAULT 1", dt, null, dflt)
	}

	const docID = "m42-default-doc"
	t.Cleanup(func() { _, _ = b.db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, docID) })
	if _, err := b.db.ExecContext(ctx, `INSERT INTO knowledge_documents (id, org_id, filename) VALUES ($1, 'm42-org', 'd.md')`, docID); err != nil {
		t.Fatalf("insert without chunker_version: %v", err)
	}
	var version int
	if err := b.db.QueryRowContext(ctx, `SELECT chunker_version FROM knowledge_documents WHERE id = $1`, docID).Scan(&version); err != nil || version != 1 {
		t.Errorf("chunker_version of a row inserted without it = %d, %v; want 1", version, err)
	}
}
