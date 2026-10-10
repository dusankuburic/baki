package service

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/cache"
)

// newOrgProviderService is newProviderServiceWithFakeSecrets with an
// organisation credential store wired into both the factory and the
// service, as di does in cloud mode.
func newOrgProviderService(t *testing.T, secrets *testutil.FakeSecretStore, orgKeys *testutil.FakeOrgSecretStore) *ProviderService {
	t.Helper()
	svc := newProviderServiceWithFakeSecrets(t, secrets)
	svc.factory.SetOrgKeyLookup(orgKeys.OrgKeyLookup)
	svc.orgKeys = orgKeys
	return svc
}

// G7: a provider the caller has not configured but their org has is listed
// as configured and Shared, from the org's document; a member's own document
// wins (not Shared); without an org id the org's credential is invisible.
func TestListProviders_OrgSharedCredential(t *testing.T) {
	configureAzureForTests(t)
	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()
	orgDoc := `{"endpoint":"https://` + azureTestHost + `","auth":{"mode":"api_key","key":"org-k"},"chatDeployment":"org-chat"}`
	ownDoc := `{"endpoint":"https://` + azureTestHost + `","auth":{"mode":"api_key","key":"own-k"},"chatDeployment":"own-chat"}`
	if err := orgKeys.SaveOrg("org-1", "azure-foundry", orgDoc, "admin"); err != nil {
		t.Fatal(err)
	}
	if err := secrets.Save("u-own", "azure-foundry", ownDoc); err != nil {
		t.Fatal(err)
	}
	svc := newOrgProviderService(t, secrets, orgKeys)

	providers, err := svc.ListProviders(context.Background(), "u-none", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	az := findProvider(providers, "azure-foundry")
	if az == nil || !az.Configured || !az.Shared {
		t.Fatalf("member without a document: azure-foundry = %+v, want configured and shared", az)
	}
	if az.DefaultModel != "org-chat" {
		t.Errorf("default model = %q, want the org document's chat deployment", az.DefaultModel)
	}
	for _, p := range providers {
		if p.ID != "azure-foundry" && p.Shared {
			t.Errorf("%s marked shared without an org credential", p.ID)
		}
	}

	providers, _ = svc.ListProviders(context.Background(), "u-own", "org-1")
	if az := findProvider(providers, "azure-foundry"); az == nil || !az.Configured || az.Shared || az.DefaultModel != "own-chat" {
		t.Errorf("member with their own document: %+v, want their own, not shared", az)
	}

	providers, _ = svc.ListProviders(context.Background(), "u-none", "")
	if az := findProvider(providers, "azure-foundry"); az == nil || az.Configured || az.Shared {
		t.Errorf("no org id: %+v, want unconfigured", az)
	}
	providers, _ = svc.ListProviders(context.Background(), "u-none", "org-2")
	if az := findProvider(providers, "azure-foundry"); az == nil || az.Configured {
		t.Errorf("another org: %+v, want unconfigured", az)
	}

	// Copilot is never shared, even if something wrote an org row for it.
	_ = orgKeys.SaveOrg("org-1", "copilot", "pat", "admin")
	providers, _ = svc.ListProviders(context.Background(), "u-none", "org-1")
	if cp := findProvider(providers, "copilot"); cp == nil || cp.Configured || cp.Shared {
		t.Errorf("copilot = %+v, want never shared", cp)
	}
}

// A shared listing is cached under the org scope, so it is shared by the
// org's members and dropped by InvalidateScope(ai.OrgScope(orgID)).
func TestDynamicModels_SharedListingCachedUnderOrgScope(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()
	_ = orgKeys.SaveOrg("org-1", "glm", "org-glm-key", "admin")
	svc := newOrgProviderService(t, secrets, orgKeys)

	ls, configured := svc.resolveListing("u-none", "org-1", "glm")
	if !configured || !ls.shared || ls.cacheScope != ai.OrgScope("org-1") {
		t.Fatalf("resolveListing = %+v, %v; want shared under the org scope", ls, configured)
	}
	if _, _, ok := svc.dynamicModels(context.Background(), ls, "glm"); !ok {
		t.Fatal("listing on the org credential failed")
	}
	key := modelListingKey(ai.OrgScope("org-1"), "glm")
	if _, cached := svc.modelCache[key]; !cached {
		t.Fatalf("listing not cached under %q: %v", key, keysOf(svc.modelCache))
	}
	if _, cached := svc.modelCache[modelListingKey("u-none", "glm")]; cached {
		t.Error("a shared listing must not be cached under the member's scope")
	}
	svc.InvalidateScope(ai.OrgScope("org-1"))
	if _, cached := svc.modelCache[key]; cached {
		t.Error("InvalidateScope(org scope) must drop the shared listing")
	}

	// A member with their own key lists — and caches — under their scope.
	_ = secrets.Save("u-own", "glm", "own-key")
	ls, _ = svc.resolveListing("u-own", "org-1", "glm")
	if ls.shared || ls.cacheScope != "u-own" {
		t.Errorf("member with own key: %+v, want personal scope", ls)
	}
}

func keysOf(m map[cache.Key]modelCacheEntry) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k.String())
	}
	return out
}

// Org config: Get never returns the secret; Save keeps the stored secret
// when the request omits it; a credential change releases the previous
// one's process state and invalidates the org scope; Delete forgets it.
func TestOrgAzureFoundryConfig_SaveMergeKeepsSecret_DeleteForgets(t *testing.T) {
	configureAzureForTests(t)
	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()
	svc := newOrgProviderService(t, secrets, orgKeys)

	// Nothing configured yet.
	pub, err := svc.GetOrgAzureFoundryConfig("org-1")
	if err != nil || pub.Configured || pub.Shared {
		t.Fatalf("empty: %+v, %v", pub, err)
	}

	cfg := ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k1"}, ChatDeployment: "chat1"}
	at1, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-1", "", cfg)
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	pub, err = svc.GetOrgAzureFoundryConfig("org-1")
	if err != nil || !pub.Configured || !pub.Shared || !pub.HasSecret || pub.UpdatedByID != "admin-1" || pub.UpdatedAt == "" {
		t.Fatalf("after save: %+v, %v", pub, err)
	}
	raw, _ := orgKeys.GetOrg("org-1", "azure-foundry")
	stored, _ := ai.ParseAzureFoundryConfig(raw)
	if stored.Auth.Key != "k1" {
		t.Fatalf("stored key = %q", stored.Auth.Key)
	}

	// Editing a deployment name without re-entering the key keeps it.
	edit := ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key"}, ChatDeployment: "chat2"}
	at2, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-2", at1, edit)
	if err != nil {
		t.Fatalf("edit: %v", err)
	}
	raw, _ = orgKeys.GetOrg("org-1", "azure-foundry")
	stored, _ = ai.ParseAzureFoundryConfig(raw)
	if stored.Auth.Key != "k1" || stored.ChatDeployment != "chat2" {
		t.Errorf("merge: key=%q chat=%q, want the kept key and the new deployment", stored.Auth.Key, stored.ChatDeployment)
	}
	if pub, _ = svc.GetOrgAzureFoundryConfig("org-1"); pub.UpdatedByID != "admin-2" {
		t.Errorf("updatedBy = %q, want the last admin", pub.UpdatedByID)
	}

	// Validation failures never overwrite the stored document.
	var cfgErr *ai.AzureConfigError
	bad := ai.AzureFoundryConfig{Endpoint: "https://evil.example", Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k9"}}
	if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-2", at2, bad); !errors.As(err, &cfgErr) || cfgErr.Code != ai.AzureCfgErrHostNotAllowed {
		t.Errorf("host not allowed: %v", err)
	}
	raw, _ = orgKeys.GetOrg("org-1", "azure-foundry")
	if stored, _ = ai.ParseAzureFoundryConfig(raw); stored.Endpoint != "https://"+azureTestHost {
		t.Errorf("stored document changed by a rejected save: %+v", stored)
	}

	// A save invalidates the org's cached listings (every member's).
	svc.modelCache[modelListingKey(ai.OrgScope("org-1"), "glm")] = modelCacheEntry{}
	rotated := ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k2"}, ChatDeployment: "chat2"}
	if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-2", at2, rotated); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	if _, cached := svc.modelCache[modelListingKey(ai.OrgScope("org-1"), "glm")]; cached {
		t.Error("save must invalidate the org scope's listings")
	}

	// Another org sees nothing; delete forgets.
	if pub, _ := svc.GetOrgAzureFoundryConfig("org-2"); pub.Configured {
		t.Error("configuration leaked across orgs")
	}
	svc.modelCache[modelListingKey(ai.OrgScope("org-1"), "glm")] = modelCacheEntry{}
	if err := svc.DeleteOrgAzureFoundryConfig("org-1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if has, _ := orgKeys.HasOrg("org-1", "azure-foundry"); has {
		t.Error("delete left the row")
	}
	if _, cached := svc.modelCache[modelListingKey(ai.OrgScope("org-1"), "glm")]; cached {
		t.Error("delete must invalidate the org scope's listings")
	}
	if pub, _ := svc.GetOrgAzureFoundryConfig("org-1"); pub.Configured || pub.Shared {
		t.Errorf("after delete: %+v", pub)
	}
	if err := svc.DeleteOrgAzureFoundryConfig("org-1"); err != nil {
		t.Errorf("deleting a missing document must be a no-op, got %v", err)
	}
}

// Without an org store every org method reports ErrOrgKeyStoreUnavailable
// and ListProviders ignores the org.
func TestOrgAzureFoundryConfig_NoOrgStore(t *testing.T) {
	svc := newProviderServiceWithFakeSecrets(t, testutil.NewFakeSecretStore())
	if svc.OrgKeysAvailable() {
		t.Fatal("no org store expected")
	}
	if _, err := svc.GetOrgAzureFoundryConfig("org-1"); !errors.Is(err, ErrOrgKeyStoreUnavailable) {
		t.Errorf("get: %v", err)
	}
	if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "a", "", ai.AzureFoundryConfig{}); !errors.Is(err, ErrOrgKeyStoreUnavailable) {
		t.Errorf("save: %v", err)
	}
	if err := svc.DeleteOrgAzureFoundryConfig("org-1"); !errors.Is(err, ErrOrgKeyStoreUnavailable) {
		t.Errorf("delete: %v", err)
	}
	if _, err := svc.TestOrgProviderConnection(context.Background(), "u1", "org-1", "azure-foundry"); !errors.Is(err, ErrOrgKeyStoreUnavailable) {
		t.Errorf("test: %v", err)
	}
	providers, err := svc.ListProviders(context.Background(), "u1", "org-1")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range providers {
		if p.Shared {
			t.Errorf("%s shared without an org store", p.ID)
		}
	}
}

// The org connection test runs on the org's credential only: an admin with
// their own key but no org document gets "not configured", not a test of
// their own key.
func TestTestOrgProviderConnection_OrgCredentialOnly(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()
	_ = secrets.Save("admin", "azure-foundry", azureTestDoc)
	svc := newOrgProviderService(t, secrets, orgKeys)

	res, err := svc.TestOrgProviderConnection(context.Background(), "admin", "org-1", "azure-foundry")
	if err != nil {
		t.Fatal(err)
	}
	if res.Ok || res.Error == "" {
		t.Errorf("result = %+v, want a failure naming the missing org credential", res)
	}
}
