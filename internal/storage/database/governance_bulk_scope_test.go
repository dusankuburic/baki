package database

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestGovernanceAlerts_BulkMutations_TwoTenants: "mark all read" and "clear" touch exactly the alerts the nested-EXISTS
// predicate (govAlertWhere, the policy's mirror) selects for the caller, for every caller kind, under RLS and on a
// bare transaction (a BYPASSRLS role, where the statement's own predicate is the only boundary), and never an alert
// of a flow the caller cannot see. Each mutation runs in a transaction that is rolled back, on a private database.
func TestGovernanceAlerts_BulkMutations_TwoTenants(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	f := &r2Fixture{t: t, b: b, ctx: context.Background(), run: r2stamp()}
	// Tenant A: a owns a personal flow and an organisation flow, am is a member. Tenant B: bo owns a flow that c
	// collaborates on.
	a, am, bo, c, outsider := f.user("a"), f.user("am"), f.user("bo"), f.user("c"), f.user("outsider")
	orgA := f.org("orgA", a, map[string]string{a: "admin", am: "member"})
	fA1 := f.flow("fA1", a, "", "A personal", "")
	fA2 := f.flow("fA2", a, orgA, "A org", "")
	fB1 := f.flow("fB1", bo, "", "B", "")
	f.collab(fB1, c)

	tenantOf := map[string]string{}
	base := time.Now().UTC().Add(-time.Hour)
	n := 0
	record := func(flowID, target, state string) {
		t.Helper()
		n++
		id := fmt.Sprintf("%s|%s|%s|%d", flowID, target, state, n)
		tenantOf[id] = map[string]string{fA1: "A", fA2: "A", fB1: "B"}[flowID]
		if err := b.RecordGovernanceAlert(f.ctx, &interfaces.GovernanceAlert{ID: id, FlowID: flowID, Type: "drift", Title: "t",
			Severity: "warning", CreatedAt: base.Add(time.Duration(n) * time.Second), TargetUser: target}); err != nil {
			t.Fatalf("RecordGovernanceAlert: %v", err)
		}
		if col := map[string]string{"read": "read_at", "dismissed": "dismissed_at"}[state]; col != "" {
			if _, err := b.db.ExecContext(f.ctx, `UPDATE gov_alerts SET `+col+` = NOW() - INTERVAL '1 minute' WHERE id = $1`, id); err != nil {
				t.Fatalf("mark %s %s: %v", id, state, err)
			}
		}
	}
	for _, fl := range []string{fA1, fA2, fB1} {
		for _, state := range []string{"unread", "read", "dismissed"} {
			record(fl, "", state)
		}
		for _, target := range []string{a, am, bo, c, outsider} {
			record(fl, target, "unread")
			record(fl, target, "dismissed")
		}
	}

	type mutation struct {
		name    string
		run     func(ctx context.Context, userID string) error
		legacy  string // the statement's WHERE without the visibility predicate
		touched string // the run's rows the mutation changed, read in the same transaction
	}
	mutations := []mutation{
		{"mark all read", b.MarkAllGovernanceAlertsRead, "read_at IS NULL AND dismissed_at IS NULL",
			`SELECT id FROM gov_alerts WHERE read_at = NOW()`},
		{"clear", b.ClearGovernanceAlerts, "dismissed_at IS NOT NULL",
			`SELECT id FROM unnest($1::text[]) AS seeded(id) WHERE NOT EXISTS (SELECT 1 FROM gov_alerts g WHERE g.id = seeded.id)`},
	}
	seeded := make([]string, 0, len(tenantOf))
	for id := range tenantOf {
		seeded = append(seeded, id)
	}
	visibleTo := map[string][]string{a: {"A"}, am: {"A"}, bo: {"B"}, c: {"B"}, outsider: nil, "": {"A", "B"}}

	got := map[string][]string{} // "<mutation> <caller> <mode>" → touched ids
	for _, m := range mutations {
		for _, caller := range []string{a, am, bo, c, outsider, ""} {
			for _, mode := range []string{"rls", "pool"} {
				label := fmt.Sprintf("%s by %s (%s)", m.name, strings.TrimSuffix(strings.TrimPrefix(caller, "r2-"), "-"+f.run), mode)
				var tx *sql.Tx
				var err error
				if mode == "rls" {
					tx, err = b.BeginRLS(f.ctx, caller)
				} else {
					tx, err = b.db.BeginTx(f.ctx, nil)
				}
				if err != nil {
					t.Fatalf("%s: begin: %v", label, err)
				}
				ctx := WithRLSTx(f.ctx, tx)
				var args sqlArgs
				want, err := rowsOfStrings(ctx, tx, `SELECT id FROM gov_alerts WHERE `+m.legacy+` AND `+govAlertWhere(caller, &args), args...)
				if err != nil {
					t.Fatalf("%s: legacy predicate: %v", label, err)
				}
				if err := m.run(ctx, caller); err != nil {
					t.Fatalf("%s: %v", label, err)
				}
				var touched []string
				if err := asSystem(ctx, tx, func() error {
					var qargs []any
					if strings.Contains(m.touched, "$1") {
						qargs = []any{seeded}
					}
					touched, err = rowsOfStrings(ctx, tx, m.touched, qargs...)
					return err
				}); err != nil {
					t.Fatalf("%s: read back: %v", label, err)
				}
				_ = tx.Rollback()

				touched = onlySeeded(touched, tenantOf)
				want = onlySeeded(want, tenantOf)
				if !slices.Equal(touched, want) {
					t.Errorf("%s: touched %v, want (legacy predicate) %v", label, touched, want)
				}
				for _, id := range touched {
					if !slices.Contains(visibleTo[caller], tenantOf[id]) {
						t.Errorf("%s: touched %s of tenant %s, a flow the caller cannot see", label, id, tenantOf[id])
					}
				}
				got[m.name+" "+caller+" "+mode] = touched
			}
		}
	}

	// Pin a few outcomes, so the comparison above is not two wrong answers agreeing.
	expect := func(key string, want ...string) {
		t.Helper()
		slices.Sort(want)
		if !slices.Equal(got[key], want) {
			t.Errorf("%s: touched %v, want %v", key, got[key], want)
		}
	}
	// c sees B's flow as a collaborator: its team-wide unread alert and the one targeted at c.
	expect("mark all read "+c+" pool", fB1+"||unread|27", fB1+"|"+c+"|unread|36")
	// am, an org member, clears the dismissed team-wide and own alerts of the org flow only, not of a's personal flow.
	expect("clear "+am+" rls", fA2+"||dismissed|16", fA2+"|"+am+"|dismissed|20")
	expect("clear " + outsider + " pool")
}

// onlySeeded keeps the ids of tenantOf, sorted.
func onlySeeded(ids []string, tenantOf map[string]string) []string {
	out := []string{}
	for _, id := range ids {
		if _, ok := tenantOf[id]; ok {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out
}
