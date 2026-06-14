package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"pad-analyzer/internal/storage/interfaces"
)

// SaveFlowAnalysis upserts a flow's latest analysis summary. Called best-effort
// after every analyze so the welcome dashboard renders persisted health/findings
// on first load. Uses the RLS-scoped executor so the write participates in the
// request transaction when one is active.
func (b *PostgresStorageBackend) SaveFlowAnalysis(ctx context.Context, fa *interfaces.FlowAnalysis) error {
	cat := fa.ByCategory
	if cat == nil {
		cat = map[string]int{}
	}
	catJSON, err := json.Marshal(cat)
	if err != nil {
		return fmt.Errorf("marshal by_category: %w", err)
	}
	analyzedAt := fa.AnalyzedAt
	_, err = b.query(ctx).ExecContext(ctx, `
		INSERT INTO flow_analysis (flow_id, health_score, errors, warnings, info, by_category, analyzed_at)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb, COALESCE($7, NOW()))
		ON CONFLICT (flow_id) DO UPDATE SET
			health_score = EXCLUDED.health_score,
			errors       = EXCLUDED.errors,
			warnings     = EXCLUDED.warnings,
			info         = EXCLUDED.info,
			by_category  = EXCLUDED.by_category,
			analyzed_at  = EXCLUDED.analyzed_at`,
		fa.FlowID, fa.HealthScore, fa.Errors, fa.Warnings, fa.Info, string(catJSON), nullableTime(analyzedAt),
	)
	if err != nil {
		return fmt.Errorf("upsert flow_analysis: %w", err)
	}
	return nil
}

// FlowDashboardData assembles the owner-scoped welcome-dashboard payload. The
// five reads are independent and indexed; running them sequentially keeps the
// code simple and is well within budget for a landing screen.
func (b *PostgresStorageBackend) FlowDashboardData(ctx context.Context, ownerID string, days int) (*interfaces.DashboardData, error) {
	if days <= 0 {
		days = 14
	}
	out := &interfaces.DashboardData{ByCategory: map[string]int{}}

	// A. Overview counts over all of the owner's flows. SubflowCount lives in the
	// metadata jsonb under its Go field name (FlowMetadata has no json tags).
	if err := b.query(ctx).QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM((metadata->>'SubflowCount')::int), 0)
		FROM flows WHERE owner_id = $1`, ownerID,
	).Scan(&out.TotalFlows, &out.TotalSubflows); err != nil {
		return nil, fmt.Errorf("dashboard overview counts: %w", err)
	}

	// B. Health/findings scalar aggregate over the owner's analyzed flows.
	if err := b.query(ctx).QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(ROUND(AVG(fa.health_score))::int, 0),
		       COALESCE(SUM(fa.errors), 0),
		       COALESCE(SUM(fa.warnings), 0),
		       COALESCE(SUM(fa.info), 0)
		FROM flow_analysis fa
		JOIN flows f ON f.id = fa.flow_id
		WHERE f.owner_id = $1`, ownerID,
	).Scan(&out.HealthCount, &out.AvgHealth, &out.Errors, &out.Warnings, &out.Info); err != nil {
		return nil, fmt.Errorf("dashboard health aggregate: %w", err)
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

	// D. Recent flows with their latest health score (LEFT JOIN ⇒ nil when never analyzed).
	recRows, err := b.query(ctx).QueryContext(ctx, `
		SELECT f.id, f.name, f.updated_at, fa.health_score
		FROM flows f
		LEFT JOIN flow_analysis fa ON fa.flow_id = f.id
		WHERE f.owner_id = $1
		ORDER BY f.updated_at DESC
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

// nullableTime converts a zero time.Time to a NULL arg so the upsert's
// COALESCE($7, NOW()) default applies when the caller didn't set AnalyzedAt.
func nullableTime(t interface{ IsZero() bool }) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return t
}
