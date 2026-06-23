package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// ---- User operations ----

func (b *PostgresStorageBackend) SaveUser(ctx context.Context, user *interfaces.User) error {
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt = now

	_, err := b.db.ExecContext(ctx, `
		INSERT INTO users (id, email, password, role, email_verified, failed_login_attempts, locked_until, display_name, avatar_url, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id) DO UPDATE SET
			email                 = EXCLUDED.email,
			password              = EXCLUDED.password,
			role                  = EXCLUDED.role,
			email_verified        = EXCLUDED.email_verified,
			failed_login_attempts = EXCLUDED.failed_login_attempts,
			locked_until          = EXCLUDED.locked_until,
			display_name          = EXCLUDED.display_name,
			avatar_url            = EXCLUDED.avatar_url,
			updated_at            = EXCLUDED.updated_at`,
		user.ID, user.Email, user.Password, string(user.Role), user.EmailVerified, user.FailedLoginAttempts, user.LockedUntil, user.DisplayName, user.AvatarURL, user.CreatedAt, user.UpdatedAt,
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
		INSERT INTO users (id, email, password, role, email_verified, failed_login_attempts, locked_until, display_name, avatar_url, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		user.ID, user.Email, user.Password, string(role), user.EmailVerified, user.FailedLoginAttempts, user.LockedUntil, user.DisplayName, user.AvatarURL, user.CreatedAt, user.UpdatedAt,
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
	email = strings.ToLower(email)
	var user interfaces.User
	var roleStr string
	err := b.db.QueryRowContext(ctx,
		`SELECT id, email, password, role, email_verified, failed_login_attempts, locked_until, display_name, avatar_url, created_at, updated_at FROM users WHERE email = $1`,
		email,
	).Scan(&user.ID, &user.Email, &user.Password, &roleStr, &user.EmailVerified, &user.FailedLoginAttempts, &user.LockedUntil, &user.DisplayName, &user.AvatarURL, &user.CreatedAt, &user.UpdatedAt)

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
		`SELECT id, email, password, role, email_verified, failed_login_attempts, locked_until, display_name, avatar_url, created_at, updated_at FROM users WHERE id = $1`,
		id,
	).Scan(&user.ID, &user.Email, &user.Password, &roleStr, &user.EmailVerified, &user.FailedLoginAttempts, &user.LockedUntil, &user.DisplayName, &user.AvatarURL, &user.CreatedAt, &user.UpdatedAt)

	if err == sql.ErrNoRows {
		return nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load user by id: %w", err)
	}
	user.Role = auth.Role(roleStr)
	return &user, nil
}

// LoadUsersByIDs resolves multiple users in a single query, avoiding the N+1
// pattern when decorating lists with owner info. Duplicate IDs are de-duplicated.
func (b *PostgresStorageBackend) LoadUsersByIDs(ctx context.Context, ids []string) (map[string]*interfaces.User, error) {
	out := make(map[string]*interfaces.User, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	// De-duplicate and build an IN ($1,$2,...) placeholder list (driver-agnostic;
	// avoids depending on a Postgres array codec).
	seen := make(map[string]bool, len(ids))
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	if len(args) == 0 {
		return out, nil
	}
	// #nosec G202 -- placeholders contains only generated "$N" tokens, never user
	// input; all id values are passed as parameterized args below.
	q := `SELECT id, email, role, email_verified, failed_login_attempts, locked_until, created_at, updated_at FROM users WHERE id IN (` +
		strings.Join(placeholders, ",") + `)`
	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("load users by ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var u interfaces.User
		var roleStr string
		if err := rows.Scan(&u.ID, &u.Email, &roleStr, &u.EmailVerified, &u.FailedLoginAttempts, &u.LockedUntil, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.Role = auth.Role(roleStr)
		uu := u
		out[u.ID] = &uu
	}
	return out, rows.Err()
}

func (b *PostgresStorageBackend) CountUsers(ctx context.Context) (int, error) {
	var count int
	err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`).Scan(&count)
	if err != nil {
		return 0, fmt.Errorf("count users: %w", err)
	}
	return count, nil
}

func (b *PostgresStorageBackend) ListUsers(ctx context.Context, limit, offset int) ([]*interfaces.User, error) {
	var rows *sql.Rows
	var err error
	if limit > 0 {
		rows, err = b.db.QueryContext(ctx, `SELECT id, email, role, email_verified, failed_login_attempts, locked_until, created_at, updated_at FROM users ORDER BY created_at DESC LIMIT $1 OFFSET $2`, limit, offset)
	} else {
		rows, err = b.db.QueryContext(ctx, `SELECT id, email, role, email_verified, failed_login_attempts, locked_until, created_at, updated_at FROM users ORDER BY created_at DESC`)
	}
	if err != nil {
		return nil, fmt.Errorf("list users: %w", err)
	}
	defer rows.Close()

	var users []*interfaces.User
	for rows.Next() {
		var u interfaces.User
		var roleStr string
		if err := rows.Scan(&u.ID, &u.Email, &roleStr, &u.EmailVerified, &u.FailedLoginAttempts, &u.LockedUntil, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan user: %w", err)
		}
		u.Role = auth.Role(roleStr)
		users = append(users, &u)
	}
	return users, rows.Err()
}

func (b *PostgresStorageBackend) ListAdmins(ctx context.Context) ([]*interfaces.User, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, email, role, email_verified, failed_login_attempts, locked_until, created_at, updated_at FROM users WHERE role = 'admin'`)
	if err != nil {
		return nil, fmt.Errorf("list admins: %w", err)
	}
	defer rows.Close()

	var users []*interfaces.User
	for rows.Next() {
		var u interfaces.User
		var roleStr string
		if err := rows.Scan(&u.ID, &u.Email, &roleStr, &u.EmailVerified, &u.FailedLoginAttempts, &u.LockedUntil, &u.CreatedAt, &u.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan admin: %w", err)
		}
		u.Role = auth.Role(roleStr)
		users = append(users, &u)
	}
	return users, rows.Err()
}

func (b *PostgresStorageBackend) UpdateUserRole(ctx context.Context, id string, role auth.Role) error {
	_, err := b.db.ExecContext(ctx, `UPDATE users SET role = $1, updated_at = NOW() WHERE id = $2`, string(role), id)
	return err
}

func (b *PostgresStorageBackend) UpdateUserPassword(ctx context.Context, id string, passwordHash string) error {
	_, err := b.db.ExecContext(ctx, `UPDATE users SET password = $1, failed_login_attempts = 0, locked_until = NULL, updated_at = NOW() WHERE id = $2`, passwordHash, id)
	return err
}

// UpdateUserProfile updates the user's display name and avatar URL.
func (b *PostgresStorageBackend) UpdateUserProfile(ctx context.Context, id string, displayName, avatarURL string) error {
	res, err := b.db.ExecContext(ctx,
		`UPDATE users SET display_name = $1, avatar_url = $2, updated_at = NOW() WHERE id = $3`,
		displayName, avatarURL, id,
	)
	if err != nil {
		return fmt.Errorf("update user profile: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update user profile rows affected: %w", err)
	}
	if n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
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
	// Best-effort purge of expired rows. Non-fatal (the insert already
	// succeeded), but log failures so unbounded table growth is observable.
	if _, err := b.db.ExecContext(ctx, `DELETE FROM refresh_tokens WHERE expires_at < NOW()`); err != nil {
		slog.Warn("refresh token cleanup failed", "err", err)
	}
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
// Returns ErrTokenAlreadyRevoked if the token was already revoked, so callers
// can detect a concurrent rotation race (two requests with the same jti).
func (b *PostgresStorageBackend) RevokeRefreshToken(ctx context.Context, jti string) error {
	res, err := b.db.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked = TRUE WHERE jti = $1 AND NOT revoked`, jti,
	)
	if err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke refresh token: rows affected: %w", err)
	}
	if n == 0 {
		return interfaces.ErrTokenAlreadyRevoked
	}
	return nil
}

// VerifyAndRevokeRefreshToken atomically verifies and revokes a refresh token
// in a single atomic operation. Returns the claims if successful, or
// ErrTokenAlreadyRevoked if the token was already revoked/invalid.
// This eliminates the race window between VerifyRefresh and RevokeRefreshToken.
func (b *PostgresStorageBackend) VerifyAndRevokeRefreshToken(ctx context.Context, jti string) (*interfaces.RefreshTokenInfo, error) {
	var info interfaces.RefreshTokenInfo
	err := b.db.QueryRowContext(ctx, `
		UPDATE refresh_tokens
		SET revoked = TRUE
		WHERE jti = $1
		  AND NOT revoked
		  AND expires_at > NOW()
		RETURNING jti, user_id, created_at, expires_at`,
		jti,
	).Scan(&info.ID, &info.UserID, &info.CreatedAt, &info.ExpiresAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, interfaces.ErrTokenAlreadyRevoked
		}
		return nil, fmt.Errorf("verify and revoke refresh token: %w", err)
	}
	return &info, nil
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

// ListUserRefreshTokens returns every active (non-revoked, non-expired)
// refresh token for a user, surfaced to the user as "active sessions".
func (b *PostgresStorageBackend) ListUserRefreshTokens(ctx context.Context, userID string) ([]interfaces.RefreshTokenInfo, error) {
	rows, err := b.db.QueryContext(ctx,
		`SELECT jti, created_at, expires_at FROM refresh_tokens
		 WHERE user_id = $1 AND NOT revoked AND expires_at > NOW()
		 ORDER BY created_at DESC`,
		userID,
	)
	if err != nil {
		return nil, fmt.Errorf("list user refresh tokens: %w", err)
	}
	defer rows.Close()

	var sessions []interfaces.RefreshTokenInfo
	for rows.Next() {
		var s interfaces.RefreshTokenInfo
		if err := rows.Scan(&s.ID, &s.CreatedAt, &s.ExpiresAt); err != nil {
			return nil, fmt.Errorf("scan refresh token: %w", err)
		}
		sessions = append(sessions, s)
	}
	return sessions, rows.Err()
}

// RevokeRefreshTokenForUser revokes a single refresh token by jti, but only if
// it belongs to userID — this lets a user revoke their own sessions without
// being able to revoke another user's by guessing a jti.
func (b *PostgresStorageBackend) RevokeRefreshTokenForUser(ctx context.Context, jti, userID string) error {
	res, err := b.db.ExecContext(ctx,
		`UPDATE refresh_tokens SET revoked = TRUE WHERE jti = $1 AND user_id = $2 AND NOT revoked`,
		jti, userID,
	)
	if err != nil {
		return fmt.Errorf("revoke refresh token: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("revoke refresh token rows affected: %w", err)
	}
	if n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

// ---- Audit log ----

func (b *PostgresStorageBackend) SaveAuditEvent(ctx context.Context, event *interfaces.AuditEvent) error {
	meta, err := json.Marshal(event.Meta)
	if err != nil {
		meta = []byte("{}")
	}
	_, err = b.db.ExecContext(ctx,
		`INSERT INTO audit_events (id, user_id, email, action, resource_type, resource_id, ip, meta, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		event.ID, event.UserID, event.Email, event.Action,
		event.ResourceType, event.ResourceID, event.IP, meta, event.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("save audit event: %w", err)
	}
	return nil
}

func (b *PostgresStorageBackend) ListAuditEvents(ctx context.Context, filter interfaces.AuditFilter) ([]*interfaces.AuditEvent, error) {
	limit := filter.Limit
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}

	args := []any{}
	where := []string{}
	i := 1
	if filter.UserID != "" {
		where = append(where, fmt.Sprintf("user_id = $%d", i))
		args = append(args, filter.UserID)
		i++
	}
	if filter.Action != "" {
		where = append(where, fmt.Sprintf("action = $%d", i))
		args = append(args, filter.Action)
		i++
	}
	if filter.Since != nil {
		where = append(where, fmt.Sprintf("created_at >= $%d", i))
		args = append(args, filter.Since.UTC())
		i++
	}

	q := `SELECT id, user_id, email, action, resource_type, resource_id, ip, meta, created_at FROM audit_events`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	// #nosec G202 -- only generated "$N" placeholders are concatenated; all values are parameterized args.
	q += fmt.Sprintf(" ORDER BY created_at DESC LIMIT $%d OFFSET $%d", i, i+1)
	args = append(args, limit, offset)

	rows, err := b.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list audit events: %w", err)
	}
	defer rows.Close()

	var events []*interfaces.AuditEvent
	for rows.Next() {
		ev := &interfaces.AuditEvent{}
		var metaRaw []byte
		if err := rows.Scan(&ev.ID, &ev.UserID, &ev.Email, &ev.Action,
			&ev.ResourceType, &ev.ResourceID, &ev.IP, &metaRaw, &ev.CreatedAt); err != nil {
			return nil, err
		}
		if len(metaRaw) > 0 {
			if err := json.Unmarshal(metaRaw, &ev.Meta); err != nil {
				slog.Warn("audit event: failed to unmarshal metadata", "eventID", ev.ID, "error", err)
			}
		}
		events = append(events, ev)
	}
	if events == nil {
		events = []*interfaces.AuditEvent{}
	}
	return events, rows.Err()
}

// ---- Usage metrics ----

func (b *PostgresStorageBackend) SaveUsageMetric(ctx context.Context, metric *interfaces.UsageMetric) error {
	query := `
		INSERT INTO usage_metrics (id, user_id, org_id, provider, model, prompt_tokens, completion_tokens, estimated_cost, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`
	_, err := b.db.ExecContext(ctx, query,
		metric.ID, metric.UserID, metric.OrgID, metric.Provider, metric.Model,
		metric.PromptTokens, metric.CompletionTokens, metric.EstimatedCost, metric.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("postgres: insert usage_metric: %w", err)
	}
	return nil
}

// GetDailyUsage returns the total estimated AI spend (in dollars) accrued so far
// in the current calendar day. When orgID is non-empty it sums the org's usage,
// otherwise it sums the user's. Used to enforce the per-account/org daily budget.
func (b *PostgresStorageBackend) GetDailyUsage(ctx context.Context, userID, orgID string) (float64, error) {
	var (
		total float64
		err   error
	)
	if orgID != "" {
		err = b.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(estimated_cost), 0) FROM usage_metrics
			 WHERE org_id = $1 AND created_at >= date_trunc('day', now())`, orgID).Scan(&total)
	} else {
		err = b.db.QueryRowContext(ctx,
			`SELECT COALESCE(SUM(estimated_cost), 0) FROM usage_metrics
			 WHERE user_id = $1 AND created_at >= date_trunc('day', now())`, userID).Scan(&total)
	}
	if err != nil {
		return 0, fmt.Errorf("postgres: get daily usage: %w", err)
	}
	return total, nil
}
