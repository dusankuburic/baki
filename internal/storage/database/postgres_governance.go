package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// Postgres governance-alerts implementation (table + RLS policy: migration v12).
// RLS inherits the parent flow's visibility; the scanner writes with a system
// context (the NOT app_rls_active() arm).

// govAlertListDefaultLimit caps a caller-omitted limit (bell panel sized,
// no unbounded query).
const govAlertListDefaultLimit = 50

// govAlertVisible restricts gov_alerts rows to those whose parent flow the caller can see (owner, collaborator,
// or org member); user is the placeholder the caller's id is bound at.
// It mirrors rls_gov_alerts_visible in migration v12 line for line — THE TWO MUST STAY IN SYNC — and IS the
// application-layer tenant boundary: the handlers do no authz of their own, and RLS is inert under a
// BYPASSRLS role. The old target_user_id-only scoping was never a boundary — with RLS off one
// MarkAllGovernanceAlertsRead cleared every org's badge; that clause survives below only as a narrowing.
func govAlertVisible(user string) string {
	return fmt.Sprintf(`EXISTS (
		    SELECT 1 FROM flows f
		    WHERE f.id = gov_alerts.flow_id
		      AND (f.owner_id = %[1]s
		           OR EXISTS (SELECT 1 FROM flow_collaborators fc WHERE fc.flow_id = f.id AND fc.user_id = %[1]s)
		           OR EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = f.org_id AND om.user_id = %[1]s)))`,
		user)
}

// govAlertWhere assembles the full row-visibility WHERE fragment for one caller: the flow-visibility scope
// (the tenant boundary) AND the target_user_id narrowing (personal vs team-wide), shared by every read and mutation.
// userID is bound on args.
func govAlertWhere(userID string, args *sqlArgs) string {
	if userID == "" {
		// Local mode: no claims, single tenant. Only team-wide alerts exist.
		return "target_user_id = ''"
	}
	user := args.add(userID)
	return "((target_user_id = '' OR target_user_id = " + user + ") AND " + govAlertVisible(user) + ")"
}

// govVisibleFlows yields the ids of every flow userID can see (the same three
// access paths govAlertVisible tests, index-backed). Reads use it as
// `gov_alerts.flow_id IN (…)` so Postgres starts from the caller's flows — the
// badge is polled every minute per tab — and the semi-join returns an alert once.
func govVisibleFlows(user string) string {
	return fmt.Sprintf(`SELECT id FROM flows WHERE owner_id = %[1]s
			    UNION ALL
			    SELECT f.id FROM flows f WHERE f.org_id IN (SELECT om.org_id FROM org_members om WHERE om.user_id = %[1]s)
			    UNION ALL
			    SELECT fc.flow_id FROM flow_collaborators fc WHERE fc.user_id = %[1]s`, user)
}

// govAlertReadWhere is govAlertWhere for the statements over many alerts (the list, the badge, "mark all read" and
// "clear"): the same target_user_id narrowing, with the flow scope expressed through govVisibleFlows, so they start
// from the caller's flows instead of testing every tenant's alerts. The one-alert mutations, keyed by id, keep
// govAlertWhere.
func govAlertReadWhere(userID string, args *sqlArgs) string {
	if userID == "" {
		// Local mode: no claims, single tenant. Only team-wide alerts exist.
		return "target_user_id = ''"
	}
	user := args.add(userID)
	return "((target_user_id = '' OR target_user_id = " + user + ") AND flow_id IN (" + govVisibleFlows(user) + "))"
}

// listGovAlertsSQL is the ListGovernanceAlerts statement (exposed for the
// plan tests). limit is already defaulted.
func listGovAlertsSQL(filter storageif.GovernanceAlertFilter, limit int) (string, []any) {
	q := `
		SELECT id, flow_id, org_id, type, title, message, severity,
		       new_errors, new_warnings, health_score, prev_health,
		       created_at, read_at, dismissed_at, target_user_id
		FROM gov_alerts`
	// WHERE assembly: the dismissed toggle AND row visibility (govAlertReadWhere
	// — the team-wide/targeted narrowing AND the flow scope).
	where := []string{}
	args := sqlArgs{}
	if !filter.IncludeDismissed {
		where = append(where, "dismissed_at IS NULL")
	}
	where = append(where, govAlertReadWhere(filter.UserID, &args))
	q += " WHERE " + strings.Join(where, " AND ")
	// `id` as final tiebreaker: LIMIT/OFFSET paging with created_at ties (one
	// scanner tick stamps several alerts alike) otherwise drops and repeats rows.
	q += ` ORDER BY created_at DESC, id ASC LIMIT ` + args.add(limit) + ` OFFSET ` + args.add(filter.Offset)
	return q, args
}

// unreadGovAlertCountSQL is the UnreadGovernanceAlertCountFor statement
// (exposed for the plan tests).
func unreadGovAlertCountSQL(userID string) (string, []any) {
	var args sqlArgs
	vis := govAlertReadWhere(userID, &args)
	return `SELECT COUNT(*) FROM gov_alerts
		 WHERE read_at IS NULL AND dismissed_at IS NULL AND ` + vis, args
}

// RecordGovernanceAlert persists a new alert; a duplicate ID is left as-is
// (ON CONFLICT DO NOTHING) so a scanner retry is safe. Server stamps
// read_at/dismissed_at NULL.
func (b *PostgresStorageBackend) RecordGovernanceAlert(ctx context.Context, a *storageif.GovernanceAlert) error {
	_, err := b.InsertGovernanceAlert(ctx, a)
	return err
}

// InsertGovernanceAlert is RecordGovernanceAlert reporting whether the row is new: RETURNING yields no row when the
// ID was already there.
func (b *PostgresStorageBackend) InsertGovernanceAlert(ctx context.Context, a *storageif.GovernanceAlert) (bool, error) {
	if a == nil || a.ID == "" {
		return false, errGovAlertMissingID
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	var id string
	err := b.query(ctx).QueryRowContext(ctx, `
		INSERT INTO gov_alerts (id, flow_id, org_id, type, title, message, severity,
		                        new_errors, new_warnings, health_score, prev_health, created_at, target_user_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (id) DO NOTHING
		RETURNING id`,
		a.ID, a.FlowID, a.OrgID, a.Type, a.Title, a.Message, a.Severity,
		a.NewErrors, a.NewWarnings, a.HealthScore, a.PrevHealth, a.CreatedAt, a.TargetUser).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ListGovernanceAlerts returns visible alerts newest-first. Dismissed alerts are
// hidden unless the filter opts in.
func (b *PostgresStorageBackend) ListGovernanceAlerts(ctx context.Context, filter storageif.GovernanceAlertFilter) ([]*storageif.GovernanceAlert, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = govAlertListDefaultLimit
	}
	q, args := listGovAlertsSQL(filter, limit)
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
	q, args := unreadGovAlertCountSQL(userID)
	var n int
	err := b.query(ctx).QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// MarkGovernanceAlertRead stamps read_at on one alert. Idempotent (a second call
// is a no-op — read_at is only set when NULL).
func (b *PostgresStorageBackend) MarkGovernanceAlertRead(ctx context.Context, userID, alertID string) error {
	// Explicit visibility predicate IN ADDITION to RLS — the application-layer
	// tenant boundary (RLS is inert under a BYPASSRLS role; see govAlertVisible).
	args := sqlArgs{alertID}
	vis := govAlertWhere(userID, &args)
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE gov_alerts SET read_at = NOW()
		 WHERE id = $1 AND read_at IS NULL AND `+vis, args...)
	return err
}

// markAllGovAlertsReadSQL is the MarkAllGovernanceAlertsRead statement (exposed for the plan tests).
func markAllGovAlertsReadSQL(userID string) (string, []any) {
	var args sqlArgs
	vis := govAlertReadWhere(userID, &args)
	return `UPDATE gov_alerts SET read_at = NOW()
		 WHERE read_at IS NULL AND dismissed_at IS NULL AND ` + vis, args
}

// MarkAllGovernanceAlertsRead clears the badge: stamps read_at on every visible
// unread alert. RLS scopes the UPDATE to the caller's visible rows.
func (b *PostgresStorageBackend) MarkAllGovernanceAlertsRead(ctx context.Context, userID string) error {
	q, args := markAllGovAlertsReadSQL(userID)
	_, err := b.query(ctx).ExecContext(ctx, q, args...)
	return err
}

// DismissGovernanceAlert stamps dismissed_at on one alert. Idempotent.
func (b *PostgresStorageBackend) DismissGovernanceAlert(ctx context.Context, userID, alertID string) error {
	args := sqlArgs{alertID}
	vis := govAlertWhere(userID, &args)
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE gov_alerts SET dismissed_at = NOW()
		 WHERE id = $1 AND dismissed_at IS NULL AND `+vis, args...)
	return err
}

// clearGovAlertsSQL is the ClearGovernanceAlerts statement (exposed for the plan tests).
func clearGovAlertsSQL(userID string) (string, []any) {
	// The most destructive of the six: an unscoped predicate here DELETED other
	// orgs' dismissed alerts, and needed no identifier to do it.
	var args sqlArgs
	vis := govAlertReadWhere(userID, &args)
	return `DELETE FROM gov_alerts
		 WHERE dismissed_at IS NOT NULL AND ` + vis, args
}

// ClearGovernanceAlerts permanently deletes the caller's visible dismissed
// alerts. Non-dismissed alerts are retained.
func (b *PostgresStorageBackend) ClearGovernanceAlerts(ctx context.Context, userID string) error {
	q, args := clearGovAlertsSQL(userID)
	_, err := b.query(ctx).ExecContext(ctx, q, args...)
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
