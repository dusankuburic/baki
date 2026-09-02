package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// Postgres governance-alerts implementation. See migration v12
// (governanceAlertsSQL) for the table + RLS policy. RLS inherits the parent
// flow's visibility, so a user calling through the API (RLS-scoped via the
// middleware tx or queryRLS) sees alerts only for flows they can see; the
// scanner writes with a system context (no app.current_user_id → RLS bypassed
// via the `NOT app_rls_active()` arm).

// govAlertListDefaultLimit is the cap when a caller omits a limit, matching the
// dashboard/list conventions (enough to populate the bell panel without an
// unbounded query).
const govAlertListDefaultLimit = 50

// RecordGovernanceAlert persists a new alert. On a duplicate ID the row is left
// as-is (ON CONFLICT DO NOTHING) so a scanner retry is safe. The caller stamps
// ID + CreatedAt; the server stamps read_at/dismissed_at as NULL.
func (b *PostgresStorageBackend) RecordGovernanceAlert(ctx context.Context, a *storageif.GovernanceAlert) error {
	if a == nil || a.ID == "" {
		return errGovAlertMissingID
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	_, err := b.query(ctx).ExecContext(ctx, `
		INSERT INTO gov_alerts (id, flow_id, org_id, type, title, message, severity,
		                        new_errors, new_warnings, health_score, prev_health, created_at, target_user_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (id) DO NOTHING`,
		a.ID, a.FlowID, a.OrgID, a.Type, a.Title, a.Message, a.Severity,
		a.NewErrors, a.NewWarnings, a.HealthScore, a.PrevHealth, a.CreatedAt, a.TargetUser)
	return err
}

// ListGovernanceAlerts returns visible alerts newest-first. Dismissed alerts are
// hidden unless the filter opts in.
func (b *PostgresStorageBackend) ListGovernanceAlerts(ctx context.Context, filter storageif.GovernanceAlertFilter) ([]*storageif.GovernanceAlert, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = govAlertListDefaultLimit
	}
	q := `
		SELECT id, flow_id, org_id, type, title, message, severity,
		       new_errors, new_warnings, health_score, prev_health,
		       created_at, read_at, dismissed_at, target_user_id
		FROM gov_alerts`
	// WHERE assembly: visibility (team-wide OR targeted at the caller) AND
	// the dismissed toggle. Targeted alerts (assignment/comment) are personal;
	// a caller never sees another user's.
	where := []string{}
	args := []any{}
	if !filter.IncludeDismissed {
		where = append(where, "dismissed_at IS NULL")
	}
	if filter.UserID != "" {
		where = append(where, fmt.Sprintf("(target_user_id = '' OR target_user_id = $%d)", len(args)+1))
		args = append(args, filter.UserID)
	} else {
		where = append(where, "target_user_id = ''")
	}
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	q += fmt.Sprintf(` ORDER BY created_at DESC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	args = append(args, limit, filter.Offset)
	rows, err := b.query(ctx).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*storageif.GovernanceAlert, 0)
	for rows.Next() {
		a, err := scanGovAlert(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UnreadGovernanceAlertCount returns the number of visible, unacknowledged
// (read_at IS NULL) and non-dismissed alerts — the bell badge value.
func (b *PostgresStorageBackend) UnreadGovernanceAlertCount(ctx context.Context) (int, error) {
	return b.UnreadGovernanceAlertCountFor(ctx, "")
}

// UnreadGovernanceAlertCountFor is the badge query scoped to one caller:
// team-wide alerts plus that user's targeted ones.
func (b *PostgresStorageBackend) UnreadGovernanceAlertCountFor(ctx context.Context, userID string) (int, error) {
	var n int
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gov_alerts
		 WHERE read_at IS NULL AND dismissed_at IS NULL
		   AND (target_user_id = '' OR target_user_id = $1)`, userID).Scan(&n)
	return n, err
}

// MarkGovernanceAlertRead stamps read_at on one alert. Idempotent (a second call
// is a no-op — read_at is only set when NULL).
func (b *PostgresStorageBackend) MarkGovernanceAlertRead(ctx context.Context, userID, alertID string) error {
	// B1.9: explicit visibility predicate IN ADDITION to RLS — the RLS
	// transaction fails open on transient errors, so the WHERE clause is the
	// load-bearing tenant boundary here. Empty userID = local mode (no
	// claims) which is single-tenant by construction.
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE gov_alerts SET read_at = NOW()
		 WHERE id = $1 AND read_at IS NULL
		   AND ($2 = '' OR target_user_id = '' OR target_user_id = $2)`, alertID, userID)
	return err
}

// MarkAllGovernanceAlertsRead clears the badge: stamps read_at on every visible
// unread alert. RLS scopes the UPDATE to the caller's visible rows.
func (b *PostgresStorageBackend) MarkAllGovernanceAlertsRead(ctx context.Context, userID string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE gov_alerts SET read_at = NOW()
		 WHERE read_at IS NULL AND dismissed_at IS NULL
		   AND ($1 = '' OR target_user_id = '' OR target_user_id = $1)`, userID)
	return err
}

// DismissGovernanceAlert stamps dismissed_at on one alert. Idempotent.
func (b *PostgresStorageBackend) DismissGovernanceAlert(ctx context.Context, userID, alertID string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE gov_alerts SET dismissed_at = NOW()
		 WHERE id = $1 AND dismissed_at IS NULL
		   AND ($2 = '' OR target_user_id = '' OR target_user_id = $2)`, alertID, userID)
	return err
}

// ClearGovernanceAlerts permanently deletes the caller's visible dismissed
// alerts. Non-dismissed alerts are retained.
func (b *PostgresStorageBackend) ClearGovernanceAlerts(ctx context.Context, userID string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM gov_alerts
		 WHERE dismissed_at IS NOT NULL
		   AND ($1 = '' OR target_user_id = '' OR target_user_id = $1)`, userID)
	return err
}

// errGovAlertMissingID is returned when RecordGovernanceAlert is called without
// an ID (the scanner always mints a UUID; a missing ID is a programmer error).
var errGovAlertMissingID = govAlertError("governance alert requires an id")

func govAlertError(msg string) error { return &govAlertErr{msg: msg} }

type govAlertErr struct{ msg string }

func (e *govAlertErr) Error() string { return e.msg }

// scanGovAlert reads one alert row into a GovernanceAlert.
func scanGovAlert(rows *sql.Rows) (*storageif.GovernanceAlert, error) {
	var a storageif.GovernanceAlert
	var readAt, dismissedAt sql.NullTime
	if err := rows.Scan(
		&a.ID, &a.FlowID, &a.OrgID, &a.Type, &a.Title, &a.Message, &a.Severity,
		&a.NewErrors, &a.NewWarnings, &a.HealthScore, &a.PrevHealth,
		&a.CreatedAt, &readAt, &dismissedAt, &a.TargetUser,
	); err != nil {
		return nil, err
	}
	if readAt.Valid {
		t := readAt.Time
		a.ReadAt = &t
	}
	if dismissedAt.Valid {
		t := dismissedAt.Time
		a.DismissedAt = &t
	}
	return &a, nil
}
