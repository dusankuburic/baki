package database

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"pad-analyzer/internal/storage/interfaces"
)

// Analytics reads for GET /api/dashboard/analytics. The route runs outside the
// per-request RLS transaction, so every statement scopes itself explicitly:
// flow tables by the authorised id set, usage/audit rows by caller id / org ids.

// parseIntMap decodes a jsonb object of counts; a malformed value counts as empty.
func parseIntMap(raw string) map[string]int {
	out := map[string]int{}
	if raw == "" {
		return out
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return map[string]int{}
	}
	return out
}

func (b *PostgresStorageBackend) LoadAnalysisSnapshots(ctx context.Context, flowIDs []string) ([]interfaces.AnalysisSnapshotRow, error) {
	out := []interfaces.AnalysisSnapshotRow{}
	if len(flowIDs) == 0 {
		return out, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT f.id, f.name, f.owner_id, COALESCE(f.org_id, ''), f.updated_at,
		       COALESCE((f.metadata->>'BlockCount')::int, 0),
		       fa.flow_id IS NOT NULL,
		       COALESCE(fa.health_score, 0), COALESCE(fa.errors, 0), COALESCE(fa.warnings, 0), COALESCE(fa.info, 0),
		       COALESCE(fa.auto_fixable_count, 0), COALESCE(fa.total_findings, 0),
		       COALESCE(fa.by_rule, '{}'::jsonb)::text,
		       COALESCE(fa.by_category, '{}'::jsonb)::text,
		       COALESCE(fa.by_confidence, '{}'::jsonb)::text,
		       COALESCE(fa.analyzed_at, TIMESTAMPTZ 'epoch')
		FROM flows f
		LEFT JOIN flow_analysis fa ON fa.flow_id = f.id
		WHERE f.id = ANY($1)`, flowIDs)
	if err != nil {
		return nil, fmt.Errorf("analytics snapshots: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r interfaces.AnalysisSnapshotRow
		var byRule, byCat, byConf string
		if err := rows.Scan(&r.FlowID, &r.Name, &r.OwnerID, &r.OrgID, &r.UpdatedAt, &r.BlockCount, &r.Analyzed,
			&r.HealthScore, &r.Errors, &r.Warnings, &r.Info, &r.AutoFixable, &r.TotalFindings,
			&byRule, &byCat, &byConf, &r.AnalyzedAt); err != nil {
			return nil, fmt.Errorf("scan analytics snapshot: %w", err)
		}
		r.ByRule, r.ByCategory, r.ByConfidence = parseIntMap(byRule), parseIntMap(byCat), parseIntMap(byConf)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate analytics snapshots: %w", err)
	}
	return out, nil
}

// analysisHistoryBaselineSQL is the baseline read of LoadAnalysisHistory: the
// latest run before the window per scoped flow — one keyed LIMIT 1 descent of
// flow_analysis_history_flow_idx per flow, replacing a DISTINCT ON form that
// read and sorted every pre-window row. $1 = scoped flow ids, $2 = window start.
const analysisHistoryBaselineSQL = `
		SELECT h.flow_id, h.health_score, h.errors, h.warnings, h.info,
		       h.auto_fixable_count, h.total_findings, COALESCE(h.by_rule, '{}'::jsonb)::text, h.analyzed_at
		FROM unnest($1::text[]) AS ids(flow_id)
		CROSS JOIN LATERAL (
			SELECT fh.flow_id, fh.health_score, fh.errors, fh.warnings, fh.info,
			       fh.auto_fixable_count, fh.total_findings, fh.by_rule, fh.analyzed_at
			FROM flow_analysis_history fh
			WHERE fh.flow_id = ids.flow_id AND fh.analyzed_at < $2
			ORDER BY fh.analyzed_at DESC
			LIMIT 1) h`

// uniqueTextArray is textArray (a fresh []string for a text[] bind) without
// repeats, in first-occurrence order.
func uniqueTextArray(ids []string) []string {
	seen := make(map[string]struct{}, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, dup := seen[id]; dup {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

// LoadAnalysisHistory returns, per flow in scope, its latest analysis before
// the window (the trend baseline) and its latest analysis per caller-local day
// inside it (days bucketed in w.TZ, so "yesterday" is the reader's).
func (b *PostgresStorageBackend) LoadAnalysisHistory(ctx context.Context, w interfaces.AnalyticsWindow) (*interfaces.AnalysisHistoryRows, error) {
	out := &interfaces.AnalysisHistoryRows{Baseline: []interfaces.AnalysisHistoryRow{}, InWindow: []interfaces.AnalysisHistoryRow{}}
	if len(w.FlowIDs) == 0 {
		return out, nil
	}
	start := w.From
	if !w.PrevFrom.IsZero() {
		start = w.PrevFrom
	}
	tz := w.TZ
	if tz == "" {
		tz = "UTC"
	}

	base, err := b.query(ctx).QueryContext(ctx, analysisHistoryBaselineSQL, uniqueTextArray(w.FlowIDs), start)
	if err != nil {
		return nil, fmt.Errorf("analytics history baseline: %w", err)
	}
	defer base.Close()
	for base.Next() {
		var r interfaces.AnalysisHistoryRow
		var byRule string
		if err := base.Scan(&r.FlowID, &r.HealthScore, &r.Errors, &r.Warnings, &r.Info, &r.AutoFixable, &r.TotalFindings, &byRule, &r.AnalyzedAt); err != nil {
			return nil, fmt.Errorf("scan history baseline: %w", err)
		}
		r.ByRule = parseIntMap(byRule)
		r.Runs = 1
		out.Baseline = append(out.Baseline, r)
	}
	if err := base.Err(); err != nil {
		return nil, fmt.Errorf("iterate history baseline: %w", err)
	}

	// Latest run per flow per local day; the window function counts every run
	// that day before DISTINCT ON keeps the latest.
	in, err := b.query(ctx).QueryContext(ctx, `
		SELECT DISTINCT ON (h.flow_id, d.day)
		       h.flow_id, TO_CHAR(d.day, 'YYYY-MM-DD'), h.health_score, h.errors, h.warnings, h.info,
		       h.auto_fixable_count, h.total_findings, COALESCE(h.by_rule, '{}'::jsonb)::text, h.analyzed_at,
		       COUNT(*) OVER (PARTITION BY h.flow_id, d.day)::int
		FROM flow_analysis_history h
		CROSS JOIN LATERAL (SELECT (h.analyzed_at AT TIME ZONE $4)::date AS day) d
		WHERE h.flow_id = ANY($1) AND h.analyzed_at >= $2 AND h.analyzed_at < $3
		ORDER BY h.flow_id, d.day, h.analyzed_at DESC`, w.FlowIDs, start, w.To, tz)
	if err != nil {
		return nil, fmt.Errorf("analytics history window: %w", err)
	}
	defer in.Close()
	for in.Next() {
		var r interfaces.AnalysisHistoryRow
		var byRule string
		if err := in.Scan(&r.FlowID, &r.Day, &r.HealthScore, &r.Errors, &r.Warnings, &r.Info, &r.AutoFixable, &r.TotalFindings, &byRule, &r.AnalyzedAt, &r.Runs); err != nil {
			return nil, fmt.Errorf("scan history window: %w", err)
		}
		r.ByRule = parseIntMap(byRule)
		out.InWindow = append(out.InWindow, r)
	}
	if err := in.Err(); err != nil {
		return nil, fmt.Errorf("iterate history window: %w", err)
	}
	return out, nil
}

func (b *PostgresStorageBackend) LoadTriageAnalytics(ctx context.Context, w interfaces.AnalyticsWindow) (*interfaces.TriageRows, error) {
	out := &interfaces.TriageRows{ByStatusRule: []interfaces.TriageStatusRule{}, ResolvedByDay: []interfaces.TriageResolvedDay{}}
	if len(w.FlowIDs) == 0 {
		return out, nil
	}
	start := w.From
	if !w.PrevFrom.IsZero() {
		start = w.PrevFrom
	}
	tz := w.TZ
	if tz == "" {
		tz = "UTC"
	}

	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT fs.status, COALESCE(fs.rule_id, ''), COUNT(*)::int
		FROM finding_status fs
		WHERE fs.flow_id = ANY($1)
		GROUP BY 1, 2`, w.FlowIDs)
	if err != nil {
		return nil, fmt.Errorf("analytics triage funnel: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r interfaces.TriageStatusRule
		if err := rows.Scan(&r.Status, &r.RuleID, &r.Count); err != nil {
			return nil, fmt.Errorf("scan triage funnel: %w", err)
		}
		out.ByStatusRule = append(out.ByStatusRule, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate triage funnel: %w", err)
	}

	res, err := b.query(ctx).QueryContext(ctx, `
		SELECT TO_CHAR((fs.updated_at AT TIME ZONE $4)::date, 'YYYY-MM-DD'), COALESCE(fs.rule_id, ''), COUNT(*)::int
		FROM finding_status fs
		WHERE fs.flow_id = ANY($1) AND fs.status = 'resolved'
		  AND fs.updated_at >= $2 AND fs.updated_at < $3
		GROUP BY 1, 2
		ORDER BY 1`, w.FlowIDs, start, w.To, tz)
	if err != nil {
		return nil, fmt.Errorf("analytics triage resolved: %w", err)
	}
	defer res.Close()
	for res.Next() {
		var r interfaces.TriageResolvedDay
		if err := res.Scan(&r.Date, &r.RuleID, &r.Count); err != nil {
			return nil, fmt.Errorf("scan triage resolved: %w", err)
		}
		out.ResolvedByDay = append(out.ResolvedByDay, r)
	}
	if err := res.Err(); err != nil {
		return nil, fmt.Errorf("iterate triage resolved: %w", err)
	}

	// Same lifecycle predicate as the home dashboard: MTTR over genuinely
	// resolved rows, stale = open/acknowledged untouched for two weeks.
	if err := b.query(ctx).QueryRowContext(ctx, `
		SELECT COALESCE(AVG(EXTRACT(EPOCH FROM (fs.updated_at - fs.created_at)) / 3600)
		           FILTER (WHERE fs.status = 'resolved' AND fs.updated_at >= fs.created_at), 0)::float,
		       COUNT(*) FILTER (WHERE fs.status = 'resolved' AND fs.updated_at >= fs.created_at)::int,
		       COUNT(*) FILTER (WHERE fs.status IN ('open', 'acknowledged') AND fs.updated_at < NOW() - INTERVAL '14 days')::int
		FROM finding_status fs
		WHERE fs.flow_id = ANY($1)`, w.FlowIDs,
	).Scan(&out.MttrHours, &out.ResolvedCount, &out.StaleCount); err != nil {
		return nil, fmt.Errorf("analytics triage mttr: %w", err)
	}
	return out, nil
}

// usageScope builds the WHERE fragment for usage_metrics: "user" = the
// caller's own calls, "org" = the scoped orgs', "both" = either. Only the
// parameters the fragment references are bound on args (Postgres rejects unused ones).
func usageScope(mode, userID string, orgIDs []string, args *sqlArgs) string {
	switch mode {
	case "org":
		if len(orgIDs) > 0 {
			return "u.org_id = ANY(" + args.add(orgIDs) + ")"
		}
	case "both":
		if len(orgIDs) > 0 {
			user := args.add(userID)
			return "(u.user_id = " + user + " OR u.org_id = ANY(" + args.add(orgIDs) + "))"
		}
	}
	return "u.user_id = " + args.add(userID)
}

func (b *PostgresStorageBackend) LoadUsageAnalytics(ctx context.Context, userID string, orgIDs []string, mode string, w interfaces.AnalyticsWindow) (*interfaces.UsageRows, error) {
	out := &interfaces.UsageRows{Daily: []interfaces.UsageDailyRow{}, ByProvider: []interfaces.ProviderCost{}, ByModel: []interfaces.UsageModelRow{}}
	start := w.From
	if !w.PrevFrom.IsZero() {
		start = w.PrevFrom
	}
	tz := w.TZ
	if tz == "" {
		tz = "UTC"
	}
	var scope sqlArgs
	pred := usageScope(mode, userID, orgIDs, &scope)
	args := append(sqlArgs{}, scope...)
	from, to, zone := args.add(start), args.add(w.To), args.add(tz)
	daily, err := b.query(ctx).QueryContext(ctx, fmt.Sprintf(`
		SELECT TO_CHAR((u.created_at AT TIME ZONE %s)::date, 'YYYY-MM-DD'),
		       COALESCE(SUM(u.prompt_tokens), 0)::int, COALESCE(SUM(u.completion_tokens), 0)::int,
		       COUNT(*)::int, COALESCE(SUM(u.estimated_cost), 0)::float
		FROM usage_metrics u
		WHERE %s AND u.created_at >= %s AND u.created_at < %s
		GROUP BY 1
		ORDER BY 1`, zone, pred, from, to), args...)
	if err != nil {
		return nil, fmt.Errorf("analytics usage daily: %w", err)
	}
	defer daily.Close()
	for daily.Next() {
		var r interfaces.UsageDailyRow
		if err := daily.Scan(&r.Date, &r.TokensIn, &r.TokensOut, &r.Calls, &r.Cost); err != nil {
			return nil, fmt.Errorf("scan usage daily: %w", err)
		}
		out.Daily = append(out.Daily, r)
	}
	if err := daily.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage daily: %w", err)
	}

	// Provider and model breakdowns cover the current window only.
	args = append(sqlArgs{}, scope...)
	from, to = args.add(w.From), args.add(w.To)
	prov, err := b.query(ctx).QueryContext(ctx, fmt.Sprintf(`
		SELECT u.provider, COALESCE(SUM(u.estimated_cost), 0)::float,
		       COALESCE(SUM(u.prompt_tokens), 0)::int, COALESCE(SUM(u.completion_tokens), 0)::int
		FROM usage_metrics u
		WHERE %s AND u.created_at >= %s AND u.created_at < %s
		GROUP BY u.provider
		ORDER BY 2 DESC`, pred, from, to), args...)
	if err != nil {
		return nil, fmt.Errorf("analytics usage providers: %w", err)
	}
	defer prov.Close()
	for prov.Next() {
		var r interfaces.ProviderCost
		if err := prov.Scan(&r.Provider, &r.Cost, &r.TokensIn, &r.TokensOut); err != nil {
			return nil, fmt.Errorf("scan usage providers: %w", err)
		}
		out.ByProvider = append(out.ByProvider, r)
	}
	if err := prov.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage providers: %w", err)
	}

	// Grouped by the model that ANSWERED (served_model, falling back to the
	// requested model): an alias serving two models over the window is two rows.
	mod, err := b.query(ctx).QueryContext(ctx, fmt.Sprintf(`
		SELECT u.provider, COALESCE(u.model, ''), COALESCE(NULLIF(u.served_model, ''), u.model, ''),
		       COALESCE(SUM(u.prompt_tokens), 0)::int,
		       COALESCE(SUM(u.completion_tokens), 0)::int, COUNT(*)::int, COALESCE(SUM(u.estimated_cost), 0)::float
		FROM usage_metrics u
		WHERE %s AND u.created_at >= %s AND u.created_at < %s
		GROUP BY 1, 2, 3
		ORDER BY 7 DESC
		LIMIT 10`, pred, from, to), args...)
	if err != nil {
		return nil, fmt.Errorf("analytics usage models: %w", err)
	}
	defer mod.Close()
	for mod.Next() {
		var r interfaces.UsageModelRow
		if err := mod.Scan(&r.Provider, &r.Model, &r.ServedModel, &r.TokensIn, &r.TokensOut, &r.Calls, &r.Cost); err != nil {
			return nil, fmt.Errorf("scan usage models: %w", err)
		}
		if r.ServedModel == r.Model {
			r.ServedModel = ""
		}
		out.ByModel = append(out.ByModel, r)
	}
	if err := mod.Err(); err != nil {
		return nil, fmt.Errorf("iterate usage models: %w", err)
	}
	return out, nil
}

// LoadActivityAnalytics lists the caller's own events plus every event on a
// flow in scope, with the actor resolved to a display name and flow resources
// to their name.
func (b *PostgresStorageBackend) LoadActivityAnalytics(ctx context.Context, userID string, w interfaces.AnalyticsWindow, limit int) (*interfaces.ActivityRows, error) {
	out := &interfaces.ActivityRows{Events: []interfaces.ActivityEventRow{}, ByDay: []interfaces.DateCountRow{}, ByAction: []interfaces.ActionCountRow{}}
	if limit <= 0 {
		limit = 50
	}
	tz := w.TZ
	if tz == "" {
		tz = "UTC"
	}
	flowIDs := w.FlowIDs
	if flowIDs == nil {
		flowIDs = []string{}
	}
	const where = `a.created_at >= $2 AND a.created_at < $3
		  AND (a.user_id = $1 OR (a.resource_type = 'flow' AND a.resource_id = ANY($4)))`

	ev, err := b.query(ctx).QueryContext(ctx, `
		SELECT a.action, COALESCE(a.user_id, ''),
		       COALESCE(NULLIF(u.display_name, ''), NULLIF(split_part(COALESCE(u.email, ''), '@', 1), ''),
		                NULLIF(split_part(COALESCE(a.email, ''), '@', 1), ''), ''),
		       COALESCE(a.resource_type, ''), COALESCE(a.resource_id, ''),
		       CASE WHEN a.resource_type = 'flow' THEN COALESCE(f.name, a.meta->>'name', '') ELSE '' END,
		       a.created_at
		FROM audit_events a
		LEFT JOIN users u ON u.id = a.user_id
		LEFT JOIN flows f ON a.resource_type = 'flow' AND f.id = a.resource_id
		WHERE `+where+`
		ORDER BY a.created_at DESC
		LIMIT $5`, userID, w.From, w.To, flowIDs, limit)
	if err != nil {
		return nil, fmt.Errorf("analytics activity: %w", err)
	}
	defer ev.Close()
	for ev.Next() {
		var r interfaces.ActivityEventRow
		if err := ev.Scan(&r.Action, &r.UserID, &r.ActorName, &r.ResourceType, &r.ResourceID, &r.Label, &r.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan activity: %w", err)
		}
		out.Events = append(out.Events, r)
	}
	if err := ev.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity: %w", err)
	}

	days, err := b.query(ctx).QueryContext(ctx, `
		SELECT TO_CHAR((a.created_at AT TIME ZONE $5)::date, 'YYYY-MM-DD'), COUNT(*)::int
		FROM audit_events a
		WHERE `+where+`
		GROUP BY 1
		ORDER BY 1`, userID, w.From, w.To, flowIDs, tz)
	if err != nil {
		return nil, fmt.Errorf("analytics activity by day: %w", err)
	}
	defer days.Close()
	for days.Next() {
		var r interfaces.DateCountRow
		if err := days.Scan(&r.Date, &r.Count); err != nil {
			return nil, fmt.Errorf("scan activity by day: %w", err)
		}
		out.ByDay = append(out.ByDay, r)
	}
	if err := days.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity by day: %w", err)
	}

	acts, err := b.query(ctx).QueryContext(ctx, `
		SELECT a.action, COUNT(*)::int
		FROM audit_events a
		WHERE `+where+`
		GROUP BY a.action
		ORDER BY 2 DESC, 1
		LIMIT 20`, userID, w.From, w.To, flowIDs)
	if err != nil {
		return nil, fmt.Errorf("analytics activity by action: %w", err)
	}
	defer acts.Close()
	for acts.Next() {
		var r interfaces.ActionCountRow
		if err := acts.Scan(&r.Action, &r.Count); err != nil {
			return nil, fmt.Errorf("scan activity by action: %w", err)
		}
		out.ByAction = append(out.ByAction, r)
	}
	if err := acts.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity by action: %w", err)
	}
	return out, nil
}

// LoadSecurityAnalytics counts the caller's failed logins in the last day —
// auth events are logged before there are claims, so the user is in
// resource_id or the typed email — and, when asked, instance-wide lockouts.
func (b *PostgresStorageBackend) LoadSecurityAnalytics(ctx context.Context, userID, email string, includeLocked bool) (*interfaces.SecurityRows, error) {
	out := &interfaces.SecurityRows{}
	if err := b.query(ctx).QueryRowContext(ctx, `
		SELECT COUNT(*)::int FROM audit_events
		WHERE action = 'auth.login_failure' AND created_at >= NOW() - INTERVAL '24 hours'
		  AND (user_id = $1 OR (resource_type = 'user' AND resource_id = $1)
		       OR (resource_type = 'email' AND $2 <> '' AND lower(resource_id) = lower($2)))`,
		userID, strings.TrimSpace(email),
	).Scan(&out.FailedLogins24h); err != nil {
		return nil, fmt.Errorf("analytics failed logins: %w", err)
	}
	if includeLocked {
		if err := b.query(ctx).QueryRowContext(ctx, `
			SELECT COUNT(*)::int FROM users
			WHERE locked_until IS NOT NULL AND locked_until > NOW()`,
		).Scan(&out.LockedAccounts); err != nil {
			return nil, fmt.Errorf("analytics locked accounts: %w", err)
		}
		out.LockedRead = true
	}
	return out, nil
}
