package service

import (
	"context"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// newProviderServiceForProfile is newProviderServiceWithFakeSecrets on a deployment profile.
func newProviderServiceForProfile(t *testing.T, secrets *testutil.FakeSecretStore, profile capability.Profile) *ProviderService {
	t.Helper()
	getKey := func(scope, provider string) (string, error) {
		v, _ := secrets.Get(scope, provider)
		return v, nil
	}
	factory := ai.NewProviderFactory(getKey, ai.NewCopilotAuth(), nil, nil)
	return NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets, nil, nil, WithProfile(profile))
}

// specIDsFor is the registry's display order filtered to a profile.
func specIDsFor(profile capability.Profile) []string {
	var ids []string
	for _, spec := range ai.Specs() {
		if slices.Contains(spec.Modes, profile) {
			ids = append(ids, spec.ID)
		}
	}
	return ids
}

// TestListProviders_SpecDrivenFields: every entry of /api/providers/list carries the spec's caps, modes,
// credential schema and pricing mode (old clients ignore them), and the list is the running profile's: the
// local provider, configured, is listed on the desktop and absent in the cloud profile — the profile decides,
// not the process-global egress gate (which stays inside the provider).
func TestListProviders_SpecDrivenFields(t *testing.T) {
	setLocalModelsEgress(t, egress.AllowLoopback)
	secrets := testutil.NewFakeSecretStore()
	if err := secrets.Save("u1", ai.LocalProviderID, `{"baseUrl":"http://127.0.0.1:11434","contextWindow":16384}`); err != nil {
		t.Fatal(err)
	}
	liveModelsBudget = 200 * time.Millisecond
	t.Cleanup(func() { liveModelsBudget = 8 * time.Second })

	desktop := newProviderServiceForProfile(t, secrets, capability.Desktop)
	providers, err := desktop.ListProviders(context.Background(), "u1", "")
	if err != nil {
		t.Fatalf("ListProviders: %v", err)
	}
	for _, p := range providers {
		spec, ok := ai.SpecOf(p.ID)
		if !ok {
			t.Errorf("%s: listed but not a registered spec", p.ID)
			continue
		}
		if !slices.Equal(p.Caps, spec.Caps.Names()) {
			t.Errorf("%s: caps = %v, want %v", p.ID, p.Caps, spec.Caps.Names())
		}
		if len(p.Modes) != len(spec.Modes) || p.Modes[0] != string(spec.Modes[0]) {
			t.Errorf("%s: modes = %v, want %v", p.ID, p.Modes, spec.Modes)
		}
		if p.Credential == nil || p.Credential.Kind != string(spec.Credential.Kind) || len(p.Credential.Fields) != len(spec.Credential.Fields) {
			t.Errorf("%s: credential = %+v, want kind %s with %d fields", p.ID, p.Credential, spec.Credential.Kind, len(spec.Credential.Fields))
		}
		if p.Pricing != string(spec.Pricing) || p.Color != spec.Color || p.Icon != spec.Icon {
			t.Errorf("%s: pricing/color/icon = %q/%q/%q, want the spec's", p.ID, p.Pricing, p.Color, p.Icon)
		}
		if p.AuthType != string(spec.AuthType) {
			t.Errorf("%s: authType = %q (unchanged wire enum expected)", p.ID, p.AuthType)
		}
	}
	local := findProvider(providers, ai.LocalProviderID)
	if local == nil || !local.Configured {
		t.Fatalf("desktop: the configured local provider is missing: %+v", local)
	}
	if local.Credential == nil || local.Credential.Kind != string(ai.CredentialDocument) || !local.Configurable || local.Endpoints == nil || local.Endpoints.Test == "" {
		t.Errorf("local: credential %+v endpoints %+v configurable %v, want a configurable document provider", local.Credential, local.Endpoints, local.Configurable)
	}

	cloud := newProviderServiceForProfile(t, secrets, capability.Cloud)
	providers, err = cloud.ListProviders(context.Background(), "u1", "")
	if err != nil {
		t.Fatalf("ListProviders(cloud): %v", err)
	}
	if findProvider(providers, ai.LocalProviderID) != nil {
		t.Error("cloud: the local provider is listed although its Modes exclude the profile")
	}
	if got, want := len(providers), len(specIDsFor(capability.Cloud)); got != want {
		t.Errorf("cloud lists %d providers, want %d", got, want)
	}
}

// TestProviderCatalog_ListsUnconfiguredAndFiltersByProfile: the catalogue is every registered provider of the
// running profile in display order, configured or not — the generic settings panel renders from it — with the
// credential schema, the routes that manage the credential and whether the caller configured it.
func TestProviderCatalog_ListsUnconfiguredAndFiltersByProfile(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	if err := secrets.Save("u1", "openai", "sk-u1"); err != nil {
		t.Fatal(err)
	}
	desktop := newProviderServiceForProfile(t, secrets, capability.Desktop)
	catalog, err := desktop.ProviderCatalog(context.Background(), "u1", "")
	if err != nil {
		t.Fatalf("ProviderCatalog: %v", err)
	}
	var ids []string
	for _, e := range catalog {
		ids = append(ids, e.ID)
	}
	if want := specIDsFor(capability.Desktop); !slices.Equal(ids, want) {
		t.Fatalf("desktop catalogue = %v, want %v (display order, every provider)", ids, want)
	}
	byID := map[string]models.ProviderCatalogEntry{}
	for _, e := range catalog {
		byID[e.ID] = e
	}

	// An unconfigured api_key provider is listed, unconfigured, with the key form and the generic routes.
	claude := byID["claude"]
	if claude.Configured || claude.Credential.Kind != string(ai.CredentialAPIKey) || len(claude.Credential.Fields) != 1 {
		t.Errorf("claude = %+v, want unconfigured with the one-field api_key schema", claude)
	}
	// The field is named as /api/keys/save reads it ({provider, key}), so a client may post the fields by name.
	if f := claude.Credential.Fields[0]; f.Name != "key" || f.Type != string(ai.FieldSecret) || !f.Required || f.Label == "" {
		t.Errorf("claude key field = %+v", f)
	}
	if claude.Endpoints.Save != "/api/keys/save" || claude.Endpoints.Test != "/api/providers/test" || claude.Endpoints.Start != "" {
		t.Errorf("claude endpoints = %+v", claude.Endpoints)
	}
	if !claude.Configurable || claude.Pricing != string(ai.PricingCatalog) || claude.AuthType != string(ai.AuthAPIKey) {
		t.Errorf("claude configurable/pricing/authType = %v/%q/%q", claude.Configurable, claude.Pricing, claude.AuthType)
	}
	if byID["openai"].Configured != true {
		t.Error("openai: the caller's stored key must read as configured")
	}
	// The device-flow providers name their own routes.
	gh := byID["github-models"]
	if gh.Endpoints.Start != "/api/providers/github/start" || gh.Endpoints.Poll != "/api/providers/github/poll" || gh.Endpoints.Revoke != "/api/providers/github/revoke" || gh.Endpoints.User != "/api/providers/github/user" || gh.Endpoints.Save != "" {
		t.Errorf("github-models endpoints = %+v", gh.Endpoints)
	}
	if cp := byID["copilot"]; cp.Endpoints.Start != "/api/providers/copilot/start" || cp.Endpoints.Test != "/api/providers/test" {
		t.Errorf("copilot endpoints = %+v", cp.Endpoints)
	}
	// A document provider is managed by its own panel: the test route only, and its select options are labelled.
	az := byID[ai.AzureFoundryProviderID]
	if az.Credential.Kind != string(ai.CredentialDocument) || az.Endpoints.Save != "" || az.Endpoints.Test != "/api/providers/test" || az.Credential.HelpURL == "" {
		t.Errorf("azure-foundry = %+v", az)
	}
	var sel *models.CredentialField
	for i := range az.Credential.Fields {
		if az.Credential.Fields[i].Type == string(ai.FieldSelect) {
			sel = &az.Credential.Fields[i]
		}
	}
	if sel == nil || len(sel.Options) == 0 || sel.Options[0].Value == "" || sel.Options[0].Label == "" {
		t.Errorf("azure select field = %+v, want value/label options", sel)
	}
	if local := byID[ai.LocalProviderID]; local.Configured || !local.Configurable {
		t.Errorf("local = %+v, want listed unconfigured and configurable on the desktop", local)
	}
	for _, e := range catalog {
		if len(e.Modes) == 0 || len(e.Caps) == 0 || e.Name == "" || e.Pricing == "" {
			t.Errorf("%s: incomplete entry %+v", e.ID, e)
		}
	}

	cloud := newProviderServiceForProfile(t, secrets, capability.Cloud)
	catalog, err = cloud.ProviderCatalog(context.Background(), "u1", "")
	if err != nil {
		t.Fatalf("ProviderCatalog(cloud): %v", err)
	}
	for _, e := range catalog {
		if e.ID == ai.LocalProviderID {
			t.Error("cloud: the local provider is in the catalogue although its Modes exclude the profile")
		}
	}
	if got, want := len(catalog), len(specIDsFor(capability.Cloud)); got != want {
		t.Errorf("cloud catalogue has %d entries, want %d", got, want)
	}
}

// TestIsConfigured_DerivedFromKeySchema: "configured" is read from the spec's keystore keys, not a per-provider
// switch — a provider the registry only knows by its spec (a stub here) is configured when its Keys.Primary row
// exists, and an OAuth provider by either of its rows.
func TestIsConfigured_DerivedFromKeySchema(t *testing.T) {
	ai.RegisterProvider("keyed-stub", func(string) ai.Provider { return &testutil.FakeProvider{} })
	secrets := testutil.NewFakeSecretStore()
	svc := newProviderServiceForProfile(t, secrets, capability.Cloud)
	if svc.isConfigured("u1", "keyed-stub") {
		t.Error("nothing stored: configured")
	}
	if err := secrets.Save("u1", "keyed-stub", "k"); err != nil {
		t.Fatal(err)
	}
	if !svc.isConfigured("u1", "keyed-stub") {
		t.Error("Keys.Primary stored: not configured")
	}
	if svc.isConfigured("u1", "nope") {
		t.Error("an unknown id reads as configured")
	}
	cp, _ := ai.SpecOf("copilot")
	if err := secrets.Save("u2", cp.Keys.Primary, "ghp_pat"); err != nil {
		t.Fatal(err)
	}
	if !svc.isConfigured("u2", "copilot") {
		t.Error("copilot: the PAT row alone must count")
	}
	if svc.isOrgConfigured("org-1", "copilot") {
		t.Error("copilot: never org-shared")
	}
}

// TestTestProviderConnection_UnknownProviderCode: the connection test of an unknown id answers the 200 shape
// clients already handle, with the PROVIDER_UNKNOWN code — no provider is built, nothing dials.
func TestTestProviderConnection_UnknownProviderCode(t *testing.T) {
	svc := newProviderServiceForProfile(t, testutil.NewFakeSecretStore(), capability.Cloud)
	res, err := svc.TestProviderConnection(context.Background(), "u1", "nope")
	if err != nil {
		t.Fatalf("TestProviderConnection(nope): %v", err)
	}
	if res.Ok || res.Code != ProviderCodeUnknown || res.Error == "" {
		t.Errorf("result = %+v, want ok=false code=%s with a message", res, ProviderCodeUnknown)
	}
}
