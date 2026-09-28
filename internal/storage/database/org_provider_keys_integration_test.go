package database_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
)

// TestOrgProviderKeys_Postgres covers G7 at the storage layer: the shared
// org credential round-trips through org_provider_keys (migration v23), is
// isolated per org, reports who wrote it and when (OrgKeyMeta), and goes
// with the organisation (ON DELETE CASCADE). Gated on DATABASE_URL (the
// podman harness).
func TestOrgProviderKeys_Postgres(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping org provider keys integration test")
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
	orgA := &interfaces.Organisation{ID: fmt.Sprintf("org-keys-a-%d", stamp), Name: "A", OwnerID: "admin-a"}
	orgB := &interfaces.Organisation{ID: fmt.Sprintf("org-keys-b-%d", stamp), Name: "B", OwnerID: "admin-b"}
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
	if has, err := ks.HasOrg(orgA.ID, provider); err != nil || has {
		t.Fatalf("HasOrg before save: has=%v err=%v", has, err)
	}
	if _, err := ks.GetOrg(orgA.ID, provider); !errors.Is(err, storage.ErrSecretNotFound) {
		t.Fatalf("GetOrg before save: want ErrSecretNotFound, got %v", err)
	}
	if _, _, ok, err := ks.OrgKeyMeta(orgA.ID, provider); err != nil || ok {
		t.Fatalf("OrgKeyMeta before save: ok=%v err=%v", ok, err)
	}

	const doc = `{"endpoint":"https://r.openai.azure.com","auth":{"mode":"api_key","key":"k1"}}`
	before := time.Now().Add(-time.Second)
	if err := ks.SaveOrg(orgA.ID, provider, doc, "admin-a"); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	got, err := ks.GetOrg(orgA.ID, provider)
	if err != nil || got != doc {
		t.Fatalf("GetOrg = %q, %v; want the stored document", got, err)
	}
	by, at, ok, err := ks.OrgKeyMeta(orgA.ID, provider)
	if err != nil || !ok || by != "admin-a" || at.Before(before) {
		t.Fatalf("OrgKeyMeta = (%q, %v, %v, %v), want admin-a and a recent timestamp", by, at, ok, err)
	}

	// Per-org isolation, and the user-scoped table is untouched.
	if has, _ := ks.HasOrg(orgB.ID, provider); has {
		t.Error("org B must not see org A's credential")
	}
	if has, _ := ks.Has(orgA.ID, provider); has {
		t.Error("an org credential must not appear as a user key of the same id")
	}

	// Upsert replaces the document and the author.
	const doc2 = `{"endpoint":"https://r2.openai.azure.com","auth":{"mode":"api_key","key":"k2"}}`
	if err := ks.SaveOrg(orgA.ID, provider, doc2, "admin-a2"); err != nil {
		t.Fatalf("SaveOrg upsert: %v", err)
	}
	if got, _ := ks.GetOrg(orgA.ID, provider); got != doc2 {
		t.Errorf("GetOrg after upsert = %q, want the new document", got)
	}
	if by, _, _, _ := ks.OrgKeyMeta(orgA.ID, provider); by != "admin-a2" {
		t.Errorf("updated_by after upsert = %q, want admin-a2", by)
	}

	// Explicit delete, then the cascade: deleting the organisation removes
	// the row without a keystore call.
	if err := ks.DeleteOrg(orgA.ID, provider); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	if has, _ := ks.HasOrg(orgA.ID, provider); has {
		t.Error("HasOrg after delete should be false")
	}
	if err := ks.SaveOrg(orgB.ID, provider, doc, "admin-b"); err != nil {
		t.Fatalf("SaveOrg org B: %v", err)
	}
	if err := b.DeleteOrg(ctx, orgB.ID); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}
	if has, err := ks.HasOrg(orgB.ID, provider); err != nil || has {
		t.Errorf("credential must cascade with the organisation: has=%v err=%v", has, err)
	}
}
