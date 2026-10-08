package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// ---- Organisation operations ----

func (b *PostgresStorageBackend) SaveOrg(ctx context.Context, org *interfaces.Organisation) error {
	return b.withTx(ctx, func(_ context.Context, tx *sql.Tx) error { return b.upsertOrgInTx(ctx, tx, org) })
}

// upsertOrgInTx is SaveOrg's body: an INSERT ... ON CONFLICT upsert — distinct from saveOrgInTx,
// MutateOrg's plain UPDATE of a row it already holds under FOR UPDATE. Neither commits; withTx owns that.
func (b *PostgresStorageBackend) upsertOrgInTx(ctx context.Context, tx *sql.Tx, org *interfaces.Organisation) error {
	now := time.Now().UTC()
	if org.CreatedAt.IsZero() {
		org.CreatedAt = now
	}
	org.UpdatedAt = now

	_, err := tx.ExecContext(ctx, `
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

	return syncOrgMembers(ctx, tx, org, now)
}

// syncOrgMembers reconciles org_members to exactly org.Members without delete-all-then-reinsert: present
// members upserted (joined_at preserved), absent ones deleted; upserts run as chunked multi-row INSERTs.
func syncOrgMembers(ctx context.Context, tx *sql.Tx, org *interfaces.Organisation, now time.Time) error {
	const rowsPerChunk = 16000 // 1 + 3×16000 params < 65535
	userIDs := make([]string, 0, len(org.Members))
	joined := make([]time.Time, 0, len(org.Members))
	roles := make([]string, 0, len(org.Members))
	for _, m := range org.Members {
		j := m.JoinedAt
		if j.IsZero() {
			j = now
		}
		userIDs = append(userIDs, m.UserID)
		joined = append(joined, j)
		roles = append(roles, string(m.Role))
	}
	for start := 0; start < len(userIDs); start += rowsPerChunk {
		end := min(start+rowsPerChunk, len(userIDs))
		var sb strings.Builder
		args := make([]any, 0, (end-start)*3+1)
		args = append(args, org.ID)
		sb.WriteString(`INSERT INTO org_members (org_id, user_id, role, joined_at) VALUES `)
		for i := start; i < end; i++ {
			if i > start {
				sb.WriteByte(',')
			}
			base := (i-start)*3 + 2
			fmt.Fprintf(&sb, "($1,$%d,$%d,$%d)", base, base+1, base+2)
			args = append(args, userIDs[i], roles[i], joined[i])
		}
		sb.WriteString(` ON CONFLICT (org_id, user_id) DO UPDATE SET role = EXCLUDED.role`)
		if _, err := tx.ExecContext(ctx, sb.String(), args...); err != nil {
			return fmt.Errorf("upsert members %d..%d: %w", start, end, err)
		}
	}
	// An empty userIDs slice makes `user_id = ANY('{}')` always false, so NOT(...) deletes every
	// row — matching the old delete-all behavior when the set is emptied.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM org_members WHERE org_id = $1 AND NOT (user_id = ANY($2))`,
		org.ID, userIDs); err != nil {
		return fmt.Errorf("prune removed members: %w", err)
	}
	return nil
}

func (b *PostgresStorageBackend) LoadOrg(ctx context.Context, id string) (*interfaces.Organisation, error) {
	row := b.query(ctx).QueryRowContext(ctx,
		`SELECT id, name, owner_id, created_at, updated_at FROM organisations WHERE id = $1`, id)

	var org interfaces.Organisation
	if err := row.Scan(&org.ID, &org.Name, &org.OwnerID, &org.CreatedAt, &org.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("scan org: %w", err)
	}

	rows, err := b.query(ctx).QueryContext(ctx,
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
	rows, err := b.query(ctx).QueryContext(ctx, `
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

	// Members come with the list: the organisations panel reads org.members to decide who is an
	// admin and render the roster (a nil list was a TypeError on the first row). One extra query.
	if err := b.attachOrgMembers(ctx, orgs); err != nil {
		return nil, err
	}
	return orgs, nil
}

// OrgMemberRole answers one membership question with one round trip and two primary-key probes instead
// of LoadOrg's org row plus the WHOLE roster — on every flow authz check. A missing org is ErrNotFound.
func (b *PostgresStorageBackend) OrgMemberRole(ctx context.Context, orgID, userID string) (auth.Role, bool, error) {
	var orgExists bool
	var role sql.NullString
	err := b.query(ctx).QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM organisations WHERE id = $1),
		       (SELECT role FROM org_members WHERE org_id = $1 AND user_id = $2)`,
		orgID, userID).Scan(&orgExists, &role)
	if err != nil {
		return "", false, fmt.Errorf("org member role: %w", err)
	}
	if !orgExists {
		return "", false, interfaces.ErrNotFound
	}
	if !role.Valid {
		return "", false, nil
	}
	return auth.Role(role.String), true, nil
}

// ListOrgMembershipsForUser returns the caller's own memberships (org id, name, role) — no rosters;
// "which orgs am I in" callers (the library's org scope, twice per page) use this, not ListOrgsForUser.
func (b *PostgresStorageBackend) ListOrgMembershipsForUser(ctx context.Context, userID string) ([]interfaces.OrgMembership, error) {
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT o.id, o.name, m.role
		FROM org_members m
		JOIN organisations o ON o.id = m.org_id
		WHERE m.user_id = $1
		ORDER BY o.updated_at DESC, o.id`, userID)
	if err != nil {
		return nil, fmt.Errorf("query org memberships: %w", err)
	}
	defer rows.Close()
	out := []interfaces.OrgMembership{}
	for rows.Next() {
		var m interfaces.OrgMembership
		var roleStr string
		if err := rows.Scan(&m.OrgID, &m.OrgName, &roleStr); err != nil {
			return nil, fmt.Errorf("scan org membership: %w", err)
		}
		m.Role = auth.Role(roleStr)
		out = append(out, m)
	}
	return out, rows.Err()
}

// attachOrgMembers fills Members on each org in one round trip.
func (b *PostgresStorageBackend) attachOrgMembers(ctx context.Context, orgs []*interfaces.Organisation) error {
	if len(orgs) == 0 {
		return nil
	}
	ids := make([]string, 0, len(orgs))
	byID := make(map[string]*interfaces.Organisation, len(orgs))
	for _, o := range orgs {
		ids = append(ids, o.ID)
		byID[o.ID] = o
		o.Members = []interfaces.OrgMember{}
	}
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT org_id, user_id, role, joined_at
		FROM org_members WHERE org_id = ANY($1)
		ORDER BY joined_at ASC`, ids)
	if err != nil {
		return fmt.Errorf("query org members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var orgID string
		var m interfaces.OrgMember
		if err := rows.Scan(&orgID, &m.UserID, &m.Role, &m.JoinedAt); err != nil {
			return fmt.Errorf("scan org member: %w", err)
		}
		if o := byID[orgID]; o != nil {
			o.Members = append(o.Members, m)
		}
	}
	return rows.Err()
}

// DeleteOrg deletes the organisation with every domain's org-delete steps (lifecycle_*.go), in one transaction: the
// configuration and content it owns, its flows detached to their owners, then the row, whose foreign keys cascade
// to members, invites, settings, credentials and the Azure registry. Usage, alerts and the audit trail keep the id
// as history. A missing organisation is not an error.
func (b *PostgresStorageBackend) DeleteOrg(ctx context.Context, id string) error {
	return b.withTx(ctx, func(_ context.Context, tx *sql.Tx) error {
		return b.deleteOrgInTx(ctx, tx, id)
	})
}

func (b *PostgresStorageBackend) MutateOrg(ctx context.Context, id string, fn func(*interfaces.Organisation) error) error {
	return b.withTx(ctx, func(_ context.Context, tx *sql.Tx) error {
		org, err := b.loadOrgForUpdate(ctx, tx, id)
		if err != nil {
			return err
		}
		if err := fn(org); err != nil {
			return err
		}
		return b.saveOrgInTx(ctx, tx, org)
	})
}

func (b *PostgresStorageBackend) loadOrgForUpdate(ctx context.Context, tx *sql.Tx, id string) (*interfaces.Organisation, error) {
	row := tx.QueryRowContext(ctx, `SELECT id, name, owner_id, created_at, updated_at FROM organisations WHERE id = $1 FOR UPDATE`, id)
	var org interfaces.Organisation
	if err := row.Scan(&org.ID, &org.Name, &org.OwnerID, &org.CreatedAt, &org.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("load org for update: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `SELECT user_id, role, joined_at FROM org_members WHERE org_id = $1`, id)
	if err != nil {
		return nil, fmt.Errorf("load org members: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var m interfaces.OrgMember
		var roleStr string
		if err := rows.Scan(&m.UserID, &roleStr, &m.JoinedAt); err != nil {
			return nil, fmt.Errorf("scan org member: %w", err)
		}
		m.Role = auth.Role(roleStr)
		org.Members = append(org.Members, m)
	}
	return &org, rows.Err()
}

func (b *PostgresStorageBackend) saveOrgInTx(ctx context.Context, tx *sql.Tx, org *interfaces.Organisation) error {
	_, err := tx.ExecContext(ctx, `UPDATE organisations SET name = $1, owner_id = $2, updated_at = $3 WHERE id = $4`,
		org.Name, org.OwnerID, org.UpdatedAt, org.ID)
	if err != nil {
		return fmt.Errorf("update org in tx: %w", err)
	}
	return syncOrgMembers(ctx, tx, org, time.Now().UTC())
}
