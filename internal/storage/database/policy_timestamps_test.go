package database_test

import (
	"context"
	"testing"
	"time"

	"pad-core/models"

	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_SavePolicy_ReturnsStoredTimestamps: the row's created_at and
// updated_at are set by the INSERT, and the save handler renders the struct it
// was handed — so a policy saved through the API came back with a zero date and
// the panel showed the year 1 until something refetched the list.
func TestPostgres_SavePolicy_ReturnsStoredTimestamps(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()
	run := itoa(time.Now().UnixNano())

	owner := "pol-owner-" + run
	orgID := "pol-org-" + run
	if err := b.SaveUser(ctx, &interfaces.User{ID: owner, Email: "pol-" + run + "@example.com", Password: "x"}); err != nil {
		t.Fatalf("SaveUser: %v", err)
	}
	if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: orgID, Name: "Policy Org", OwnerID: owner}); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	t.Cleanup(func() { _ = b.DeleteOrg(ctx, orgID); cleanupUser(t, b, owner) })

	p := &models.Policy{ID: "p-" + run, OrgID: orgID, Name: "Gate", GateSeverity: models.SeverityError}
	if err := b.SavePolicy(ctx, p); err != nil {
		t.Fatalf("SavePolicy: %v", err)
	}
	if p.CreatedAt.IsZero() || p.UpdatedAt.IsZero() {
		t.Fatalf("SavePolicy must stamp the policy it stored: created=%v updated=%v", p.CreatedAt, p.UpdatedAt)
	}

	first := p.UpdatedAt
	time.Sleep(10 * time.Millisecond)
	p.Name = "Gate v2"
	if err := b.SavePolicy(ctx, p); err != nil {
		t.Fatalf("SavePolicy update: %v", err)
	}
	if !p.UpdatedAt.After(first) {
		t.Errorf("an update must move updated_at: %v -> %v", first, p.UpdatedAt)
	}
}
