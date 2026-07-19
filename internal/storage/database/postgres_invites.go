package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// ---- Organisation invite operations ----

func (b *PostgresStorageBackend) SaveOrgInvite(ctx context.Context, invite *interfaces.OrgInvite) error {
	if invite.CreatedAt.IsZero() {
		invite.CreatedAt = time.Now().UTC()
	}
	_, err := b.db.ExecContext(ctx, `
		INSERT INTO org_invites (id, org_id, email, role, invited_by, token_hash, expires_at, accepted_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		invite.ID, invite.OrgID, invite.Email, string(invite.Role), invite.InvitedBy,
		invite.TokenHash, invite.ExpiresAt, invite.AcceptedAt, invite.CreatedAt)
	if err != nil {
		if isPgErrCode(err, pgErrUniqueViolation) {
			return interfaces.ErrOrgInviteExists
		}
		return fmt.Errorf("insert org invite: %w", err)
	}
	return nil
}

func (b *PostgresStorageBackend) ListOrgInvites(ctx context.Context, orgID string) ([]*interfaces.OrgInvite, error) {
	rows, err := b.db.QueryContext(ctx, `
		SELECT id, org_id, email, role, invited_by, token_hash, expires_at, accepted_at, created_at
		FROM org_invites WHERE org_id = $1 ORDER BY created_at DESC`, orgID)
	if err != nil {
		return nil, fmt.Errorf("query org invites: %w", err)
	}
	defer rows.Close()

	var invites []*interfaces.OrgInvite
	for rows.Next() {
		inv, err := scanOrgInvite(rows)
		if err != nil {
			return nil, err
		}
		invites = append(invites, inv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate org invites: %w", err)
	}
	return invites, nil
}

func (b *PostgresStorageBackend) GetOrgInvite(ctx context.Context, orgID, inviteID string) (*interfaces.OrgInvite, error) {
	row := b.db.QueryRowContext(ctx, `
		SELECT id, org_id, email, role, invited_by, token_hash, expires_at, accepted_at, created_at
		FROM org_invites WHERE id = $1 AND org_id = $2`, inviteID, orgID)
	return scanOrgInvite(row)
}

func (b *PostgresStorageBackend) GetOrgInviteByTokenHash(ctx context.Context, tokenHash string) (*interfaces.OrgInvite, error) {
	row := b.db.QueryRowContext(ctx, `
		SELECT id, org_id, email, role, invited_by, token_hash, expires_at, accepted_at, created_at
		FROM org_invites WHERE token_hash = $1`, tokenHash)
	return scanOrgInvite(row)
}

func (b *PostgresStorageBackend) DeleteOrgInvite(ctx context.Context, orgID, inviteID string) error {
	res, err := b.db.ExecContext(ctx, `DELETE FROM org_invites WHERE id = $1 AND org_id = $2`, inviteID, orgID)
	if err != nil {
		return fmt.Errorf("delete org invite: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete org invite rows affected: %w", err)
	}
	if n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

func (b *PostgresStorageBackend) MarkOrgInviteAccepted(ctx context.Context, inviteID string, acceptedAt time.Time) error {
	// H11: WHERE accepted_at IS NULL + RETURNING makes the single-use contract
	// transactional. Without the guard two concurrent AcceptInvite calls both
	// pass the read-side nil check and both succeed (AddMember is idempotent).
	// RETURNING id lets us detect a missing/already-accepted row — the
	// ExecContext result's RowsAffected would also work but RETURNING is
	// unambiguous about WHICH row matched.
	var matchedID string
	err := b.db.QueryRowContext(ctx,
		`UPDATE org_invites SET accepted_at = $1 WHERE id = $2 AND accepted_at IS NULL RETURNING id`,
		acceptedAt, inviteID,
	).Scan(&matchedID)
	if err == sql.ErrNoRows {
		return interfaces.ErrOrgInviteAlreadyAccepted
	}
	if err != nil {
		return fmt.Errorf("mark org invite accepted: %w", err)
	}
	return nil
}

// rowScanner abstracts *sql.Row and *sql.Rows so scanOrgInvite can be used by
// both single-row and multi-row queries.
type rowScanner interface {
	Scan(dest ...any) error
}

func scanOrgInvite(row rowScanner) (*interfaces.OrgInvite, error) {
	var inv interfaces.OrgInvite
	var roleStr string
	var acceptedAt sql.NullTime
	if err := row.Scan(&inv.ID, &inv.OrgID, &inv.Email, &roleStr, &inv.InvitedBy,
		&inv.TokenHash, &inv.ExpiresAt, &acceptedAt, &inv.CreatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("scan org invite: %w", err)
	}
	inv.Role = auth.Role(roleStr)
	if acceptedAt.Valid {
		t := acceptedAt.Time
		inv.AcceptedAt = &t
	}
	return &inv, nil
}
