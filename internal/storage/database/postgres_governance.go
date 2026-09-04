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

// govAlertVisible returns a SQL predicate restricting gov_alerts rows to those
// whose parent flow userID can see — owner, explicit collaborator, or member of
// the flow's org — plus the single bind arg it needs. It returns an empty clause
// for an empty userID: that is local/desktop mode, which has no claims and is
// single-tenant by construction.
//
// It is a deliberate, line-for-line mirror of the rls_gov_alerts_visible policy
// in migration v12 (see postgres_migrations.go). THE TWO MUST STAY IN SYNC.
//
// Why duplicate the policy in Go at all: the alert endpoints do no authz of
// their own — the handlers pass CallerID straight to these methods — so this
// clause IS the application-layer tenant boundary. Postgres RLS enforces the
// same rule, but only when the app connects as a role without BYPASSRLS; a
// superuser DSN (the docker-compose default until recently) turns RLS off
// entirely and leaves nothing behind it.
//
// The scoping used to be `target_user_id = ” OR target_user_id = $n` alone.
// `target_user_id = ”` means "team-wide", which is true of every team-wide
// alert in the deployment regardless of org — so with RLS off, one call to
// MarkAllGovernanceAlertsRead cleared every org's badge and one call to
// ClearGovernanceAlerts DELETED every org's dismissed alerts, with no
// identifier needed. That clause is still applied below: it correctly narrows
// personal (assignment/comment) alerts to their target. It was just never a
// tenant boundary.
//
// gov_alerts.flow_id is NOT NULL and REFERENCES flows(id) ON DELETE CASCADE, so
// this EXISTS can never strand a row whose flow has gone away.
func govAlertVisible(userID string, n int) (string, []any) {
	if userID == "" {
		return "", nil
	}
	return fmt.Sprintf(`EXISTS (
		    SELECT 1 FROM flows f
		    WHERE f.id = gov_alerts.flow_id
		      AND (f.owner_id = $%d
		           OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = $%d)
		           OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = $%d)))`,
		n, n, n), []any{userID}
}

// govAlertWhere assembles the full row-visibility WHERE fragment for one
// caller: the flow-visibility scope (the tenant boundary) AND the
// target_user_id narrowing (personal vs team-wide). Shared by every read and
// mutation below so they cannot drift apart.
func govAlertWhere(userID string, n int) (string, []any) {
	target := fmt.Sprintf("(target_user_id = '' OR target_user_id = $%d)", n)
	if userID == "" {
		// Local mode: no claims, single tenant. Only team-wide alerts exist.
		return "target_user_id = ''", nil
	}
	visible, args := govAlertVisible(userID, n)
	return "(" + target + " AND " + visible + ")", args
}

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
	// WHERE assembly: row visibility (govAlertWhere — flow scope AND the
	// team-wide/targeted narrowing) AND the dismissed toggle.
	where := []string{}
	args := []any{}
	if !filter.IncludeDismissed {
		where = append(where, "dismissed_at IS NULL")
	}
	visClause, visArgs := govAlertWhere(filter.UserID, len(args)+1)
	where = append(where, visClause)
	args = append(args, visArgs...)
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	// `id` as a final tiebreaker: the bell panel pages this list with
	// LIMIT/OFFSET, and created_at ties readily — one scanner tick records
	// several alerts for a flow, all stamped from the same time.Now(), and
	// TIMESTAMPTZ only keeps microseconds. Without a unique tiebreaker, SQL may
	// order a tied group differently on each page, so paging drops some alerts
	// and repeats others.
	q += fmt.Sprintf(` ORDER BY created_at DESC, id ASC LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
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
	vis, args := govAlertWhere(userID, 1)
	var n int
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT COUNT(*) FROM gov_alerts
		 WHERE read_at IS NULL AND dismissed_at IS NULL AND `+vis, args...).Scan(&n)
	return n, err
}

// MarkGovernanceAlertRead stamps read_at on one alert. Idempotent (a second call
// is a no-op — read_at is only set when NULL).
func (b *PostgresStorageBackend) MarkGovernanceAlertRead(ctx context.Context, userID, alertID string) error {
	// Explicit visibility predicate IN ADDITION to RLS. This clause is the
	// application-layer tenant boundary — the handler does no authz of its own,
	// and RLS is inert whenever the app connects as a superuser/BYPASSRLS role.
	// See govAlertVisible for why the old target_user_id-only form was not a
	// boundary at all.
	vis, args := govAlertWhere(userID, 2)
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE gov_alerts SET read_at = NOW()
		 WHERE id = $1 AND read_at IS NULL AND `+vis,
		append([]any{alertID}, args...)...)
	return err
}

// MarkAllGovernanceAlertsRead clears the badge: stamps read_at on every visible
// unread alert. RLS scopes the UPDATE to the caller's visible rows.
func (b *PostgresStorageBackend) MarkAllGovernanceAlertsRead(ctx context.Context, userID string) error {
	vis, args := govAlertWhere(userID, 1)
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE gov_alerts SET read_at = NOW()
		 WHERE read_at IS NULL AND dismissed_at IS NULL AND `+vis, args...)
	return err
}

// DismissGovernanceAlert stamps dismissed_at on one alert. Idempotent.
func (b *PostgresStorageBackend) DismissGovernanceAlert(ctx context.Context, userID, alertID string) error {
	vis, args := govAlertWhere(userID, 2)
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE gov_alerts SET dismissed_at = NOW()
		 WHERE id = $1 AND dismissed_at IS NULL AND `+vis,
		append([]any{alertID}, args...)...)
	return err
}

// ClearGovernanceAlerts permanently deletes the caller's visible dismissed
// alerts. Non-dismissed alerts are retained.
func (b *PostgresStorageBackend) ClearGovernanceAlerts(ctx context.Context, userID string) error {
	// The most destructive of the six: an unscoped predicate here DELETED other
	// orgs' dismissed alerts, and needed no identifier to do it.
	vis, args := govAlertWhere(userID, 1)
	_, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM gov_alerts
		 WHERE dismissed_at IS NOT NULL AND `+vis, args...)
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
