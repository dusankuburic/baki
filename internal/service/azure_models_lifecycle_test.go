package service

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// A departed lender's credentials go family-wide — every resource of the
// organisation whose secret they supplied, in one delete — each key named
// for its own keys.save {op:"disable"} audit row; the registry rows stay and
// read needs_credentials; another admin's resource is kept.
func TestAzureModels_LenderDisableFamilyWide(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	providers := newProviderServiceWithFakeSecrets(t, e.secrets)
	providers.orgKeys = e.orgKeys
	providers.azure = e.svc
	lender := AzureScope{Scope: "lender", OrgID: "org-1"}
	other := AzureScope{Scope: "other", OrgID: "org-1"}
	_ = e.orgKeys.SaveOrg("org-1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"c1"`), "lender")
	a := e.mustConn(t, lender, "a", azmKeyConfig("ka"))
	b := e.mustConn(t, lender, "b", azmEntraConfig("sb"))
	kept := e.mustConn(t, other, "kept", azmKeyConfig("kc"))
	e.mustConn(t, AzureScope{Scope: "lender", OrgID: "org-2"}, "elsewhere", azmKeyConfig("kd"))

	keys, err := providers.DisableOrgAzureCredentialsSavedBy("org-1", "lender")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"azure-foundry", "azure-foundry/" + a.ID, "azure-foundry/" + b.ID}
	if strings.Join(keys, ",") != strings.Join(sortedCopy(want), ",") {
		t.Errorf("disabled keys = %v, want %v", keys, sortedCopy(want))
	}
	list, err := e.svc.ListConnections(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]string{}
	for _, c := range list.Connections {
		statuses[c.ID] = c.Status
	}
	if statuses[a.ID] != AzureStatusNeedsCredentials || statuses[b.ID] != AzureStatusNeedsCredentials || statuses["default"] != AzureStatusNeedsCredentials || statuses[kept.ID] != AzureStatusReady {
		t.Errorf("statuses after the disable = %v", statuses)
	}
	if again, _ := providers.DisableOrgAzureFoundryConfigSavedBy("org-1", "lender"); again {
		t.Error("a second disable found more")
	}
	// Account erasure: every org the user lent to, each key named.
	refs, err := providers.DisableOrgAzureCredentialsLentBy("lender")
	if err != nil || len(refs) != 1 || refs[0].OrgID != "org-2" || !strings.HasPrefix(refs[0].Provider, "azure-foundry/") {
		t.Errorf("erasure refs = %+v, %v", refs, err)
	}
	if has, _ := e.orgKeys.HasOrg("org-1", "azure-foundry/"+kept.ID); !has {
		t.Error("another admin's credential was removed")
	}
	if disabled, err := providers.DisableOrgAzureFoundryConfigSavedBy("org-1", "other"); err != nil || !disabled {
		t.Errorf("the other lender's departure: %v %v", disabled, err)
	}
}

func sortedCopy(in []string) []string {
	out := append([]string(nil), in...)
	for i := range out {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}

// Organisation deletion and account erasure read every family credential
// before the cascade and release them after; the registry forgets the
// owner.
func TestAzureModels_ErasureAndOrgDeletionReleaseTheFamily(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	providers := newProviderServiceWithFakeSecrets(t, e.secrets)
	providers.orgKeys = e.orgKeys
	providers.azure = e.svc
	e.mustConn(t, azmUser, "a", azmEntraConfig("s1"))
	e.mustConn(t, azmUser, "b", azmKeyConfig("k2"))
	_, _ = e.svc.ListConnections(ctx, azmUser) // the import memo
	if !e.svc.importMemoFresh(e.svc.userOwner("u1")) {
		t.Fatal("no memo to forget")
	}
	docs := userAzureDocuments(e.secrets, "u1", nil)
	if len(docs) != 2 {
		t.Fatalf("family documents = %d, want 2", len(docs))
	}
	release := providers.PrepareAccountErasure("u1")
	release()
	if e.svc.importMemoFresh(e.svc.userOwner("u1")) {
		t.Error("the erased user's memo survived")
	}
	org := AzureScope{Scope: "admin", OrgID: "org-1"}
	e.mustConn(t, org, "o1", azmKeyConfig("k"))
	e.mustConn(t, org, "o2", azmKeyConfig("k"))
	if docs := orgAzureDocuments(e.orgKeys, "org-1"); len(docs) != 2 {
		t.Fatalf("org family documents = %d", len(docs))
	}
	releaseOrg := providers.PrepareOrgDeletion("org-1")
	releaseOrg()
	if e.svc.importMemoFresh(storageif.AzureOrgOwner("org-1")) {
		t.Error("the deleted org's memo survived")
	}
}

// The orphan sweep deletes family keys without a registry row once they are
// older than the grace period — at most hourly per owner, only after the
// import, never a key outside the id space.
func TestAzureModels_OrphanSweep(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	now := time.Now()
	e.svc.now = func() time.Time { return now }
	c := e.mustConn(t, azmUser, "live", azmKeyConfig("k"))
	_ = e.secrets.Save("u1", "azure-foundry/oldorphan1", azmLegacyDoc(""))
	e.secrets.SetUpdatedAt("u1", "azure-foundry/oldorphan1", now.Add(-2*time.Hour))
	_ = e.secrets.Save("u1", "azure-foundry/neworphan1", azmLegacyDoc(""))
	_ = e.secrets.Save("u1", "azure-foundry/NotAnId", azmLegacyDoc(""))
	e.secrets.SetUpdatedAt("u1", "azure-foundry/NotAnId", now.Add(-2*time.Hour))
	e.secrets.SetUpdatedAt("u1", "azure-foundry/"+c.ID, now.Add(-2*time.Hour))

	if _, err := e.svc.ListConnections(ctx, azmUser); err != nil {
		t.Fatal(err)
	}
	has := func(key string) bool { ok, _ := e.secrets.Has("u1", key); return ok }
	if has("azure-foundry/oldorphan1") {
		t.Error("an old orphan survived")
	}
	if !has("azure-foundry/neworphan1") || !has("azure-foundry/NotAnId") || !has("azure-foundry/"+c.ID) {
		t.Error("the sweep removed a key it must keep")
	}
	// Hourly: an orphan that ages meanwhile waits for the next hour.
	e.secrets.SetUpdatedAt("u1", "azure-foundry/neworphan1", now.Add(-2*time.Hour))
	_, _ = e.svc.ListConnections(ctx, azmUser)
	if !has("azure-foundry/neworphan1") {
		t.Error("swept twice within the hour")
	}
	now = now.Add(61 * time.Minute)
	_, _ = e.svc.ListConnections(ctx, azmUser)
	if has("azure-foundry/neworphan1") {
		t.Error("not swept after the hour")
	}

	// Before the import ran, the legacy document has no row by design.
	e2 := newAzureModelsEnv(t)
	e2.reg.FailOn("ImportedAt", errors.New("db down"))
	_ = e2.secrets.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"c"`))
	e2.secrets.SetUpdatedAt("u1", "azure-foundry", time.Now().Add(-48*time.Hour))
	_, _ = e2.svc.ListConnections(ctx, azmUser)
	if ok, _ := e2.secrets.Has("u1", "azure-foundry"); !ok {
		t.Error("the sweep removed a legacy document before its import")
	}

	// An organisation's orphans.
	org := AzureScope{Scope: "admin", OrgID: "org-1"}
	e.mustConn(t, org, "o", azmKeyConfig("k"))
	_ = e.orgKeys.SaveOrg("org-1", "azure-foundry/orgorphan1", azmLegacyDoc(""), "admin")
	e.orgKeys.SetOrgKeyUpdatedAt("org-1", "azure-foundry/orgorphan1", now.Add(-3*time.Hour))
	if _, err := e.svc.ListConnections(ctx, org); err != nil {
		t.Fatal(err)
	}
	if ok, _ := e.orgKeys.HasOrg("org-1", "azure-foundry/orgorphan1"); ok {
		t.Error("an org orphan survived")
	}
}

// The desktop rolls back a pending row an earlier process left behind — its
// keyring entry, then the row — and leaves this process's (a create in
// flight) and a young one (another process sharing the directory may be
// finishing it) alone.
func TestAzureModels_DesktopPendingRollback(t *testing.T) {
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) { d.PerUserOwners = false })
	ctx := context.Background()
	desk := storageif.AzureUserOwner("")
	e.reg.EnablePendingWrites("old-boot")
	if _, err := e.reg.CreateConnection(ctx, desk, storageif.AzureConnectionRecord{ID: "earlierrun", Name: "earlier", Pending: true}); err != nil {
		t.Fatal(err)
	}
	_ = e.secrets.Save("", "azure-foundry/earlierrun", azmLegacyDoc(""))
	e.reg.EnablePendingWrites("new-boot") // the app restarted
	if _, err := e.reg.CreateConnection(ctx, desk, storageif.AzureConnectionRecord{ID: "inflight00", Name: "inflight", Pending: true}); err != nil {
		t.Fatal(err)
	}
	list, err := e.svc.ListConnections(ctx, AzureScope{})
	if err != nil || len(list.Connections) != 0 {
		t.Fatalf("pending rows are never listed: %+v, %v", list.Connections, err)
	}
	// Young: kept, whoever wrote it.
	if _, err := e.reg.GetConnection(ctx, desk, "earlierrun"); err != nil {
		t.Errorf("a young pending row was rolled back: %v", err)
	}
	// Past the grace period: the earlier run's row goes, this run's stays.
	e.svc.now = func() time.Time { return time.Now().Add(azurePendingGrace + time.Minute) }
	if _, err := e.svc.ListConnections(ctx, AzureScope{}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.reg.GetConnection(ctx, desk, "earlierrun"); !errors.Is(err, storageif.ErrAzureConnectionNotFound) {
		t.Errorf("the earlier run's pending row: %v", err)
	}
	if ok, _ := e.secrets.Has("", "azure-foundry/earlierrun"); ok {
		t.Error("its keyring entry survived")
	}
	if _, err := e.reg.GetConnection(ctx, desk, "inflight00"); err != nil {
		t.Errorf("this run's pending row was rolled back: %v", err)
	}
}

// The data-subject export shows each resource's public configuration —
// never a secret — marks an unreadable one and leaves a missing one out.
func TestAzureModels_ExportConnectionConfigs(t *testing.T) {
	e := newAzureModelsEnv(t)
	a := e.mustConn(t, azmUser, "a", azmEntraConfig("super-secret"))
	_ = e.secrets.Save("u1", "azure-foundry/brokenbrok", `{"endpoint":`)
	got, err := e.svc.ExportConnectionConfigs(context.Background(), "u1", []string{a.ID, "brokenbrok", "missingxxx", "../evil"})
	if err != nil {
		t.Fatal(err)
	}
	if c := got[a.ID]; c.Endpoint != "https://"+azureTestHost || c.AuthMode != "entra" || c.TenantID != azmTenant || c.ClientID != azmClient || c.ProjectEndpoint == "" || c.Unreadable {
		t.Errorf("public config = %+v", c)
	}
	if !got["brokenbrok"].Unreadable {
		t.Errorf("unreadable = %+v", got["brokenbrok"])
	}
	if _, ok := got["missingxxx"]; ok {
		t.Error("a missing credential was exported")
	}
	if len(got) != 2 {
		t.Errorf("exported %d entries", len(got))
	}
	secrets := &failingSecrets{FakeSecretStore: testutil.NewFakeSecretStore(), getErr: map[string]error{"azure-foundry/" + a.ID: errors.New("db down")}}
	e.svc.secrets = secrets
	if _, err := e.svc.ExportConnectionConfigs(context.Background(), "u1", []string{a.ID}); err == nil {
		t.Error("a keystore failure must fail the export")
	}
}

// U1: the desktop rollback of a pending row an earlier run left leaves a
// READ-ONLY registry alone (a newer app version wrote the list file): the
// row cannot be removed there, and deleting its keyring entry first would
// strand the newer app's row without its secret. A writable file still
// rolls the same row back.
func TestAzureModels_DesktopRollbackSkipsAReadOnlyRegistry(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		version    int
		rolledBack bool
	}{{2, false}, {1, true}} {
		dir := t.TempDir()
		file := `{"version":` + strconv.Itoa(tc.version) + `,"revision":3,"connections":[` +
			`{"id":"earlierrun","name":"earlier","pending":true,"bootId":"an-earlier-boot",` +
			`"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-01T00:00:00Z"}],"models":[]}`
		if err := os.WriteFile(filepath.Join(dir, storage.AzureRegistryFileName), []byte(file), 0o600); err != nil {
			t.Fatal(err)
		}
		reg, err := storage.NewFileAzureRegistry(dir)
		if err != nil {
			t.Fatal(err)
		}
		secrets := testutil.NewFakeSecretStore()
		_ = secrets.Save("", "azure-foundry/earlierrun", azmLegacyDoc(""))
		svc := NewAzureModelService(AzureModelServiceDeps{Registry: reg, Secrets: secrets})
		svc.refresher = nil
		list, err := svc.ListConnections(ctx, AzureScope{})
		if err != nil {
			t.Fatalf("version %d: %v", tc.version, err)
		}
		if tc.version == 2 && list.RegistryState != string(storageif.AzureRegistryReadOnly) {
			t.Errorf("version 2: registryState = %q", list.RegistryState)
		}
		has, _ := secrets.Has("", "azure-foundry/earlierrun")
		_, rowErr := reg.GetConnection(ctx, storageif.AzureUserOwner(""), "earlierrun")
		if tc.rolledBack {
			if has || !errors.Is(rowErr, storageif.ErrAzureConnectionNotFound) {
				t.Errorf("writable file: keyring entry kept %v, row %v; want both rolled back", has, rowErr)
			}
			continue
		}
		if !has || rowErr != nil {
			t.Errorf("read-only file: keyring entry kept %v, row %v; want both left alone", has, rowErr)
		}
	}
}
