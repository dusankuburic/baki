// Package database implements StorageBackend backed by PostgreSQL.
// It is intended for cloud/multi-tenant deployments.  Local desktop mode uses
// the filesystem backend instead.
package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/opentelemetry-go-extra/otelsql"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
)

// pgErrUniqueViolation is the SQLSTATE for a unique constraint violation.
const pgErrUniqueViolation = "23505"

// pgErrSerializationFailure is the SQLSTATE returned to a transaction that
// could not be serialized; the caller should retry.
const pgErrSerializationFailure = "40001"

// isPgErrCode reports whether err is a *pgconn.PgError with the given SQLSTATE.
func isPgErrCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == code
	}
	return false
}

// azureRefreshState holds the credential and pgx config needed to keep the
// Managed Identity token alive for connections opened after each refresh cycle.
type azureRefreshState struct {
	provider *azureTokenProvider
	pgxCfg   *pgx.ConnConfig
}

// PostgresStorageBackend implements interfaces.StorageBackend using PostgreSQL.
type PostgresStorageBackend struct {
	db           *sql.DB
	azureRefresh *azureRefreshState // non-nil when using Azure Managed Identity
}

// Config holds the connection settings for the PostgreSQL backend.
type Config struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// DefaultConfig returns conservative connection-pool defaults.
func DefaultConfig(dsn string) Config {
	return Config{
		DSN:             dsn,
		MaxOpenConns:    25,
		MaxIdleConns:    5,
		ConnMaxLifetime: time.Hour,
	}
}

// New opens a PostgreSQL connection, configures the pool, and runs migrations.
// ctx governs the application lifetime: it is used for migrations and, when
// Azure Managed Identity is configured, drives the background token-refresh
// goroutine (cancelled on SIGTERM / graceful shutdown).
func New(ctx context.Context, cfg Config) (*PostgresStorageBackend, error) {
	pgxCfg, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	b := &PostgresStorageBackend{}

	connStr := stdlib.RegisterConnConfig(pgxCfg)

	if pgxCfg.Password == "managed-identity" {
		provider, err := newAzureTokenProvider()
		if err != nil {
			return nil, fmt.Errorf("azure: create credential: %w", err)
		}
		token, err := provider.GetToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure: initial token fetch: %w", err)
		}
		pgxCfg.Password = token
		// Re-register so the pool's connStr picks up the resolved token.
		stdlib.UnregisterConnConfig(connStr)
		connStr = stdlib.RegisterConnConfig(pgxCfg)
		b.azureRefresh = &azureRefreshState{provider: provider, pgxCfg: pgxCfg}
	}

	db, err := otelsql.Open("pgx", connStr,
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
		otelsql.WithDBName(pgxCfg.Database),
	)
	if err != nil {
		return nil, fmt.Errorf("open pgx with otel: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	b.db = db
	if err := b.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	if b.azureRefresh != nil {
		go b.runAzureTokenRefresh(ctx)
	}

	return b, nil
}

// runAzureTokenRefresh refreshes the Managed Identity token every 20 minutes so
// that new connections opened after each cycle always get a valid token.
// Azure tokens expire after ~24 hours; refreshing every 20 minutes gives ample
// margin.  Existing idle connections are cycled out naturally by ConnMaxLifetime
// (default 1 hour), so they too will pick up fresh tokens before expiry.
func (b *PostgresStorageBackend) runAzureTokenRefresh(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			token, err := b.azureRefresh.provider.GetToken(ctx)
			if err != nil {
				slog.Error("azure: managed identity token refresh failed", "err", err)
				continue
			}
			b.azureRefresh.pgxCfg.Password = token
			slog.Info("azure: managed identity token refreshed")
		}
	}
}

// ---- StorageBackend implementation ----

func (b *PostgresStorageBackend) Ping(ctx context.Context) error {
	return b.db.PingContext(ctx)
}

func (b *PostgresStorageBackend) Close() error {
	return b.db.Close()
}

// DB exposes the underlying *sql.DB for callers that need pool-level
// telemetry (e.g. the metrics package's ObservePostgresPool). Not part
// of the StorageBackend interface — only the concrete Postgres backend
// has a pool to observe.
func (b *PostgresStorageBackend) DB() *sql.DB { return b.db }

// SaveFlow upserts a flow document.
func (b *PostgresStorageBackend) SaveFlow(ctx context.Context, flow *interfaces.FlowDocument) error {
	content := flow.Content
	if len(content) == 0 {
		content = []byte("{}")
	}
	meta, err := json.Marshal(flow.Metadata)
	if err != nil {
		return fmt.Errorf("marshal metadata: %w", err)
	}
	now := time.Now().UTC()
	_, err = b.db.ExecContext(ctx, `
		INSERT INTO flows (id, name, description, content, metadata, owner_id, org_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4::jsonb, $5::jsonb, $6, $7, $8, $9)
		ON CONFLICT (id) DO UPDATE SET
			name        = EXCLUDED.name,
			description = EXCLUDED.description,
			content     = EXCLUDED.content,
			metadata    = EXCLUDED.metadata,
			owner_id    = EXCLUDED.owner_id,
			org_id      = EXCLUDED.org_id,
			updated_at  = EXCLUDED.updated_at`,
		flow.ID, flow.Name, flow.Description, string(content), string(meta),
		flow.OwnerID, flow.OrganizationID, now, now,
	)
	if err != nil {
		return fmt.Errorf("upsert flow: %w", err)
	}
	flow.UpdatedAt = now
	return nil
}

// LoadFlow retrieves a flow document by ID.
func (b *PostgresStorageBackend) LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	row := b.db.QueryRowContext(ctx,
		`SELECT id, name, description, content, metadata, owner_id, org_id, created_at, updated_at
		 FROM flows WHERE id = $1`, id)

	var flow interfaces.FlowDocument
	var contentRaw, metaRaw []byte
	if err := row.Scan(
		&flow.ID, &flow.Name, &flow.Description,
		&contentRaw, &metaRaw,
		&flow.OwnerID, &flow.OrganizationID,
		&flow.CreatedAt, &flow.UpdatedAt,
	); err != nil {
		if err == sql.ErrNoRows {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("scan flow: %w", err)
	}
	flow.Content = contentRaw
	if err := json.Unmarshal(metaRaw, &flow.Metadata); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}
	return &flow, nil
}

// ListFlows returns flows matching the filter.
func (b *PostgresStorageBackend) ListFlows(ctx context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	where := []string{"1=1"}
	args := []any{}
	n := 1

	// Ownership / org filtering: show flows owned by UserID, belonging to OrganizationID,
	// or where the user is an explicit collaborator.
	if filter.UserID != "" {
		collabSubquery := fmt.Sprintf("(SELECT 1 FROM flow_collaborators WHERE flow_id = id AND user_id = $%d)", n)
		if filter.OrganizationID != "" {
			where = append(where, fmt.Sprintf("(owner_id = $%d OR org_id = $%d OR EXISTS %s)", n, n+1, collabSubquery))
			args = append(args, filter.UserID, filter.OrganizationID)
			n += 2
		} else {
			where = append(where, fmt.Sprintf("(owner_id = $%d OR EXISTS %s)", n, collabSubquery))
			args = append(args, filter.UserID)
			n++
		}
	} else if filter.OrganizationID != "" {
		where = append(where, fmt.Sprintf("org_id = $%d", n))
		args = append(args, filter.OrganizationID)
		n++
	}

	if filter.Query != "" {
		where = append(where, fmt.Sprintf("name ILIKE $%d", n))
		args = append(args, "%"+filter.Query+"%")
		n++
	}
	if filter.CreatedAfter != nil {
		where = append(where, fmt.Sprintf("created_at > $%d", n))
		args = append(args, *filter.CreatedAfter)
		n++
	}
	if filter.CreatedBefore != nil {
		where = append(where, fmt.Sprintf("created_at < $%d", n))
		args = append(args, *filter.CreatedBefore)
		n++
	}

	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	args = append(args, limit, filter.Offset)

	q := fmt.Sprintf(`
		SELECT id, name, description, content, metadata, owner_id, org_id, created_at, updated_at
		FROM flows
		WHERE %s
		ORDER BY updated_at DESC
		LIMIT $%d OFFSET $%d`,
		strings.Join(where, " AND "), n, n+1)

	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list flows: %w", err)
	}
	defer rows.Close()

	var result []*interfaces.FlowDocument
	for rows.Next() {
		var flow interfaces.FlowDocument
		var contentRaw, metaRaw []byte
		if err := rows.Scan(
			&flow.ID, &flow.Name, &flow.Description,
			&contentRaw, &metaRaw,
			&flow.OwnerID, &flow.OrganizationID,
			&flow.CreatedAt, &flow.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan row: %w", err)
		}
		flow.Content = contentRaw
		if err := json.Unmarshal(metaRaw, &flow.Metadata); err != nil {
			return nil, fmt.Errorf("unmarshal metadata: %w", err)
		}
		result = append(result, &flow)
	}
	return result, rows.Err()
}

// DeleteFlow removes a flow by ID.
func (b *PostgresStorageBackend) DeleteFlow(ctx context.Context, id string) error {
	res, err := b.db.ExecContext(ctx, `DELETE FROM flows WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete flow: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

// SaveSettings upserts the single-row app settings record.
func (b *PostgresStorageBackend) SaveSettings(ctx context.Context, settings *interfaces.AppSettings) error {
	data, err := json.Marshal(settings)
	if err != nil {
		return fmt.Errorf("marshal settings: %w", err)
	}
	_, err = b.db.ExecContext(ctx, `
		INSERT INTO app_settings (id, data, updated_at) VALUES (1, $1, $2)
		ON CONFLICT (id) DO UPDATE SET data = EXCLUDED.data, updated_at = EXCLUDED.updated_at`,
		data, time.Now().UTC())
	return err
}

// LoadSettings retrieves the app settings.
func (b *PostgresStorageBackend) LoadSettings(ctx context.Context) (*interfaces.AppSettings, error) {
	var data []byte
	err := b.db.QueryRowContext(ctx, `SELECT data FROM app_settings WHERE id = 1`).Scan(&data)
	if err == sql.ErrNoRows {
		return &interfaces.AppSettings{Version: 1}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load settings: %w", err)
	}
	var s interfaces.AppSettings
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("unmarshal settings: %w", err)
	}
	return &s, nil
}

// SaveConversation upserts a conversation (chat history) for a flow+scope.
func (b *PostgresStorageBackend) SaveConversation(ctx context.Context, flowID, scope string, messages []interfaces.ChatMessage) error {
	data, err := json.Marshal(messages)
	if err != nil {
		return fmt.Errorf("marshal messages: %w", err)
	}
	_, err = b.db.ExecContext(ctx, `
		INSERT INTO conversations (flow_id, scope, messages, updated_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (flow_id, scope) DO UPDATE SET
			messages   = EXCLUDED.messages,
			updated_at = EXCLUDED.updated_at`,
		flowID, scope, data, time.Now().UTC())
	return err
}

// LoadConversation retrieves the conversation for a flow+scope. Returns a
// non-nil empty slice when no conversation exists yet — this matches the
// filesystem backend's semantics so callers can use a single nil-safe check
// across both backends. A `nil` return therefore always indicates an error.
func (b *PostgresStorageBackend) LoadConversation(ctx context.Context, flowID, scope string) ([]interfaces.ChatMessage, error) {
	var data []byte
	err := b.db.QueryRowContext(ctx,
		`SELECT messages FROM conversations WHERE flow_id = $1 AND scope = $2`,
		flowID, scope,
	).Scan(&data)
	if err == sql.ErrNoRows {
		return []interfaces.ChatMessage{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("load conversation: %w", err)
	}
	var msgs []interfaces.ChatMessage
	if err := json.Unmarshal(data, &msgs); err != nil {
		return nil, fmt.Errorf("unmarshal messages: %w", err)
	}
	if msgs == nil {
		// JSON "null" unmarshals to a nil slice; normalize to empty for parity.
		msgs = []interfaces.ChatMessage{}
	}
	return msgs, nil
}

// ---- User operations ----

func (b *PostgresStorageBackend) SaveUser(ctx context.Context, user *interfaces.User) error {
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt = now

	_, err := b.db.ExecContext(ctx, `
		INSERT INTO users (id, email, password, role, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (id) DO UPDATE SET
			email      = EXCLUDED.email,
			password   = EXCLUDED.password,
			role       = EXCLUDED.role,
			updated_at = EXCLUDED.updated_at`,
		user.ID, user.Email, user.Password, string(user.Role), user.CreatedAt, user.UpdatedAt,
	)
	if err != nil {
		if isPgErrCode(err, pgErrUniqueViolation) {
			return interfaces.ErrEmailExists
		}
		return fmt.Errorf("upsert user: %w", err)
	}
	return nil
}

// CreateUser atomically inserts a new user. If the users table is empty at the
// moment of insert, the user is promoted to RoleAdmin so the instance always
// has an initial administrator. The count and the insert run in a single
// SERIALIZABLE transaction; if the transaction loses a serialization race it
// is retried a few times before giving up. Returns ErrEmailExists on a unique
// email-constraint violation.
func (b *PostgresStorageBackend) CreateUser(ctx context.Context, user *interfaces.User) error {
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt = now

	const maxRetries = 5
	for range maxRetries {
		err := b.tryCreateUser(ctx, user)
		if err == nil {
			return nil
		}
		if isPgErrCode(err, pgErrSerializationFailure) {
			continue
		}
		return err
	}
	return fmt.Errorf("create user: serialization conflict after %d retries", maxRetries)
}

func (b *PostgresStorageBackend) tryCreateUser(ctx context.Context, user *interfaces.User) error {
	tx, err := b.db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count); err != nil {
		return fmt.Errorf("count users: %w", err)
	}
	role := user.Role
	if count == 0 {
		role = auth.RoleAdmin
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO users (id, email, password, role, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`,
		user.ID, user.Email, user.Password, string(role), user.CreatedAt, user.UpdatedAt,
	); err != nil {
		if isPgErrCode(err, pgErrUniqueViolation) {
			return interfaces.ErrEmailExists
		}
		return fmt.Errorf("insert user: %w", err)
	}

	if err := tx.Commit(); err != nil {
		// Commit may surface the serialization failure for a SERIALIZABLE tx.
		return err
	}
	user.Role = role
	return nil
}

func (b *PostgresStorageBackend) LoadUserByEmail(ctx context.Context, email string) (*interfaces.User, error) {
	var user interfaces.User
	var roleStr string
	err := b.db.QueryRowContext(ctx,
		`SELECT id, email, password, role, created_at, updated_at FROM users WHERE email = $1`,
		email,
	).Scan(&user.ID, &user.Email, &user.Password, &roleStr, &user.CreatedAt, &user.UpdatedAt)
	
	if err == sql.ErrNoRows {
		return nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user by email: %w", err)
	}
	user.Role = auth.Role(roleStr)
	return &user, nil
}

func (b *PostgresStorageBackend) LoadUserByID(ctx context.Context, id string) (*interfaces.User, error) {
	var user interfaces.User
	var roleStr string
	err := b.db.QueryRowContext(ctx,
		`SELECT id, email, password, role, created_at, updated_at FROM users WHERE id = $1`,
		id,
	).Scan(&user.ID, &user.Email, &user.Password, &roleStr, &user.CreatedAt, &user.UpdatedAt)
	
	if err == sql.ErrNoRows {
		return nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user by id: %w", err)
	}
	user.Role = auth.Role(roleStr)
	return &user, nil
}

func (b *PostgresStorageBackend) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return count, nil
}

func (b *PostgresStorageBackend) ListUsers(ctx context.Context) ([]*interfaces.User, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, email, role, created_at, updated_at FROM users ORDER BY created_at DESC`)
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []*interfaces.User
	for rows.Next() {
		var u interfaces.User
		var roleStr string
		if err := rows.Scan(&u.ID, &u.Email, &roleStr, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.Role = auth.Role(roleStr)
		users = append(users, &u)
	}
	return users, rows.Err()
}

// ---- Organisation operations ----

func (b *PostgresStorageBackend) SaveOrg(ctx context.Context, org *interfaces.Organisation) error {
	tx, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	now := time.Now().UTC()
	if org.CreatedAt.IsZero() {
		org.CreatedAt = now
	}
	org.UpdatedAt = now

	_, err = tx.ExecContext(ctx, `
		INSERT INTO organisations (id, name, owner_id, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (id) DO UPDATE SET
			name = EXCLUDED.name,
			owner_id = EXCLUDED.owner_id,
			updated_at = EXCLUDED.updated_at`,
		org.ID, org.Name, org.OwnerID, org.CreatedAt, org.UpdatedAt)
	if err != nil {
		return fmt.Errorf("upsert org: %w", err)
	}

	// Simple member sync: delete all and re-insert
	_, err = tx.ExecContext(ctx, `DELETE FROM org_members WHERE org_id = $1`, org.ID)
	if err != nil {
		return fmt.Errorf("clear members: %w", err)
	}

	for _, m := range org.Members {
		if m.JoinedAt.IsZero() {
			m.JoinedAt = now
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO org_members (org_id, user_id, role, joined_at)
			VALUES ($1, $2, $3, $4)`,
			org.ID, m.UserID, string(m.Role), m.JoinedAt)
		if err != nil {
			return fmt.Errorf("insert member: %w", err)
		}
	}

	return tx.Commit()
}

func (b *PostgresStorageBackend) LoadOrg(ctx context.Context, id string) (*interfaces.Organisation, error) {
	row := b.db.QueryRowContext(ctx,
		`SELECT id, name, owner_id, created_at, updated_at FROM organisations WHERE id = $1`, id)

	var org interfaces.Organisation
	if err := row.Scan(&org.ID, &org.Name, &org.OwnerID, &org.CreatedAt, &org.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("scan org: %w", err)
	}

	rows, err := b.db.QueryContext(ctx,
		`SELECT user_id, role, joined_at FROM org_members WHERE org_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("query members: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var m interfaces.OrgMember
		var roleStr string
		if err := rows.Scan(&m.UserID, &roleStr, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan member: %w", err)
		}
		m.Role = auth.Role(roleStr)
		org.Members = append(org.Members, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate members: %w", err)
	}

	return &org, nil
}

func (b *PostgresStorageBackend) ListOrgsForUser(ctx context.Context, userID string) ([]*interfaces.Organisation, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT o.id, o.name, o.owner_id, o.created_at, o.updated_at
		FROM organisations o
		JOIN org_members m ON o.id = m.org_id
		WHERE m.user_id = $1
		ORDER BY o.updated_at DESC`, userID)
	if err != nil {
		return nil, fmt.Errorf("query orgs: %w", err)
	}
	defer rows.Close()

	var orgs []*interfaces.Organisation
	for rows.Next() {
		var org interfaces.Organisation
		if err := rows.Scan(&org.ID, &org.Name, &org.OwnerID, &org.CreatedAt, &org.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan org: %w", err)
		}
		orgs = append(orgs, &org)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate orgs: %w", err)
	}

	// We don't load full member lists for the user's org list to keep it fast.
	// If needed, the caller can LoadOrg(id) for a specific one.
	return orgs, nil
}

func (b *PostgresStorageBackend) DeleteOrg(ctx context.Context, id string) error {
	_, err := b.db.ExecContext(ctx, `DELETE FROM organisations WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete org: %w", err)
	}
	return nil
}

// ---- Sharing operations ----

func (b *PostgresStorageBackend) ListCollaborators(ctx context.Context, flowID string) ([]*interfaces.Collaborator, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT c.user_id, u.email, c.permission, c.granted_at
		FROM flow_collaborators c
		JOIN users u ON c.user_id = u.id
		WHERE c.flow_id = $1
		ORDER BY c.granted_at ASC`, flowID)
	if err != nil {
		return nil, fmt.Errorf("query collaborators: %w", err)
	}
	defer rows.Close()

	// Contract: empty result is a non-nil empty slice (matches filesystem
	// backend), so callers can use `len(collabs) == 0` without backend-
	// specific nil checks. A nil return is reserved for errors.
	collabs := []*interfaces.Collaborator{}
	for rows.Next() {
		var c interfaces.Collaborator
		if err := rows.Scan(&c.UserID, &c.Email, &c.Permission, &c.GrantedAt); err != nil {
			return nil, fmt.Errorf("scan collaborator: %w", err)
		}
		collabs = append(collabs, &c)
	}
	return collabs, rows.Err()
}

func (b *PostgresStorageBackend) AddCollaborator(ctx context.Context, flowID string, c *interfaces.Collaborator) error {
	if c.GrantedAt.IsZero() {
		c.GrantedAt = time.Now().UTC()
	}
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO flow_collaborators (flow_id, user_id, permission, granted_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (flow_id, user_id) DO UPDATE SET
			permission = EXCLUDED.permission`,
		flowID, c.UserID, c.Permission, c.GrantedAt)
	if err != nil {
		return fmt.Errorf("add collaborator: %w", err)
	}
	return nil
}

func (b *PostgresStorageBackend) UpdateCollaborator(ctx context.Context, flowID, userID string, permission string) error {
	res, err := b.db.ExecContext(ctx, `
		UPDATE flow_collaborators SET permission = $1
		WHERE flow_id = $2 AND user_id = $3`,
		permission, flowID, userID)
	if err != nil {
		return fmt.Errorf("update collaborator: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

func (b *PostgresStorageBackend) RemoveCollaborator(ctx context.Context, flowID, userID string) error {
	res, err := b.db.ExecContext(ctx, `
		DELETE FROM flow_collaborators
		WHERE flow_id = $1 AND user_id = $2`,
		flowID, userID)
	if err != nil {
		return fmt.Errorf("remove collaborator: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

// ---- Schema migration ----

// migrate creates the required tables if they do not exist.
func (b *PostgresStorageBackend) migrate(ctx context.Context) error {
	_, err := b.db.ExecContext(ctx, schema)
	return err
}

// ---- Refresh-token rotation store ----
// These back the auth refresh-token rotation/revocation flow in cloud mode.

// StoreRefreshToken records an issued refresh token by its jti. It also makes a
// best-effort purge of already-expired rows to keep the table small.
func (b *PostgresStorageBackend) StoreRefreshToken(ctx context.Context, jti, userID string, expiresAt time.Time) error {
	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO refresh_tokens (jti, user_id, expires_at) VALUES ($1, $2, $3)
		 ON CONFLICT (jti) DO NOTHING`,
		jti, userID, expiresAt.UTC(),
	); err != nil {
		return fmt.Errorf("store refresh token: %w", err)
	}
	_, _ = b.db.ExecContext(ctx, `DELETE FROM refresh_tokens WHERE expires_at < NOW()`)
	return nil
}

// IsRefreshTokenValid reports whether the jti exists, is not revoked, and has
// not expired.
func (b *PostgresStorageBackend) IsRefreshTokenValid(ctx context.Context, jti string) (bool, error) {
	var ok bool
	err := b.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM refresh_tokens
		 WHERE jti = $1 AND NOT revoked AND expires_at > NOW())`,
		jti,
	).Scan(&ok)
	if err != nil {
		return false, fmt.Errorf("check refresh token: %w", err)
	}
	return ok, nil
}

// RevokeRefreshToken revokes a single refresh token by jti (used on rotation).
func (b *PostgresStorageBackend) RevokeRefreshToken(ctx context.Context, jti string) error {
	if _, err := b.db.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked = TRUE WHERE jti = $1`, jti,
	); err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	return nil
}

// RevokeUserRefreshTokens revokes every refresh token for a user (used on logout).
func (b *PostgresStorageBackend) RevokeUserRefreshTokens(ctx context.Context, userID string) error {
	if _, err := b.db.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked = TRUE WHERE user_id = $1 AND NOT revoked`, userID,
	); err != nil {
		return fmt.Errorf("revoke user refresh tokens: %w", err)
	}
	return nil
}

const schema = `
CREATE TABLE IF NOT EXISTS users (
	id          TEXT        PRIMARY KEY,
	email       TEXT        UNIQUE NOT NULL,
	password    TEXT        NOT NULL,
	role        TEXT        NOT NULL,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS flows (
	id          TEXT        PRIMARY KEY,
	name        TEXT        NOT NULL,
	description TEXT        NOT NULL DEFAULT '',
	content     JSONB       NOT NULL DEFAULT '{}',
	metadata    JSONB       NOT NULL DEFAULT '{}',
	owner_id    TEXT        NOT NULL DEFAULT '',
	org_id      TEXT        NOT NULL DEFAULT '',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS flows_owner_id_idx ON flows (owner_id);
CREATE INDEX IF NOT EXISTS flows_org_id_idx   ON flows (org_id);

CREATE TABLE IF NOT EXISTS app_settings (
	id         INTEGER     PRIMARY KEY DEFAULT 1 CHECK (id = 1),
	data       JSONB       NOT NULL DEFAULT '{}',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS conversations (
	flow_id    TEXT        NOT NULL,
	scope      TEXT        NOT NULL,
	messages   JSONB       NOT NULL DEFAULT '[]',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (flow_id, scope)
);

CREATE TABLE IF NOT EXISTS organisations (
	id          TEXT        PRIMARY KEY,
	name        TEXT        NOT NULL,
	owner_id    TEXT        NOT NULL,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS org_members (
	org_id    TEXT        NOT NULL REFERENCES organisations(id) ON DELETE CASCADE,
	user_id   TEXT        NOT NULL,
	role      TEXT        NOT NULL,
	joined_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (org_id, user_id)
);

CREATE TABLE IF NOT EXISTS flow_collaborators (
	flow_id    TEXT        NOT NULL,
	user_id    TEXT        NOT NULL,
	permission TEXT        NOT NULL DEFAULT 'viewer',
	granted_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (flow_id, user_id)
);
CREATE INDEX IF NOT EXISTS flow_collaborators_flow_id_idx ON flow_collaborators (flow_id);

CREATE TABLE IF NOT EXISTS refresh_tokens (
	jti        TEXT        PRIMARY KEY,
	user_id    TEXT        NOT NULL,
	expires_at TIMESTAMPTZ NOT NULL,
	revoked    BOOLEAN     NOT NULL DEFAULT FALSE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS refresh_tokens_user_id_idx ON refresh_tokens (user_id);

CREATE TABLE IF NOT EXISTS provider_keys (
	user_id    TEXT        NOT NULL DEFAULT '',
	provider   TEXT        NOT NULL,
	ciphertext TEXT        NOT NULL,
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (user_id, provider)
);

-- Migrate a pre-existing provider-only-PK table to the per-user (user_id, provider) PK.
ALTER TABLE provider_keys ADD COLUMN IF NOT EXISTS user_id TEXT NOT NULL DEFAULT '';
DO $$
BEGIN
	IF EXISTS (
		SELECT 1 FROM pg_constraint
		WHERE conrelid = 'provider_keys'::regclass AND contype = 'p'
		  AND array_length(conkey, 1) = 1
	) THEN
		ALTER TABLE provider_keys DROP CONSTRAINT provider_keys_pkey;
		ALTER TABLE provider_keys ADD PRIMARY KEY (user_id, provider);
	END IF;
END $$;
`
