package database_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_GovernanceAlerts_CrossTenantIsolation pins the tenant boundary on
// the six governance-alert methods.
//
// It runs WITHOUT an RLS transaction — no BeginRLS, so app.current_user_id is
// unset and every policy short-circuits through its `NOT app_rls_active()` arm.
// That is deliberate. RLS already enforces this rule correctly, so a test that
// ran under it would pass no matter what the Go/SQL layer did. The whole
// exposure is that RLS is inert whenever the app connects as a
// superuser/BYPASSRLS role (the docker-compose default), leaving these WHERE
// clauses as the only boundary — and they used to scope on `target_user_id = ”`
// alone, which is true of EVERY team-wide alert in the deployment.
//
// Two of the operations need no identifier at all to cross tenants:
// MarkAllGovernanceAlertsRead and ClearGovernanceAlerts, the latter being a
// DELETE.
func TestPostgres_GovernanceAlerts_CrossTenantIsolation(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	run := time.Now().UnixNano()

	type tenant struct {
		user    string
		flowID  string
		alertID string
	}
	mk := func(label string) tenant {
		tn := tenant{
			user:   label + "-user-" + itoa(run),
			flowID: label + "-flow-" + itoa(run),
		}
		tn.alertID = tn.flowID + "|drift|e1w0"
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
			ID: tn.flowID, Name: label + " Flow",
			Content: json.RawMessage(`{}`), OwnerID: tn.user,
		}); err != nil {
			t.Fatalf("SaveFlow(%s): %v", label, err)
		}
		// target_user_id defaults to '' — a TEAM-WIDE alert, which is the case
		// the old predicate matched globally.
		if err := b.RecordGovernanceAlert(ctx, &interfaces.GovernanceAlert{
			ID: tn.alertID, FlowID: tn.flowID, FlowName: label + " Flow",
			Type: "drift", Title: "New findings", Severity: "error",
			NewErrors: 1, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("RecordGovernanceAlert(%s): %v", label, err)
		}
		t.Cleanup(func() { _ = b.DeleteFlow(ctx, tn.flowID) }) // CASCADEs the alert
		return tn
	}

	victim := mk("victim")
	attacker := mk("attacker")

	// state reports (read_at set, dismissed_at set, still exists) for one alert,
	// read with the OWNER's identity so visibility never hides a mutation that
	// actually landed.
	state := func(t *testing.T, tn tenant) (read, dismissed, exists bool) {
		t.Helper()
		alerts, err := b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{
			UserID: tn.user, IncludeDismissed: true,
		})
		if err != nil {
			t.Fatalf("ListGovernanceAlerts: %v", err)
		}
		for _, a := range alerts {
			if a.ID == tn.alertID {
				return a.ReadAt != nil, a.DismissedAt != nil, true
			}
		}
		return false, false, false
	}

	assertVictimUntouched := func(t *testing.T, after string) {
		t.Helper()
		read, dismissed, exists := state(t, victim)
		if !exists {
			t.Fatalf("after %s: the victim org's alert was DELETED", after)
		}
		if read {
			t.Errorf("after %s: the victim org's alert was marked read", after)
		}
		if dismissed {
			t.Errorf("after %s: the victim org's alert was dismissed", after)
		}
	}

	t.Run("read list does not leak another tenant's alerts", func(t *testing.T) {
		alerts, err := b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{
			UserID: attacker.user, IncludeDismissed: true,
		})
		if err != nil {
			t.Fatalf("ListGovernanceAlerts: %v", err)
		}
		for _, a := range alerts {
			if a.ID == victim.alertID {
				t.Fatal("attacker's alert list includes the victim org's alert")
			}
		}
	})

	t.Run("MarkGovernanceAlertRead by id", func(t *testing.T) {
		if err := b.MarkGovernanceAlertRead(ctx, attacker.user, victim.alertID); err != nil {
			t.Fatalf("MarkGovernanceAlertRead: %v", err)
		}
		assertVictimUntouched(t, "cross-tenant MarkGovernanceAlertRead")
	})

	t.Run("DismissGovernanceAlert by id", func(t *testing.T) {
		if err := b.DismissGovernanceAlert(ctx, attacker.user, victim.alertID); err != nil {
			t.Fatalf("DismissGovernanceAlert: %v", err)
		}
		assertVictimUntouched(t, "cross-tenant DismissGovernanceAlert")
	})

	t.Run("MarkAllGovernanceAlertsRead needs no id", func(t *testing.T) {
		if err := b.MarkAllGovernanceAlertsRead(ctx, attacker.user); err != nil {
			t.Fatalf("MarkAllGovernanceAlertsRead: %v", err)
		}
		assertVictimUntouched(t, "MarkAllGovernanceAlertsRead")

		// ...and it DID do its job for the caller's own alert, so the scoping
		// is not simply refusing everything.
		read, _, exists := state(t, attacker)
		if !exists {
			t.Fatal("attacker's own alert vanished")
		}
		if !read {
			t.Error("MarkAllGovernanceAlertsRead did not mark the caller's OWN alert read")
		}
	})

	t.Run("ClearGovernanceAlerts deletes only the caller's own", func(t *testing.T) {
		// Dismiss both alerts as their respective owners so both are eligible
		// for the DELETE, then clear as the attacker.
		if err := b.DismissGovernanceAlert(ctx, victim.user, victim.alertID); err != nil {
			t.Fatalf("DismissGovernanceAlert(victim, own): %v", err)
		}
		if err := b.DismissGovernanceAlert(ctx, attacker.user, attacker.alertID); err != nil {
			t.Fatalf("DismissGovernanceAlert(attacker, own): %v", err)
		}
		if _, dismissed, _ := state(t, victim); !dismissed {
			t.Fatal("setup: victim's alert should be dismissed and therefore clearable")
		}

		if err := b.ClearGovernanceAlerts(ctx, attacker.user); err != nil {
			t.Fatalf("ClearGovernanceAlerts: %v", err)
		}

		if _, _, exists := state(t, victim); !exists {
			t.Error("ClearGovernanceAlerts DELETED the victim org's dismissed alert")
		}
		if _, _, exists := state(t, attacker); exists {
			t.Error("ClearGovernanceAlerts did not delete the caller's OWN dismissed alert")
		}
	})

	t.Run("unread count excludes other tenants", func(t *testing.T) {
		// The victim's alert is dismissed by now, so re-record a fresh unread
		// team-wide one under the victim's flow and confirm it stays invisible
		// to the attacker's badge.
		fresh := victim.flowID + "|health_regression|h80<90"
		if err := b.RecordGovernanceAlert(ctx, &interfaces.GovernanceAlert{
			ID: fresh, FlowID: victim.flowID, FlowName: "victim Flow",
			Type: "health_regression", Title: "Health regressed", Severity: "error",
			HealthScore: 80, PrevHealth: 90, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("RecordGovernanceAlert: %v", err)
		}
		before, err := b.UnreadGovernanceAlertCountFor(ctx, attacker.user)
		if err != nil {
			t.Fatalf("UnreadGovernanceAlertCountFor: %v", err)
		}
		if before != 0 {
			t.Errorf("attacker's unread badge counts %d alert(s) it cannot see", before)
		}
	})
}

// itoa avoids pulling strconv in for one call.
func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
