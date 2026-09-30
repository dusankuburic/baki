package database_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/storage/interfaces"
)

const familyTestSecret = "a-sufficiently-long-deployment-secret-123456"

func openFamilyKeystore(t *testing.T) (*database.PostgresStorageBackend, *database.EncryptedKeyStore) {
	t.Helper()
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set — skipping keystore family integration test")
	}
	b := openTestDB(t)
	ks, err := b.NewEncryptedKeyStore([]byte(familyTestSecret))
	if err != nil {
		t.Fatalf("NewEncryptedKeyStore: %v", err)
	}
	return b, ks
}

func providersOf[T any](entries []T, provider func(T) string) string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, provider(e))
	}
	return strings.Join(out, ",")
}

// TestKeystoreFamily_Personal: a family is the key itself plus key/…, by
// starts_with — "azure-foundryX" and "azure-foundry-legacy" are other
// providers, and a family name is never a LIKE pattern ("_" would match any
// character). Each row reports its last save time for the orphan sweep.
func TestKeystoreFamily_Personal(t *testing.T) {
	b, ks := openFamilyKeystore(t)
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	scope, other := "family-user-"+stamp, "family-other-"+stamp
	t.Cleanup(func() {
		for _, s := range []string{scope, other} {
			_, _ = b.DB().ExecContext(context.Background(), `DELETE FROM provider_keys WHERE user_id = $1`, s)
		}
	})
	for _, p := range []string{"azure-foundry", "azure-foundry/abcdefghij", "azure-foundry/klmnopqrst", "azure-foundryX", "azure-foundry-legacy", "azureXfoundry/abcdefghij", "openai"} {
		if err := ks.Save(scope, p, "doc:"+p); err != nil {
			t.Fatalf("Save %s: %v", p, err)
		}
	}
	if err := ks.Save(other, "azure-foundry/zzzzzzzzzz", "theirs"); err != nil {
		t.Fatal(err)
	}

	got, err := ks.ListFamily(scope, "azure-foundry")
	if err != nil {
		t.Fatalf("ListFamily: %v", err)
	}
	if list := providersOf(got, func(e interfaces.SecretFamilyEntry) string { return e.Provider }); list != "azure-foundry,azure-foundry/abcdefghij,azure-foundry/klmnopqrst" {
		t.Errorf("ListFamily = %s, want the key and its two members only", list)
	}
	for _, e := range got {
		if e.UpdatedAt.IsZero() || time.Since(e.UpdatedAt) > time.Hour {
			t.Errorf("%s UpdatedAt = %v, want the save time", e.Provider, e.UpdatedAt)
		}
	}
	if got, err := ks.ListFamily(scope, "azure_foundry"); err != nil || len(got) != 0 {
		t.Errorf("family %q = (%+v, %v): a family name must never act as a LIKE pattern", "azure_foundry", got, err)
	}
	if got, err := ks.ListFamily("family-nobody-"+stamp, "azure-foundry"); err != nil || got == nil || len(got) != 0 {
		t.Errorf("an owner without keys = (%v, %v), want a non-nil empty list", got, err)
	}
	for _, bad := range []string{"", "azure-foundry/abcdefghij"} {
		if _, err := ks.ListFamily(scope, bad); err == nil {
			t.Errorf("ListFamily(%q) must be refused", bad)
		}
	}
}

// TestKeystoreFamily_OrgLenderRules: the org family helpers follow
// OrgsWithKeySavedBy's lender rule (secret_set_by; updated_by only for a row
// no save has stamped since v24), never reach outside the family, and the
// conditional delete returns every deleted key's document — decrypted, or
// flagged unreadable — so each credential can be forgotten from caches.
func TestKeystoreFamily_OrgLenderRules(t *testing.T) {
	b, ks := openFamilyKeystore(t)
	ctx := context.Background()
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	orgA := &interfaces.Organisation{ID: "family-org-a-" + stamp, Name: "A", OwnerID: "x"}
	orgB := &interfaces.Organisation{ID: "family-org-b-" + stamp, Name: "B", OwnerID: "x"}
	for _, o := range []*interfaces.Organisation{orgA, orgB} {
		if err := b.SaveOrg(ctx, o); err != nil {
			t.Fatalf("SaveOrg: %v", err)
		}
	}
	t.Cleanup(func() {
		_ = b.DeleteOrg(context.Background(), orgA.ID)
		_ = b.DeleteOrg(context.Background(), orgB.ID)
	})
	lender, editor, other := "lender-"+stamp, "editor-"+stamp, "other-"+stamp
	save := func(org, provider, by string) {
		t.Helper()
		if err := ks.SaveOrg(org, provider, "doc:"+org+":"+provider, by); err != nil {
			t.Fatalf("SaveOrg %s %s: %v", org, provider, err)
		}
	}
	save(orgA.ID, "azure-foundry", lender)
	save(orgA.ID, "azure-foundry/abcdefghij", lender)
	save(orgA.ID, "azure-foundry/klmnopqrst", other)
	save(orgA.ID, "azure-foundryX", lender)
	save(orgA.ID, "azure-foundry-legacy", lender)
	save(orgB.ID, "azure-foundry/zzzzzzzzzz", lender)

	// An editor who carried the lender's secret is not a lender.
	meta, _, err := ks.OrgKeyMeta(orgA.ID, "azure-foundry/abcdefghij")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ks.SaveOrgIfUnchanged(orgA.ID, "azure-foundry/abcdefghij", "doc:edited", editor, true, meta.UpdatedAt); err != nil || !ok {
		t.Fatalf("carrying edit = (%v, %v)", ok, err)
	}
	// A row no save has stamped since the v24 backfill: updated_by decides.
	save(orgB.ID, "azure-foundry", other)
	if _, err := b.DB().ExecContext(ctx, `UPDATE org_provider_keys SET secret_set_by = '', updated_by = $3 WHERE org_id = $1 AND provider = $2`,
		orgB.ID, "azure-foundry", lender); err != nil {
		t.Fatal(err)
	}

	entries, err := ks.ListOrgKeyFamily(orgA.ID, "azure-foundry")
	if err != nil {
		t.Fatalf("ListOrgKeyFamily: %v", err)
	}
	if list := providersOf(entries, func(e interfaces.OrgKeyFamilyEntry) string { return e.Provider }); list != "azure-foundry,azure-foundry/abcdefghij,azure-foundry/klmnopqrst" {
		t.Errorf("ListOrgKeyFamily = %s", list)
	}
	for _, e := range entries {
		if e.Provider == "azure-foundry/abcdefghij" && (e.UpdatedBy != editor || e.SecretSetBy != lender || e.UpdatedAt.IsZero()) {
			t.Errorf("the carried key's metadata = %+v, want writer %s and lender %s", e.OrgKeyInfo, editor, lender)
		}
	}

	refs, err := ks.OrgKeyFamilySavedBy("azure-foundry", lender)
	if err != nil {
		t.Fatal(err)
	}
	wantRefs := fmt.Sprintf("%s:azure-foundry,%s:azure-foundry/abcdefghij,%s:azure-foundry,%s:azure-foundry/zzzzzzzzzz", orgA.ID, orgA.ID, orgB.ID, orgB.ID)
	if got := providersOf(refs, func(r interfaces.OrgKeyRef) string { return r.OrgID + ":" + r.Provider }); got != wantRefs {
		t.Errorf("OrgKeyFamilySavedBy(lender) = %s,\nwant %s", got, wantRefs)
	}
	if refs, err := ks.OrgKeyFamilySavedBy("azure-foundry", editor); err != nil || len(refs) != 0 {
		t.Errorf("OrgKeyFamilySavedBy(editor) = (%v, %v): a carrying edit does not make a lender", refs, err)
	}
	if refs, err := ks.OrgKeyFamilySavedBy("azure-foundry", ""); err != nil || len(refs) != 0 {
		t.Errorf("OrgKeyFamilySavedBy(\"\") = (%v, %v), want nothing", refs, err)
	}

	// A lent row the current key cannot decrypt is still deleted, flagged.
	rotated, err := b.NewEncryptedKeyStore([]byte("the-previous-deployment-secret-0000000000"))
	if err != nil {
		t.Fatal(err)
	}
	if err := rotated.SaveOrg(orgA.ID, "azure-foundry/unreadabl", "old", lender); err != nil {
		t.Fatal(err)
	}

	if deleted, err := ks.DeleteOrgKeyFamilyIfSavedBy(orgA.ID, "azure-foundry", ""); err != nil || len(deleted) != 0 {
		t.Errorf("delete by \"\" = (%v, %v), want nothing", deleted, err)
	}
	deleted, err := ks.DeleteOrgKeyFamilyIfSavedBy(orgA.ID, "azure-foundry", lender)
	if err != nil {
		t.Fatalf("DeleteOrgKeyFamilyIfSavedBy: %v", err)
	}
	want := []interfaces.DeletedOrgKey{
		{Provider: "azure-foundry", Document: "doc:" + orgA.ID + ":azure-foundry"},
		{Provider: "azure-foundry/abcdefghij", Document: "doc:edited"},
		{Provider: "azure-foundry/unreadabl", Unreadable: true},
	}
	if fmt.Sprintf("%+v", deleted) != fmt.Sprintf("%+v", want) {
		t.Errorf("deleted = %+v,\nwant %+v", deleted, want)
	}
	left, _ := ks.ListOrgKeyFamily(orgA.ID, "azure-foundry")
	if got := providersOf(left, func(e interfaces.OrgKeyFamilyEntry) string { return e.Provider }); got != "azure-foundry/klmnopqrst" {
		t.Errorf("org A's family after the delete = %s, want only the other lender's key", got)
	}
	for _, p := range []string{"azure-foundryX", "azure-foundry-legacy"} {
		if has, _ := ks.HasOrg(orgA.ID, p); !has {
			t.Errorf("%s is not in the family but was deleted", p)
		}
	}
	if left, _ := ks.ListOrgKeyFamily(orgB.ID, "azure-foundry"); len(left) != 2 {
		t.Errorf("org B's keys = %+v, want both untouched by org A's delete", left)
	}
	if _, err := ks.DeleteOrgKeyFamilyIfSavedBy(orgA.ID, "", lender); err == nil {
		t.Error("an empty family must be refused")
	}
}

// TestKeystoreFamily_ConditionalDelete: a delete decided on an earlier read
// (the orphan sweep's listing, a removed resource's post-commit delete)
// takes the key only while its row still carries the stamp that read saw —
// a key saved again since is kept.
func TestKeystoreFamily_ConditionalDelete(t *testing.T) {
	b, ks := openFamilyKeystore(t)
	ctx := context.Background()
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	scope := "family-cond-" + stamp
	org := &interfaces.Organisation{ID: "family-cond-org-" + stamp, Name: "C", OwnerID: "x"}
	if err := b.SaveOrg(ctx, org); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	t.Cleanup(func() {
		_, _ = b.DB().ExecContext(context.Background(), `DELETE FROM provider_keys WHERE user_id = $1`, scope)
		_ = b.DeleteOrg(context.Background(), org.ID)
	})
	stampOf := func(provider string) time.Time {
		t.Helper()
		entries, err := ks.ListFamily(scope, "azure-foundry")
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			if e.Provider == provider {
				return e.UpdatedAt
			}
		}
		t.Fatalf("%s not listed", provider)
		return time.Time{}
	}

	const key = "azure-foundry/abcdefghij"
	if err := ks.Save(scope, key, "v1"); err != nil {
		t.Fatal(err)
	}
	first := stampOf(key)
	time.Sleep(5 * time.Millisecond) // a later NOW() for the second save
	if err := ks.Save(scope, key, "v2"); err != nil {
		t.Fatal(err)
	}
	second := stampOf(key)
	if second.Equal(first) {
		t.Fatalf("two saves share the stamp %v", first)
	}
	if deleted, err := ks.DeleteIfUnchanged(scope, key, first); err != nil || deleted {
		t.Errorf("delete on the first save's stamp = (%v, %v), want kept", deleted, err)
	}
	if got, _ := ks.Get(scope, key); got != "v2" {
		t.Errorf("the re-saved key = %q, want v2", got)
	}
	if deleted, err := ks.DeleteIfUnchanged(scope, key, second); err != nil || !deleted {
		t.Errorf("delete on the current stamp = (%v, %v), want deleted", deleted, err)
	}
	if deleted, err := ks.DeleteIfUnchanged(scope, key, second); err != nil || deleted {
		t.Errorf("delete of a missing key = (%v, %v), want nothing", deleted, err)
	}

	if err := ks.SaveOrg(org.ID, key, "o1", "admin"); err != nil {
		t.Fatal(err)
	}
	m1, _, err := ks.OrgKeyMeta(org.ID, key)
	if err != nil {
		t.Fatal(err)
	}
	m2At, ok, err := ks.SaveOrgIfUnchanged(org.ID, key, "o2", "admin", false, m1.UpdatedAt)
	if err != nil || !ok {
		t.Fatalf("SaveOrgIfUnchanged = (%v, %v)", ok, err)
	}
	if deleted, err := ks.DeleteOrgKeyIfUnchanged(org.ID, key, m1.UpdatedAt); err != nil || deleted {
		t.Errorf("org delete on a stale stamp = (%v, %v), want kept", deleted, err)
	}
	if deleted, err := ks.DeleteOrgKeyIfUnchanged(org.ID, key, m2At); err != nil || !deleted {
		t.Errorf("org delete on the current stamp = (%v, %v), want deleted", deleted, err)
	}
	if has, _ := ks.HasOrg(org.ID, key); has {
		t.Error("the org key survived its conditional delete")
	}
}

// TestKeystore_OrgSnapshotRestore: a failed cross-store write puts the org
// row back exactly — document, writer, lender and stamp (the admin's token
// is current again) — even a row the current key cannot decrypt, deletes a
// row the save created, and never undoes a newer save.
func TestKeystore_OrgSnapshotRestore(t *testing.T) {
	b, ks := openFamilyKeystore(t)
	ctx := context.Background()
	stamp := fmt.Sprintf("%d", time.Now().UnixNano())
	org := &interfaces.Organisation{ID: "restore-org-" + stamp, Name: "R", OwnerID: "x"}
	if err := b.SaveOrg(ctx, org); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}
	t.Cleanup(func() { _ = b.DeleteOrg(context.Background(), org.ID) })
	const key = "azure-foundry/abcdefghij"

	if _, ok, err := ks.SnapshotOrg(org.ID, key); err != nil || ok {
		t.Fatalf("snapshot of no row = (%v, %v)", ok, err)
	}
	if err := ks.SaveOrg(org.ID, key, "lender-doc", "lender"); err != nil {
		t.Fatal(err)
	}
	snap, ok, err := ks.SnapshotOrg(org.ID, key)
	if err != nil || !ok || snap.UpdatedBy != "lender" || snap.SecretSetBy != "lender" || snap.UpdatedAt.IsZero() || snap.Sealed == "" || snap.Sealed == "lender-doc" {
		t.Fatalf("snapshot = (%+v, %v, %v), want the sealed row with its lender", snap, ok, err)
	}
	at, ok, err := ks.SaveOrgIfUnchanged(org.ID, key, "editor-doc", "editor", false, snap.UpdatedAt)
	if err != nil || !ok {
		t.Fatalf("the save to undo = (%v, %v)", ok, err)
	}
	if restored, err := ks.RestoreOrgIfUnchanged(org.ID, key, snap.UpdatedAt, &snap); err != nil || restored {
		t.Errorf("restore on a stale stamp = (%v, %v), want refused", restored, err)
	}
	if restored, err := ks.RestoreOrgIfUnchanged(org.ID, key, at, &snap); err != nil || !restored {
		t.Fatalf("restore = (%v, %v)", restored, err)
	}
	doc, err := ks.GetOrg(org.ID, key)
	meta, _, _ := ks.OrgKeyMeta(org.ID, key)
	if err != nil || doc != "lender-doc" || meta.UpdatedBy != "lender" || meta.SecretSetBy != "lender" || !meta.UpdatedAt.Equal(snap.UpdatedAt) {
		t.Errorf("after the restore: %q (%v), meta %+v; want the lender's row and stamp %v back", doc, err, meta, snap.UpdatedAt)
	}

	// A row the current deployment key cannot decrypt goes back sealed as
	// it was: the key that sealed it still opens it.
	rotated, err := b.NewEncryptedKeyStore([]byte("the-previous-deployment-secret-0000000000"))
	if err != nil {
		t.Fatal(err)
	}
	const unreadable = "azure-foundry/unreadabl"
	if err := rotated.SaveOrg(org.ID, unreadable, "sealed-under-the-old-key", "lender"); err != nil {
		t.Fatal(err)
	}
	usnap, ok, err := ks.SnapshotOrg(org.ID, unreadable)
	if err != nil || !ok {
		t.Fatalf("snapshot of an unreadable row = (%v, %v)", ok, err)
	}
	uat, ok, err := ks.SaveOrgIfUnchanged(org.ID, unreadable, "repair", "admin", false, usnap.UpdatedAt)
	if err != nil || !ok {
		t.Fatalf("repair save = (%v, %v)", ok, err)
	}
	if restored, err := ks.RestoreOrgIfUnchanged(org.ID, unreadable, uat, &usnap); err != nil || !restored {
		t.Fatalf("restore of the unreadable row = (%v, %v)", restored, err)
	}
	if doc, err := rotated.GetOrg(org.ID, unreadable); err != nil || doc != "sealed-under-the-old-key" {
		t.Errorf("the restored unreadable row = (%q, %v), want its original ciphertext back", doc, err)
	}

	// No row before the save: the restore deletes the one it created.
	const fresh = "azure-foundry/klmnopqrst"
	fat, ok, err := ks.SaveOrgIfUnchanged(org.ID, fresh, "new", "admin", false, time.Time{})
	if err != nil || !ok {
		t.Fatalf("insert = (%v, %v)", ok, err)
	}
	if restored, err := ks.RestoreOrgIfUnchanged(org.ID, fresh, fat, nil); err != nil || !restored {
		t.Fatalf("undo of an insert = (%v, %v)", restored, err)
	}
	if has, _ := ks.HasOrg(org.ID, fresh); has {
		t.Error("the undone insert survived")
	}
}
