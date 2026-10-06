package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"pad-analyzer/internal/storage/interfaces"
)

// PostgresAzureRegistry is the AzureRegistryStore over the v25 user_azure_*/org_azure_* tables. Every statement
// names its owner explicitly (WHERE user_id/org_id = $1): /api/chat/* runs without the RLS transaction, so the
// predicate must hold on its own. Caller-tx writes run under a SAVEPOINT so ErrAzureDuplicate* doesn't abort the tx.
// ErrAzureDuplicate* does not abort the whole request transaction.
type PostgresAzureRegistry struct {
	b *PostgresStorageBackend
}

var _ interfaces.AzureRegistryStore = (*PostgresAzureRegistry)(nil)

// NewPostgresAzureRegistry returns the registry store over b's pool.
func NewPostgresAzureRegistry(b *PostgresStorageBackend) *PostgresAzureRegistry {
	return &PostgresAzureRegistry{b: b}
}

// AzureRegistry returns the backend's Azure AI Foundry registry store (see
// AzureRegistryProvider). Kept out of the StorageBackend composite.
func (b *PostgresStorageBackend) AzureRegistry() interfaces.AzureRegistryStore {
	return NewPostgresAzureRegistry(b)
}

// SetAzureExportConfigSource installs the data-subject export's source for
// each personal Azure resource's public configuration: those fields live only
// in the encrypted credential document, which this package by design cannot
// read. A nil fn clears the hook.
func (b *PostgresStorageBackend) SetAzureExportConfigSource(fn interfaces.AzureExportConfigFunc) {
	if fn == nil {
		b.azureExportConfig.Store(nil)
		return
	}
	b.azureExportConfig.Store(&fn)
}

// exportAzureRegistry builds the export's registry section (userID's personal
// resources and models); org registries hold no user ids, and any failure —
// the hook's included — fails the all-or-nothing export.
func (b *PostgresStorageBackend) exportAzureRegistry(ctx context.Context, userID string) (*interfaces.ExportedAzureRegistry, error) {
	reg := NewPostgresAzureRegistry(b)
	owner := interfaces.AzureUserOwner(userID)
	conns, err := reg.ListConnections(ctx, owner)
	if err != nil {
		return nil, err
	}
	models, err := reg.ListModels(ctx, owner)
	if err != nil {
		return nil, err
	}
	var config map[string]interfaces.AzureConnectionPublicConfig
	if fn := b.azureExportConfig.Load(); fn != nil && len(conns) > 0 {
		ids := make([]string, 0, len(conns))
		for _, c := range conns {
			ids = append(ids, c.ID)
		}
		if config, err = (*fn)(ctx, userID, ids); err != nil {
			return nil, fmt.Errorf("azure resource configuration: %w", err)
		}
	}
	return interfaces.NewExportedAzureRegistry(conns, models, config), nil
}

// ---- statements ----

// azureStatements are one owner kind's statements; user and org sets differ
// only in names, so both come from one template — no string is ever built
// from input, only from the constants below.
type azureStatements struct {
	listConns, getConn, countConns, createConn, importConn, updateConn, connExists, connNameTaken,
	dismissConn, testConn, deleteConnModels, deleteConn string
	listModels, getModel, countModels, createModel, importModel, updateModel, modelExists,
	testModel, deleteModel string
	clearKB, setKB, clearDefault, setDefault string
	counts, importedAt, markImported         string
}

const azureConnCols = `id, name, discovery_dismissed_at, last_test_ok, last_test_at, last_test_latency_ms, last_test_error, created_at, updated_at`

const azureModelCols = `id, connection_id, kind, deployment, display_name, purpose, purpose_custom, description,
	context_window, max_output_tokens, is_kb_embedding, {isdef}, last_test_ok, last_test_at, last_test_latency_ms,
	last_test_error, created_at, updated_at`

// azureNowCTE is one clock reading for both timestamps of an insert.
const azureNowCTE = `FROM (SELECT clock_timestamp() AS t) n`

// azureBumpUpdatedAt moves updated_at strictly forward so every update hands
// out a fresh CAS token — now() would repeat within one transaction or microsecond.
const azureBumpUpdatedAt = `updated_at = GREATEST(clock_timestamp(), updated_at + interval '1 microsecond')`

var azureStatementTemplate = azureStatements{
	listConns:  `SELECT ` + azureConnCols + ` FROM {conns} WHERE {owner} = $1 ORDER BY created_at, id COLLATE "C"`,
	getConn:    `SELECT ` + azureConnCols + ` FROM {conns} WHERE {owner} = $1 AND id = $2`,
	countConns: `SELECT count(*) FROM {conns} WHERE {owner} = $1`,
	// The limit is part of the insert, so it holds even for a writer that
	// skipped the owner lock.
	createConn: `INSERT INTO {conns} ({owner}, id, name, created_at, updated_at)
		SELECT $1::text, $2::text, $3::text, n.t, n.t ` + azureNowCTE + `
		 WHERE (SELECT count(*) FROM {conns} WHERE {owner} = $1) < {max_conns}
		RETURNING ` + azureConnCols,
	// ON CONFLICT without a target: any uniqueness rule skips the row — the
	// deployment index is an expression index, not the key.
	importConn: `INSERT INTO {conns} ({owner}, id, name, created_at, updated_at)
		SELECT $1::text, $2::text, $3::text, n.t, n.t ` + azureNowCTE + `
		ON CONFLICT DO NOTHING
		RETURNING id`,
	updateConn: `UPDATE {conns}
		   SET name = COALESCE(NULLIF($3::text, ''), name), ` + azureBumpUpdatedAt + `
		 WHERE {owner} = $1 AND id = $2 AND ($4::timestamptz IS NULL OR updated_at = $4::timestamptz)
		RETURNING ` + azureConnCols,
	connExists: `SELECT EXISTS (SELECT 1 FROM {conns} WHERE {owner} = $1 AND id = $2)`,
	// The unique index's own expression — lower() under the database's
	// collation, which Go's case folding cannot stand in for.
	connNameTaken: `SELECT EXISTS (SELECT 1 FROM {conns} WHERE {owner} = $1 AND lower(name) = lower($2::text) AND id <> $3::text)`,
	dismissConn: `UPDATE {conns}
		   SET discovery_dismissed_at = CASE WHEN $3::boolean THEN COALESCE(discovery_dismissed_at, clock_timestamp()) END
		 WHERE {owner} = $1 AND id = $2
		RETURNING ` + azureConnCols,
	testConn: `UPDATE {conns}
		   SET last_test_ok = $3::boolean, last_test_at = $4::timestamptz,
		       last_test_latency_ms = $5::integer, last_test_error = $6::text
		 WHERE {owner} = $1 AND id = $2`,
	deleteConnModels: `DELETE FROM {models} WHERE {owner} = $1 AND connection_id = $2 RETURNING ` + azureModelCols,
	deleteConn:       `DELETE FROM {conns} WHERE {owner} = $1 AND id = $2`,

	listModels:  `SELECT ` + azureModelCols + ` FROM {models} WHERE {owner} = $1 ORDER BY created_at, id COLLATE "C"`,
	getModel:    `SELECT ` + azureModelCols + ` FROM {models} WHERE {owner} = $1 AND id = $2`,
	countModels: `SELECT count(*) FROM {models} WHERE {owner} = $1`,
	createModel: `INSERT INTO {models} ({owner}, id, connection_id, kind, deployment, display_name, purpose,
			purpose_custom, description, context_window, max_output_tokens, is_kb_embedding, created_at, updated_at{isdef_col})
		SELECT $1::text, $2::text, $3::text, $4::text, $5::text, $6::text, $7::text, $8::text, $9::text,
		       $10::integer, $11::integer, $12::boolean, n.t, n.t{isdef_val} ` + azureNowCTE + `
		 WHERE (SELECT count(*) FROM {models} WHERE {owner} = $1) < {max_models}
		RETURNING ` + azureModelCols,
	importModel: `INSERT INTO {models} ({owner}, id, connection_id, kind, deployment, display_name, purpose,
			purpose_custom, description, context_window, max_output_tokens, is_kb_embedding, created_at, updated_at{isdef_col})
		SELECT $1::text, $2::text, $3::text, $4::text, $5::text, $6::text, $7::text, $8::text, $9::text,
		       $10::integer, $11::integer, $12::boolean, n.t, n.t{isdef_val} ` + azureNowCTE + `
		ON CONFLICT DO NOTHING
		RETURNING id`,
	updateModel: `UPDATE {models}
		   SET connection_id = $3::text, kind = $4::text, deployment = $5::text, display_name = $6::text,
		       purpose = $7::text, purpose_custom = $8::text, description = $9::text,
		       context_window = $10::integer, max_output_tokens = $11::integer, ` + azureBumpUpdatedAt + `
		 WHERE {owner} = $1 AND id = $2 AND ($12::timestamptz IS NULL OR updated_at = $12::timestamptz)
		RETURNING ` + azureModelCols,
	modelExists: `SELECT EXISTS (SELECT 1 FROM {models} WHERE {owner} = $1 AND id = $2)`,
	testModel: `UPDATE {models}
		   SET last_test_ok = $3::boolean, last_test_at = $4::timestamptz,
		       last_test_latency_ms = $5::integer, last_test_error = $6::text
		 WHERE {owner} = $1 AND id = $2`,
	deleteModel: `DELETE FROM {models} WHERE {owner} = $1 AND id = $2 RETURNING ` + azureModelCols,

	clearKB: `UPDATE {models} SET is_kb_embedding = FALSE WHERE {owner} = $1 AND is_kb_embedding AND id <> $2`,
	setKB:   `UPDATE {models} SET is_kb_embedding = TRUE WHERE {owner} = $1 AND id = $2`,

	counts: `SELECT (SELECT count(*) FROM {conns} WHERE {owner} = $1),
		       (SELECT count(*) FROM {models} WHERE {owner} = $1)`,
	importedAt: `SELECT imported_at FROM {state} WHERE {owner} = $1`,
	markImported: `INSERT INTO {state} ({owner}, imported_at) VALUES ($1, clock_timestamp())
		ON CONFLICT ({owner}) DO NOTHING`,
}

// Org-only statements (users have no default for members).
const (
	azureClearOrgDefault = `UPDATE org_azure_models SET is_default = FALSE WHERE org_id = $1 AND is_default AND id <> $2`
	azureSetOrgDefault   = `UPDATE org_azure_models SET is_default = TRUE WHERE org_id = $1 AND id = $2`
)

var azureSQL = map[interfaces.AzureOwnerKind]*azureStatements{
	interfaces.AzureOwnerUser: buildAzureStatements(strings.NewReplacer(
		"{conns}", "user_azure_connections", "{models}", "user_azure_models", "{state}", "user_azure_registry_state",
		"{owner}", "user_id", "{isdef}", "FALSE", "{isdef_col}", "", "{isdef_val}", "",
		"{max_conns}", strconv.Itoa(interfaces.AzureMaxConnections), "{max_models}", strconv.Itoa(interfaces.AzureMaxModels),
	), "", ""),
	interfaces.AzureOwnerOrg: buildAzureStatements(strings.NewReplacer(
		"{conns}", "org_azure_connections", "{models}", "org_azure_models", "{state}", "org_azure_registry_state",
		"{owner}", "org_id", "{isdef}", "is_default", "{isdef_col}", ", is_default", "{isdef_val}", ", $13::boolean",
		"{max_conns}", strconv.Itoa(interfaces.AzureMaxConnections), "{max_models}", strconv.Itoa(interfaces.AzureMaxModels),
	), azureClearOrgDefault, azureSetOrgDefault),
}

func buildAzureStatements(r *strings.Replacer, clearDefault, setDefault string) *azureStatements {
	t := azureStatementTemplate
	for _, s := range []*string{
		&t.listConns, &t.getConn, &t.countConns, &t.createConn, &t.importConn, &t.updateConn, &t.connExists, &t.connNameTaken,
		&t.dismissConn, &t.testConn, &t.deleteConnModels, &t.deleteConn,
		&t.listModels, &t.getModel, &t.countModels, &t.createModel, &t.importModel, &t.updateModel, &t.modelExists,
		&t.testModel, &t.deleteModel, &t.clearKB, &t.setKB, &t.counts, &t.importedAt, &t.markImported,
	} {
		*s = r.Replace(*s)
	}
	t.clearDefault, t.setDefault = clearDefault, setDefault
	return &t
}

// stmts returns owner's statements, refusing owners Postgres cannot hold:
// an empty id has no users / organisations row for the foreign key.
func (r *PostgresAzureRegistry) stmts(owner interfaces.AzureOwner) (*azureStatements, error) {
	if err := owner.Validate(); err != nil {
		return nil, err
	}
	if owner.ID == "" {
		return nil, fmt.Errorf("%w: the database registry needs a user or organisation id", interfaces.ErrAzureRegistryOwnerUnsupported)
	}
	return azureSQL[owner.Kind], nil
}

// ---- execution helpers ----

// write runs fn atomically: under a SAVEPOINT inside the caller's
// transaction, otherwise in a transaction of its own.
func (r *PostgresAzureRegistry) write(ctx context.Context, fn func(q DBTX) error) error {
	tx, ok, err := rlsTxFromContext(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return r.b.withTx(ctx, func(_ context.Context, tx *sql.Tx) error { return fn(tx) })
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT azure_registry_write`); err != nil {
		return fmt.Errorf("azure registry: savepoint: %w", err)
	}
	if err := fn(tx); err != nil {
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT azure_registry_write`); rbErr != nil {
			return errors.Join(err, fmt.Errorf("azure registry: rollback to savepoint: %w", rbErr))
		}
		_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT azure_registry_write`)
		return err
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT azure_registry_write`); err != nil {
		return fmt.Errorf("azure registry: release savepoint: %w", err)
	}
	return nil
}

// SQLSTATEs mapped by mapAzureRegistryErr besides pgErrUniqueViolation.
const (
	pgErrCheckViolation      = "23514"
	pgErrForeignKeyViolation = "23503"
)

// mapAzureRegistryErr turns a constraint violation into the store's typed
// error by constraint name: Postgres lower() and Go's strings.ToLower can
// disagree, so the unique indexes decide what a duplicate is.
func mapAzureRegistryErr(op string, err error) error {
	if err == nil {
		return nil
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		if isAzureRegistrySentinel(err) {
			return err
		}
		return fmt.Errorf("azure registry: %s: %w", op, err)
	}
	c := pgErr.ConstraintName
	typed := func(sentinel error) error { return fmt.Errorf("%w (%s)", sentinel, c) }
	switch {
	case pgErr.Code == pgErrUniqueViolation:
		switch {
		case strings.HasSuffix(c, "_connections_pkey"):
			return typed(interfaces.ErrAzureDuplicateConnectionID)
		case strings.HasSuffix(c, "_connections_name_idx"):
			return typed(interfaces.ErrAzureDuplicateConnectionName)
		case strings.HasSuffix(c, "_models_pkey"):
			return typed(interfaces.ErrAzureDuplicateModelID)
		case strings.HasSuffix(c, "_models_deployment_idx"):
			return typed(interfaces.ErrAzureDuplicateModel)
		case strings.HasSuffix(c, "_models_kb_idx"), strings.HasSuffix(c, "_models_default_idx"):
			return typed(interfaces.ErrAzureDuplicateDesignation)
		}
	case pgErr.Code == pgErrCheckViolation:
		if strings.HasSuffix(c, "_kb_check") || strings.HasSuffix(c, "_default_check") {
			return typed(interfaces.ErrAzureInvalidDesignation)
		}
		return typed(interfaces.ErrAzureRegistryInvalid)
	case pgErr.Code == pgErrForeignKeyViolation:
		switch {
		case strings.HasSuffix(c, "_connection_fk"):
			return typed(interfaces.ErrAzureConnectionNotFound)
		case strings.HasSuffix(c, "_user_fk"), strings.HasSuffix(c, "_org_fk"):
			return typed(interfaces.ErrAzureOwnerNotFound)
		}
	case strings.HasPrefix(pgErr.Code, "22"):
		// Data exceptions: text Postgres cannot store (NUL, invalid UTF-8)
		// or a value out of its type's range.
		return fmt.Errorf("%w: %s", interfaces.ErrAzureRegistryInvalid, pgErr.Message)
	}
	return fmt.Errorf("azure registry: %s: %w", op, err)
}

func isAzureRegistrySentinel(err error) bool {
	for _, s := range []error{
		interfaces.ErrAzureConnectionNotFound, interfaces.ErrAzureModelNotFound, interfaces.ErrAzureOwnerNotFound,
		interfaces.ErrAzureDuplicateConnectionID, interfaces.ErrAzureDuplicateConnectionName,
		interfaces.ErrAzureDuplicateModelID, interfaces.ErrAzureDuplicateModel,
		interfaces.ErrAzureDuplicateDesignation, interfaces.ErrAzureInvalidDesignation,
		interfaces.ErrAzureTooManyConnections, interfaces.ErrAzureTooManyModels,
		interfaces.ErrAzureRegistryConflict, interfaces.ErrAzureRegistryReadOnly,
		interfaces.ErrAzureRegistryInvalid, interfaces.ErrAzureRegistryOwnerUnsupported,
	} {
		if errors.Is(err, s) {
			return true
		}
	}
	return false
}

type azureRowScanner interface{ Scan(dest ...any) error }

func azureTestFromColumns(ok sql.NullBool, at sql.NullTime, latency sql.NullInt64, errText string) *interfaces.AzureTestResult {
	if !ok.Valid {
		return nil
	}
	return &interfaces.AzureTestResult{OK: ok.Bool, At: at.Time.UTC(), LatencyMs: int(latency.Int64), Error: errText}
}

func scanAzureConnection(s azureRowScanner) (interfaces.AzureConnectionRecord, error) {
	var (
		c         interfaces.AzureConnectionRecord
		dismissed sql.NullTime
		ok        sql.NullBool
		at        sql.NullTime
		latency   sql.NullInt64
		errText   string
	)
	if err := s.Scan(&c.ID, &c.Name, &dismissed, &ok, &at, &latency, &errText, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return interfaces.AzureConnectionRecord{}, err
	}
	if dismissed.Valid {
		t := dismissed.Time.UTC()
		c.DiscoveryDismissedAt = &t
	}
	c.LastTest = azureTestFromColumns(ok, at, latency, errText)
	c.CreatedAt, c.UpdatedAt = c.CreatedAt.UTC(), c.UpdatedAt.UTC()
	return c, nil
}

func scanAzureModel(s azureRowScanner) (interfaces.AzureModelRecord, error) {
	var (
		m               interfaces.AzureModelRecord
		contextWindow   sql.NullInt64
		maxOutputTokens sql.NullInt64
		ok              sql.NullBool
		at              sql.NullTime
		latency         sql.NullInt64
		errText         string
	)
	if err := s.Scan(&m.ID, &m.ConnectionID, &m.Kind, &m.Deployment, &m.DisplayName, &m.Purpose, &m.PurposeCustom,
		&m.Description, &contextWindow, &maxOutputTokens, &m.IsKnowledgeEmbedding, &m.IsDefault,
		&ok, &at, &latency, &errText, &m.CreatedAt, &m.UpdatedAt); err != nil {
		return interfaces.AzureModelRecord{}, err
	}
	m.ContextWindow, m.MaxOutputTokens = int(contextWindow.Int64), int(maxOutputTokens.Int64)
	m.LastTest = azureTestFromColumns(ok, at, latency, errText)
	m.CreatedAt, m.UpdatedAt = m.CreatedAt.UTC(), m.UpdatedAt.UTC()
	return m, nil
}

// azureNullInt maps the "0 = unset" overrides to NULL.
func azureNullInt(v int) any {
	if v == 0 {
		return nil
	}
	return v
}

// azureNullTime maps a zero compare-and-swap token to NULL (no check).
func azureNullTime(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t
}

// azureTestArgs are the four last_test_* parameters for a stamp (nil
// clears it).
func azureTestArgs(result *interfaces.AzureTestResult) ([]any, error) {
	if result == nil {
		return []any{nil, nil, nil, ""}, nil
	}
	if result.LatencyMs < 0 {
		return nil, fmt.Errorf("%w: negative test latency", interfaces.ErrAzureRegistryInvalid)
	}
	at := result.At
	if at.IsZero() {
		at = time.Now()
	}
	return []any{result.OK, at, result.LatencyMs, interfaces.TruncateAzureTestError(result.Error)}, nil
}

// azureLockKey is the owner's advisory-lock key: the 64-bit FNV-1a hash of
// owner.LockName(); a collision only serialises two unrelated owners.
func azureLockKey(owner interfaces.AzureOwner) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(owner.LockName()))
	return int64(h.Sum64()) //nolint:gosec // G115: the hash's bits reinterpreted as the signed advisory-lock key; wraparound is intended
}

func sortAzureModels(models []interfaces.AzureModelRecord) {
	sort.SliceStable(models, func(i, j int) bool {
		if !models[i].CreatedAt.Equal(models[j].CreatedAt) {
			return models[i].CreatedAt.Before(models[j].CreatedAt)
		}
		return models[i].ID < models[j].ID
	})
}

// ---- AzureRegistryStore ----

// State is always ok: there is no file to recover.
func (r *PostgresAzureRegistry) State(context.Context) interfaces.AzureRegistryStatus {
	return interfaces.AzureRegistryStatus{State: interfaces.AzureRegistryOK}
}

// PendingWrites is false: writes join the request transaction instead.
func (r *PostgresAzureRegistry) PendingWrites() bool { return false }

// BootID is empty: rows are never pending.
func (r *PostgresAzureRegistry) BootID() string { return "" }

// WithOwnerLock takes pg_advisory_xact_lock on the owner's key inside the
// request transaction when ctx carries one, else inside a transaction of its
// own that commits when fn returns nil (post-commit hooks run after it).
// Advisory locks are reentrant within a session, so nesting is safe.
func (r *PostgresAzureRegistry) WithOwnerLock(ctx context.Context, owner interfaces.AzureOwner, fn func(ctx context.Context) error) error {
	if _, err := r.stmts(owner); err != nil {
		return err
	}
	key := azureLockKey(owner)
	// withTx joins the request's transaction, or opens one whose post-commit hooks run after it commits.
	return r.b.withTx(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(txCtx, `SELECT pg_advisory_xact_lock($1)`, key); err != nil {
			return fmt.Errorf("azure registry: owner lock: %w", err)
		}
		return fn(txCtx)
	})
}

func (r *PostgresAzureRegistry) ListConnections(ctx context.Context, owner interfaces.AzureOwner) ([]interfaces.AzureConnectionRecord, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return nil, err
	}
	rows, err := r.b.query(ctx).QueryContext(ctx, st.listConns, owner.ID)
	if err != nil {
		return nil, mapAzureRegistryErr("list resources", err)
	}
	defer rows.Close()
	out := []interfaces.AzureConnectionRecord{}
	for rows.Next() {
		c, err := scanAzureConnection(rows)
		if err != nil {
			return nil, mapAzureRegistryErr("list resources", err)
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, mapAzureRegistryErr("list resources", err)
	}
	return out, nil
}

func (r *PostgresAzureRegistry) GetConnection(ctx context.Context, owner interfaces.AzureOwner, id string) (interfaces.AzureConnectionRecord, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return interfaces.AzureConnectionRecord{}, err
	}
	c, err := scanAzureConnection(r.b.query(ctx).QueryRowContext(ctx, st.getConn, owner.ID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return interfaces.AzureConnectionRecord{}, interfaces.ErrAzureConnectionNotFound
	}
	return c, mapAzureRegistryErr("get resource", err)
}

// ConnectionNameTaken asks the unique index's own question (lower(name)
// under the database's collation) inside the request's transaction, so it
// sees that request's own writes.
func (r *PostgresAzureRegistry) ConnectionNameTaken(ctx context.Context, owner interfaces.AzureOwner, name, exceptID string) (bool, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return false, err
	}
	var taken bool
	if err := r.b.query(ctx).QueryRowContext(ctx, st.connNameTaken, owner.ID, name, exceptID).Scan(&taken); err != nil {
		return false, mapAzureRegistryErr("check resource name", err)
	}
	return taken, nil
}

func (r *PostgresAzureRegistry) CreateConnection(ctx context.Context, owner interfaces.AzureOwner, rec interfaces.AzureConnectionRecord) (interfaces.AzureConnectionRecord, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return interfaces.AzureConnectionRecord{}, err
	}
	if rec.Pending {
		return interfaces.AzureConnectionRecord{}, fmt.Errorf("%w: the database registry writes no pending rows (its writes are transactional)", interfaces.ErrAzureRegistryInvalid)
	}
	if err := interfaces.ValidateAzureConnectionRecord(rec); err != nil {
		return interfaces.AzureConnectionRecord{}, err
	}
	var out interfaces.AzureConnectionRecord
	err = r.write(ctx, func(q DBTX) error {
		c, err := scanAzureConnection(q.QueryRowContext(ctx, st.createConn, owner.ID, rec.ID, rec.Name))
		if errors.Is(err, sql.ErrNoRows) {
			return interfaces.ErrAzureTooManyConnections
		}
		out = c
		return err
	})
	return out, mapAzureRegistryErr("create resource", err)
}

func (r *PostgresAzureRegistry) ImportConnection(ctx context.Context, owner interfaces.AzureOwner, rec interfaces.AzureConnectionRecord) (bool, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return false, err
	}
	rec.Pending = false
	if err := interfaces.ValidateAzureConnectionRecord(rec); err != nil {
		return false, err
	}
	created := false
	err = r.write(ctx, func(q DBTX) error {
		var n int
		if err := q.QueryRowContext(ctx, st.countConns, owner.ID).Scan(&n); err != nil {
			return err
		}
		if n >= interfaces.AzureMaxConnections {
			return interfaces.ErrAzureTooManyConnections
		}
		var id string
		err := q.QueryRowContext(ctx, st.importConn, owner.ID, rec.ID, rec.Name).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // a uniqueness rule skipped it
		}
		created = err == nil
		return err
	})
	return created, mapAzureRegistryErr("import resource", err)
}

func (r *PostgresAzureRegistry) UpdateConnection(ctx context.Context, owner interfaces.AzureOwner, id string, u interfaces.AzureConnectionUpdate) (interfaces.AzureConnectionRecord, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return interfaces.AzureConnectionRecord{}, err
	}
	if u.Name != "" {
		if err := interfaces.ValidateAzureConnectionRecord(interfaces.AzureConnectionRecord{ID: interfaces.AzureDefaultConnectionID, Name: u.Name}); err != nil {
			return interfaces.AzureConnectionRecord{}, err
		}
	}
	var out interfaces.AzureConnectionRecord
	err = r.write(ctx, func(q DBTX) error {
		c, err := scanAzureConnection(q.QueryRowContext(ctx, st.updateConn, owner.ID, id, u.Name, azureNullTime(u.ExpectedUpdatedAt)))
		if errors.Is(err, sql.ErrNoRows) {
			return r.missingOrConflict(ctx, q, st.connExists, owner, id, interfaces.ErrAzureConnectionNotFound)
		}
		out = c
		return err
	})
	return out, mapAzureRegistryErr("update resource", err)
}

// missingOrConflict tells a stale compare-and-swap token (the row exists)
// from a missing row after an UPDATE matched nothing.
func (r *PostgresAzureRegistry) missingOrConflict(ctx context.Context, q DBTX, existsSQL string, owner interfaces.AzureOwner, id string, notFound error) error {
	var exists bool
	if err := q.QueryRowContext(ctx, existsSQL, owner.ID, id).Scan(&exists); err != nil {
		return err
	}
	if exists {
		return interfaces.ErrAzureRegistryConflict
	}
	return notFound
}

// FinalizeConnection only checks the row exists: database rows are never
// pending.
func (r *PostgresAzureRegistry) FinalizeConnection(ctx context.Context, owner interfaces.AzureOwner, id string) error {
	st, err := r.stmts(owner)
	if err != nil {
		return err
	}
	var exists bool
	if err := r.b.query(ctx).QueryRowContext(ctx, st.connExists, owner.ID, id).Scan(&exists); err != nil {
		return mapAzureRegistryErr("finalize resource", err)
	}
	if !exists {
		return interfaces.ErrAzureConnectionNotFound
	}
	return nil
}

func (r *PostgresAzureRegistry) SetDiscoveryDismissed(ctx context.Context, owner interfaces.AzureOwner, id string, dismissed bool) (interfaces.AzureConnectionRecord, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return interfaces.AzureConnectionRecord{}, err
	}
	var out interfaces.AzureConnectionRecord
	err = r.write(ctx, func(q DBTX) error {
		c, err := scanAzureConnection(q.QueryRowContext(ctx, st.dismissConn, owner.ID, id, dismissed))
		if errors.Is(err, sql.ErrNoRows) {
			return interfaces.ErrAzureConnectionNotFound
		}
		out = c
		return err
	})
	return out, mapAzureRegistryErr("dismiss discovery", err)
}

func (r *PostgresAzureRegistry) RecordConnectionTest(ctx context.Context, owner interfaces.AzureOwner, id string, result *interfaces.AzureTestResult) error {
	st, err := r.stmts(owner)
	if err != nil {
		return err
	}
	return r.recordTest(ctx, "record resource test", st.testConn, owner, id, result, interfaces.ErrAzureConnectionNotFound)
}

func (r *PostgresAzureRegistry) recordTest(ctx context.Context, op, stmt string, owner interfaces.AzureOwner, id string, result *interfaces.AzureTestResult, notFound error) error {
	testArgs, err := azureTestArgs(result)
	if err != nil {
		return err
	}
	args := append([]any{owner.ID, id}, testArgs...)
	err = r.write(ctx, func(q DBTX) error {
		res, err := q.ExecContext(ctx, stmt, args...)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err != nil {
				return err
			}
			return notFound
		}
		return nil
	})
	return mapAzureRegistryErr(op, err)
}

func (r *PostgresAzureRegistry) DeleteConnection(ctx context.Context, owner interfaces.AzureOwner, id string) ([]interfaces.AzureModelRecord, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return nil, err
	}
	deleted := []interfaces.AzureModelRecord{}
	err = r.write(ctx, func(q DBTX) error {
		rows, err := q.QueryContext(ctx, st.deleteConnModels, owner.ID, id)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			m, err := scanAzureModel(rows)
			if err != nil {
				return err
			}
			deleted = append(deleted, m)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		res, err := q.ExecContext(ctx, st.deleteConn, owner.ID, id)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err != nil {
				return err
			}
			return interfaces.ErrAzureConnectionNotFound
		}
		return nil
	})
	if err != nil {
		return nil, mapAzureRegistryErr("delete resource", err)
	}
	sortAzureModels(deleted)
	return deleted, nil
}

func (r *PostgresAzureRegistry) ListModels(ctx context.Context, owner interfaces.AzureOwner) ([]interfaces.AzureModelRecord, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return nil, err
	}
	rows, err := r.b.query(ctx).QueryContext(ctx, st.listModels, owner.ID)
	if err != nil {
		return nil, mapAzureRegistryErr("list models", err)
	}
	defer rows.Close()
	out := []interfaces.AzureModelRecord{}
	for rows.Next() {
		m, err := scanAzureModel(rows)
		if err != nil {
			return nil, mapAzureRegistryErr("list models", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, mapAzureRegistryErr("list models", err)
	}
	return out, nil
}

func (r *PostgresAzureRegistry) GetModel(ctx context.Context, owner interfaces.AzureOwner, id string) (interfaces.AzureModelRecord, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return interfaces.AzureModelRecord{}, err
	}
	m, err := scanAzureModel(r.b.query(ctx).QueryRowContext(ctx, st.getModel, owner.ID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return interfaces.AzureModelRecord{}, interfaces.ErrAzureModelNotFound
	}
	return m, mapAzureRegistryErr("get model", err)
}

// modelInsertArgs are createModel's / importModel's parameters (the org's
// is_default last).
func modelInsertArgs(owner interfaces.AzureOwner, rec interfaces.AzureModelRecord) []any {
	args := []any{owner.ID, rec.ID, rec.ConnectionID, rec.Kind, rec.Deployment, rec.DisplayName, rec.Purpose,
		rec.PurposeCustom, rec.Description, azureNullInt(rec.ContextWindow), azureNullInt(rec.MaxOutputTokens),
		rec.IsKnowledgeEmbedding}
	if owner.IsOrg() {
		args = append(args, rec.IsDefault)
	}
	return args
}

func (r *PostgresAzureRegistry) CreateModel(ctx context.Context, owner interfaces.AzureOwner, rec interfaces.AzureModelRecord) (interfaces.AzureModelRecord, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return interfaces.AzureModelRecord{}, err
	}
	if err := interfaces.ValidateAzureModelRecord(owner, rec); err != nil {
		return interfaces.AzureModelRecord{}, err
	}
	var out interfaces.AzureModelRecord
	err = r.write(ctx, func(q DBTX) error {
		m, err := scanAzureModel(q.QueryRowContext(ctx, st.createModel, modelInsertArgs(owner, rec)...))
		if errors.Is(err, sql.ErrNoRows) {
			return interfaces.ErrAzureTooManyModels
		}
		out = m
		return err
	})
	return out, mapAzureRegistryErr("create model", err)
}

func (r *PostgresAzureRegistry) ImportModel(ctx context.Context, owner interfaces.AzureOwner, rec interfaces.AzureModelRecord) (bool, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return false, err
	}
	if err := interfaces.ValidateAzureModelRecord(owner, rec); err != nil {
		return false, err
	}
	created := false
	err = r.write(ctx, func(q DBTX) error {
		var n int
		if err := q.QueryRowContext(ctx, st.countModels, owner.ID).Scan(&n); err != nil {
			return err
		}
		if n >= interfaces.AzureMaxModels {
			return interfaces.ErrAzureTooManyModels
		}
		var id string
		err := q.QueryRowContext(ctx, st.importModel, modelInsertArgs(owner, rec)...).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			return nil // a uniqueness rule skipped it
		}
		created = err == nil
		return err
	})
	return created, mapAzureRegistryErr("import model", err)
}

func (r *PostgresAzureRegistry) UpdateModel(ctx context.Context, owner interfaces.AzureOwner, id string, u interfaces.AzureModelUpdate) (interfaces.AzureModelRecord, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return interfaces.AzureModelRecord{}, err
	}
	if err := interfaces.ValidateAzureModelUpdate(u); err != nil {
		return interfaces.AzureModelRecord{}, err
	}
	var out interfaces.AzureModelRecord
	err = r.write(ctx, func(q DBTX) error {
		m, err := scanAzureModel(q.QueryRowContext(ctx, st.updateModel, owner.ID, id, u.ConnectionID, u.Kind,
			u.Deployment, u.DisplayName, u.Purpose, u.PurposeCustom, u.Description,
			azureNullInt(u.ContextWindow), azureNullInt(u.MaxOutputTokens), azureNullTime(u.ExpectedUpdatedAt)))
		if errors.Is(err, sql.ErrNoRows) {
			return r.missingOrConflict(ctx, q, st.modelExists, owner, id, interfaces.ErrAzureModelNotFound)
		}
		out = m
		return err
	})
	return out, mapAzureRegistryErr("update model", err)
}

func (r *PostgresAzureRegistry) RecordModelTest(ctx context.Context, owner interfaces.AzureOwner, id string, result *interfaces.AzureTestResult) error {
	st, err := r.stmts(owner)
	if err != nil {
		return err
	}
	return r.recordTest(ctx, "record model test", st.testModel, owner, id, result, interfaces.ErrAzureModelNotFound)
}

func (r *PostgresAzureRegistry) DeleteModel(ctx context.Context, owner interfaces.AzureOwner, id string) (interfaces.AzureModelRecord, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return interfaces.AzureModelRecord{}, err
	}
	var out interfaces.AzureModelRecord
	err = r.write(ctx, func(q DBTX) error {
		m, err := scanAzureModel(q.QueryRowContext(ctx, st.deleteModel, owner.ID, id))
		if errors.Is(err, sql.ErrNoRows) {
			return interfaces.ErrAzureModelNotFound
		}
		out = m
		return err
	})
	return out, mapAzureRegistryErr("delete model", err)
}

// SetDesignation clears the previous holder, then marks modelID, in one
// savepoint/transaction: a missing model or a refused CHECK rolls the clear
// back too; racing writers meet at the partial unique index.
func (r *PostgresAzureRegistry) SetDesignation(ctx context.Context, owner interfaces.AzureOwner, d interfaces.AzureDesignation, modelID string) error {
	st, err := r.stmts(owner)
	if err != nil {
		return err
	}
	var clearSQL, setSQL string
	switch d {
	case interfaces.AzureDesignationKnowledgeEmbedding:
		clearSQL, setSQL = st.clearKB, st.setKB
	case interfaces.AzureDesignationOrgDefault:
		if !owner.IsOrg() {
			return fmt.Errorf("%w: only organisations have a default for members", interfaces.ErrAzureInvalidDesignation)
		}
		clearSQL, setSQL = st.clearDefault, st.setDefault
	default:
		return fmt.Errorf("%w: designation %q", interfaces.ErrAzureRegistryInvalid, d)
	}
	err = r.write(ctx, func(q DBTX) error {
		if _, err := q.ExecContext(ctx, clearSQL, owner.ID, modelID); err != nil {
			return err
		}
		if modelID == "" {
			return nil
		}
		res, err := q.ExecContext(ctx, setSQL, owner.ID, modelID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n == 0 {
			if err != nil {
				return err
			}
			return interfaces.ErrAzureModelNotFound
		}
		return nil
	})
	return mapAzureRegistryErr("set designation", err)
}

func (r *PostgresAzureRegistry) Counts(ctx context.Context, owner interfaces.AzureOwner) (interfaces.AzureRegistryCounts, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return interfaces.AzureRegistryCounts{}, err
	}
	var c interfaces.AzureRegistryCounts
	if err := r.b.query(ctx).QueryRowContext(ctx, st.counts, owner.ID).Scan(&c.Connections, &c.Models); err != nil {
		return interfaces.AzureRegistryCounts{}, mapAzureRegistryErr("count", err)
	}
	return c, nil
}

func (r *PostgresAzureRegistry) ImportedAt(ctx context.Context, owner interfaces.AzureOwner) (time.Time, bool, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return time.Time{}, false, err
	}
	var at time.Time
	err = r.b.query(ctx).QueryRowContext(ctx, st.importedAt, owner.ID).Scan(&at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, mapAzureRegistryErr("read import marker", err)
	}
	return at.UTC(), true, nil
}

func (r *PostgresAzureRegistry) MarkImported(ctx context.Context, owner interfaces.AzureOwner) (time.Time, error) {
	st, err := r.stmts(owner)
	if err != nil {
		return time.Time{}, err
	}
	var at time.Time
	err = r.write(ctx, func(q DBTX) error {
		if _, err := q.ExecContext(ctx, st.markImported, owner.ID); err != nil {
			return err
		}
		return q.QueryRowContext(ctx, st.importedAt, owner.ID).Scan(&at)
	})
	if err != nil {
		return time.Time{}, mapAzureRegistryErr("write import marker", err)
	}
	return at.UTC(), nil
}
