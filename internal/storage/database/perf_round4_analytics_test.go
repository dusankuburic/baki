package database

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// Tests for the fourth performance round's analytics baseline
// (analysisHistoryBaselineSQL): one keyed, ordered, LIMIT 1 probe of
// flow_analysis_history_flow_idx per scoped flow instead of a DISTINCT ON
// over every pre-window row. DATABASE_URL-gated (throwaway DB).

const (
	r4HistFlows = 5  // flows in the fixture
	r4HistRuns  = 30 // analyses per flow, one every two days, newest 2 days ago
)

// seedAnalysisHistory creates a user, r4HistFlows flows they own and
// r4HistRuns analyses per flow spread over 60 days (health 50+r, r findings
// for the run r*2 days ago), ANALYZEd; removed at cleanup (history rows
// cascade with the flows). It returns the user, the flow ids and the
// reference time the runs are spaced from.
func seedAnalysisHistory(t *testing.T, b *PostgresStorageBackend) (user string, flowIDs []string, now time.Time) {
	t.Helper()
	ctx := context.Background()
	stamp := time.Now().Format("150405.000000000")
	user = "r4-hist-user-" + stamp
	for i := 0; i < r4HistFlows; i++ {
		flowIDs = append(flowIDs, fmt.Sprintf("r4-hist-%s-%d", stamp, i))
	}
	now = time.Now().UTC().Truncate(time.Second)
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(ctx, `DELETE FROM flows WHERE id = ANY($1)`, flowIDs)
		_, _ = b.db.ExecContext(ctx, `DELETE FROM users WHERE id = $1`, user)
	})
	if err := b.CreateUser(ctx, &interfaces.User{
		ID: user, Email: user + "@test.local", Password: "$2a$12$testhash", Role: "member",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	for _, s := range []struct {
		sql  string
		args []any
	}{
		{`INSERT INTO flows (id, name, content, owner_id) SELECT f, 'history ' || f, '{}'::jsonb, $2 FROM unnest($1::text[]) f`, []any{flowIDs, user}},
		{`INSERT INTO flow_analysis_history (flow_id, health_score, errors, warnings, info, auto_fixable_count, total_findings, by_rule, analyzed_at)
		  SELECT f, 50 + r, r, 0, 0, 0, r, jsonb_build_object('r', r), $2::timestamptz - (r * 2 || ' days')::interval
		  FROM unnest($1::text[]) f, generate_series(1, $3::int) r`, []any{flowIDs, now, r4HistRuns}},
		{`ANALYZE flows, flow_analysis_history`, nil},
	} {
		if _, err := b.db.ExecContext(ctx, s.sql, s.args...); err != nil {
			t.Fatalf("seed analysis history: %v\n%s", err, s.sql)
		}
	}
	return user, flowIDs, now
}

// TestPlan_AnalysisHistoryBaseline_OneProbePerFlow: as the owner under RLS,
// the baseline read reaches flow_analysis_history by
// flow_analysis_history_flow_idx with BOTH the flow and the window start in
// the index condition, directly under a Limit (the first entry below the
// start, per flow) — and nothing in the plan sorts or deduplicates history
// rows.
func TestPlan_AnalysisHistoryBaseline_OneProbePerFlow(t *testing.T) {
	b := openMigrationTestDB(t)
	user, flowIDs, now := seedAnalysisHistory(t, b)
	nodes := explainAsUser(t, b, user, analysisHistoryBaselineSQL, []any{flowIDs, now.Add(-20 * 24 * time.Hour)})
	t.Logf("analysis history baseline plan:\n%s", describePlan(nodes))
	scan, ok := nodeOn(nodes, "flow_analysis_history")
	if !ok {
		t.Fatalf("no flow_analysis_history scan:\n%s", describePlan(nodes))
	}
	if scan.IndexName != "flow_analysis_history_flow_idx" {
		t.Errorf("flow_analysis_history is reached by %s using %q, want flow_analysis_history_flow_idx:\n%s", scan.NodeType, scan.IndexName, describePlan(nodes))
	}
	if !strings.Contains(scan.IndexCond, "flow_id") || !strings.Contains(scan.IndexCond, "analyzed_at") {
		t.Errorf("index condition %q must key on flow_id AND analyzed_at:\n%s", scan.IndexCond, describePlan(nodes))
	}
	isHistory := func(n planNode) bool { return n.RelationName == "flow_analysis_history" }
	if p := parentOf(nodes[0], isHistory); p == nil || p.NodeType != "Limit" {
		got := "<none>"
		if p != nil {
			got = p.NodeType
		}
		t.Errorf("the flow_analysis_history scan's parent is %s, want Limit (LIMIT 1 straight off the index):\n%s", got, describePlan(nodes))
	}
	for _, n := range nodes {
		if n.NodeType == "Sort" || n.NodeType == "Incremental Sort" || n.NodeType == "Unique" {
			t.Errorf("the baseline plan still has a %s node:\n%s", n.NodeType, describePlan(nodes))
		}
	}
}

// oldAnalysisHistoryBaselineSQL is the statement the baseline read sent
// before the round (the oracle): DISTINCT ON over every pre-window row.
const oldAnalysisHistoryBaselineSQL = `
		SELECT DISTINCT ON (h.flow_id)
		       h.flow_id, h.health_score, h.errors, h.warnings, h.info,
		       h.auto_fixable_count, h.total_findings, COALESCE(h.by_rule, '{}'::jsonb)::text, h.analyzed_at
		FROM flow_analysis_history h
		WHERE h.flow_id = ANY($1) AND h.analyzed_at < $2
		ORDER BY h.flow_id, h.analyzed_at DESC`

// r4BaselineKey is the comparable projection of a baseline row.
type r4BaselineKey struct {
	health, errs, total int
	byRule              string
	at                  time.Time
}

// TestAnalysisHistoryBaseline_MatchesDistinctOn: LoadAnalysisHistory's
// baseline rows are exactly the DISTINCT ON statement's — one per flow
// with pre-window history (none for a flow without), the latest run before
// the window, same columns — with a repeated id in the scope yielding one
// row, as DISTINCT ON did.
func TestAnalysisHistoryBaseline_MatchesDistinctOn(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	_, flowIDs, now := seedAnalysisHistory(t, b)
	// Thin one flow out so its latest pre-window run differs from the rest,
	// and leave one with no run before the window at all.
	if _, err := b.db.ExecContext(ctx, `DELETE FROM flow_analysis_history WHERE flow_id = $1 AND analyzed_at > $2`, flowIDs[1], now.Add(-30*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(ctx, `DELETE FROM flow_analysis_history WHERE flow_id = $1 AND analyzed_at < $2`, flowIDs[2], now.Add(-5*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	scope := append(append([]string{}, flowIDs...), flowIDs[0], "r4-hist-no-such-flow")
	w := interfaces.AnalyticsWindow{FlowIDs: scope, From: now.Add(-10 * 24 * time.Hour), To: now, TZ: "UTC"}

	got, err := b.LoadAnalysisHistory(ctx, w)
	if err != nil {
		t.Fatalf("LoadAnalysisHistory: %v", err)
	}
	have := map[string]r4BaselineKey{}
	for _, r := range got.Baseline {
		if _, dup := have[r.FlowID]; dup {
			t.Errorf("flow %s has two baseline rows", r.FlowID)
		}
		have[r.FlowID] = r4BaselineKey{r.HealthScore, r.Errors, r.TotalFindings, fmt.Sprint(r.ByRule), r.AnalyzedAt.UTC()}
	}

	rows, err := b.db.QueryContext(ctx, oldAnalysisHistoryBaselineSQL, scope, w.From)
	if err != nil {
		t.Fatalf("oracle: %v", err)
	}
	defer rows.Close()
	want := map[string]r4BaselineKey{}
	for rows.Next() {
		var r interfaces.AnalysisHistoryRow
		var byRule string
		if err := rows.Scan(&r.FlowID, &r.HealthScore, &r.Errors, &r.Warnings, &r.Info, &r.AutoFixable, &r.TotalFindings, &byRule, &r.AnalyzedAt); err != nil {
			t.Fatal(err)
		}
		want[r.FlowID] = r4BaselineKey{r.HealthScore, r.Errors, r.TotalFindings, fmt.Sprint(parseIntMap(byRule)), r.AnalyzedAt.UTC()}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(want) != r4HistFlows-1 {
		t.Fatalf("oracle returned %d flows, want %d (fixture wrong)", len(want), r4HistFlows-1)
	}
	if len(have) != len(want) {
		t.Errorf("baseline has %d flows, the DISTINCT ON oracle %d", len(have), len(want))
	}
	for id, w := range want {
		if h, ok := have[id]; !ok || h != w {
			t.Errorf("flow %s: baseline %+v, oracle %+v", id, h, w)
		}
	}
	for id := range have {
		if _, ok := want[id]; !ok {
			t.Errorf("flow %s has a baseline row the oracle does not", id)
		}
	}
	// The thinned flow's baseline is its newest surviving run, 30 days back
	// (health 65); the cleared flow has none.
	if r := have[flowIDs[1]]; r.health != 65 {
		t.Errorf("thinned flow baseline health = %d, want 65 (the run 30 days back)", r.health)
	}
	if _, ok := have[flowIDs[2]]; ok {
		t.Errorf("a flow with no run before the window has a baseline row")
	}
}
