package database_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
)

// TestOrgProviderKeys_CompareAndSwap_Postgres pins the org credential's
// compare-and-swap save and the lender-scoped delete at the SQL layer: the
// updated_at token survives the RFC 3339 round trip the API gives it,
// a stale or missing token writes nothing, and DeleteOrgIfUpdatedBy /
// OrgsWithKeyUpdatedBy only touch the rows the named admin last wrote.
// Gated on DATABASE_URL (the podman harness).
func TestOrgProviderKeys_CompareAndSwap_Postgres(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping org provider keys CAS integration test")
	}
	ctx := context.Background()
	b, err := database.New(ctx, database.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	ks, err := b.NewEncryptedKeyStore([]byte("a-sufficiently-long-deployment-secret-123456"))
	if err != nil {
		t.Fatalf("NewEncryptedKeyStore: %v", err)
	}

	stamp := time.Now().UnixNano()
	orgA := &interfaces.Organisation{ID: fmt.Sprintf("org-cas-a-%d", stamp), Name: "A", OwnerID: "admin-a"}
	orgB := &interfaces.Organisation{ID: fmt.Sprintf("org-cas-b-%d", stamp), Name: "B", OwnerID: "admin-b"}
	for _, org := range []*interfaces.Organisation{orgA, orgB} {
		if err := b.SaveOrg(ctx, org); err != nil {
			t.Fatalf("SaveOrg %s: %v", org.ID, err)
		}
	}
	t.Cleanup(func() {
		_ = b.DeleteOrg(context.Background(), orgA.ID)
		_ = b.DeleteOrg(context.Background(), orgB.ID)
	})
	const provider = "azure-foundry"
	lender := fmt.Sprintf("lender-%d", stamp)

	at1, ok, err := ks.SaveOrgIfUnchanged(orgA.ID, provider, "doc-1", lender, time.Time{})
	if err != nil || !ok || at1.IsZero() {
		t.Fatalf("first save = (%v, %v, %v)", at1, ok, err)
	}
	if _, ok, err := ks.SaveOrgIfUnchanged(orgA.ID, provider, "doc-x", "admin-b", time.Time{}); err != nil || ok {
		t.Fatalf("token-less save over a row: ok=%v err=%v, want refused", ok, err)
	}
	// The token as the API hands it out and parses it back.
	_, metaAt, _, err := ks.OrgKeyMeta(orgA.ID, provider)
	if err != nil {
		t.Fatal(err)
	}
	token, err := time.Parse(time.RFC3339Nano, metaAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	at2, ok, err := ks.SaveOrgIfUnchanged(orgA.ID, provider, "doc-2", lender, token)
	if err != nil || !ok {
		t.Fatalf("save with the current token = (%v, %v)", ok, err)
	}
	if _, ok, _ := ks.SaveOrgIfUnchanged(orgA.ID, provider, "doc-3", lender, token); ok {
		t.Error("a stale token wrote")
	}
	if got, _ := ks.GetOrg(orgA.ID, provider); got != "doc-2" {
		t.Errorf("stored = %q, want doc-2", got)
	}
	if !at2.After(at1) && !at2.Equal(at1) {
		t.Errorf("new token %v precedes %v", at2, at1)
	}

	if _, _, err := ks.SaveOrgIfUnchanged(orgB.ID, provider, "doc-b", "admin-b", time.Time{}); err != nil {
		t.Fatal(err)
	}
	orgs, err := ks.OrgsWithKeyUpdatedBy(provider, lender)
	if err != nil || len(orgs) != 1 || orgs[0] != orgA.ID {
		t.Errorf("OrgsWithKeyUpdatedBy = %v, %v; want only org A", orgs, err)
	}
	if deleted, err := ks.DeleteOrgIfUpdatedBy(orgB.ID, provider, lender); err != nil || deleted {
		t.Errorf("delete of another admin's row: deleted=%v err=%v", deleted, err)
	}
	if deleted, err := ks.DeleteOrgIfUpdatedBy(orgA.ID, provider, lender); err != nil || !deleted {
		t.Errorf("delete of the lender's row: deleted=%v err=%v", deleted, err)
	}
	if has, _ := ks.HasOrg(orgB.ID, provider); !has {
		t.Error("org B's row was removed")
	}
}
