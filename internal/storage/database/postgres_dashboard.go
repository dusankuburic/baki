package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"pad-analyzer/internal/storage/interfaces"

	"pad-core/logger"
)

// SaveFlowAnalysis upserts a flow's latest analysis summary (best-effort,
// after every analyze) and appends the trend-history row. Uses the RLS-scoped
// executor so the write participates in the request transaction when active.
func (b *PostgresStorageBackend) SaveFlowAnalysis(ctx context.Context, fa *interfaces.FlowAnalysis) error {
	cat := fa.ByCategory
	if cat == nil {
		cat = map[string]int{}
	}
	catJSON, err := json.Marshal(cat)
	if err != nil {
		return fmt.Errorf("marshal by_category: %w", err)
	}
	rule := fa.ByRule
	if rule == nil {
		rule = map[string]int{}
	}
	ruleJSON, err := json.Marshal(rule)
	if err != nil {
		return fmt.Errorf("marshal by_rule: %w", err)
	}
	conf := fa.ByConfidence
	if conf == nil {
		conf = map[string]int{}
	}
	confJSON, err := json.Marshal(conf)
	if err != nil {
		return fmt.Errorf("marshal by_confidence: %w", err)
	}
	analyzedAt := fa.AnalyzedAt
	_, err = b.query(ctx).ExecContext(ctx, `
		INSERT INTO flow_analysis (flow_id, health_score, errors, warnings, info, by_category, by_rule, by_confidence, auto_fixable_count, total_findings, analyzed_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7::jsonb, $8::jsonb, $9, $10, COALESCE($11, NOW()))
		ON CONFLICT (flow_id) DO UPDATE SET
			health_score       = EXCLUDED.health_score,
			errors             = EXCLUDED.errors,
			warnings           = EXCLUDED.warnings,
			info               = EXCLUDED.info,
			by_category        = EXCLUDED.by_category,
			by_rule            = EXCLUDED.by_rule,
			by_confidence      = EXCLUDED.by_confidence,
			auto_fixable_count = EXCLUDED.auto_fixable_count,
			total_findings     = EXCLUDED.total_findings,
			analyzed_at        = EXCLUDED.analyzed_at`,
		fa.FlowID, fa.HealthScore, fa.Errors, fa.Warnings, fa.Info, string(catJSON), string(ruleJSON), string(confJSON), fa.AutoFixableCount, fa.TotalFindings, nullableTime(analyzedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert flow_analysis: %w", err)
	}

	// Append to the history table for trend charts, on the pool (autocommit):
	// on the tx executor a failure would poison the request transaction. The
	// history table's RLS policy allows this internal write via NOT app_rls_active().
	if _, hErr := b.db.ExecContext(ctx, `
		INSERT INTO flow_analysis_history (flow_id, health_score, errors, warnings, info, by_rule, by_confidence, auto_fixable_count, total_findings, analyzed_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, $7, $8, $9, COALESCE($10, NOW()))`,
		fa.FlowID, fa.HealthScore, fa.Errors, fa.Warnings, fa.Info, string(ruleJSON), string(confJSON), fa.AutoFixableCount, fa.TotalFindings, nullableTime(analyzedAt),
	); hErr != nil {
		slog.Warn("flow_analysis_history insert failed (best-effort)", "flowId", fa.FlowID, "err", hErr)
	}

	return nil
}

// LoadFlowHealthBatch returns each listed flow's latest persisted analysis
// snapshot; flows never analyzed are absent from the map.
func (b *PostgresStorageBackend) LoadFlowHealthBatch(ctx context.Context, flowIDs []string) (map[string]*interfaces.HealthSnapshot, error) {
	out := make(map[string]*interfaces.HealthSnapshot, len(flowIDs))
	if len(flowIDs) == 0 {
		return out, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT flow_id, health_score, errors, warnings, info, analyzed_at
		FROM flow_analysis WHERE flow_id = ANY($1)`, flowIDs)
	if err != nil {
		return nil, fmt.Errorf("load flow health batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var h interfaces.HealthSnapshot
		if err := rows.Scan(&id, &h.HealthScore, &h.Errors, &h.Warnings, &h.Info, &h.AnalyzedAt); err != nil {
			return nil, fmt.Errorf("scan flow health batch: %w", err)
		}
		hh := h
		out[id] = &hh
	}
	return out, rows.Err()
}

func (b *PostgresStorageBackend) LoadFlowHealth(ctx context.Context, flowID string) (*interfaces.HealthSnapshot, error) {
	row := b.query(ctx).QueryRowContext(ctx, `
		SELECT health_score, errors, warnings, info, analyzed_at
		FROM flow_analysis WHERE flow_id = $1`, flowID)
	var h interfaces.HealthSnapshot
	if err := row.Scan(&h.HealthScore, &h.Errors, &h.Warnings, &h.Info, &h.AnalyzedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, fmt.Errorf("load flow health: %w", err)
	}
	return &h, nil
}

// FlowDashboardData assembles the owner-scoped welcome-dashboard payload from
// independent indexed reads, run sequentially for simplicity.
func (b *PostgresStorageBackend) FlowDashboardData(ctx context.Context, ownerID string, days int) (*interfaces.DashboardData, error) {
	if days <= 0 {
		days = 14
	}
	out := &interfaces.DashboardData{ByCategory: map[string]int{}, Confidence: map[string]int{}}

	// A. Overview counts over all of the owner's flows; SubflowCount lives in
	// the metadata jsonb under its Go field name (FlowMetadata has no json tags).
	if err := b.query(ctx).QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM((metadata->>'SubflowCount')::int), 0)
		FROM flows WHERE owner_id = $1`, ownerID,
	).Scan(&out.TotalFlows, &out.TotalSubflows); err != nil {
		return nil, fmt.Errorf("dashboard overview counts: %w", err)
	}

	// B. Health/findings aggregate: the scalar sums back the fix-availability
	// KPI, the five FILTER counts the health-score histogram.
	var b0, b20, b40, b60, b80 int
	if err := b.query(ctx).QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(ROUND(AVG(fa.health_score))::int, 0),
		       COALESCE(SUM(fa.errors), 0),
		       COALESCE(SUM(fa.warnings), 0),
		       COALESCE(SUM(fa.info), 0),
		       COALESCE(SUM(fa.total_findings), 0),
		       COALESCE(SUM(fa.auto_fixable_count), 0),
		       COUNT(*) FILTER (WHERE fa.health_score < 20),
		       COUNT(*) FILTER (WHERE fa.health_score >= 20 AND fa.health_score < 40),
		       COUNT(*) FILTER (WHERE fa.health_score >= 40 AND fa.health_score < 60),
		       COUNT(*) FILTER (WHERE fa.health_score >= 60 AND fa.health_score < 80),
		       COUNT(*) FILTER (WHERE fa.health_score >= 80)
		FROM flow_analysis fa
		JOIN flows f ON f.id = fa.flow_id
		WHERE f.owner_id = $1`, ownerID,
	).Scan(&out.HealthCount, &out.AvgHealth, &out.Errors, &out.Warnings, &out.Info, &out.TotalFindings, &out.AutoFixable,
		&b0, &b20, &b40, &b60, &b80); err != nil {
		return nil, fmt.Errorf("dashboard health aggregate: %w", err)
	}
	out.HealthBuckets = []interfaces.HealthBucket{
		{Label: "0-20", Lo: 0, Hi: 20, Count: b0},
		{Label: "20-40", Lo: 20, Hi: 40, Count: b20},
		{Label: "40-60", Lo: 40, Hi: 60, Count: b40},
		{Label: "60-80", Lo: 60, Hi: 80, Count: b60},
		{Label: "80-100", Lo: 80, Hi: 100, Count: b80},
	}

	// C. Findings by category (jsonb fan-out).
	catRows, err := b.query(ctx).QueryContext(ctx, `
		SELECT kv.key, SUM(kv.value::int)::int
		FROM flow_analysis fa
		JOIN flows f ON f.id = fa.flow_id,
		     LATERAL jsonb_each_text(fa.by_category) AS kv(key, value)
		WHERE f.owner_id = $1
		GROUP BY kv.key`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("dashboard category aggregate: %w", err)
	}
	defer catRows.Close()
	for catRows.Next() {
		var cat string
		var n int
		if err := catRows.Scan(&cat, &n); err != nil {
			return nil, fmt.Errorf("scan category: %w", err)
		}
		out.ByCategory[cat] = n
	}
	if err := catRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate categories: %w", err)
	}

	// C2. Confidence fan-out (mirrors the category fan-out): rolls each flow's
	// by_confidence jsonb up into an org-wide high/medium/low tally.
	confRows, err := b.query(ctx).QueryContext(ctx, `
		SELECT kv.key, SUM(kv.value::int)::int
		FROM flow_analysis fa
		JOIN flows f ON f.id = fa.flow_id,
		     LATERAL jsonb_each_text(fa.by_confidence) AS kv(key, value)
		WHERE f.owner_id = $1
		GROUP BY kv.key`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("dashboard confidence aggregate: %w", err)
	}
	defer confRows.Close()
	for confRows.Next() {
		var tier string
		var n int
		if err := confRows.Scan(&tier, &n); err != nil {
			return nil, fmt.Errorf("scan confidence: %w", err)
		}
		out.Confidence[tier] = n
	}
	if err := confRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate confidence: %w", err)
	}

	// D. Recent flows with latest health (LEFT JOIN ⇒ nil when never
	// analyzed); DISTINCT ON name so one re-uploaded flow cannot fill three
	// of the five slots.
	recRows, err := b.query(ctx).QueryContext(ctx, `
		SELECT r.id, r.name, r.updated_at, r.health_score
		FROM (
		    SELECT DISTINCT ON (lower(f.name)) f.id, f.name, f.updated_at, fa.health_score
		    FROM flows f
		    LEFT JOIN flow_analysis fa ON fa.flow_id = f.id
		    WHERE f.owner_id = $1
		    ORDER BY lower(f.name), f.updated_at DESC
		) r
		ORDER BY r.updated_at DESC
		LIMIT 5`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("dashboard recent flows: %w", err)
	}
	defer recRows.Close()
	for recRows.Next() {
		var rf interfaces.RecentFlowHealth
		var health sql.NullInt64
		if err := recRows.Scan(&rf.FlowID, &rf.Name, &rf.UpdatedAt, &health); err != nil {
			return nil, fmt.Errorf("scan recent flow: %w", err)
		}
		if health.Valid {
			h := int(health.Int64)
			rf.HealthScore = &h
		}
		out.Recent = append(out.Recent, rf)
	}
	if err := recRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate recent flows: %w", err)
	}

	// E. Gap-filled token usage: LEFT JOIN a generated day series so days with no
	// usage render as 0 instead of being absent (which makes a line chart lie).
	tokRows, err := b.query(ctx).QueryContext(ctx, `
		SELECT TO_CHAR(d.day, 'YYYY-MM-DD'),
		       COALESCE(SUM(u.prompt_tokens), 0)::int,
		       COALESCE(SUM(u.completion_tokens), 0)::int
		FROM generate_series(
		        date_trunc('day', NOW()) - (($2::int - 1) * INTERVAL '1 day'),
		        date_trunc('day', NOW()),
		        INTERVAL '1 day'
		     ) AS d(day)
		LEFT JOIN usage_metrics u
		       ON u.user_id = $1
		      AND u.created_at >= d.day
		      AND u.created_at <  d.day + INTERVAL '1 day'
		GROUP BY d.day
		ORDER BY d.day ASC`, ownerID, days)
	if err != nil {
		return nil, fmt.Errorf("dashboard token usage: %w", err)
	}
	defer tokRows.Close()
	for tokRows.Next() {
		var dt interfaces.DailyTokens
		if err := tokRows.Scan(&dt.Date, &dt.TokensIn, &dt.TokensOut); err != nil {
			return nil, fmt.Errorf("scan token usage: %w", err)
		}
		out.TokenUsage = append(out.TokenUsage, dt)
	}
	if err := tokRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate token usage: %w", err)
	}

	return out, nil
}

// FlowDashboardAdvanced returns the extended dashboard data — trends, cost by
// provider, rule frequency, activity, complexity, security posture — all owner-scoped.
func (b *PostgresStorageBackend) FlowDashboardAdvanced(ctx context.Context, ownerID string, days int) (*interfaces.DashboardAdvancedData, error) {
	if days <= 0 {
		days = 30
	}
	out := &interfaces.DashboardAdvancedData{
		Security: interfaces.DashboardSecurity{},
	}

	// A. Health score trend: daily avg from flow_analysis_history. The lateral
	// joins flows BY OWNER first so the planner walks flows(owner_id) →
	// history(flow_id, analyzed_at) instead of scanning every tenant's rows.
	trendRows, err := b.query(ctx).QueryContext(ctx, `
		SELECT TO_CHAR(d.day, 'YYYY-MM-DD'),
		       COALESCE(ROUND(AVG(h.health_score))::int, 0),
		       COUNT(h.id)::int
		FROM generate_series(
		         date_trunc('day', NOW()) - (($2::int - 1) * INTERVAL '1 day'),
		         date_trunc('day', NOW()),
		         INTERVAL '1 day'
		     ) AS d(day)
		LEFT JOIN LATERAL (
		    SELECT DISTINCT ON (h.flow_id) h.health_score, h.id
		    FROM flow_analysis_history h
		    JOIN flows f ON f.id = h.flow_id AND f.owner_id = $1
		    WHERE h.analyzed_at >= d.day
		      AND h.analyzed_at <  d.day + INTERVAL '1 day'
		    ORDER BY h.flow_id, h.analyzed_at DESC
		) h ON TRUE
		GROUP BY d.day
		ORDER BY d.day ASC`, ownerID, days)
	if err != nil {
		return nil, fmt.Errorf("dashboard health trend: %w", err)
	}
	defer trendRows.Close()
	for trendRows.Next() {
		var p interfaces.DailyHealthPoint
		if err := trendRows.Scan(&p.Date, &p.AvgHealth, &p.FlowCount); err != nil {
			return nil, fmt.Errorf("scan health trend: %w", err)
		}
		out.HealthTrend = append(out.HealthTrend, p)
	}
	if err := trendRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate health trend: %w", err)
	}

	// A2. Org-wide severity trend, gap-filled like A: the lateral takes only
	// the latest history row per flow per day — history is append-only, so a
	// flow re-analyzed N times in a day would otherwise count N×.
	sevRows, err := b.query(ctx).QueryContext(ctx, `
		SELECT TO_CHAR(d.day, 'YYYY-MM-DD'),
		       COALESCE(SUM(h.errors), 0)::int,
		       COALESCE(SUM(h.warnings), 0)::int,
		       COALESCE(SUM(h.info), 0)::int
		FROM generate_series(
		         date_trunc('day', NOW()) - (($2::int - 1) * INTERVAL '1 day'),
		         date_trunc('day', NOW()),
		         INTERVAL '1 day'
		     ) AS d(day)
		LEFT JOIN LATERAL (
		    SELECT DISTINCT ON (h.flow_id) h.errors, h.warnings, h.info
		    FROM flow_analysis_history h
		    JOIN flows f ON f.id = h.flow_id AND f.owner_id = $1
		    WHERE h.analyzed_at >= d.day
		      AND h.analyzed_at <  d.day + INTERVAL '1 day'
		    ORDER BY h.flow_id, h.analyzed_at DESC
		) h ON TRUE
		GROUP BY d.day
		ORDER BY d.day ASC`, ownerID, days)
	if err != nil {
		return nil, fmt.Errorf("dashboard severity trend: %w", err)
	}
	defer sevRows.Close()
	for sevRows.Next() {
		var p interfaces.DailySeverityPoint
		if err := sevRows.Scan(&p.Date, &p.Errors, &p.Warnings, &p.Info); err != nil {
			return nil, fmt.Errorf("scan severity trend: %w", err)
		}
		out.SeverityTrend = append(out.SeverityTrend, p)
	}
	if err := sevRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate severity trend: %w", err)
	}

	// B. Cost breakdown by provider.
	costRows, err := b.query(ctx).QueryContext(ctx, `
		SELECT provider,
		       COALESCE(SUM(estimated_cost), 0)::float,
		       COALESCE(SUM(prompt_tokens), 0)::int,
		       COALESCE(SUM(completion_tokens), 0)::int
		FROM usage_metrics
		WHERE user_id = $1 AND created_at >= NOW() - ($2::int * INTERVAL '1 day')
		GROUP BY provider
		ORDER BY SUM(estimated_cost) DESC`, ownerID, days)
	if err != nil {
		return nil, fmt.Errorf("dashboard cost by provider: %w", err)
	}
	defer costRows.Close()
	for costRows.Next() {
		var pc interfaces.ProviderCost
		if err := costRows.Scan(&pc.Provider, &pc.Cost, &pc.TokensIn, &pc.TokensOut); err != nil {
			return nil, fmt.Errorf("scan cost: %w", err)
		}
		out.CostByProv = append(out.CostByProv, pc)
	}
	if err := costRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate cost by provider: %w", err)
	}

	// C. Rule frequency distribution (jsonb fan-out on flow_analysis.by_rule).
	ruleRows, err := b.query(ctx).QueryContext(ctx, `
		SELECT kv.key, SUM(kv.value::int)::int
		FROM flow_analysis fa
		JOIN flows f ON f.id = fa.flow_id,
		     LATERAL jsonb_each_text(fa.by_rule) AS kv(key, value)
		WHERE f.owner_id = $1
		GROUP BY kv.key
		ORDER BY SUM(kv.value::int) DESC
		LIMIT 15`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("dashboard rule frequency: %w", err)
	}
	defer ruleRows.Close()
	for ruleRows.Next() {
		var rf interfaces.RuleFrequency
		if err := ruleRows.Scan(&rf.Rule, &rf.Count); err != nil {
			return nil, fmt.Errorf("scan rule frequency: %w", err)
		}
		out.RuleFreq = append(out.RuleFreq, rf)
	}
	if err := ruleRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate rule frequency: %w", err)
	}

	// D. Activity feed from audit_events: only a flow resource carries a name
	// worth showing; anything else renders empty, not the raw resource id.
	actRows, err := b.query(ctx).QueryContext(ctx, `
		SELECT action,
		       CASE WHEN resource_type = 'flow'
		            THEN COALESCE((SELECT name FROM flows WHERE id = resource_id LIMIT 1), meta->>'name', '')
		            ELSE '' END,
		       created_at
		FROM audit_events
		WHERE user_id = $1
		ORDER BY created_at DESC
		LIMIT 20`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("dashboard activity: %w", err)
	}
	defer actRows.Close()
	for actRows.Next() {
		var ae interfaces.ActivityEntry
		if err := actRows.Scan(&ae.Action, &ae.FlowName, &ae.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan activity: %w", err)
		}
		out.Activity = append(out.Activity, ae)
	}
	if err := actRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate activity: %w", err)
	}

	// E. Flow complexity scatter: blocks vs findings, colored by health.
	compRows, err := b.query(ctx).QueryContext(ctx, `
		SELECT f.id, f.name,
		       COALESCE((f.metadata->>'BlockCount')::int, 0),
		       COALESCE(fa.errors + fa.warnings + fa.info, 0),
		       COALESCE(fa.health_score, 0)
		FROM flows f
		LEFT JOIN flow_analysis fa ON fa.flow_id = f.id
		WHERE f.owner_id = $1
		  AND f.metadata->>'BlockCount' IS NOT NULL
		ORDER BY f.updated_at DESC
		LIMIT 50`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("dashboard complexity: %w", err)
	}
	defer compRows.Close()
	for compRows.Next() {
		var cp interfaces.FlowComplexityPoint
		if err := compRows.Scan(&cp.FlowID, &cp.FlowName, &cp.BlockCount, &cp.FindingCount, &cp.HealthScore); err != nil {
			return nil, fmt.Errorf("scan complexity: %w", err)
		}
		out.Complexity = append(out.Complexity, cp)
	}
	if err := compRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate complexity: %w", err)
	}

	// F. Security posture: each count is best-effort, but a failure must be
	// reported — three zeros used to render as "no security issues detected".
	securityOK := true
	// Auth events are logged before there are claims: the user is in
	// resource_id, and user_id is empty — the old user_id predicate never hit.
	if err := b.query(ctx).QueryRowContext(ctx, `
		SELECT COUNT(*)::int FROM audit_events
		WHERE action = 'auth.login_failure'
		  AND (user_id = $1 OR (resource_type = 'user' AND resource_id = $1))
		  AND created_at >= NOW() - INTERVAL '24 hours'`, ownerID,
	).Scan(&out.Security.FailedLogins24h); err != nil {
		logger.Warn("dashboard: failed-login count unavailable", "error", err)
		securityOK = false
	}

	// Credential-related findings across all analyzed flows
	if err := b.query(ctx).QueryRowContext(ctx, `
		SELECT COALESCE(SUM(rv.value::int), 0)::int
		FROM flow_analysis fa
		JOIN flows f ON f.id = fa.flow_id,
		     LATERAL jsonb_each_text(fa.by_rule) AS rv(key, value)
		WHERE f.owner_id = $1
		  AND (rv.key LIKE '%credential%' OR rv.key LIKE '%sensitive%' OR rv.key LIKE '%injection%')`, ownerID,
	).Scan(&out.Security.CredentialFindings); err != nil {
		logger.Warn("dashboard: credential-finding count unavailable", "error", err)
		securityOK = false
	}

	// Currently locked accounts. Instance-wide: the users table has no org/owner
	// scoping, and lockouts are a fleet-security signal rather than a per-owner one.
	if err := b.query(ctx).QueryRowContext(ctx, `
		SELECT COUNT(*)::int FROM users
		WHERE locked_until IS NOT NULL AND locked_until > NOW()`,
	).Scan(&out.Security.LockedAccounts); err != nil {
		logger.Warn("dashboard: locked-account count unavailable", "error", err)
		securityOK = false
	}
	out.Security.Available = securityOK

	// G. Team-triage workflow (funnel, MTTR, stale count), owner-scoped via
	// the flow join; MTTR excludes updated_at < created_at backfill artifacts.
	out.Workflow.Funnel = map[string]int{}
	funRows, err := b.query(ctx).QueryContext(ctx, `
		SELECT fs.status, COUNT(*)::int
		FROM finding_status fs
		JOIN flows f ON f.id = fs.flow_id
		WHERE f.owner_id = $1
		GROUP BY fs.status`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("dashboard workflow funnel: %w", err)
	}
	defer funRows.Close()
	for funRows.Next() {
		var status string
		var n int
		if err := funRows.Scan(&status, &n); err != nil {
			return nil, fmt.Errorf("scan workflow funnel: %w", err)
		}
		out.Workflow.Funnel[status] = n
	}
	if err := funRows.Err(); err != nil {
		return nil, fmt.Errorf("iterate workflow funnel: %w", err)
	}
	_ = b.query(ctx).QueryRowContext(ctx, `
		SELECT COALESCE(AVG(EXTRACT(EPOCH FROM (fs.updated_at - fs.created_at)) / 3600)
		           FILTER (WHERE fs.status = 'resolved' AND fs.updated_at >= fs.created_at), 0)::float,
		       COUNT(*) FILTER (WHERE fs.status = 'resolved' AND fs.updated_at >= fs.created_at)::int,
		       COUNT(*) FILTER (WHERE fs.status IN ('open', 'acknowledged') AND fs.updated_at < NOW() - INTERVAL '14 days')::int
		FROM finding_status fs
		JOIN flows f ON f.id = fs.flow_id
		WHERE f.owner_id = $1`, ownerID,
	).Scan(&out.Workflow.MttrHours, &out.Workflow.ResolvedCount, &out.Workflow.StaleCount)

	return out, nil
}

// nullableTime converts a zero time.Time to a NULL arg so the upsert's
// COALESCE($7, NOW()) default applies when the caller didn't set AnalyzedAt.
func nullableTime(t interface{ IsZero() bool }) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t
}
