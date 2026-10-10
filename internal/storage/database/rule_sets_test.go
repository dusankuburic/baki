package database

import (
	"context"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// Rule sets on baselines and health snapshots and the "new rules" notices (migration 0037). DATABASE_URL-gated
// (throwaway DB).

func testRuleSet(run, name string, ids ...string) *models.RuleSet {
	rs := &models.RuleSet{Rules: []models.RuleRef{}, Digest: "rs1:test-" + name + "-" + run}
	for _, id := range ids {
		rs.Rules = append(rs.Rules, models.RuleRef{ID: id})
	}
	return rs
}

// The statements the release before 0037 runs (postgres_triage.go SetFlowBaseline, postgres_dashboard.go
// SaveFlowAnalysis at 6b87f4e), verbatim: explicit column lists that know nothing of the rule-set columns.
const (
	previousReleaseSetBaseline = `
		INSERT INTO flow_baselines (flow_id, keys, created_by, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (flow_id) DO UPDATE SET
			keys = EXCLUDED.keys,
			created_by = EXCLUDED.created_by,
			created_at = NOW()`
	previousReleaseSaveFlowAnalysis = `
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
			analyzed_at        = EXCLUDED.analyzed_at`
)

// TestPostgres_RuleSets_PreviousReleaseWritesReadAsLegacy: after 0037, a baseline or snapshot the previous release
// inserts has no rule set, and one it rewrites loses the rule set it carried (its rule_set_at no longer equals
// created_at / analyzed_at): every read, the batch reads and RuleSetsByDigest treat both as legacy records.
func TestPostgres_RuleSets_PreviousReleaseWritesReadAsLegacy(t *testing.T) {
	f := newR2Fixture(t)
	b, ctx := f.b, f.ctx
	if v, _ := b.CurrentSchemaVersion(ctx); v < 37 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	owner := f.user("rs-owner")
	rewritten, inserted := f.flow("rs-rewritten", owner, "", "Rewritten", ""), f.flow("rs-inserted", owner, "", "Inserted", "")
	rs := testRuleSet(f.run, "current", "rule-a", "rule-b")

	if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: rewritten, Keys: []string{"rule-a:x"}, CreatedBy: owner, RuleSet: rs}); err != nil {
		t.Fatal(err)
	}
	// No AnalyzedAt: rule_set_at must take analyzed_at's NOW() too.
	if err := b.SaveFlowAnalysis(ctx, &interfaces.FlowAnalysis{FlowID: rewritten, HealthScore: 90, RuleSet: rs}); err != nil {
		t.Fatal(err)
	}
	bl, err := b.GetFlowBaseline(ctx, rewritten)
	if err != nil || bl == nil || bl.RuleSet == nil || bl.RuleSetDigest != rs.Digest || !slices.Equal(bl.RuleSet.Rules, rs.Rules) {
		t.Fatalf("baseline before the rewrite = %+v, %v; want the rule set", bl, err)
	}
	if h, err := b.LoadFlowHealth(ctx, rewritten); err != nil || h == nil || h.RuleSetDigest != rs.Digest {
		t.Fatalf("snapshot before the rewrite = %+v, %v; want the digest", h, err)
	}
	if got, err := b.RuleSetsByDigest(ctx, []string{rs.Digest}); err != nil || got[rs.Digest] == nil {
		t.Fatalf("RuleSetsByDigest before the rewrite = %v, %v", got, err)
	}

	// The previous release rewrites the rewritten flow's baseline and snapshot and inserts the other's, each in its
	// own transaction, as its requests do.
	for _, id := range []string{rewritten, inserted} {
		if _, err := b.db.ExecContext(ctx, previousReleaseSetBaseline, id, []byte(`["rule-a:x","rule-b:y"]`), owner); err != nil {
			t.Fatalf("previous release's SetFlowBaseline %s: %v", id, err)
		}
		if _, err := b.db.ExecContext(ctx, previousReleaseSaveFlowAnalysis, id, 70, 1, 0, 0, `{}`, `{}`, `{}`, 0, 1,
			time.Now().UTC()); err != nil {
			t.Fatalf("previous release's SaveFlowAnalysis %s: %v", id, err)
		}
	}
	for _, id := range []string{rewritten, inserted} {
		bl, err := b.GetFlowBaseline(ctx, id)
		if err != nil || bl == nil || len(bl.Keys) != 2 || bl.RuleSet != nil || bl.RuleSetDigest != "" {
			t.Errorf("%s: baseline after the previous release wrote it = %+v, %v; want its keys and no rule set", id, bl, err)
		}
		h, err := b.LoadFlowHealth(ctx, id)
		if err != nil || h == nil || h.HealthScore != 70 || h.RuleSetDigest != "" {
			t.Errorf("%s: snapshot after the previous release wrote it = %+v, %v; want no digest", id, h, err)
		}
	}
	batch, err := b.GetFlowBaselinesBatch(ctx, []string{rewritten, inserted})
	if err != nil || len(batch) != 2 || batch[rewritten].RuleSetDigest != "" || batch[inserted].RuleSetDigest != "" {
		t.Errorf("GetFlowBaselinesBatch = %+v, %v; want no digests", batch, err)
	}
	health, err := b.LoadFlowHealthBatch(ctx, []string{rewritten, inserted})
	if err != nil || len(health) != 2 || health[rewritten].RuleSetDigest != "" || health[inserted].RuleSetDigest != "" {
		t.Errorf("LoadFlowHealthBatch = %+v, %v; want no digests", health, err)
	}
	if got, err := b.RuleSetsByDigest(ctx, []string{rs.Digest}); err != nil || len(got) != 0 {
		t.Errorf("RuleSetsByDigest after the rewrite = %v, %v; want nothing (the stale rows do not count)", got, err)
	}

	// This release writing again records the set again.
	if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: rewritten, Keys: []string{}, RuleSet: rs}); err != nil {
		t.Fatal(err)
	}
	if bl, err := b.GetFlowBaseline(ctx, rewritten); err != nil || bl == nil || bl.RuleSetDigest != rs.Digest {
		t.Errorf("baseline set again = %+v, %v; want the rule set", bl, err)
	}
}

// TestPostgres_RuleNotices_Visibility: inside a user's RLS scope the policy alone admits exactly that user's and their
// organisations' notices, and on the pool (no RLS, as under a BYPASSRLS role) the methods' own predicate does the
// same, admitting the app notices too.
func TestPostgres_RuleNotices_Visibility(t *testing.T) {
	f := newR2Fixture(t)
	b, ctx := f.b, f.ctx
	if v, _ := b.CurrentSchemaVersion(ctx); v < 37 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	member, solo, stranger := f.user("rn-member"), f.user("rn-solo"), f.user("rn-stranger")
	org := f.org("rn-org", member, map[string]string{member: "member"})
	digest := "rs1:test-visibility-" + f.run
	mk := func(scope, scopeID string) *interfaces.RuleNotice {
		return &interfaces.RuleNotice{ID: interfaces.RuleNoticeID(scope, scopeID, digest), Kind: interfaces.RuleNoticeKindNewRules,
			Scope: scope, ScopeID: scopeID, RuleSetDigest: digest, RuleIDs: []string{"rule-a"}, FlowCount: 1}
	}
	orgN, userN, appN := mk(interfaces.RuleNoticeScopeOrg, org), mk(interfaces.RuleNoticeScopeUser, solo), mk(interfaces.RuleNoticeScopeApp, "")
	if _, err := b.UpsertRuleNotices(ctx, []*interfaces.RuleNotice{orgN, userN, appN}); err != nil {
		t.Fatalf("UpsertRuleNotices: %v", err)
	}
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(context.Background(), `DELETE FROM rule_notices WHERE rule_set_digest = $1`, digest)
	})
	ours := func(ns []*interfaces.RuleNotice) []string {
		var out []string
		for _, n := range ns {
			if n.RuleSetDigest == digest {
				out = append(out, n.ID)
			}
		}
		slices.Sort(out)
		return out
	}
	sorted := func(ids ...string) []string { slices.Sort(ids); return ids }

	// The methods' predicate, on the pool.
	for user, want := range map[string][]string{
		member:   sorted(orgN.ID, appN.ID),
		solo:     sorted(userN.ID, appN.ID),
		stranger: {appN.ID},
	} {
		got, err := b.ListRuleNotices(ctx, user, 100)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(ours(got), want) {
			t.Errorf("pool: %s lists %v, want %v", user, ours(got), want)
		}
	}

	skipIfRLSBypassedInternal(t, b)
	// The policy alone, in each user's scope: a raw read with no predicate (rolled back at cleanup).
	for user, want := range map[string][]string{member: {orgN.ID}, solo: {userN.ID}, stranger: nil} {
		rctx := f.rls(user)
		got, err := rowsOfStrings(rctx, b.query(rctx), `SELECT id FROM rule_notices WHERE rule_set_digest = $1 ORDER BY id`, digest)
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(got, want) {
			t.Errorf("rls: %s reads %v, want %v", user, got, want)
		}
	}
	// Writes in a user's committed scope reach only what that user sees.
	as := func(user string, fn func(ctx context.Context)) {
		t.Helper()
		tx, err := b.BeginRLS(ctx, user)
		if err != nil {
			t.Fatalf("BeginRLS: %v", err)
		}
		fn(WithRLSTx(ctx, tx))
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	stamped := func() map[string]string {
		t.Helper()
		rows, err := b.db.QueryContext(ctx, `SELECT id, (read_at IS NOT NULL)::text || '/' || (dismissed_at IS NOT NULL)::text
			FROM rule_notices WHERE rule_set_digest = $1`, digest)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]string{}
		for rows.Next() {
			var id, st string
			if err := rows.Scan(&id, &st); err != nil {
				t.Fatal(err)
			}
			out[id] = st
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	as(stranger, func(rctx context.Context) {
		for _, id := range []string{orgN.ID, userN.ID, appN.ID} {
			if err := b.DismissRuleNotice(rctx, stranger, id); err != nil {
				t.Fatal(err)
			}
		}
		if err := b.MarkAllRuleNoticesRead(rctx, stranger); err != nil {
			t.Fatal(err)
		}
	})
	for id, st := range stamped() {
		if st != "false/false" {
			t.Errorf("a stranger's scope stamped %s (read/dismissed = %s)", id, st)
		}
	}
	as(member, func(rctx context.Context) {
		if err := b.DismissRuleNotice(rctx, member, orgN.ID); err != nil {
			t.Fatal(err)
		}
	})
	if got := stamped(); got[orgN.ID] != "false/true" || got[userN.ID] != "false/false" {
		t.Errorf("after the member's dismiss: %v; want only the org notice dismissed", got)
	}
}

// TestPostgres_RuleNotices_RetentionWindow: the sweep removes a notice no sweep touched within the window and keeps
// a recent one.
func TestPostgres_RuleNotices_RetentionWindow(t *testing.T) {
	f := newR2Fixture(t)
	b, ctx := f.b, f.ctx
	if v, _ := b.CurrentSchemaVersion(ctx); v < 37 {
		t.Skipf("DB at v%d; another test may have rolled it back", v)
	}
	digestOld, digestNew := "rs1:test-old-"+f.run, "rs1:test-new-"+f.run
	var ns []*interfaces.RuleNotice
	for _, d := range []string{digestOld, digestNew} {
		ns = append(ns, &interfaces.RuleNotice{ID: interfaces.RuleNoticeID(interfaces.RuleNoticeScopeApp, "", d),
			Kind: interfaces.RuleNoticeKindNewRules, Scope: interfaces.RuleNoticeScopeApp, RuleSetDigest: d, RuleIDs: []string{"rule-a"}})
	}
	if _, err := b.UpsertRuleNotices(ctx, ns); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(context.Background(), `DELETE FROM rule_notices WHERE rule_set_digest = ANY($1)`, []string{digestOld, digestNew})
	})
	if _, err := b.db.ExecContext(ctx, `UPDATE rule_notices SET updated_at = NOW() - INTERVAL '100 days' WHERE rule_set_digest = $1`, digestOld); err != nil {
		t.Fatal(err)
	}
	res, err := b.PurgeExpiredData(ctx, 90)
	if err != nil {
		t.Fatalf("PurgeExpiredData: %v", err)
	}
	if res.RuleNotices < 1 {
		t.Errorf("RuleNotices purged = %d, want the 100-day-old notice", res.RuleNotices)
	}
	left, err := rowsOfStrings(ctx, b.db, `SELECT rule_set_digest FROM rule_notices WHERE rule_set_digest = ANY($1)`, []string{digestOld, digestNew})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(left, []string{digestNew}) {
		t.Errorf("left after the sweep: %v, want only %s", left, digestNew)
	}
}
