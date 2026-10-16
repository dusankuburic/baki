package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// The "new rules" notices (table and RLS policy: migration 0037). The scanner upserts them on the pool; users read,
// mark and dismiss them through the alerts inbox (GovernanceAlertService).

// ruleNoticeListDefaultLimit caps a caller-omitted limit: the inbox shows a few notices above the alerts.
const ruleNoticeListDefaultLimit = 10

// ruleNoticeVisible restricts rule_notices to the caller's: an org notice to the org's members, a user notice to that
// user; user is the placeholder the caller's id is bound at. It mirrors rls_rule_notices_visible (migration 0037) and
// IS the application-layer tenant boundary (RLS is inert under a BYPASSRLS role) — THE TWO MUST STAY IN SYNC. The
// policy admits an app notice only outside a user's scope; the scanner writes app notices only in a deployment
// without accounts (one tenant), so here every caller sees them.
//
// It names the generated columns, the policy scope and scope_id: user_id is scope_id exactly when scope = 'user' (NULL
// otherwise), org_id exactly when scope = 'org', so the rows are the same, and each arm is a probe of its own index
// (rule_notices_user_idx, rule_notices_org_idx, and rule_notices_app_idx since v49) combined in a BitmapOr. The member
// organisations are an uncorrelated ARRAY sub-select, evaluated once per statement: an EXISTS inside the OR could only
// run per row.
func ruleNoticeVisible(user string) string {
	return fmt.Sprintf(`(scope = 'app'
		 OR user_id = %[1]s
		 OR org_id = ANY(ARRAY(SELECT om.org_id FROM org_members om WHERE om.user_id = %[1]s)))`, user)
}

// upsertRuleNoticesSQL inserts a page's notices, skipping those whose user or organisation is gone (the foreign key
// would fail the whole page), and merges a known id's rule ids (sorted byte-wise, as Go sorts them). xmax is 0 only
// on a row this statement inserted.
const upsertRuleNoticesSQL = `
	INSERT INTO rule_notices AS n (id, kind, scope, scope_id, rule_set_digest, rule_ids, flow_count)
	SELECT v.id, v.kind, v.scope, v.scope_id, v.digest, v.rule_ids::jsonb, v.flow_count
	  FROM unnest($1::text[], $2::text[], $3::text[], $4::text[], $5::text[], $6::text[], $7::int[])
	       AS v(id, kind, scope, scope_id, digest, rule_ids, flow_count)
	 WHERE (v.scope <> 'user' OR EXISTS (SELECT 1 FROM users u WHERE u.id = v.scope_id))
	   AND (v.scope <> 'org' OR EXISTS (SELECT 1 FROM organisations o WHERE o.id = v.scope_id))
	ON CONFLICT (id) DO UPDATE SET
		rule_ids = (SELECT COALESCE(jsonb_agg(r ORDER BY r COLLATE "C"), '[]'::jsonb)
		              FROM (SELECT DISTINCT jsonb_array_elements_text(n.rule_ids || EXCLUDED.rule_ids) AS r) ids),
		flow_count = GREATEST(n.flow_count, EXCLUDED.flow_count),
		updated_at = NOW()
	RETURNING id, (xmax = 0)`

// UpsertRuleNotices records the scanner's notices for one page; returns the ids it inserted.
func (b *PostgresStorageBackend) UpsertRuleNotices(ctx context.Context, notices []*storageif.RuleNotice) ([]string, error) {
	merged, err := mergeRuleNotices(notices)
	if err != nil || len(merged) == 0 {
		return nil, err
	}
	var ids, kinds, scopes, scopeIDs, digests, ruleIDs []string
	var flowCounts []int
	for _, n := range merged {
		data, err := json.Marshal(n.RuleIDs)
		if err != nil {
			return nil, fmt.Errorf("marshal notice rule ids: %w", err)
		}
		ids, kinds, scopes = append(ids, n.ID), append(kinds, n.Kind), append(scopes, n.Scope)
		scopeIDs, digests, ruleIDs = append(scopeIDs, n.ScopeID), append(digests, n.RuleSetDigest), append(ruleIDs, string(data))
		flowCounts = append(flowCounts, n.FlowCount)
	}
	rows, err := b.query(ctx).QueryContext(ctx, upsertRuleNoticesSQL, ids, kinds, scopes, scopeIDs, digests, ruleIDs, flowCounts)
	if err != nil {
		return nil, fmt.Errorf("upsert rule notices: %w", err)
	}
	defer rows.Close()
	var inserted []string
	for rows.Next() {
		var id string
		var isNew bool
		if err := rows.Scan(&id, &isNew); err != nil {
			return nil, fmt.Errorf("scan rule notices upsert: %w", err)
		}
		if isNew {
			inserted = append(inserted, id)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("upsert rule notices: %w", err)
	}
	slices.Sort(inserted)
	return inserted, nil
}

// mergeRuleNotices validates notices and merges those sharing an id (one statement cannot upsert a row twice): the
// union of their rule ids, sorted, and the larger flow count. Ordered by id.
func mergeRuleNotices(notices []*storageif.RuleNotice) ([]*storageif.RuleNotice, error) {
	byID := make(map[string]*storageif.RuleNotice, len(notices))
	for _, n := range notices {
		if n == nil {
			continue
		}
		if err := validRuleNotice(n); err != nil {
			return nil, err
		}
		m := byID[n.ID]
		if m == nil {
			m = &storageif.RuleNotice{ID: n.ID, Kind: n.Kind, Scope: n.Scope, ScopeID: n.ScopeID, RuleSetDigest: n.RuleSetDigest}
			byID[n.ID] = m
		}
		m.RuleIDs = append(m.RuleIDs, n.RuleIDs...)
		m.FlowCount = max(m.FlowCount, n.FlowCount)
	}
	out := make([]*storageif.RuleNotice, 0, len(byID))
	for _, m := range byID {
		slices.Sort(m.RuleIDs)
		m.RuleIDs = slices.Compact(m.RuleIDs)
		if m.RuleIDs == nil {
			m.RuleIDs = []string{}
		}
		out = append(out, m)
	}
	slices.SortFunc(out, func(a, b *storageif.RuleNotice) int { return strings.Compare(a.ID, b.ID) })
	return out, nil
}

// validRuleNotice refuses a notice the table's checks would refuse, or whose id is not its scope's and digest's.
func validRuleNotice(n *storageif.RuleNotice) error {
	switch {
	case n.Kind == "":
		return fmt.Errorf("rule notice %q: kind is required", n.ID)
	case n.Scope != storageif.RuleNoticeScopeOrg && n.Scope != storageif.RuleNoticeScopeUser && n.Scope != storageif.RuleNoticeScopeApp:
		return fmt.Errorf("rule notice %q: unknown scope %q", n.ID, n.Scope)
	case (n.Scope == storageif.RuleNoticeScopeApp) != (n.ScopeID == ""):
		return fmt.Errorf("rule notice %q: scope %q with scope id %q", n.ID, n.Scope, n.ScopeID)
	case n.RuleSetDigest == "":
		return fmt.Errorf("rule notice %q: rule-set digest is required", n.ID)
	case n.ID != storageif.RuleNoticeID(n.Scope, n.ScopeID, n.RuleSetDigest):
		return fmt.Errorf("rule notice %q: id is not RuleNoticeID(%q, %q, %q)", n.ID, n.Scope, n.ScopeID, n.RuleSetDigest)
	}
	return nil
}

// listRuleNoticesSQL is the ListRuleNotices statement (exposed for the plan tests).
func listRuleNoticesSQL(userID string, limit int) (string, []any) {
	var args sqlArgs
	vis := ruleNoticeVisible(args.add(userID))
	return `SELECT id, kind, scope, scope_id, rule_set_digest, rule_ids, flow_count, created_at, updated_at, read_at, dismissed_at
		  FROM rule_notices
		 WHERE dismissed_at IS NULL AND ` + vis + `
		 ORDER BY created_at DESC, id ASC LIMIT ` + args.add(limit), args
}

// ListRuleNotices returns the caller's visible notices that are not dismissed, newest first.
func (b *PostgresStorageBackend) ListRuleNotices(ctx context.Context, userID string, limit int) ([]*storageif.RuleNotice, error) {
	if limit <= 0 {
		limit = ruleNoticeListDefaultLimit
	}
	q, args := listRuleNoticesSQL(userID, limit)
	rows, err := b.query(ctx).QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("list rule notices: %w", err)
	}
	defer rows.Close()
	out := make([]*storageif.RuleNotice, 0)
	for rows.Next() {
		var n storageif.RuleNotice
		var ruleIDs []byte
		var readAt, dismissedAt sql.NullTime
		if err := rows.Scan(&n.ID, &n.Kind, &n.Scope, &n.ScopeID, &n.RuleSetDigest, &ruleIDs, &n.FlowCount,
			&n.CreatedAt, &n.UpdatedAt, &readAt, &dismissedAt); err != nil {
			return nil, fmt.Errorf("scan rule notice: %w", err)
		}
		if err := json.Unmarshal(ruleIDs, &n.RuleIDs); err != nil {
			return nil, fmt.Errorf("decode rule notice %s rule ids: %w", n.ID, err)
		}
		if n.RuleIDs == nil {
			n.RuleIDs = []string{}
		}
		if readAt.Valid {
			t := readAt.Time
			n.ReadAt = &t
		}
		if dismissedAt.Valid {
			t := dismissedAt.Time
			n.DismissedAt = &t
		}
		out = append(out, &n)
	}
	return out, rows.Err()
}

// unreadRuleNoticeCountSQL is the UnreadRuleNoticeCount statement (exposed for the plan tests).
func unreadRuleNoticeCountSQL(userID string) (string, []any) {
	var args sqlArgs
	vis := ruleNoticeVisible(args.add(userID))
	return `SELECT COUNT(*) FROM rule_notices WHERE read_at IS NULL AND dismissed_at IS NULL AND ` + vis, args
}

// UnreadRuleNoticeCount counts the caller's visible notices that are neither read nor dismissed.
func (b *PostgresStorageBackend) UnreadRuleNoticeCount(ctx context.Context, userID string) (int, error) {
	q, args := unreadRuleNoticeCountSQL(userID)
	var n int
	err := b.query(ctx).QueryRowContext(ctx, q, args...).Scan(&n)
	return n, err
}

// markAllRuleNoticesReadSQL is the MarkAllRuleNoticesRead statement (exposed for the plan tests).
func markAllRuleNoticesReadSQL(userID string) (string, []any) {
	var args sqlArgs
	vis := ruleNoticeVisible(args.add(userID))
	return `UPDATE rule_notices SET read_at = NOW() WHERE read_at IS NULL AND dismissed_at IS NULL AND ` + vis, args
}

// MarkAllRuleNoticesRead stamps read_at on the caller's visible unread notices.
func (b *PostgresStorageBackend) MarkAllRuleNoticesRead(ctx context.Context, userID string) error {
	q, args := markAllRuleNoticesReadSQL(userID)
	_, err := b.query(ctx).ExecContext(ctx, q, args...)
	return err
}

// DismissRuleNotice stamps dismissed_at on one of the caller's visible notices.
func (b *PostgresStorageBackend) DismissRuleNotice(ctx context.Context, userID, id string) error {
	args := sqlArgs{id}
	vis := ruleNoticeVisible(args.add(userID))
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE rule_notices SET dismissed_at = NOW() WHERE id = $1 AND dismissed_at IS NULL AND `+vis, args...)
	return err
}
