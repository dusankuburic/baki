package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"
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

// EnsureVectorIndex builds the HNSW index for the configured embedding width
// if it is missing, outside any transaction (CREATE INDEX CONCURRENTLY cannot
// run in one). A no-op when dims <= 0 or pgvector is absent. An invalid index
// of that name (the leftover of an interrupted build) is dropped first: IF NOT
// EXISTS would keep it forever and the planner never uses an invalid index.
// Serialised across replicas by an advisory lock, with session timeouts lifted
// (a large corpus takes minutes). Best effort: failures are logged and
// reported as nil — the search works without the index, only slower; the
// caller's context ending is returned as-is.
func (b *PostgresStorageBackend) EnsureVectorIndex(ctx context.Context, dims int) error {
	if dims <= 0 || !b.hasPgvector {
		return nil
	}
	name := knowledgeVectorIndexName(dims)
	release, acquired, err := b.TryGlobalLock(ctx, vectorIndexLockKey)
	if err != nil {
		slog.Warn("knowledge vector index: advisory lock failed; skipping build", "index", name, "err", err)
		return ctx.Err()
	}
	if !acquired {
		slog.Info("knowledge vector index: another replica holds the build lock; skipping", "index", name)
		return nil
	}
	defer release()

	conn, err := b.db.Conn(ctx)
	if err != nil {
		slog.Warn("knowledge vector index: no connection; skipping build", "index", name, "err", err)
		return ctx.Err()
	}
	defer conn.Close()
	restore, err := liftSessionTimeouts(ctx, conn)
	if err != nil {
		slog.Warn("knowledge vector index: could not lift session timeouts; skipping build", "index", name, "err", err)
		return ctx.Err()
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
			slog.Warn("knowledge vector index: drop of invalid index failed; skipping build", "index", name, "err", err)
			return ctx.Err()
		}
	case errors.Is(err, sql.ErrNoRows):
		// Absent: build it.
	default:
		slog.Warn("knowledge vector index: catalog lookup failed; skipping build", "index", name, "err", err)
		return ctx.Err()
	}

	start := time.Now()
	if _, err := conn.ExecContext(ctx, knowledgeVectorIndexDDL(dims)); err != nil {
		slog.Warn("knowledge vector index: build failed; search stays an exact scan", "index", name, "dims", dims, "err", err)
		return ctx.Err()
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
