package database

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// TestPurgeExpiredData_ShareTokensAndAlerts: the purge covered tokens, invites,
// audit, history and usage — and missed two tables that only ever grow:
// expired share_tokens (has expires_at, never read once expired) and
// gov_alerts (one row per alert per sweep). Both are now purged; a permanent
// share link (no expiry) and a fresh alert must survive.
func TestPurgeExpiredData_ShareTokensAndAlerts(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	b := openMigrationTestDB(t)
	ctx := context.Background()
	flowID := "purge-cov-flow-" + time.Now().Format("150405.000")
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: flowID, Name: "Purge", Content: json.RawMessage(`{}`), OwnerID: "purge-user"}); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), flowID) }) // cascades tokens + alerts

	past := time.Now().Add(-time.Hour)
	if err := b.CreateShareToken(ctx, &interfaces.ShareToken{FlowID: flowID, TokenHash: "expired-" + flowID, ExpiresAt: &past}); err != nil {
		t.Fatalf("expired token: %v", err)
	}
	if err := b.CreateShareToken(ctx, &interfaces.ShareToken{FlowID: flowID, TokenHash: "permanent-" + flowID}); err != nil {
		t.Fatalf("permanent token: %v", err)
	}
	old := time.Now().Add(-100 * 24 * time.Hour)
	if err := b.RecordGovernanceAlert(ctx, &interfaces.GovernanceAlert{ID: flowID + "|old", FlowID: flowID, FlowName: "Purge", Type: "drift", Title: "old", Severity: "warning", CreatedAt: old}); err != nil {
		t.Fatalf("old alert: %v", err)
	}
	if err := b.RecordGovernanceAlert(ctx, &interfaces.GovernanceAlert{ID: flowID + "|new", FlowID: flowID, FlowName: "Purge", Type: "drift", Title: "new", Severity: "warning", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("new alert: %v", err)
	}

	res, err := b.PurgeExpiredData(ctx, 30)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if res.ShareTokens < 1 {
		t.Errorf("expired share token not purged (ShareTokens=%d)", res.ShareTokens)
	}
	if res.GovAlerts < 1 {
		t.Errorf("100-day-old alert not purged (GovAlerts=%d)", res.GovAlerts)
	}
	var tokens, alerts int
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM share_tokens WHERE flow_id = $1`, flowID).Scan(&tokens); err != nil {
		t.Fatal(err)
	}
	if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM gov_alerts WHERE flow_id = $1`, flowID).Scan(&alerts); err != nil {
		t.Fatal(err)
	}
	if tokens != 1 {
		t.Errorf("the permanent share link must survive: %d token rows left, want 1", tokens)
	}
	if alerts != 1 {
		t.Errorf("the fresh alert must survive: %d alert rows left, want 1", alerts)
	}
}
