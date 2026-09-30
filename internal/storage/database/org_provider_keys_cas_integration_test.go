package database_test

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
)

// The database keystore is what DI hands the provider service as its org
// store (a type assertion, which would fail silently at runtime): pin that it
// satisfies the service's interface, not only the storage one.
var _ service.OrgKeyStore = (*database.EncryptedKeyStore)(nil)

// TestOrgProviderKeys_CompareAndSwap_Postgres pins the org credential's
// compare-and-swap save and the lender-scoped delete at the SQL layer: the
// updated_at token survives the RFC 3339 round trip the API gives it,
// a stale or missing token writes nothing, a save that carries the secret
// keeps its lender (secret_set_by, migration v24), and DeleteOrgIfSavedBy /
// OrgsWithKeySavedBy only touch the rows the named admin lent — a carrying
// edit does not make its editor a lender.
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

	at1, ok, err := ks.SaveOrgIfUnchanged(orgA.ID, provider, "doc-1", lender, false, time.Time{})
	if err != nil || !ok || at1.IsZero() {
		t.Fatalf("first save = (%v, %v, %v)", at1, ok, err)
	}
	if _, ok, err := ks.SaveOrgIfUnchanged(orgA.ID, provider, "doc-x", "admin-b", false, time.Time{}); err != nil || ok {
		t.Fatalf("token-less save over a row: ok=%v err=%v, want refused", ok, err)
	}
	// The token as the API hands it out and parses it back.
	meta, _, err := ks.OrgKeyMeta(orgA.ID, provider)
	if err != nil {
		t.Fatal(err)
	}
	if meta.UpdatedBy != lender || meta.SecretSetBy != lender {
		t.Errorf("first save meta = %+v, want the lender as writer and lender", meta)
	}
	token, err := time.Parse(time.RFC3339Nano, meta.UpdatedAt.UTC().Format(time.RFC3339Nano))
	if err != nil {
		t.Fatal(err)
	}
	at2, ok, err := ks.SaveOrgIfUnchanged(orgA.ID, provider, "doc-2", lender, false, token)
	if err != nil || !ok {
		t.Fatalf("save with the current token = (%v, %v)", ok, err)
	}
	if _, ok, _ := ks.SaveOrgIfUnchanged(orgA.ID, provider, "doc-3", lender, false, token); ok {
		t.Error("a stale token wrote")
	}
	if got, _ := ks.GetOrg(orgA.ID, provider); got != "doc-2" {
		t.Errorf("stored = %q, want doc-2", got)
	}
	if !at2.After(at1) && !at2.Equal(at1) {
		t.Errorf("new token %v precedes %v", at2, at1)
	}

	// Another admin edits org A carrying the stored secret: they become the
	// last writer, the lender stays.
	editor := fmt.Sprintf("editor-%d", stamp)
	if _, ok, err := ks.SaveOrgIfUnchanged(orgA.ID, provider, "doc-2b", editor, true, at2); err != nil || !ok {
		t.Fatalf("carrying edit = (%v, %v)", ok, err)
	}
	if meta, _, _ := ks.OrgKeyMeta(orgA.ID, provider); meta.UpdatedBy != editor || meta.SecretSetBy != lender {
		t.Errorf("after a carrying edit meta = %+v, want writer %s and lender %s", meta, editor, lender)
	}

	if _, _, err := ks.SaveOrgIfUnchanged(orgB.ID, provider, "doc-b", "admin-b", false, time.Time{}); err != nil {
		t.Fatal(err)
	}
	orgs, err := ks.OrgsWithKeySavedBy(provider, lender)
	if err != nil || len(orgs) != 1 || orgs[0] != orgA.ID {
		t.Errorf("OrgsWithKeySavedBy(lender) = %v, %v; want only org A", orgs, err)
	}
	if orgs, err := ks.OrgsWithKeySavedBy(provider, editor); err != nil || len(orgs) != 0 {
		t.Errorf("OrgsWithKeySavedBy(editor) = %v, %v; want none (a carrying edit does not make the editor a lender)", orgs, err)
	}
	if orgs, err := ks.OrgsWithKeySavedBy(provider, ""); err != nil || len(orgs) != 0 {
		t.Errorf("OrgsWithKeySavedBy(empty) = %v, %v; want none", orgs, err)
	}
	if deleted, err := ks.DeleteOrgIfSavedBy(orgB.ID, provider, lender); err != nil || deleted {
		t.Errorf("delete of another admin's row: deleted=%v err=%v", deleted, err)
	}
	// The editor's departure must NOT delete the lender's still-valid
	// credential (F-32: an edit that carried the secret is not a loan).
	if deleted, err := ks.DeleteOrgIfSavedBy(orgA.ID, provider, editor); err != nil || deleted {
		t.Errorf("delete by the carrying editor: deleted=%v err=%v, want the lender's credential kept", deleted, err)
	}
	if has, _ := ks.HasOrg(orgA.ID, provider); !has {
		t.Fatal("the editor's departure removed the lender's credential")
	}
	// The lender's departure disables the credential another admin edited.
	if deleted, err := ks.DeleteOrgIfSavedBy(orgA.ID, provider, lender); err != nil || !deleted {
		t.Errorf("delete of the lender's row: deleted=%v err=%v", deleted, err)
	}
	if has, _ := ks.HasOrg(orgB.ID, provider); !has {
		t.Error("org B's row was removed")
	}

	// A new secret makes its supplier the lender, and the last writer's
	// departure still disables the credential.
	at3, _, err := ks.SaveOrgIfUnchanged(orgA.ID, provider, "doc-4", lender, false, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ks.SaveOrgIfUnchanged(orgA.ID, provider, "doc-5", editor, false, at3); err != nil || !ok {
		t.Fatalf("new-secret save = (%v, %v)", ok, err)
	}
	if meta, _, _ := ks.OrgKeyMeta(orgA.ID, provider); meta.UpdatedBy != editor || meta.SecretSetBy != editor {
		t.Errorf("after a new secret meta = %+v, want %s as writer and lender", meta, editor)
	}
	if deleted, _ := ks.DeleteOrgIfSavedBy(orgA.ID, provider, lender); deleted {
		t.Error("the previous lender's departure removed a credential they no longer lend")
	}
	if deleted, err := ks.DeleteOrgIfSavedBy(orgA.ID, provider, editor); err != nil || !deleted {
		t.Errorf("delete of the new lender's row: deleted=%v err=%v", deleted, err)
	}
}
