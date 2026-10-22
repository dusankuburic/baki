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

// hnswMaxVectorDims is the widest `vector` pgvector's HNSW indexes (halfvec goes to 4,000). A configured width
// above it gets no index — the build could only fail — and every search at it is an exact scan, which the boot
// log says once instead of a failed CREATE INDEX.
const hnswMaxVectorDims = 2000

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

// EnsureVectorIndex reconciles the vector column with the configured embedding
// width and builds the HNSW index for it if it is missing, outside any
// transaction (CREATE INDEX CONCURRENTLY cannot run in one). The reconciliation
// puts every chunk at width dims stored as JSON only into embedding_vec: the
// rows a PAD_EMBEDDING_DIM change towards their width leaves outside the index
// (written at another configured width, or before pgvector was installed),
// which the vector search would otherwise never see although the width now
// fits — "set PAD_EMBEDDING_DIM to the model's width and restart" completes
// on its own. A no-op when dims <= 0 or pgvector is absent; above
// hnswMaxVectorDims the column is reconciled but no index is built, logged
// once (hnswIndexable). An invalid index of that name (the leftover of an
// interrupted build) is dropped first: IF NOT EXISTS would keep it forever and
// the planner never uses an invalid index. A job on one replica at a time
// (vectorIndexJob: its lease ends the run when the lock is lost), with session
// timeouts lifted (a large corpus takes minutes); while the pool has no
// connection to spare for its lease it asks again (runWhenTheDatabaseAllows).
// Best effort: failures are logged and reported as nil — the search works
// without the index, only slower; the caller's context ending is returned as-is.
func (b *PostgresStorageBackend) EnsureVectorIndex(ctx context.Context, dims int) error {
	return b.ensureVectorIndex(ctx, dims, lifecycle.NewSingleton)
}

// ensureVectorIndex is EnsureVectorIndex with the job built by newJob (a test's own leaser or lifecycle group).
func (b *PostgresStorageBackend) ensureVectorIndex(ctx context.Context, dims int, newJob func(lifecycle.Job, lock.Leaser, lifecycle.JobState) *lifecycle.Singleton) error {
	if dims <= 0 || !b.hasPgvector {
		return nil
	}
	build := hnswIndexable(dims)
	// Two connections, like the schema upkeep's: a smaller pool would hand the lease its only one and wait.
	if n := b.db.Stats().MaxOpenConnections; n > 0 && n < upkeepMinPool {
		slog.Warn("knowledge vector index: run skipped: the pool cannot spare the two connections it holds; raise PAD_DB_MAX_OPEN_CONNS",
			"max_open_conns", n, "needs", upkeepMinPool)
		return nil
	}
	job := newJob(lifecycle.Job{
		Spec: vectorIndexJob,
		Run:  func(ctx context.Context) error { return b.reconcileVectorIndex(ctx, dims, build) },
	}, b, b)
	defer job.Stop()
	if err := runWhenTheDatabaseAllows(ctx, job, "knowledge vector index", false); errors.Is(err, lifecycle.ErrJobHeld) {
		slog.Info("knowledge vector index: another replica holds the build lock; skipping", "index", knowledgeVectorIndexName(dims))
	} else if err != nil {
		return ctx.Err() // the job logged why
	}
	return nil
}

// hnswIndexable reports whether pgvector's HNSW indexes vectors of width dims, saying why not when it does not:
// above hnswMaxVectorDims a build could only fail, so none is attempted and every search at that width is an exact
// scan over the vector column (which is still reconciled).
func hnswIndexable(dims int) bool {
	if dims <= hnswMaxVectorDims {
		return true
	}
	slog.Warn("knowledge vector index: PAD_EMBEDDING_DIM is above pgvector's HNSW limit for the vector type; no index is built and every knowledge search is an exact scan — use an embedding width within the limit",
		"dims", dims, "max_dims", hnswMaxVectorDims)
	return false
}

// reconcileVectorIndex is the job's run, on a connection of its own: the vector column's backfill for width dims,
// then — when build — the index build. A backfill that fails is logged and does not stop the build: the rows
// already in the column are what the index serves either way.
func (b *PostgresStorageBackend) reconcileVectorIndex(ctx context.Context, dims int, build bool) error {
	name := knowledgeVectorIndexName(dims)
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("knowledge vector index %s: no connection; run skipped: %w", name, err)
	}
	defer conn.Close()
	restore, err := liftSessionTimeouts(ctx, conn)
	if err != nil {
		return fmt.Errorf("knowledge vector index %s: could not lift session timeouts; run skipped: %w", name, err)
	}
	defer restore()
	if err := backfillVectorColumn(ctx, conn, dims); err != nil {
		if ctx.Err() != nil {
			return err
		}
		slog.Warn("knowledge vector column: backfill failed; chunks at the configured width stored as JSON only stay out of the vector search until a re-index", "dims", dims, "error", err)
	}
	if !build {
		return nil
	}
	return buildVectorIndex(ctx, conn, dims)
}

// backfillVectorColumnSQL puts the JSON embedding of every chunk at width $1 that is outside the vector index into
// embedding_vec — the v20 repair's statement scoped to the configured width, since the index holds one width and a
// row of another must stay out of it. IS NULL is the null bitmap, so the rows in the index cost no detoast; the
// CASE keeps jsonb_array_length off non-arrays (Postgres need not short-circuit an AND).
const backfillVectorColumnSQL = `
		UPDATE knowledge_chunks
		SET embedding_vec = ('[' ||
			(SELECT string_agg(e, ',') FROM jsonb_array_elements_text(embedding) AS e)
			|| ']')::vector
		WHERE embedding_vec IS NULL
		  AND CASE WHEN jsonb_typeof(embedding) = 'array' THEN jsonb_array_length(embedding) ELSE 0 END = $1`

// backfillVectorColumn runs backfillVectorColumnSQL for width dims on conn and says what it converted.
func backfillVectorColumn(ctx context.Context, conn *sql.Conn, dims int) error {
	start := time.Now()
	res, err := conn.ExecContext(ctx, backfillVectorColumnSQL, dims)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		slog.Info("knowledge vector column backfilled: chunks at the configured width stored as JSON only joined the vector search",
			"dims", dims, "chunks", n, "took", time.Since(start).Round(time.Millisecond))
	}
	return nil
}

// buildVectorIndex builds the index for width dims on conn, or says why not.
func buildVectorIndex(ctx context.Context, conn *sql.Conn, dims int) error {
	name := knowledgeVectorIndexName(dims)
	var valid bool
	err := conn.QueryRowContext(ctx, `
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
