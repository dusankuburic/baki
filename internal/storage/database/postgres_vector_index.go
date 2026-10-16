package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/lock"
)

// vectorIndexLockKey is the advisory lock replicas take while building the
// knowledge HNSW index, so two instances booting together do not run the
// same CREATE INDEX CONCURRENTLY against each other. Arbitrary but stable;
// distinct from the migration and scanner keys.
const vectorIndexLockKey int64 = 0x62616b6968_6e7377 // "bakihnsw"

// knowledgeVectorIndexName is the HNSW index serving searches of width dims.
// One index per width: embedding_vec is a dimensionless `vector` column
// (chunks of several widths coexist across embedding-provider switches), and
// HNSW can only index a fixed width, so the index is an expression index on
// the column cast to vector(dims), partial over the rows of that width.
func knowledgeVectorIndexName(dims int) string {
	return fmt.Sprintf("knowledge_chunks_hnsw_%d", dims)
}

// knowledgeVectorIndexDDL is the CONCURRENTLY build of that index. The
// partial predicate is what lets the cast be safe (a row of another width
// would make vector(dims) raise) and is repeated verbatim in the search
// statement (knowledgeVectorSearchSQL) so the planner can prove the index
// covers the query.
func knowledgeVectorIndexDDL(dims int) string {
	return fmt.Sprintf(
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS %s ON knowledge_chunks USING hnsw ((embedding_vec::vector(%d)) vector_cosine_ops) WHERE vector_dims(embedding_vec) = %d`,
		knowledgeVectorIndexName(dims), dims, dims)
}

// vectorIndexJob serialises the build across replicas, so two instances booting together do not run the same CREATE
// INDEX CONCURRENTLY against each other.
var vectorIndexJob = lifecycle.DeclareJob(lifecycle.JobDecl{
	Name:      "vector_index_build",
	LegacyKey: vectorIndexLockKey,
	Schedule:  "once per boot, in the background (database with pgvector)",
})

// EnsureVectorIndex builds the HNSW index for the configured embedding width
// if it is missing, outside any transaction (CREATE INDEX CONCURRENTLY cannot
// run in one). A no-op when dims <= 0 or pgvector is absent. An invalid index
// of that name (the leftover of an interrupted build) is dropped first: IF NOT
// EXISTS would keep it forever and the planner never uses an invalid index.
// A job on one replica at a time (vectorIndexJob: its lease ends the build when
// the lock is lost), with session timeouts lifted (a large corpus takes
// minutes); while the pool has no connection to spare for its lease it asks
// again (runWhenTheDatabaseAllows). Best effort: failures are logged and
// reported as nil — the search works without the index, only slower; the
// caller's context ending is returned as-is.
func (b *PostgresStorageBackend) EnsureVectorIndex(ctx context.Context, dims int) error {
	return b.ensureVectorIndex(ctx, dims, lifecycle.NewSingleton)
}

// ensureVectorIndex is EnsureVectorIndex with the job built by newJob (a test's own leaser or lifecycle group).
func (b *PostgresStorageBackend) ensureVectorIndex(ctx context.Context, dims int, newJob func(lifecycle.Job, lock.Leaser, lifecycle.JobState) *lifecycle.Singleton) error {
	if dims <= 0 || !b.hasPgvector {
		return nil
	}
	// Two connections, like the schema upkeep's: a smaller pool would hand the lease its only one and wait.
	if n := b.db.Stats().MaxOpenConnections; n > 0 && n < upkeepMinPool {
		slog.Warn("knowledge vector index: build skipped: the pool cannot spare the two connections it holds; raise PAD_DB_MAX_OPEN_CONNS",
			"max_open_conns", n, "needs", upkeepMinPool)
		return nil
	}
	job := newJob(lifecycle.Job{
		Spec: vectorIndexJob,
		Run:  func(ctx context.Context) error { return b.buildVectorIndex(ctx, dims) },
	}, b, b)
	defer job.Stop()
	if err := runWhenTheDatabaseAllows(ctx, job, "knowledge vector index", false); errors.Is(err, lifecycle.ErrJobHeld) {
		slog.Info("knowledge vector index: another replica holds the build lock; skipping", "index", knowledgeVectorIndexName(dims))
	} else if err != nil {
		return ctx.Err() // the job logged why
	}
	return nil
}

// buildVectorIndex is the job's run: it builds the index on a connection of its own, or says why not.
func (b *PostgresStorageBackend) buildVectorIndex(ctx context.Context, dims int) error {
	name := knowledgeVectorIndexName(dims)
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("knowledge vector index %s: no connection; build skipped: %w", name, err)
	}
	defer conn.Close()
	restore, err := liftSessionTimeouts(ctx, conn)
	if err != nil {
		return fmt.Errorf("knowledge vector index %s: could not lift session timeouts; build skipped: %w", name, err)
	}
	defer restore()

	var valid bool
	err = conn.QueryRowContext(ctx, `
		SELECT i.indisvalid
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relname = $1 AND c.relnamespace = current_schema()::regnamespace`, name).Scan(&valid)
	switch {
	case err == nil && valid:
		return nil
	case err == nil:
		slog.Warn("knowledge vector index: dropping invalid index left by an interrupted build", "index", name)
		if _, err := conn.ExecContext(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+name); err != nil {
			return fmt.Errorf("knowledge vector index %s: drop of invalid index failed; build skipped: %w", name, err)
		}
	case errors.Is(err, sql.ErrNoRows):
		// Absent: build it.
	default:
		return fmt.Errorf("knowledge vector index %s: catalog lookup failed; build skipped: %w", name, err)
	}

	start := time.Now()
	if _, err := conn.ExecContext(ctx, knowledgeVectorIndexDDL(dims)); err != nil {
		return fmt.Errorf("knowledge vector index %s (dims %d): build failed; search stays an exact scan: %w", name, dims, err)
	}
	slog.Info("knowledge vector index built", "index", name, "dims", dims, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// detectHNSWIterativeScan reports whether this pgvector supports
// hnsw.iterative_scan (0.8+). The GUC is registered when the extension's
// library loads into the session, which happens on first use of a vector
// function — so the probe forces that load on the same connection before
// reading the setting; current_setting(..., true) is NULL when the GUC is
// unknown, and a reserved-prefix pgvector would reject the SET outright.
func detectHNSWIterativeScan(ctx context.Context, db *sql.DB) bool {
	conn, err := db.Conn(ctx)
	if err != nil {
		return false
	}
	defer conn.Close()
	var probe string
	if err := conn.QueryRowContext(ctx, `SELECT '[1]'::vector::text`).Scan(&probe); err != nil {
		return false
	}
	var setting sql.NullString
	if err := conn.QueryRowContext(ctx, `SELECT current_setting('hnsw.iterative_scan', true)`).Scan(&setting); err != nil {
		return false
	}
	return setting.Valid
}
