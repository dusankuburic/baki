package contract

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// runGovernanceAlertScenarios holds the governance alert inbox.
func runGovernanceAlertScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	// Governance alerts inbox: record → list (newest-first) → unread count →
	// mark-read → dismiss → clear. Mirrors the notifications-bell lifecycle.
	run("GovernanceAlerts_record_list_read_dismiss_clear", func(t *testing.T) {
		owner := "contract-gov-owner-" + runID
		flowID := "contract-flow-gov-" + runID
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
			ID: flowID, Name: "Gov Flow", Content: json.RawMessage(`{}`), OwnerID: owner,
		}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		// The alert inbox is a GLOBAL list — GovernanceAlertFilter has no FlowID field — so this subtest
		// filters the returned list to its own flow and measures the unread badge as a DELTA from a
		// baseline, so re-running the suite against a persistent database does not fail on old rows.
		mine := func(alerts []*interfaces.GovernanceAlert) []*interfaces.GovernanceAlert {
			out := make([]*interfaces.GovernanceAlert, 0, len(alerts))
			for _, a := range alerts {
				if a.FlowID == flowID {
					out = append(out, a)
				}
			}
			return out
		}

		// Empty list returns a non-nil slice, never nil.
		got, err := b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{})
		if err != nil {
			t.Fatalf("ListGovernanceAlerts empty: %v", err)
		}
		if got == nil {
			t.Fatal("empty list: expected non-nil empty slice, got nil")
		}
		if len(mine(got)) != 0 {
			t.Errorf("expected 0 alerts for this run's flow, got %d", len(mine(got)))
		}

		baseUnread, err := b.UnreadGovernanceAlertCount(ctx)
		if err != nil {
			t.Fatalf("UnreadGovernanceAlertCount empty: %v", err)
		}

		// Record two alerts (drift + regression). Timestamps ensure stable
		// newest-first ordering even if both run within the same millisecond.
		a1 := &interfaces.GovernanceAlert{
			ID: flowID + "|drift|e1w0", FlowID: flowID, FlowName: "Gov Flow",
			Type: "drift", Title: "New findings in Gov Flow", Severity: "error",
			NewErrors: 1, CreatedAt: time.Now().Add(-1 * time.Minute).UTC(),
		}
		a2 := &interfaces.GovernanceAlert{
			ID: flowID + "|health_regression|h80<90", FlowID: flowID, FlowName: "Gov Flow",
			Type: "health_regression", Title: "Health regressed in Gov Flow", Severity: "error",
			HealthScore: 80, PrevHealth: 90, CreatedAt: time.Now().UTC(),
		}
		if err := b.RecordGovernanceAlert(ctx, a1); err != nil {
			t.Fatalf("RecordGovernanceAlert a1: %v", err)
		}
		// Duplicate ID is a no-op (scanner retry safety).
		if err := b.RecordGovernanceAlert(ctx, a1); err != nil {
			t.Fatalf("RecordGovernanceAlert dup: %v", err)
		}
		if err := b.RecordGovernanceAlert(ctx, a2); err != nil {
			t.Fatalf("RecordGovernanceAlert a2: %v", err)
		}

		got, err = b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{})
		if err != nil {
			t.Fatalf("ListGovernanceAlerts: %v", err)
		}
		if len(mine(got)) != 2 {
			t.Fatalf("expected 2 alerts, got %d", len(mine(got)))
		}
		if mine(got)[0].ID != a2.ID {
			t.Errorf("expected newest-first (a2), got %q first", mine(got)[0].ID)
		}
		n, _ := b.UnreadGovernanceAlertCount(ctx)
		if n != baseUnread+2 {
			t.Errorf("expected %d unread, got %d", baseUnread+2, n)
		}

		if err := b.MarkGovernanceAlertRead(ctx, "", a1.ID); err != nil {
			t.Fatalf("MarkGovernanceAlertRead: %v", err)
		}
		if err := b.MarkGovernanceAlertRead(ctx, "", a1.ID); err != nil {
			t.Errorf("MarkGovernanceAlertRead (idempotent): %v", err)
		}
		n, _ = b.UnreadGovernanceAlertCount(ctx)
		if n != baseUnread+1 {
			t.Errorf("expected %d unread after read, got %d", baseUnread+1, n)
		}

		if err := b.DismissGovernanceAlert(ctx, "", a2.ID); err != nil {
			t.Fatalf("DismissGovernanceAlert: %v", err)
		}
		got, _ = b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{})
		if len(mine(got)) != 1 || mine(got)[0].ID != a1.ID {
			t.Errorf("expected only the non-dismissed a1, got %+v", mine(got))
		}
		got, _ = b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{IncludeDismissed: true})
		if len(mine(got)) != 2 {
			t.Errorf("expected 2 alerts including dismissed, got %d", len(mine(got)))
		}
		n, _ = b.UnreadGovernanceAlertCount(ctx)
		if n != baseUnread {
			t.Errorf("expected %d unread after dismiss, got %d", baseUnread, n)
		}

		if err := b.ClearGovernanceAlerts(ctx, ""); err != nil {
			t.Fatalf("ClearGovernanceAlerts: %v", err)
		}
		got, _ = b.ListGovernanceAlerts(ctx, interfaces.GovernanceAlertFilter{IncludeDismissed: true})
		if len(mine(got)) != 1 || mine(got)[0].ID != a1.ID {
			t.Errorf("expected only a1 after clear, got %+v", mine(got))
		}

		if err := b.MarkAllGovernanceAlertsRead(ctx, ""); err != nil {
			t.Fatalf("MarkAllGovernanceAlertsRead: %v", err)
		}
		n, _ = b.UnreadGovernanceAlertCount(ctx)
		if n != 0 {
			t.Errorf("expected 0 unread after mark-all, got %d", n)
		}
	})
}
