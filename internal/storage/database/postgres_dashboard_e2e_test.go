package database_test

import (
	"context"
	"fmt"
	"math"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// seedDashFlow saves a flow for owner and registers cleanup. finding_status and
// flow_analysis_history rows cascade on flow delete.
func seedDashFlow(t *testing.T, ctx context.Context, b interface {
	SaveFlow(context.Context, *interfaces.FlowDocument) error
	DeleteFlow(context.Context, string) error
}, id, owner string) {
	t.Helper()
	flow := &interfaces.FlowDocument{
		ID:      id,
		Name:    "Dash V3 Flow",
		OwnerID: owner,
		Content: []byte(`{"subflows":[]}`),
		Metadata: interfaces.FlowMetadata{
			BlockCount: 3, SubflowCount: 1, ParsedAt: time.Now().UTC(),
		},
	}
	if err := b.SaveFlow(ctx, flow); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	t.Cleanup(func() { _ = b.DeleteFlow(ctx, id) })
}

// TestPostgres_Workflow_MTTROnlyResolved is the regression test for the MTTR
// query averaging over every finding_status row: MTTR must cover only resolved
// findings with a sane lifecycle (updated_at >= created_at), matching the
// ResolvedCount computed alongside it. Open/aged rows count as stale, and
// pre-migration backfill artifacts (created_at > updated_at) count as neither.
func TestPostgres_Workflow_MTTROnlyResolved(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	owner := "dash-v3-mttr-owner"
	flowID := "dash-v3-mttr-flow"
	seedDashFlow(t, ctx, b, flowID, owner)

	seed := func(key, status string, createdAgo, updatedAgo time.Duration) {
		t.Helper()
		_, err := b.DB().ExecContext(ctx, `
			INSERT INTO finding_status (flow_id, finding_key, status, created_at, updated_at)
			VALUES ($1, $2, $3, NOW() - $4::interval, NOW() - $5::interval)`,
			flowID, key, status,
			fmt.Sprintf("%f seconds", createdAgo.Seconds()),
			fmt.Sprintf("%f seconds", updatedAgo.Seconds()))
		if err != nil {
			t.Fatalf("seed finding_status %s: %v", key, err)
		}
	}
	// Genuine 2h lifecycle → the only row MTTR may see.
	seed("k-resolved", "resolved", 3*time.Hour, 1*time.Hour)
	// Open for 21 days, untouched for 20 → stale, not resolved.
	seed("k-stale", "open", 21*24*time.Hour, 20*24*time.Hour)
	// Backfill artifact: created_at (NOW at migration time) after updated_at.
	seed("k-backfill", "resolved", 0, 5*time.Hour)

	out, err := b.FlowDashboardAdvanced(ctx, owner, 30)
	if err != nil {
		t.Fatalf("FlowDashboardAdvanced: %v", err)
	}
	if out.Workflow.ResolvedCount != 1 {
		t.Errorf("ResolvedCount: want 1, got %d", out.Workflow.ResolvedCount)
	}
	if out.Workflow.StaleCount != 1 {
		t.Errorf("StaleCount: want 1, got %d", out.Workflow.StaleCount)
	}
	if math.Abs(out.Workflow.MttrHours-2) > 0.1 {
		t.Errorf("MttrHours: want ~2 (only the resolved 2h lifecycle), got %v", out.Workflow.MttrHours)
	}
}

// TestPostgres_SeverityTrend_DedupesSameDayReanalyses verifies the severity
// trend counts each flow once per day (its latest analysis), not once per
// append-only history row — re-analyzing a flow must not inflate the chart.
func TestPostgres_SeverityTrend_DedupesSameDayReanalyses(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	owner := "dash-v3-trend-owner"
	flowID := "dash-v3-trend-flow"
	seedDashFlow(t, ctx, b, flowID, owner)

	// Seed into a fixed hour of YESTERDAY rather than NOW() minus an offset.
	//
	// The previous version seeded at NOW()-2h and NOW()-1h and asserted on the
	// LAST trend bucket ("today"). Both assumptions break in the two hours
	// after midnight in the DB's timezone: at 00:07 UTC, NOW()-2h and NOW()-1h
	// are both YESTERDAY, so today's bucket is legitimately 0 and the test
	// fails. CI runs in UTC, which made this a ~8% chance of a spurious
	// failure on any push landing between 00:00 and 02:00 UTC. Worse, between
	// 01:00 and 02:00 the two seeds straddle midnight and land on DIFFERENT
	// days — at which point the test would pass while no longer testing
	// same-day deduplication at all.
	//
	// Yesterday is always exactly one calendar day and always fully in the
	// past, so anchoring there is correct at every wall-clock time.
	seedHist := func(errors int, hourOfDay int) {
		t.Helper()
		_, err := b.DB().ExecContext(ctx, `
			INSERT INTO flow_analysis_history (flow_id, errors, warnings, info, analyzed_at)
			VALUES ($1, $2, 1, 0,
			        date_trunc('day', NOW()) - INTERVAL '1 day' + ($3::int * INTERVAL '1 hour'))`,
			flowID, errors, hourOfDay)
		if err != nil {
			t.Fatalf("seed history: %v", err)
		}
	}
	// Same flow analyzed twice on the same day: 5 errors, then 3. Only the
	// latest (11:00) may count.
	seedHist(5, 10)
	seedHist(3, 11)

	// Resolve the expected bucket from the database rather than assuming a
	// position in the slice.
	var wantDay string
	if err := b.DB().QueryRowContext(ctx,
		`SELECT TO_CHAR(date_trunc('day', NOW()) - INTERVAL '1 day', 'YYYY-MM-DD')`).Scan(&wantDay); err != nil {
		t.Fatalf("resolve expected day: %v", err)
	}

	out, err := b.FlowDashboardAdvanced(ctx, owner, 30)
	if err != nil {
		t.Fatalf("FlowDashboardAdvanced: %v", err)
	}
	if len(out.SeverityTrend) == 0 {
		t.Fatal("SeverityTrend is empty")
	}

	var day *interfaces.DailySeverityPoint
	for i := range out.SeverityTrend {
		if out.SeverityTrend[i].Date == wantDay {
			day = &out.SeverityTrend[i]
			break
		}
	}
	if day == nil {
		t.Fatalf("no severity-trend bucket for %s (got %d buckets: %s..%s)",
			wantDay, len(out.SeverityTrend),
			out.SeverityTrend[0].Date, out.SeverityTrend[len(out.SeverityTrend)-1].Date)
	}
	if day.Errors != 3 {
		t.Errorf("%s errors: want 3 (latest analysis only), got %d", wantDay, day.Errors)
	}
	if day.Warnings != 1 {
		t.Errorf("%s warnings: want 1, got %d", wantDay, day.Warnings)
	}
}

// TestPostgres_Security_LockedAccounts verifies the security posture counts
// currently locked accounts (locked_until in the future) and ignores unlocked
// ones. Delta-based: the shared test DB may hold other locked users.
func TestPostgres_Security_LockedAccounts(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	owner := "dash-v3-locked-owner"

	before, err := b.FlowDashboardAdvanced(ctx, owner, 30)
	if err != nil {
		t.Fatalf("FlowDashboardAdvanced (before): %v", err)
	}

	seedUser := func(id string, lockedUntil any) {
		t.Helper()
		_, err := b.DB().ExecContext(ctx, `
			INSERT INTO users (id, email, password, role, locked_until)
			VALUES ($1, $1 || '@dash-v3.test', 'x', 'user', $2)`, id, lockedUntil)
		if err != nil {
			t.Fatalf("seed user %s: %v", id, err)
		}
		t.Cleanup(func() { _, _ = b.DB().ExecContext(ctx, `DELETE FROM users WHERE id = $1`, id) })
	}
	seedUser("dash-v3-locked-user", time.Now().Add(1*time.Hour))
	seedUser("dash-v3-unlocked-user", nil)

	after, err := b.FlowDashboardAdvanced(ctx, owner, 30)
	if err != nil {
		t.Fatalf("FlowDashboardAdvanced (after): %v", err)
	}
	if got := after.Security.LockedAccounts - before.Security.LockedAccounts; got != 1 {
		t.Errorf("LockedAccounts delta: want 1 (locked yes, unlocked no), got %d", got)
	}
}
