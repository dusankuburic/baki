package database

import (
	"context"
	"os"
	"strings"
	"testing"
)

// TestMigrate_RefusesNewerSchema: an OLD binary against a NEWER schema must
// refuse to boot. It used to boot clean and log "schema up to date,
// version=N" where N was the binary's max, not the database's — so the
// rollback path, the one recovery move an operator actually makes, ran
// queries against a schema the binary did not know, silently.
//
// Seeds a version row one past the binary's latest, then boots a second
// backend against the same database. The row is removed on cleanup.
func TestMigrate_RefusesNewerSchema(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()

	latest := migrations[len(migrations)-1].version
	future := latest + 1
	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, 'from-the-future', 'n/a')`, future); err != nil {
		t.Fatalf("seed future version: %v", err)
	}
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(context.Background(), `DELETE FROM schema_migrations WHERE version = $1`, future)
	})

	b2, err := New(ctx, DefaultConfig(os.Getenv("DATABASE_URL")))
	if err == nil {
		b2.Close()
		t.Fatalf("New() booted against schema v%d with a binary that only knows v%d", future, latest)
	}
	if !strings.Contains(err.Error(), "newer") {
		t.Errorf("the error must say the schema is newer than the binary, got: %v", err)
	}
}
