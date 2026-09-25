package database_test

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

func seedNamedFlow(t *testing.T, ctx context.Context, b interface {
	SaveFlow(context.Context, *interfaces.FlowDocument) error
	DeleteFlow(context.Context, string) error
}, id, owner, name string) {
	t.Helper()
	flow := &interfaces.FlowDocument{
		ID: id, Name: name, OwnerID: owner, Content: []byte(`{"subflows":[]}`),
		Metadata: interfaces.FlowMetadata{BlockCount: 3, SubflowCount: 1, ParsedAt: time.Now().UTC()},
	}
	if err := b.SaveFlow(ctx, flow); err != nil {
		t.Fatalf("SaveFlow %s: %v", id, err)
	}
	t.Cleanup(func() { _ = b.DeleteFlow(ctx, id) })
}

// TestPostgres_Analytics_HistoryBaselineAndPerDay: the trend read returns the
// latest run before the window (the carry-forward seed) and, inside it, one
// row per flow per day — the day's latest run — with the day's run count.
func TestPostgres_Analytics_HistoryBaselineAndPerDay(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	flowID := "ana-hist-flow"
	seedDashFlow(t, ctx, b, flowID, "ana-hist-owner")

	seed := func(health int, at time.Time) {
		t.Helper()
		if _, err := b.DB().ExecContext(ctx, `
			INSERT INTO flow_analysis_history (flow_id, health_score, errors, by_rule, total_findings, analyzed_at)
			VALUES ($1, $2, 1, '{"r1": 1}', 1, $3)`, flowID, health, at); err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}
	day := func(y, m, d int) time.Time { return time.Date(y, time.Month(m), d, 0, 0, 0, 0, time.UTC) }
	seed(50, day(2026, 9, 7).Add(9*time.Hour))
	seed(60, day(2026, 9, 9).Add(10*time.Hour))
	seed(70, day(2026, 9, 9).Add(15*time.Hour))

	w := interfaces.AnalyticsWindow{FlowIDs: []string{flowID}, From: day(2026, 9, 8), To: day(2026, 9, 11), TZ: "UTC"}
	rows, err := b.LoadAnalysisHistory(ctx, w)
	if err != nil {
		t.Fatalf("LoadAnalysisHistory: %v", err)
	}
	if len(rows.Baseline) != 1 || rows.Baseline[0].HealthScore != 50 {
		t.Errorf("baseline = %+v, want the Sep 7 run (50)", rows.Baseline)
	}
	if len(rows.InWindow) != 1 {
		t.Fatalf("in-window rows = %+v, want one (Sep 9 collapsed)", rows.InWindow)
	}
	got := rows.InWindow[0]
	if got.Day != "2026-09-09" || got.HealthScore != 70 || got.Runs != 2 || got.ByRule["r1"] != 1 || got.TotalFindings != 1 {
		t.Errorf("Sep 9 = %+v, want the 15:00 run (70) with 2 runs", got)
	}

	// With a comparison period the baseline moves before it and Sep 7 joins
	// the window.
	w.PrevFrom, w.PrevTo = day(2026, 9, 5), day(2026, 9, 8)
	rows, err = b.LoadAnalysisHistory(ctx, w)
	if err != nil {
		t.Fatalf("LoadAnalysisHistory (compare): %v", err)
	}
	if len(rows.Baseline) != 0 || len(rows.InWindow) != 2 || rows.InWindow[0].Day != "2026-09-07" {
		t.Errorf("compare window: baseline %d, rows %+v", len(rows.Baseline), rows.InWindow)
	}
}

// TestPostgres_Analytics_DaysFollowTheReadersZone: 23:30 UTC is already the next
// day in Tokyo; the day bucket must follow the requested zone.
func TestPostgres_Analytics_DaysFollowTheReadersZone(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	flowID := "ana-tz-flow"
	seedDashFlow(t, ctx, b, flowID, "ana-tz-owner")
	if _, err := b.DB().ExecContext(ctx, `
		INSERT INTO flow_analysis_history (flow_id, health_score, analyzed_at)
		VALUES ($1, 80, $2)`, flowID, time.Date(2026, 9, 20, 23, 30, 0, 0, time.UTC)); err != nil {
		t.Fatalf("seed history: %v", err)
	}
	w := interfaces.AnalyticsWindow{FlowIDs: []string{flowID}, From: time.Date(2026, 9, 15, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 9, 25, 0, 0, 0, 0, time.UTC)}
	for tz, want := range map[string]string{"Asia/Tokyo": "2026-09-21", "UTC": "2026-09-20", "America/Los_Angeles": "2026-09-20"} {
		w.TZ = tz
		rows, err := b.LoadAnalysisHistory(ctx, w)
		if err != nil {
			t.Fatalf("LoadAnalysisHistory(%s): %v", tz, err)
		}
		if len(rows.InWindow) != 1 || rows.InWindow[0].Day != want {
			t.Errorf("%s: rows = %+v, want day %s", tz, rows.InWindow, want)
		}
	}
}

// TestPostgres_Analytics_ActivityActorAndFailedLogins: activity resolves the
// actor to a display name and flow resources to their name; failed logins are
// found where the auth handlers log them (the user in resource_id, or the
// typed email) — never by user_id, which is empty before there are claims.
func TestPostgres_Analytics_ActivityActorAndFailedLogins(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	userID, email := "ana-actor-1", "ana.actor@example.com"
	flowID := "ana-act-flow"
	seedDashFlow(t, ctx, b, flowID, userID)

	cleanup := func() {
		_, _ = b.DB().ExecContext(ctx, `DELETE FROM audit_events WHERE id LIKE 'ana-ev-%'`)
		_, _ = b.DB().ExecContext(ctx, `DELETE FROM users WHERE id = $1`, userID)
	}
	cleanup()
	t.Cleanup(cleanup)
	if _, err := b.DB().ExecContext(ctx, `
		INSERT INTO users (id, email, password, role, display_name) VALUES ($1, $2, 'x', 'member', 'Ada L')`, userID, email); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	now := time.Now()
	ev := func(id, uid, action, rtype, rid string, at time.Time) {
		t.Helper()
		if _, err := b.DB().ExecContext(ctx, `
			INSERT INTO audit_events (id, user_id, email, action, resource_type, resource_id, created_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7)`, id, uid, email, action, rtype, rid, at); err != nil {
			t.Fatalf("seed audit event %s: %v", id, err)
		}
	}
	ev("ana-ev-1", userID, "flow.upload", "flow", flowID, now.Add(-time.Hour))
	ev("ana-ev-2", "", "auth.login_failure", "user", userID, now.Add(-2*time.Hour))
	ev("ana-ev-3", "", "auth.login_failure", "email", email, now.Add(-3*time.Hour))
	ev("ana-ev-4", "", "auth.login_failure", "user", userID, now.Add(-30*time.Hour))

	w := interfaces.AnalyticsWindow{FlowIDs: []string{flowID}, From: now.AddDate(0, 0, -7), To: now.AddDate(0, 0, 1), TZ: "UTC"}
	act, err := b.LoadActivityAnalytics(ctx, userID, w, 50)
	if err != nil {
		t.Fatalf("LoadActivityAnalytics: %v", err)
	}
	var upload *interfaces.ActivityEventRow
	for i := range act.Events {
		if act.Events[i].Action == "flow.upload" {
			upload = &act.Events[i]
		}
	}
	if upload == nil || upload.ActorName != "Ada L" || upload.Label != "Dash V3 Flow" || upload.UserID != userID {
		t.Errorf("upload event = %+v, want actor Ada L on Dash V3 Flow", upload)
	}
	if len(act.ByDay) == 0 || len(act.ByAction) == 0 {
		t.Errorf("activity series empty: %+v / %+v", act.ByDay, act.ByAction)
	}

	sec, err := b.LoadSecurityAnalytics(ctx, userID, email, false)
	if err != nil {
		t.Fatalf("LoadSecurityAnalytics: %v", err)
	}
	if sec.FailedLogins24h != 2 || sec.LockedRead {
		t.Errorf("security = %+v, want 2 failed logins in 24h and no locked-account read", sec)
	}
	if adm, err := b.LoadSecurityAnalytics(ctx, userID, email, true); err != nil || !adm.LockedRead {
		t.Errorf("admin read = %+v (err %v), want the locked-account count read", adm, err)
	}
}

// TestPostgres_Analytics_SnapshotsAndTriage covers the flow-scoped reads: a
// never-analyzed flow reads as Analyzed=false, triage rows group by status and
// rule, and resolved-per-day honours the window.
func TestPostgres_Analytics_SnapshotsAndTriage(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	owner := "ana-snap-owner"
	seedNamedFlow(t, ctx, b, "ana-snap-a", owner, "Analyzed")
	seedNamedFlow(t, ctx, b, "ana-snap-b", owner, "Never analyzed")
	if err := b.SaveFlowAnalysis(ctx, &interfaces.FlowAnalysis{
		FlowID: "ana-snap-a", HealthScore: 64, Errors: 1, Warnings: 2, ByCategory: map[string]int{"Security": 3},
		ByRule: map[string]int{"hardcoded-credential": 1, "x": 2}, ByConfidence: map[string]int{"high": 3}, AutoFixableCount: 2, AnalyzedAt: time.Now(),
	}); err != nil {
		t.Fatalf("SaveFlowAnalysis: %v", err)
	}
	snaps, err := b.LoadAnalysisSnapshots(ctx, []string{"ana-snap-a", "ana-snap-b", "ana-snap-missing"})
	if err != nil {
		t.Fatalf("LoadAnalysisSnapshots: %v", err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %+v, want the two existing flows", snaps)
	}
	byID := map[string]interfaces.AnalysisSnapshotRow{}
	for _, s := range snaps {
		byID[s.FlowID] = s
	}
	a, bb := byID["ana-snap-a"], byID["ana-snap-b"]
	if !a.Analyzed || a.HealthScore != 64 || a.ByRule["x"] != 2 || a.ByConfidence["high"] != 3 || a.AutoFixable != 2 || a.BlockCount != 3 || a.OwnerID != owner {
		t.Errorf("analyzed snapshot = %+v", a)
	}
	if bb.Analyzed || bb.Name != "Never analyzed" {
		t.Errorf("unanalyzed snapshot = %+v", bb)
	}

	now := time.Now()
	seedStatus := func(key, rule, status string, updatedAgo time.Duration) {
		t.Helper()
		if _, err := b.DB().ExecContext(ctx, `
			INSERT INTO finding_status (flow_id, finding_key, rule_id, status, created_at, updated_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, "ana-snap-a", key, rule, status, now.Add(-updatedAgo-time.Hour), now.Add(-updatedAgo)); err != nil {
			t.Fatalf("seed finding_status: %v", err)
		}
	}
	seedStatus("k1", "x", "open", time.Hour)
	seedStatus("k2", "x", "resolved", 2*time.Hour)
	seedStatus("k3", "hardcoded-credential", "resolved", 40*24*time.Hour)
	w := interfaces.AnalyticsWindow{FlowIDs: []string{"ana-snap-a"}, From: now.AddDate(0, 0, -6), To: now.AddDate(0, 0, 1), TZ: "UTC"}
	tr, err := b.LoadTriageAnalytics(ctx, w)
	if err != nil {
		t.Fatalf("LoadTriageAnalytics: %v", err)
	}
	counts := map[string]int{}
	for _, r := range tr.ByStatusRule {
		counts[r.Status+"/"+r.RuleID] = r.Count
	}
	if counts["open/x"] != 1 || counts["resolved/x"] != 1 || counts["resolved/hardcoded-credential"] != 1 {
		t.Errorf("status×rule = %v", counts)
	}
	if len(tr.ResolvedByDay) != 1 || tr.ResolvedByDay[0].RuleID != "x" || tr.ResolvedByDay[0].Count != 1 {
		t.Errorf("resolved per day (7d window) = %+v, want only k2", tr.ResolvedByDay)
	}
	if tr.ResolvedCount != 2 {
		t.Errorf("ResolvedCount = %d, want 2 (all time)", tr.ResolvedCount)
	}
}

// TestPostgres_Home_RecentFlowsDedupedAndLabels covers the home-payload query
// fixes: one recent-flow row per name, no raw ids for non-flow activity, and
// failed logins counted where auth logs them.
func TestPostgres_Home_RecentFlowsDedupedAndLabels(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	owner := "ana-home-owner"
	seedNamedFlow(t, ctx, b, "ana-home-1", owner, "Invoice Flow")
	seedNamedFlow(t, ctx, b, "ana-home-2", owner, "invoice flow")
	seedNamedFlow(t, ctx, b, "ana-home-3", owner, "Other")

	data, err := b.FlowDashboardData(ctx, owner, 30)
	if err != nil {
		t.Fatalf("FlowDashboardData: %v", err)
	}
	if len(data.Recent) != 2 {
		t.Errorf("recent flows = %+v, want one per name", data.Recent)
	}
	for _, r := range data.Recent {
		if r.Name == "Invoice Flow" {
			t.Errorf("the older duplicate must lose to the newer upload: %+v", data.Recent)
		}
	}

	cleanup := func() { _, _ = b.DB().ExecContext(ctx, `DELETE FROM audit_events WHERE id LIKE 'ana-home-ev-%'`) }
	cleanup()
	t.Cleanup(cleanup)
	now := time.Now()
	if _, err := b.DB().ExecContext(ctx, `
		INSERT INTO audit_events (id, user_id, action, resource_type, resource_id, created_at) VALUES
		('ana-home-ev-1', $1, 'settings.update', 'settings', 'setting-row-id', $2),
		('ana-home-ev-2', $1, 'flow.upload', 'flow', 'ana-home-3', $2),
		('ana-home-ev-3', '', 'auth.login_failure', 'user', $1, $2)`, owner, now.Add(-time.Hour)); err != nil {
		t.Fatalf("seed audit events: %v", err)
	}
	adv, err := b.FlowDashboardAdvanced(ctx, owner, 30)
	if err != nil {
		t.Fatalf("FlowDashboardAdvanced: %v", err)
	}
	labels := map[string]string{}
	for _, e := range adv.Activity {
		labels[e.Action] = e.FlowName
	}
	if labels["settings.update"] != "" || labels["flow.upload"] != "Other" {
		t.Errorf("activity labels = %v, want no label for settings and the flow name for uploads", labels)
	}
	if adv.Security.FailedLogins24h != 1 {
		t.Errorf("FailedLogins24h = %d, want 1 (logged with the user in resource_id)", adv.Security.FailedLogins24h)
	}
}
