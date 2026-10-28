package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/lock"
)

// A schema step holds its locks until it commits, while the release before still serves, so it leaves to after
// boot what reads a table that grows with use or locks a busy one: the VALIDATE of a foreign key it added NOT VALID,
// and an index built or dropped CONCURRENTLY (which cannot run in a transaction). SchemaUpkeep does both, on a schema
// at or above the step that asked for them. A later step that drops one of these takes it off its list here.

// pendingConstraint is a foreign key or CHECK a step added NOT VALID. orphans deletes (or, for a CHECK, fixes) the
// rows it would refuse. 0048 deletes those of its small tables itself (so only a write that committed between its
// delete and its ADD CONSTRAINT can have left one) and leaves those of the tables that grow with use (knowledge
// documents and chunks, sessions, the expired ones the retention sweep's too) wholly to this: it neither reads nor
// row-locks such a table, since a request of the release before reaches such a row (a re-index, a refresh) and
// deadlocks with a row lock taken before the table's.
type pendingConstraint struct {
	since   int // the step that added it
	table   string
	name    string
	orphans string
}

// concurrentIndex is an index a step left to after boot: ddl is its CREATE INDEX CONCURRENTLY IF NOT EXISTS, or, for
// an index the step retires, "" (a DROP INDEX CONCURRENTLY IF EXISTS).
type concurrentIndex struct {
	since int
	name  string
	ddl   string
}

// schemaUpkeepConstraints are v48's foreign keys (0048_integrity_constraints.up.sql), each with the delete of the
// rows it refuses.
var schemaUpkeepConstraints = []pendingConstraint{
	{48, "knowledge_documents", "knowledge_documents_org_id_fkey",
		`DELETE FROM knowledge_documents d WHERE NOT EXISTS (SELECT 1 FROM organisations o WHERE o.id = d.org_id)`},
	{48, "knowledge_chunks", "knowledge_chunks_doc_id_org_id_fkey",
		`DELETE FROM knowledge_chunks c WHERE NOT EXISTS (SELECT 1 FROM knowledge_documents d WHERE d.id = c.doc_id AND d.org_id = c.org_id)`},
	{48, "policies", "policies_org_id_fkey",
		`DELETE FROM policies p WHERE NOT EXISTS (SELECT 1 FROM organisations o WHERE o.id = p.org_id)`},
	{48, "org_channels", "org_channels_org_id_fkey",
		`DELETE FROM org_channels c WHERE NOT EXISTS (SELECT 1 FROM organisations o WHERE o.id = c.org_id)`},
	{48, "org_custom_rules", "org_custom_rules_org_id_fkey",
		`DELETE FROM org_custom_rules r WHERE NOT EXISTS (SELECT 1 FROM organisations o WHERE o.id = r.org_id)`},
	{48, "refresh_tokens", "refresh_tokens_user_id_fkey",
		`DELETE FROM refresh_tokens t WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = t.user_id)`},
}

// schemaUpkeepIndexes are the indexes v48 and v49 build or retire on tables a step must not hold a lock on: those
// every sign-in, flow save or audit write touches, or that grow with use. In order: a retirement comes after the
// build that replaces it.
//
// v48's serve the alert inbox's "clear" and account erasure (the database-design review, Medium 9 and 10). Partial on
// a non-empty actor: the empty string is the anonymised and the local-mode value, never looked up, and the erase
// statements' `col = $1` (a non-empty id, planned with its value) implies the predicate.
var schemaUpkeepIndexes = []concurrentIndex{
	// "Clear" (ClearGovernanceAlerts) deletes the caller's dismissed alerts, starting from the caller's flows
	// (govAlertReadWhere): this probes only the dismissed alerts of each flow instead of every alert of it.
	{48, "gov_alerts_dismissed_flow_idx",
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS gov_alerts_dismissed_flow_idx ON gov_alerts (flow_id) WHERE dismissed_at IS NOT NULL`},
	// The erase step `UPDATE flow_versions SET created_by = '' WHERE created_by = $1`.
	{48, "flow_versions_created_by_idx",
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS flow_versions_created_by_idx ON flow_versions (created_by) WHERE created_by <> ''`},
	// The erase step on author_id, eraseActorFromAlerts' `SELECT c.flow_id FROM finding_comments c WHERE c.author_id =
	// $1`, and the export's comments section (exportAuthoredComments).
	{48, "finding_comments_author_idx",
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS finding_comments_author_idx ON finding_comments (author_id) WHERE author_id <> ''`},
	// The erase step `... WHERE updated_by = $1 OR assignee_id = $1` (a BitmapOr of the two) and eraseActorFromAlerts'
	// SELECT of the same; the export's triage section is keyed by flow_id and needs neither.
	{48, "finding_status_updated_by_idx",
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS finding_status_updated_by_idx ON finding_status (updated_by) WHERE updated_by <> ''`},
	{48, "finding_status_assignee_idx",
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS finding_status_assignee_idx ON finding_status (assignee_id) WHERE assignee_id <> ''`},

	// v49 (0049_index_upkeep.up.sql). The admin user list's search: a case-insensitive substring of the email or the
	// display name (QueryUsers: lower(col) LIKE '%term%', a prefix for a term without a run of three letters or digits).
	// users has no row-level security, so the LIKE can use these. pg_trgm is installed since v13.
	{49, "users_email_trgm_idx",
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS users_email_trgm_idx ON users USING gin (lower(email) gin_trgm_ops)`},
	{49, "users_display_name_trgm_idx",
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS users_display_name_trgm_idx ON users USING gin (lower(display_name) gin_trgm_ops)`},
	// The locked-account counts of the dashboard and the analytics page (locked_until IS NOT NULL AND locked_until >
	// NOW()). Partial: an account is unlocked (NULL) almost always.
	{49, "users_locked_until_idx",
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS users_locked_until_idx ON users (locked_until) WHERE locked_until IS NOT NULL`},
	// Retired by v49, after the step built knowledge_documents_org_created_idx, which replaces the first; the leading
	// column of org_custom_rules' UNIQUE (org_id, rule_id) serves every org_id lookup and the cascade the second did.
	{49, "knowledge_docs_org_idx", ""},
	{49, "org_custom_rules_org_idx", ""},
	// An organisation's audit trail (v33): no statement reads audit_events by organisation (the admin log filters by
	// user and action, the org_id backfill looks for org_id IS NULL, which this partial index does not hold), and every
	// audit write paid for it. An organisation audit view would add the index it reads under a new name: a retired name
	// is never reused.
	{49, "audit_events_org_created_idx", ""},
	// v11 and v20 asked for an HNSW index over the dimensionless embedding_vec, which pgvector cannot build: the step
	// skipped it, so this finds nothing. The searchable indexes are the per-width knowledge_chunks_hnsw_<dims>
	// (EnsureVectorIndex).
	{49, "knowledge_chunks_embedding_hnsw", ""},

	// v52 (0052_conversation_owner.up.sql, R-security-3): a user's threads on a flow (ListConversations: owner_id =
	// $1 AND flow_id = $2 ORDER BY updated_at DESC, scope DESC, read backwards), and the user's own conversations
	// wherever they are (erasure's `OR owner_id = $1`, the export). Partial: unowned rows are never looked up by owner.
	{52, "conversations_owner_idx",
		`CREATE INDEX CONCURRENTLY IF NOT EXISTS conversations_owner_idx ON conversations (owner_id, flow_id, updated_at, scope) WHERE owner_id IS NOT NULL`},
}

// schemaUpkeepJob runs the upkeep on one replica at a time: two replicas booting together would validate the same
// constraints and race the same concurrent builds.
var schemaUpkeepJob = lifecycle.DeclareJob(lifecycle.JobDecl{
	Name:     "schema_upkeep",
	Schedule: "once per boot, in the background (database)",
})

// Each VALIDATE waits at most upkeepLockTimeout for its table locks, and an item that met a lock timeout or lost a
// deadlock is tried upkeepAttempts times in all, the pause growing by upkeepBackoff. A replica that finds the job
// running elsewhere asks again every upkeepHeldPoll; one that got no connection for its lease in time asks again
// after upkeepHeldPoll, then twice as long each time up to upkeepHeldPollMax (each ask that fails is a WARN and a
// skip of the job's). The run holds two connections, its lease and its work, so a pool of fewer than upkeepMinPool
// skips it.
var (
	upkeepLockTimeout = 5 * time.Second
	upkeepAttempts    = 3
	upkeepBackoff     = 2 * time.Second
	upkeepHeldPoll    = 5 * time.Second
	upkeepHeldPollMax = time.Minute
	upkeepMinPool     = 3
)

// SchemaUpkeep validates the constraints a step added NOT VALID and builds the indexes it left to a concurrent build,
// once per boot, in the background. Idempotent: a validated constraint and a valid index are skipped, and an index an
// interrupted build left invalid is dropped and built again. A VALIDATE takes SHARE UPDATE EXCLUSIVE on its table and
// ROW SHARE on the referenced one, and a concurrent build SHARE UPDATE EXCLUSIVE: reads and writes go on. While
// another replica runs it, it waits for that run and then runs (finding nothing left, or what that run left), so the
// caller's HNSW build never runs against that run's VALIDATE of knowledge_chunks; while the pool has no connection to
// spare for its lease it asks again (runWhenTheDatabaseAllows). Best effort, like EnsureVectorIndex:
// what failed is logged (the job's run fails) and tried again at the next boot; the caller's context ending is
// returned as-is, and lifecycle.ErrJobStopped when shutdown stopped the job.
func (b *PostgresStorageBackend) SchemaUpkeep(ctx context.Context) error {
	return b.schemaUpkeep(ctx, lifecycle.NewSingleton)
}

// schemaUpkeep is SchemaUpkeep with the job built by newJob (a test's own lifecycle group).
func (b *PostgresStorageBackend) schemaUpkeep(ctx context.Context, newJob func(lifecycle.Job, lock.Leaser, lifecycle.JobState) *lifecycle.Singleton) error {
	if n := b.db.Stats().MaxOpenConnections; n > 0 && n < upkeepMinPool {
		slog.Warn("schema upkeep skipped: the pool cannot spare the two connections it holds; raise PAD_DB_MAX_OPEN_CONNS",
			"max_open_conns", n, "needs", upkeepMinPool)
		return nil
	}
	job := newJob(lifecycle.Job{Spec: schemaUpkeepJob, Run: b.runSchemaUpkeep}, b, b)
	defer job.Stop()
	err := runWhenTheDatabaseAllows(ctx, job, "schema upkeep", true)
	switch {
	case errors.Is(err, lifecycle.ErrJobStopped):
		return err
	case err != nil:
		return ctx.Err() // the job logged why
	}
	return nil
}

// runWhenTheDatabaseAllows runs job once and returns its outcome, asking again, until ctx ends, while the lease's
// connection was not to be had in time (lifecycle.ErrJobLockUnavailable: in its first minutes a replica serves a full
// pool, whose queue can outlast the lease's budget; giving up there left the whole run to the next boot) and, when
// waitHeld, while another replica holds the lease. A held lease is asked for again every upkeepHeldPoll (the other
// run ends any moment); an unavailable one after upkeepHeldPoll, then twice as long each time up to
// upkeepHeldPollMax, since the database may be unreachable for a while and the job logs each ask that fails
// (lifecycle.LogJobLockUnavailable).
func runWhenTheDatabaseAllows(ctx context.Context, job *lifecycle.Singleton, what string, waitHeld bool) error {
	pause := upkeepHeldPoll
	for waiting := false; ; waiting = true {
		err := job.RunOnce(ctx)
		held := errors.Is(err, lifecycle.ErrJobHeld)
		busy := errors.Is(err, lifecycle.ErrJobLockUnavailable)
		if !busy && (!held || !waitHeld) {
			return err
		}
		if !waiting {
			if held {
				slog.Info(what + ": another replica runs it; waiting for it to finish")
			} else {
				slog.Warn(what+": no connection for its lease in time (the pool is busy); asking again", "after", upkeepHeldPoll, "at_most_every", upkeepHeldPollMax)
			}
		}
		if held {
			pause = upkeepHeldPoll
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(pause):
		}
		if busy {
			pause = min(2*pause, upkeepHeldPollMax)
		}
	}
}

// runSchemaUpkeep is the job's run, on a connection of its own with the session timeouts lifted (a VALIDATE or a
// build over a large table outlasts the statement budget). Every item is tried; the run fails when one did not finish.
// Once its context has ended it returns the context's cause (lifecycle.ErrJobStopped at shutdown). On a schema ahead
// of this binary it does nothing: the release that moved it ahead runs its own upkeep, and its steps may have built an
// index this one would drop.
func (b *PostgresStorageBackend) runSchemaUpkeep(ctx context.Context) (err error) {
	defer func() {
		if ctx.Err() != nil {
			err = context.Cause(ctx)
		}
	}()
	conn, err := b.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("schema upkeep: no connection: %w", err)
	}
	defer conn.Close()
	restore, err := liftSessionTimeouts(ctx, conn)
	if err != nil {
		return fmt.Errorf("schema upkeep: could not lift session timeouts: %w", err)
	}
	defer restore()
	version, err := currentVersion(ctx, conn)
	if err != nil {
		return fmt.Errorf("schema upkeep: read schema version: %w", err)
	}
	if latest := migrations[len(migrations)-1].version; version > latest {
		slog.Info("schema upkeep: the schema is ahead of this binary; its release does the upkeep", "schema_version", version, "binary_version", latest)
		return nil
	}

	var failed []string
	for _, c := range schemaUpkeepConstraints {
		if c.since > version {
			continue
		}
		if err := upkeepRetry(ctx, func() error { return validateConstraint(ctx, conn, c) }); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("schema upkeep: constraint not validated; tried again at the next boot", "constraint", c.name, "error", err)
			failed = append(failed, c.name)
		}
	}
	for _, ix := range schemaUpkeepIndexes {
		if ix.since > version {
			continue
		}
		if err := upkeepRetry(ctx, func() error { return buildIndexConcurrently(ctx, conn, ix) }); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("schema upkeep: index not built; tried again at the next boot", "index", ix.name, "error", err)
			failed = append(failed, ix.name)
		}
	}
	for _, bf := range schemaUpkeepBackfills {
		if bf.since > version {
			continue
		}
		if err := upkeepRetry(ctx, func() error { return runBackfill(ctx, conn, bf) }); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			slog.Warn("schema upkeep: backfill not finished; resumed at the next boot", "backfill", bf.name, "error", err)
			failed = append(failed, bf.name)
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("schema upkeep: %s did not finish", strings.Join(failed, ", "))
	}
	return nil
}

// upkeepRetry runs fn again after a lock timeout or a lost deadlock, with a growing pause.
func upkeepRetry(ctx context.Context, fn func() error) error {
	for attempt := 1; ; attempt++ {
		err := fn()
		retry := isPgErrCode(err, pgLockNotAvailable) || isPgErrCode(err, pgDeadlockDetected)
		if err == nil || attempt >= upkeepAttempts || !retry {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(upkeepBackoff * time.Duration(attempt)):
		}
	}
}

// validateConstraint validates c when it exists and is not yet valid. The rows it refuses (see pendingConstraint) are
// deleted (or fixed), counted in the log, and the VALIDATE runs again. While an index is being built on the table
// (another replica's HNSW build of knowledge_chunks runs for minutes) the VALIDATE would only wait out its lock
// timeouts: it is left for the next boot at once.
func validateConstraint(ctx context.Context, conn *sql.Conn, c pendingConstraint) error {
	var valid, building bool
	err := conn.QueryRowContext(ctx,
		`SELECT convalidated, EXISTS (SELECT 1 FROM pg_stat_progress_create_index p WHERE p.relid = conrelid AND p.datid = (SELECT oid FROM pg_database WHERE datname = current_database()))
		   FROM pg_constraint WHERE conrelid = to_regclass($1) AND conname = $2`, c.table, c.name).Scan(&valid, &building)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && valid) {
		return nil
	}
	if err != nil {
		return err
	}
	if building {
		return fmt.Errorf("an index is being built on %s; left for the next boot", c.table)
	}
	// Each statement is a transaction of its own (autocommit on the job's connection), the VALIDATE with the lock
	// timeout set around it; the session's lifted value comes back after it, and liftSessionTimeouts' restore resets
	// the connection at the end either way.
	validate := func() error {
		if _, err := conn.ExecContext(ctx, fmt.Sprintf("SET lock_timeout = '%dms'", upkeepLockTimeout.Milliseconds())); err != nil {
			return err
		}
		defer func() { _, _ = conn.ExecContext(context.Background(), "SET lock_timeout = 0") }()
		_, err := conn.ExecContext(ctx, `ALTER TABLE `+c.table+` VALIDATE CONSTRAINT `+c.name)
		return err
	}
	start := time.Now()
	err = validate()
	if (isPgErrCode(err, pgErrForeignKeyViolation) || isPgErrCode(err, pgErrCheckViolation)) && c.orphans != "" {
		res, derr := conn.ExecContext(ctx, c.orphans)
		if derr != nil {
			return fmt.Errorf("delete the rows %s refuses: %w", c.name, derr)
		}
		n, _ := res.RowsAffected()
		slog.Warn("schema upkeep: cleaned up rows the constraint refuses", "constraint", c.name, "rows", n)
		err = validate()
	}
	if err != nil {
		return err
	}
	slog.Info("schema upkeep: constraint validated", "constraint", c.name, "took", time.Since(start).Round(time.Millisecond))
	return nil
}

// buildIndexConcurrently builds ix when it is missing, or drops it when ix retires it. An invalid index of that name
// (the leftover of an interrupted concurrent build) is dropped first: IF NOT EXISTS would keep it forever, the planner
// never uses it, and every write still maintains it.
func buildIndexConcurrently(ctx context.Context, conn *sql.Conn, ix concurrentIndex) error {
	if ix.ddl == "" {
		var exists bool
		if err := conn.QueryRowContext(ctx, `SELECT to_regclass($1) IS NOT NULL`, ix.name).Scan(&exists); err != nil || !exists {
			return err
		}
		if _, err := conn.ExecContext(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+ix.name); err != nil {
			return err
		}
		slog.Info("schema upkeep: index dropped", "index", ix.name)
		return nil
	}
	var valid bool
	err := conn.QueryRowContext(ctx, `
		SELECT i.indisvalid
		  FROM pg_index i
		  JOIN pg_class c ON c.oid = i.indexrelid
		 WHERE c.relname = $1 AND c.relnamespace = current_schema()::regnamespace`, ix.name).Scan(&valid)
	switch {
	case err == nil && valid:
		return nil
	case err == nil:
		slog.Warn("schema upkeep: dropping invalid index left by an interrupted build", "index", ix.name)
		if _, err := conn.ExecContext(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+ix.name); err != nil {
			return fmt.Errorf("drop the invalid index: %w", err)
		}
	case errors.Is(err, sql.ErrNoRows):
		// Absent: build it.
	default:
		return fmt.Errorf("catalog lookup: %w", err)
	}
	start := time.Now()
	if _, err := conn.ExecContext(ctx, ix.ddl); err != nil {
		return err
	}
	slog.Info("schema upkeep: index built", "index", ix.name, "took", time.Since(start).Round(time.Millisecond))
	return nil
}
