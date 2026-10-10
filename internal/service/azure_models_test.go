package service

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// ---- Test environment for the Azure AI Foundry model registry ----

// azmEnv is an AzureModelService over in-memory fakes: the registry, the
// personal and org keystores, an organisation directory and a knowledge
// corpus counter. Azure itself is never reached: the endpoint host is
// azureTestHost (allow-listed, unresolvable) and the probe seams fail the
// test when a test did not expect a network call.
type azmEnv struct {
	svc     *AzureModelService
	reg     *testutil.FakeAzureRegistry
	secrets *testutil.FakeSecretStore
	orgKeys *testutil.FakeOrgSecretStore
	orgs    *azmOrgDir
	corpus  *azmCorpus
	factory *ai.ProviderFactory
}

func newAzureModelsEnv(t *testing.T, mutate ...func(*AzureModelServiceDeps)) *azmEnv {
	t.Helper()
	configureAzureForTests(t)
	e := &azmEnv{
		reg:     testutil.NewFakeAzureRegistry(),
		secrets: testutil.NewFakeSecretStore(),
		orgKeys: testutil.NewFakeOrgSecretStore(),
		orgs:    &azmOrgDir{orgs: map[string]*storageif.Organisation{}},
		corpus:  &azmCorpus{counts: map[string]int{}},
	}
	e.factory = ai.NewProviderFactory(func(scope, provider string) (string, error) {
		v, _ := e.secrets.Get(scope, provider)
		return v, nil
	}, nil, nil, nil)
	e.factory.SetOrgKeyLookup(e.orgKeys.OrgKeyLookup)
	deps := AzureModelServiceDeps{
		Registry: e.reg, Secrets: e.secrets, OrgKeys: e.orgKeys, Factory: e.factory,
		Orgs: e.orgs, Corpus: e.corpus, PerUserOwners: true,
	}
	for _, m := range mutate {
		m(&deps)
	}
	if deps.KeyFamilies == nil {
		// As provideKeyStore wires the database keystore: a keystore that lists its key families is the lister too.
		deps.KeyFamilies, _ = deps.Secrets.(storageif.SecretFamilyLister)
	}
	e.svc = NewAzureModelService(deps)
	noNetwork := func(what string) error {
		t.Errorf("unexpected %s: tests stub the Azure calls they expect", what)
		return errors.New("no network in tests")
	}
	e.svc.probeResource = func(context.Context, string) error { return noNetwork("resource probe") }
	e.svc.probeModel = func(context.Context, string, ai.AzureRegisteredModel) error { return noNetwork("model probe") }
	e.svc.discover = func(context.Context, string, []ai.AzureRegisteredModel) (ai.AzureDiscovery, error) {
		return ai.AzureDiscovery{}, noNetwork("discovery")
	}
	// No background listing refresh: its jobs read the keystore when they
	// run, which would race the tests that swap or fail the keystore under
	// a finished read. The refresh tests install their own refresher.
	e.svc.refresher = nil
	return e
}

// azmOrgDir is an in-memory organisation directory.
type azmOrgDir struct {
	mu   sync.Mutex
	orgs map[string]*storageif.Organisation
	err  error
}

func (d *azmOrgDir) add(id, name string, members map[string]auth.Role) {
	d.mu.Lock()
	defer d.mu.Unlock()
	o := &storageif.Organisation{ID: id, Name: name}
	for uid, role := range members {
		o.Members = append(o.Members, storageif.OrgMember{UserID: uid, Role: role})
	}
	d.orgs[id] = o
}

func (d *azmOrgDir) ListOrgsForUser(_ context.Context, userID string) ([]*storageif.Organisation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return nil, d.err
	}
	var out []*storageif.Organisation
	for _, o := range d.orgs {
		for _, m := range o.Members {
			if m.UserID == userID {
				out = append(out, o)
				break
			}
		}
	}
	return out, nil
}

func (d *azmOrgDir) OrgMemberRole(_ context.Context, orgID, userID string) (auth.Role, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err != nil {
		return "", false, d.err
	}
	o, ok := d.orgs[orgID]
	if !ok {
		return "", false, storageif.ErrNotFound
	}
	for _, m := range o.Members {
		if m.UserID == userID {
			return m.Role, true, nil
		}
	}
	return "", false, nil
}

func (d *azmOrgDir) LoadOrg(_ context.Context, id string) (*storageif.Organisation, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if o, ok := d.orgs[id]; ok {
		return o, nil
	}
	return nil, storageif.ErrNotFound
}

// azmCorpus counts knowledge chunks per organisation.
type azmCorpus struct {
	mu     sync.Mutex
	counts map[string]int
}

func (c *azmCorpus) set(orgID string, n int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.counts[orgID] = n
}

func (c *azmCorpus) CountKnowledgeChunks(_ context.Context, orgID string) (int, int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.counts[orgID], c.counts[orgID], nil
}

const (
	azmTenant = "11111111-2222-3333-4444-555555555555"
	azmClient = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// azmKeyConfig is an API-key resource on the test host.
func azmKeyConfig(key string) AzureConnectionConfigInput {
	return AzureConnectionConfigInput{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: ai.AzureAuthModeAPIKey, Key: key}}
}

// azmEntraConfig is an Entra ID resource with a project endpoint on the
// test host.
func azmEntraConfig(secret string) AzureConnectionConfigInput {
	return AzureConnectionConfigInput{
		Endpoint:        "https://" + azureTestHost,
		ProjectEndpoint: "https://" + azureTestHost + "/api/projects/p1",
		Auth:            ai.AzureFoundryAuth{Mode: ai.AzureAuthModeEntra, TenantID: azmTenant, ClientID: azmClient, ClientSecret: secret},
	}
}

// azmLegacyDoc is a legacy single document on the test host.
func azmLegacyDoc(extra string) string {
	return `{"endpoint":"https://` + azureTestHost + `","auth":{"mode":"api_key","key":"legacy-key"}` + extra + `}`
}

func (e *azmEnv) mustConn(t *testing.T, as AzureScope, name string, cfg AzureConnectionConfigInput) AzureConnectionView {
	t.Helper()
	v, err := e.svc.CreateConnection(context.Background(), as, AzureConnectionInput{Name: name, Config: cfg})
	if err != nil {
		t.Fatalf("create resource %q: %v", name, err)
	}
	return v
}

func (e *azmEnv) mustModel(t *testing.T, as AzureScope, in AzureModelInput) AzureModelView {
	t.Helper()
	v, err := e.svc.CreateModel(context.Background(), as, in)
	if err != nil {
		t.Fatalf("create model %q: %v", in.Deployment, err)
	}
	return v
}

// azmCode is err's client code (AzureRegistryHTTPError), "" when unmapped.
func azmCode(err error) string {
	_, code, _, ok := AzureRegistryHTTPError(err)
	if !ok {
		return ""
	}
	return code
}

func azmDetails(err error) map[string]any {
	_, _, details, _ := AzureRegistryHTTPError(err)
	return details
}

var azmUser = AzureScope{Scope: "u1"}

// ---- Resources ----

func TestAzureModels_ConnectionLifecycle(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	// The key is a value no generated id, name or timestamp in the view can contain: "k1" matched a random id now and
	// then, failing the leak check below.
	const key = "Secret-Key-Sentinel"
	c := e.mustConn(t, azmUser, "  Contoso   prod ", azmKeyConfig(key))
	if len(c.ID) != 10 || !ai.ValidAzureConnectionID(c.ID) || c.ID == ai.AzureDefaultConnectionID {
		t.Fatalf("id = %q, want ten [a-z0-9]", c.ID)
	}
	if c.Name != "Contoso prod" || c.Legacy || c.Shared || c.Status != AzureStatusReady || c.ModelCount != 0 {
		t.Errorf("view = %+v", c)
	}
	if c.Config.Endpoint != "https://"+azureTestHost || c.Config.AuthMode != ai.AzureAuthModeAPIKey || !c.Config.HasSecret || c.CanDiscover || c.SupportsAgents || c.SuggestDiscovery {
		t.Errorf("config view = %+v (discover %v)", c.Config, c.CanDiscover)
	}
	raw, _ := e.secrets.Get("u1", "azure-foundry/"+c.ID)
	stored, err := ai.ParseAzureFoundryConfig(raw)
	if err != nil || stored.Auth.Key != key || stored.ChatDeployment != "" || len(stored.Deployments) != 0 {
		t.Fatalf("stored credential = %+v (%v)", stored, err)
	}
	if b, _ := json.Marshal(c); strings.Contains(string(b), key) {
		t.Errorf("the view leaks the secret: %s", b)
	}

	list, err := e.svc.ListConnections(ctx, azmUser)
	if err != nil || len(list.Connections) != 1 || list.RegistryState != "ok" || list.Limits.Connections != 10 || list.Limits.Models != 50 || list.KnowledgeEmbeddingEnabled {
		t.Fatalf("listing = %+v, %v", list, err)
	}
	// The endpoint's first label names a resource created without a name.
	named := e.mustConn(t, azmUser, "", AzureConnectionConfigInput{Endpoint: "https://unit-test.invalid", Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k2"}})
	if named.Name != "unit-test" {
		t.Errorf("default name = %q, want the host's first label", named.Name)
	}

	// Rename with compare-and-swap: the current token works, a stale one is
	// a conflict.
	newName := "Contoso"
	renamed, err := e.svc.UpdateConnection(ctx, azmUser, c.ID, AzureConnectionPatch{Name: &newName, UpdatedAt: c.UpdatedAt})
	if err != nil || renamed.Name != "Contoso" || renamed.UpdatedAt == c.UpdatedAt {
		t.Fatalf("rename = %+v, %v", renamed, err)
	}
	if _, err := e.svc.UpdateConnection(ctx, azmUser, c.ID, AzureConnectionPatch{Name: &newName, UpdatedAt: c.UpdatedAt}); azmCode(err) != AzureCodeConflict {
		t.Errorf("stale token: %v (%s), want CONFLICT", err, azmCode(err))
	}
	dup := "UNIT-TEST"
	if _, err := e.svc.UpdateConnection(ctx, azmUser, c.ID, AzureConnectionPatch{Name: &dup}); azmCode(err) != AzureCodeDuplicateConnectionName {
		t.Errorf("rename onto another's name: %v, want DUPLICATE_CONNECTION_NAME", err)
	}
	dismiss := true
	if v, err := e.svc.UpdateConnection(ctx, azmUser, c.ID, AzureConnectionPatch{DismissDiscovery: &dismiss}); err != nil || v.UpdatedAt != renamed.UpdatedAt {
		t.Errorf("dismissal = %+v, %v; the token must not move", v, err)
	}

	// Delete: refused while it has models, then with them.
	m := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "gpt-4o"})
	if got, err := e.svc.GetConnection(ctx, azmUser, c.ID); err != nil || got.ModelCount != 1 {
		t.Errorf("model count = %+v, %v", got, err)
	}
	_, err = e.svc.DeleteConnection(ctx, azmUser, c.ID, AzureDeleteOptions{})
	if azmCode(err) != AzureCodeConnectionInUse || azmDetails(err)["modelCount"] != 1 {
		t.Fatalf("delete in use: %v %v", err, azmDetails(err))
	}
	res, err := e.svc.DeleteConnection(ctx, azmUser, c.ID, AzureDeleteOptions{Cascade: true})
	if err != nil || res.Status != "ok" || res.DeletedModels != 1 || res.ClearedKnowledgeEmbedding {
		t.Fatalf("delete = %+v, %v", res, err)
	}
	if has, _ := e.secrets.Has("u1", "azure-foundry/"+c.ID); has {
		t.Error("the deleted resource's credential is still stored")
	}
	if _, err := e.svc.GetModel(ctx, azmUser, m.ID); !errors.Is(err, storageif.ErrAzureModelNotFound) {
		t.Errorf("model after cascade: %v", err)
	}
	if _, err := e.svc.GetConnection(ctx, azmUser, c.ID); azmCode(err) != AzureCodeConnectionNotFound {
		t.Errorf("deleted resource: %v, want CONNECTION_NOT_FOUND", err)
	}
	// Another user sees none of it.
	if other, _ := e.svc.ListConnections(ctx, AzureScope{Scope: "u2"}); len(other.Connections) != 0 {
		t.Errorf("u2 sees %d resources", len(other.Connections))
	}
}

func TestAzureModels_ResourceValidation(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	e.mustConn(t, azmUser, "taken", azmKeyConfig("k"))
	cases := []struct {
		name  string
		in    AzureConnectionInput
		code  string
		field string
	}{
		{"bidi control in the name", AzureConnectionInput{Name: "evil\u202egnp", Config: azmKeyConfig("k")}, AzureCodeInvalidConnectionName, ""},
		{"zero-width in the name", AzureConnectionInput{Name: "a\u200bb", Config: azmKeyConfig("k")}, AzureCodeInvalidConnectionName, ""},
		{"NUL in the name", AzureConnectionInput{Name: "a\x00b", Config: azmKeyConfig("k")}, AzureCodeInvalidConnectionName, ""},
		{"65 characters", AzureConnectionInput{Name: strings.Repeat("a", 65), Config: azmKeyConfig("k")}, AzureCodeInvalidConnectionName, ""},
		{"duplicate name, any case", AzureConnectionInput{Name: "TAKEN", Config: azmKeyConfig("k")}, AzureCodeDuplicateConnectionName, ""},
		{"host not allowed", AzureConnectionInput{Name: "x", Config: AzureConnectionConfigInput{Endpoint: "https://evil.example", Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k"}}}, ai.AzureCfgErrHostNotAllowed, ""},
		{"missing key", AzureConnectionInput{Name: "x", Config: azmKeyConfig("")}, ai.AzureCfgErrMissingSecret, "key"},
		{"bad tenant", AzureConnectionInput{Name: "x", Config: func() AzureConnectionConfigInput { c := azmEntraConfig("s"); c.Auth.TenantID = "common"; return c }()}, ai.AzureCfgErrInvalidAuth, "tenantId"},
		{"bad client", AzureConnectionInput{Name: "x", Config: func() AzureConnectionConfigInput { c := azmEntraConfig("s"); c.Auth.ClientID = "nope"; return c }()}, ai.AzureCfgErrInvalidAuth, "clientId"},
		{"missing client secret", AzureConnectionInput{Name: "x", Config: azmEntraConfig("")}, ai.AzureCfgErrMissingSecret, "clientSecret"},
		{"unknown mode", AzureConnectionInput{Name: "x", Config: AzureConnectionConfigInput{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "magic"}}}, ai.AzureCfgErrInvalidAuth, "mode"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.svc.CreateConnection(ctx, azmUser, tc.in)
			if code := azmCode(err); code != tc.code {
				t.Fatalf("code = %q (%v), want %q", code, err, tc.code)
			}
			if tc.field != "" && azmDetails(err)["field"] != tc.field {
				t.Errorf("details = %v, want field %q", azmDetails(err), tc.field)
			}
		})
	}
	// 64 characters of a two-byte letter are 128 bytes: lengths count
	// characters.
	if _, err := e.svc.CreateConnection(ctx, azmUser, AzureConnectionInput{Name: strings.Repeat("é", 64), Config: azmKeyConfig("k")}); err != nil {
		t.Errorf("64 two-byte characters: %v", err)
	}
	// The limit.
	for i := 0; ; i++ {
		_, err := e.svc.CreateConnection(ctx, azmUser, AzureConnectionInput{Name: "r" + strings.Repeat("x", i), Config: azmKeyConfig("k")})
		if err != nil {
			if azmCode(err) != AzureCodeTooManyConnections || azmDetails(err)["limit"] != storageif.AzureMaxConnections {
				t.Fatalf("at the limit: %v %v", err, azmDetails(err))
			}
			break
		}
		if i > storageif.AzureMaxConnections {
			t.Fatal("no limit")
		}
	}
}

func TestAzureModels_ModelValidation(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	key := e.mustConn(t, azmUser, "key", azmKeyConfig("k"))
	chat := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: key.ID, Kind: "chat", Deployment: "gpt-4o"})
	if chat.DisplayName != "gpt-4o" || chat.Purpose != "general" || chat.ModelID != "m/"+chat.ID || chat.Target != "gpt-4o" || chat.Status != AzureStatusReady {
		t.Errorf("defaults = %+v", chat)
	}
	cases := []struct {
		name string
		in   AzureModelInput
		code string
	}{
		{"unknown kind", AzureModelInput{Kind: "vision", Deployment: "x"}, ai.AzureCfgErrInvalidKind},
		{"bad deployment", AzureModelInput{Kind: "chat", Deployment: "a/b"}, ai.AzureCfgErrInvalidDeploy},
		{"bidi display name", AzureModelInput{Kind: "chat", Deployment: "d1", DisplayName: "GPT\u2066x"}, AzureCodeInvalidDisplayName},
		{"zero-width display name", AzureModelInput{Kind: "chat", Deployment: "d1", DisplayName: "G\u200dPT"}, AzureCodeInvalidDisplayName},
		{"NUL display name", AzureModelInput{Kind: "chat", Deployment: "d1", DisplayName: "a\x00"}, AzureCodeInvalidDisplayName},
		{"long display name", AzureModelInput{Kind: "chat", Deployment: "d1", DisplayName: strings.Repeat("x", 65)}, AzureCodeInvalidDisplayName},
		{"unknown purpose", AzureModelInput{Kind: "chat", Deployment: "d1", Purpose: "poetry"}, AzureCodeInvalidPurpose},
		{"long custom purpose", AzureModelInput{Kind: "chat", Deployment: "d1", Purpose: "custom", PurposeCustom: strings.Repeat("p", 33)}, AzureCodeInvalidPurpose},
		{"empty custom purpose", AzureModelInput{Kind: "chat", Deployment: "d1", Purpose: "custom", PurposeCustom: "   "}, AzureCodeInvalidPurpose},
		{"bidi custom purpose", AzureModelInput{Kind: "chat", Deployment: "d1", Purpose: "custom", PurposeCustom: "doc\u202e"}, AzureCodeInvalidPurpose},
		{"long description", AzureModelInput{Kind: "chat", Deployment: "d1", Description: strings.Repeat("d", 201)}, AzureCodeInvalidDescription},
		{"description with a control", AzureModelInput{Kind: "chat", Deployment: "d1", Description: "a\x1bb"}, AzureCodeInvalidDescription},
		{"small window", AzureModelInput{Kind: "chat", Deployment: "d1", ContextWindow: 1000}, ai.AzureCfgErrInvalidContextWindow},
		{"embedding window", AzureModelInput{Kind: "embedding", Deployment: "e1", ContextWindow: 8192}, ai.AzureCfgErrInvalidContextWindow},
		{"small max output", AzureModelInput{Kind: "chat", Deployment: "d1", MaxOutputTokens: 100}, ai.AzureCfgErrInvalidMaxOutput},
		{"agent on an API key", AzureModelInput{Kind: "agent", Deployment: "helper"}, AzureCodeKindRequiresProject},
		{"claude named like a chat model", AzureModelInput{Kind: "claude", Deployment: "GPT-4O"}, AzureCodeDuplicateModel},
		{"user-scope default for members", AzureModelInput{Kind: "chat", Deployment: "d2", IsDefault: true}, AzureCodeInvalidDesignation},
		{"knowledge base where it is not offered", AzureModelInput{Kind: "embedding", Deployment: "e2", IsKnowledgeEmbedding: true}, AzureCodeInvalidDesignation},
		{"unknown resource", AzureModelInput{ConnectionID: "zzzzzzzzzz", Kind: "chat", Deployment: "d3"}, AzureCodeConnectionNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.in
			if in.ConnectionID == "" {
				in.ConnectionID = key.ID
			}
			_, err := e.svc.CreateModel(ctx, azmUser, in)
			if code := azmCode(err); code != tc.code {
				t.Fatalf("code = %q (%v), want %q", code, err, tc.code)
			}
			if tc.code == AzureCodeDuplicateModel && azmDetails(err)["existingModelId"] != chat.ID {
				t.Errorf("details = %v, want existingModelId %q", azmDetails(err), chat.ID)
			}
		})
	}
	// Normalisation: NFC, whitespace collapsed; a custom purpose naming a
	// preset (key or label) is stored as the preset; lengths count
	// characters.
	m := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: key.ID, Kind: "chat", Deployment: "docs",
		DisplayName: "  Docs\n\twriter ", Purpose: "custom", PurposeCustom: " fixes & DEBUGGING ",
		Description: strings.Repeat("é", 200)})
	if m.DisplayName != "Docs writer" || m.Purpose != "fixes" || m.PurposeCustom != "" || len([]rune(m.Description)) != 200 {
		t.Errorf("normalised = %+v", m)
	}
	nfc := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: key.ID, Kind: "chat", Deployment: "nfc", DisplayName: "Café", Purpose: "custom", PurposeCustom: "Release notes"})
	if nfc.DisplayName != "Café" || nfc.Purpose != "custom" || nfc.PurposeCustom != "Release notes" {
		t.Errorf("NFC / custom = %+v", nfc)
	}
	// An agent on an Entra ID + project resource is fine.
	entra := e.mustConn(t, azmUser, "entra", azmEntraConfig("s"))
	if !entra.CanDiscover || !entra.SupportsAgents || !entra.SuggestDiscovery {
		t.Errorf("entra view = %+v", entra)
	}
	agent := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: entra.ID, Kind: "agent", Deployment: "Helper bot"})
	if agent.Target != "agent:Helper bot" || agent.Status != AzureStatusReady {
		t.Errorf("agent = %+v", agent)
	}
}

func TestAzureModels_ModelLimit(t *testing.T) {
	e := newAzureModelsEnv(t)
	c := e.mustConn(t, azmUser, "c", azmKeyConfig("k"))
	for i := 0; i < storageif.AzureMaxModels; i++ {
		e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "d" + strings.Repeat("x", i)})
	}
	_, err := e.svc.CreateModel(context.Background(), azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "one-more"})
	if azmCode(err) != AzureCodeTooManyModels || azmDetails(err)["limit"] != storageif.AzureMaxModels {
		t.Fatalf("51st model: %v %v", err, azmDetails(err))
	}
}

// PUT /models: an absent field keeps its value, null clears an override,
// "" clears the description, a preset purpose clears the custom text, and a
// kind that cannot hold a designation drops it.
func TestAzureModels_UpdatePatchSemantics(t *testing.T) {
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) { d.KnowledgeEmbeddingEnabled = true })
	ctx := context.Background()
	c := e.mustConn(t, azmUser, "c", azmKeyConfig("k"))
	m := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "d1", DisplayName: "One",
		Purpose: "custom", PurposeCustom: "Release notes", Description: "desc", ContextWindow: 32000, MaxOutputTokens: 4096})

	var patch AzureModelPatch
	if err := json.Unmarshal([]byte(`{"displayName":"Uno","contextWindow":null}`), &patch); err != nil {
		t.Fatal(err)
	}
	got, err := e.svc.UpdateModel(ctx, azmUser, m.ID, patch)
	if err != nil {
		t.Fatal(err)
	}
	if got.DisplayName != "Uno" || got.ContextWindow != 0 || got.MaxOutputTokens != 4096 || got.Description != "desc" || got.PurposeCustom != "Release notes" || got.ID != m.ID {
		t.Errorf("after the patch = %+v", got)
	}
	if err := json.Unmarshal([]byte(`{"description":"","purpose":"review","maxOutputTokens":2048}`), &patch); err != nil {
		t.Fatal(err)
	}
	got, err = e.svc.UpdateModel(ctx, azmUser, m.ID, patch)
	if err != nil || got.Description != "" || got.Purpose != "review" || got.PurposeCustom != "" || got.MaxOutputTokens != 2048 {
		t.Errorf("clearing patch = %+v, %v", got, err)
	}
	// The id stays across a resource, kind and deployment change.
	c2 := e.mustConn(t, azmUser, "c2", azmKeyConfig("k2"))
	kind, dep := "claude", "claude-sonnet-4-6"
	got, err = e.svc.UpdateModel(ctx, azmUser, m.ID, AzureModelPatch{ConnectionID: &c2.ID, Kind: &kind, Deployment: &dep})
	if err != nil || got.ID != m.ID || got.ConnectionID != c2.ID || got.Target != "claude:claude-sonnet-4-6" || got.ConnectionName != "c2" {
		t.Errorf("moved = %+v, %v", got, err)
	}
	// A knowledge-base model turned into a chat model drops the designation.
	emb := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "embedding", Deployment: "text-embedding-3-small", IsKnowledgeEmbedding: true})
	if !emb.IsKnowledgeEmbedding {
		t.Fatalf("designation = %+v", emb)
	}
	chatKind := "chat"
	got, err = e.svc.UpdateModel(ctx, azmUser, emb.ID, AzureModelPatch{Kind: &chatKind})
	if err != nil || got.IsKnowledgeEmbedding || got.Kind != "chat" {
		t.Errorf("kind change = %+v, %v", got, err)
	}
	// A stale updatedAt is a conflict.
	if _, err := e.svc.UpdateModel(ctx, azmUser, m.ID, AzureModelPatch{DisplayName: &dep, UpdatedAt: m.UpdatedAt}); azmCode(err) != AzureCodeConflict {
		t.Errorf("stale model token: %v", err)
	}
	if _, err := e.svc.UpdateModel(ctx, azmUser, "zzzzzzzzzz", AzureModelPatch{}); azmCode(err) != AzureCodeModelNotFound {
		t.Errorf("unknown model: %v", err)
	}
}

func TestOptionalInt_JSON(t *testing.T) {
	var p AzureModelPatch
	if err := json.Unmarshal([]byte(`{}`), &p); err != nil || p.ContextWindow.Set || p.MaxOutputTokens.Set {
		t.Errorf("absent: %+v %v", p, err)
	}
	if err := json.Unmarshal([]byte(`{"contextWindow":null,"maxOutputTokens":4096}`), &p); err != nil {
		t.Fatal(err)
	}
	if !p.ContextWindow.Set || !p.ContextWindow.Null || !p.MaxOutputTokens.Set || p.MaxOutputTokens.Null || p.MaxOutputTokens.Value != 4096 {
		t.Errorf("null / value: %+v %+v", p.ContextWindow, p.MaxOutputTokens)
	}
	if p.ContextWindow.apply(9) != 0 || p.MaxOutputTokens.apply(9) != 4096 || (OptionalInt{}).apply(9) != 9 {
		t.Error("apply")
	}
	if err := json.Unmarshal([]byte(`{"contextWindow":"big"}`), &p); err == nil {
		t.Error("a string must not decode")
	}
	if b, _ := json.Marshal(SetOptionalInt(5)); string(b) != "5" {
		t.Errorf("marshal = %s", b)
	}
	if b, _ := json.Marshal(NullOptionalInt()); string(b) != "null" {
		t.Errorf("marshal null = %s", b)
	}
}

// Each resource carries its own stored secret, by the host rules; a draft
// test goes through exactly the same rules before any network call.
func TestAzureModels_SecretCarryPerResource(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	a := e.mustConn(t, azmUser, "a", azmKeyConfig("key-a"))
	b := e.mustConn(t, azmUser, "b", azmKeyConfig("key-b"))
	edit := azmKeyConfig("")
	edit.ProjectEndpoint = "https://" + azureTestHost + "/api/projects/p2"
	if _, err := e.svc.UpdateConnection(ctx, azmUser, a.ID, AzureConnectionPatch{Config: &edit}); err != nil {
		t.Fatalf("secret-less edit: %v", err)
	}
	for id, want := range map[string]string{a.ID: "key-a", b.ID: "key-b"} {
		raw, _ := e.secrets.Get("u1", "azure-foundry/"+id)
		if cfg, _ := ai.ParseAzureFoundryConfig(raw); cfg.Auth.Key != want {
			t.Errorf("resource %s key = %q, want %q", id, cfg.Auth.Key, want)
		}
	}
	moved := AzureConnectionConfigInput{Endpoint: "https://other.openai.azure.com", Auth: ai.AzureFoundryAuth{Mode: "api_key"}}
	_, err := e.svc.UpdateConnection(ctx, azmUser, a.ID, AzureConnectionPatch{Config: &moved})
	if azmCode(err) != ai.AzureCfgErrMissingSecret || azmDetails(err)["field"] != "key" || !strings.Contains(err.Error(), "endpoint changed") {
		t.Errorf("host change without the key: %v %v", err, azmDetails(err))
	}
	// Another principal never inherits the stored client secret.
	en := e.mustConn(t, azmUser, "entra", azmEntraConfig("old-secret"))
	other := azmEntraConfig("")
	other.Auth.ClientID = "bbbbbbbb-bbbb-cccc-dddd-eeeeeeeeeeee"
	_, err = e.svc.UpdateConnection(ctx, azmUser, en.ID, AzureConnectionPatch{Config: &other})
	if azmCode(err) != ai.AzureCfgErrMissingSecret || azmDetails(err)["field"] != "clientSecret" {
		t.Errorf("principal change: %v %v", err, azmDetails(err))
	}

	// Draft tests: the carried secret reaches the probe; a draft for another
	// host never does (400 before any call).
	var probed []string
	e.svc.probeResource = func(_ context.Context, raw string) error {
		probed = append(probed, raw)
		return nil
	}
	res, err := e.svc.Test(ctx, azmUser, AzureTestInput{ConnectionID: a.ID, Config: &edit})
	if err != nil || !res.OK || len(probed) != 1 || !strings.Contains(probed[0], `"key":"key-a"`) {
		t.Fatalf("draft test = %+v, %v, probed %v", res, err, probed)
	}
	if _, err := e.svc.Test(ctx, azmUser, AzureTestInput{ConnectionID: a.ID, Config: &moved}); azmCode(err) != ai.AzureCfgErrMissingSecret {
		t.Errorf("draft for another host: %v", err)
	}
	if _, err := e.svc.Test(ctx, azmUser, AzureTestInput{Config: &AzureConnectionConfigInput{Endpoint: "https://evil.example", Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k"}}}); azmCode(err) != ai.AzureCfgErrHostNotAllowed {
		t.Errorf("draft for a disallowed host: %v", err)
	}
	if len(probed) != 1 {
		t.Errorf("refused drafts reached the network: %v", probed)
	}
	// A draft never records a last test; the saved resource's test does.
	if v, _ := e.svc.GetConnection(ctx, azmUser, a.ID); v.LastTest != nil {
		t.Errorf("a draft recorded %+v", v.LastTest)
	}
	res, err = e.svc.Test(ctx, azmUser, AzureTestInput{ConnectionID: a.ID})
	if err != nil || !res.OK {
		t.Fatal(res, err)
	}
	if v, _ := e.svc.GetConnection(ctx, azmUser, a.ID); v.LastTest == nil || !v.LastTest.OK || v.LastTest.At == "" {
		t.Errorf("saved test not recorded: %+v", v.LastTest)
	}
}

// An organisation's resource never signs in with the Azure CLI: create,
// credential change and draft test are refused alike.
func TestAzureModels_OrgRefusesEntraCLI(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	org := AzureScope{Scope: "admin", OrgID: "org-1"}
	cli := AzureConnectionConfigInput{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: ai.AzureAuthModeEntraCLI}}
	_, err := e.svc.CreateConnection(ctx, org, AzureConnectionInput{Name: "cli", Config: cli})
	if azmCode(err) != ai.AzureCfgErrInvalidAuth || azmDetails(err)["field"] != "mode" {
		t.Fatalf("org create: %v %v", err, azmDetails(err))
	}
	c := e.mustConn(t, org, "keyed", azmKeyConfig("k"))
	if _, err := e.svc.UpdateConnection(ctx, org, c.ID, AzureConnectionPatch{Config: &cli}); azmCode(err) != ai.AzureCfgErrInvalidAuth {
		t.Errorf("org credential change: %v", err)
	}
	if _, err := e.svc.Test(ctx, org, AzureTestInput{Config: &cli}); azmCode(err) != ai.AzureCfgErrInvalidAuth {
		t.Errorf("org draft test: %v", err)
	}
	// Without the org keystore there are no org registries.
	noOrg := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) { d.OrgKeys = nil })
	if _, err := noOrg.svc.ListConnections(ctx, org); !errors.Is(err, ErrOrgKeyStoreUnavailable) || azmCode(err) != AzureCodeOrgKeystoreUnavailable {
		t.Errorf("no org keystore: %v", err)
	}
}

// Org credential writes are compare-and-swap on credentialUpdatedAt, and the
// admin who supplies the secret lends it.
func TestAzureModels_OrgCredentialCompareAndSwap(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	org := AzureScope{Scope: "admin-a", OrgID: "org-1"}
	c := e.mustConn(t, org, "shared", azmKeyConfig("lender-key"))
	if !c.Shared || c.OrgID != "org-1" || c.CredentialUpdatedAt == "" || c.SharedByID != "admin-a" {
		t.Fatalf("org view = %+v", c)
	}
	edit := azmKeyConfig("")
	adminB := AzureScope{Scope: "admin-b", OrgID: "org-1"}
	if _, err := e.svc.UpdateConnection(ctx, adminB, c.ID, AzureConnectionPatch{Config: &edit}); azmCode(err) != AzureCodeConflict {
		t.Errorf("credential change without its token: %v", err)
	}
	v, err := e.svc.UpdateConnection(ctx, adminB, c.ID, AzureConnectionPatch{Config: &edit, CredentialUpdatedAt: c.CredentialUpdatedAt})
	if err != nil || v.CredentialUpdatedAt == c.CredentialUpdatedAt {
		t.Fatalf("carrying edit = %+v, %v", v, err)
	}
	if meta, ok, _ := e.orgKeys.OrgKeyMeta("org-1", "azure-foundry/"+c.ID); !ok || meta.UpdatedBy != "admin-b" || meta.SecretSetBy != "admin-a" {
		t.Errorf("a carried secret keeps its lender: %+v", meta)
	}
	if _, err := e.svc.UpdateConnection(ctx, adminB, c.ID, AzureConnectionPatch{Config: &edit, CredentialUpdatedAt: c.CredentialUpdatedAt}); azmCode(err) != AzureCodeConflict {
		t.Errorf("stale credential token: %v", err)
	}
}

// The default resource's document keeps the legacy deployment fields it
// had, untouched, through a credential change; no other document ever
// carries them.
func TestAzureModels_DefaultKeepsLegacyFields(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	_ = e.secrets.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"chat1","deployments":["gpt-4o"],"embeddingDeployment":"emb1"`))
	list, err := e.svc.ListConnections(ctx, azmUser)
	if err != nil || len(list.Connections) != 1 || list.Connections[0].ID != "default" || !list.Connections[0].Legacy {
		t.Fatalf("import = %+v, %v", list, err)
	}
	cfg := azmKeyConfig("rotated")
	if _, err := e.svc.UpdateConnection(ctx, azmUser, "default", AzureConnectionPatch{Config: &cfg}); err != nil {
		t.Fatal(err)
	}
	raw, _ := e.secrets.Get("u1", "azure-foundry")
	doc, err := ai.ParseAzureFoundryConfig(raw)
	if err != nil || doc.Auth.Key != "rotated" || doc.ChatDeployment != "chat1" || strings.Join(doc.Deployments, ",") != "gpt-4o" || doc.EmbeddingDeployment != "emb1" {
		t.Errorf("default document = %+v (%v)", doc, err)
	}
	other := e.mustConn(t, azmUser, "other", azmKeyConfig("k"))
	raw, _ = e.secrets.Get("u1", "azure-foundry/"+other.ID)
	if strings.Contains(raw, "eployment") {
		t.Errorf("a non-default document carries deployment fields: %s", raw)
	}
}

// failingSecrets fails the keystore on demand.
type failingSecrets struct {
	*testutil.FakeSecretStore
	saveErr error
	getErr  map[string]error
}

func (s *failingSecrets) Save(scope, provider, key string) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	return s.FakeSecretStore.Save(scope, provider, key)
}

func (s *failingSecrets) Get(scope, provider string) (string, error) {
	if err := s.getErr[provider]; err != nil {
		return "", err
	}
	return s.FakeSecretStore.Get(scope, provider)
}

// The two-store write order: a registry row never exists without its
// secret — a failed keystore save writes no row, a failed row write leaves
// (at worst) an orphaned secret the sweep removes — and a deleted
// resource's secret goes only after the commit.
func TestAzureModels_WriteOrder(t *testing.T) {
	ctx := context.Background()
	t.Run("keystore failure writes no row", func(t *testing.T) {
		secrets := &failingSecrets{FakeSecretStore: testutil.NewFakeSecretStore(), saveErr: storage.ErrSecretStorageUnavailable}
		e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) { d.Secrets = secrets })
		_, err := e.svc.CreateConnection(ctx, azmUser, AzureConnectionInput{Name: "x", Config: azmKeyConfig("k")})
		if status, code, _, _ := AzureRegistryHTTPError(err); status != http.StatusServiceUnavailable || code != AzureCodeSecretStorageUnavailable {
			t.Fatalf("err = %v (%d %s)", err, status, code)
		}
		if conns, _ := e.reg.ListConnections(ctx, storageif.AzureUserOwner("u1")); len(conns) != 0 {
			t.Errorf("a row without its secret: %+v", conns)
		}
	})
	t.Run("row failure keeps no row and drops the secret", func(t *testing.T) {
		e := newAzureModelsEnv(t)
		e.reg.FailOn("CreateConnection", errors.New("db down"))
		if _, err := e.svc.CreateConnection(ctx, azmUser, AzureConnectionInput{Name: "x", Config: azmKeyConfig("k")}); err == nil {
			t.Fatal("create succeeded")
		}
		if n := e.reg.Calls("CreateConnection"); n != 1 {
			t.Errorf("CreateConnection calls = %d", n)
		}
		if entries, _ := e.secrets.ListFamily("u1", "azure-foundry"); len(entries) != 0 {
			t.Errorf("secret left behind: %+v", entries)
		}
	})
	t.Run("delete: the secret goes after the commit", func(t *testing.T) {
		e := newAzureModelsEnv(t)
		c := e.mustConn(t, azmUser, "x", azmKeyConfig("k"))
		reqCtx, postCommit := txhook.With(ctx) // the request's transaction
		if _, err := e.svc.DeleteConnection(reqCtx, azmUser, c.ID, AzureDeleteOptions{}); err != nil {
			t.Fatal(err)
		}
		if _, err := e.reg.GetConnection(ctx, storageif.AzureUserOwner("u1"), c.ID); !errors.Is(err, storageif.ErrAzureConnectionNotFound) {
			t.Errorf("row after delete: %v", err)
		}
		if has, _ := e.secrets.Has("u1", "azure-foundry/"+c.ID); !has {
			t.Fatal("the secret went before the commit")
		}
		postCommit.Commit(nil)
		if has, _ := e.secrets.Has("u1", "azure-foundry/"+c.ID); has {
			t.Error("the secret outlived the commit")
		}
	})
	t.Run("desktop: pending row, keyring entry, finalised row", func(t *testing.T) {
		e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) { d.PerUserOwners = false })
		e.reg.EnablePendingWrites("boot-1")
		desk := AzureScope{}
		c := e.mustConn(t, desk, "desk", azmKeyConfig("k"))
		rec, err := e.reg.GetConnection(ctx, storageif.AzureUserOwner(""), c.ID)
		if err != nil || rec.Pending || rec.BootID != "" {
			t.Fatalf("finalised row = %+v, %v", rec, err)
		}
		if has, _ := e.secrets.Has("", "azure-foundry/"+c.ID); !has {
			t.Error("keyring entry missing")
		}
		failing := &failingSecrets{FakeSecretStore: testutil.NewFakeSecretStore(), saveErr: storage.ErrSecretStorageUnavailable}
		e2 := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) { d.PerUserOwners = false; d.Secrets = failing })
		e2.reg.EnablePendingWrites("boot-1")
		if _, err := e2.svc.CreateConnection(ctx, desk, AzureConnectionInput{Name: "x", Config: azmKeyConfig("k")}); err == nil {
			t.Fatal("create succeeded without its keyring entry")
		}
		if conns, _ := e2.reg.ListConnections(ctx, storageif.AzureUserOwner("")); len(conns) != 0 {
			t.Errorf("pending row left behind: %+v", conns)
		}
	})
}

// Status is computed when read: ready, needs_credentials, unreadable,
// invalid_config; a model inherits it or is unsupported.
func TestAzureModels_Status(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	en := e.mustConn(t, azmUser, "entra", azmEntraConfig("s"))
	agent := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: en.ID, Kind: "agent", Deployment: "helper"})
	chat := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: en.ID, Kind: "chat", Deployment: "gpt-4o"})
	status := func() (string, map[string]string) {
		t.Helper()
		v, err := e.svc.GetConnection(ctx, azmUser, en.ID)
		if err != nil {
			t.Fatal(err)
		}
		ml, err := e.svc.ListModels(ctx, azmUser)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]string{}
		for _, m := range ml.Models {
			out[m.ID] = m.Status
		}
		return v.Status, out
	}
	key := "azure-foundry/" + en.ID
	// An agent on a resource that lost its project access: unsupported.
	_ = e.secrets.Save("u1", key, `{"endpoint":"https://`+azureTestHost+`","auth":{"mode":"api_key","key":"k"}}`)
	if s, models := status(); s != AzureStatusReady || models[agent.ID] != AzureStatusUnsupported || models[chat.ID] != AzureStatusReady {
		t.Errorf("api key: %s %v", s, models)
	}
	_ = e.secrets.Delete("u1", key)
	if s, models := status(); s != AzureStatusNeedsCredentials || models[chat.ID] != AzureStatusNeedsCredentials {
		t.Errorf("no credential: %s %v", s, models)
	}
	_ = e.secrets.Save("u1", key, `{"endpoint":`)
	if s, _ := status(); s != AzureStatusUnreadable {
		t.Errorf("unparseable: %s", s)
	}
	_ = e.secrets.Save("u1", key, `{"endpoint":"https://gone.example","auth":{"mode":"api_key","key":"k"}}`)
	if s, models := status(); s != AzureStatusInvalidConfig || models[chat.ID] != AzureStatusInvalidConfig {
		t.Errorf("host no longer allowed: %s %v", s, models)
	}
	// A decrypt failure is unreadable, another keystore failure an error.
	secrets := &failingSecrets{FakeSecretStore: e.secrets, getErr: map[string]error{key: storage.ErrSecretDecryptFailed}}
	e.svc.secrets = secrets
	if s, _ := status(); s != AzureStatusUnreadable {
		t.Errorf("decrypt failure: %s", s)
	}
	secrets.getErr[key] = errors.New("keystore: connection refused")
	if _, err := e.svc.GetConnection(ctx, azmUser, en.ID); err == nil {
		t.Error("a keystore failure must not read as a status")
	}
}

// User scope lists the shared resources and models of every organisation
// where the caller is an admin or member — read-only, marked shared, named;
// viewers see none. Org scope lists only that organisation's.
func TestAzureModels_SharedListing(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	e.orgs.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "member": auth.RoleMember, "viewer": auth.RoleViewer})
	e.orgs.add("org-2", "Fabrikam", map[string]auth.Role{"admin2": auth.RoleAdmin})
	orgScope := AzureScope{Scope: "admin", OrgID: "org-1"}
	oc := e.mustConn(t, orgScope, "org-res", azmKeyConfig("org-key"))
	om := e.mustModel(t, orgScope, AzureModelInput{ConnectionID: oc.ID, Kind: "chat", Deployment: "gpt-4.1", DisplayName: "Review bot", Purpose: "review", IsDefault: true})
	e.mustModel(t, AzureScope{Scope: "admin2", OrgID: "org-2"}, AzureModelInput{ConnectionID: e.mustConn(t, AzureScope{Scope: "admin2", OrgID: "org-2"}, "f", azmKeyConfig("k")).ID, Kind: "chat", Deployment: "x"})
	own := e.mustConn(t, AzureScope{Scope: "member"}, "mine", azmKeyConfig("k"))
	e.mustModel(t, AzureScope{Scope: "member"}, AzureModelInput{ConnectionID: own.ID, Kind: "chat", Deployment: "mine"})

	list, err := e.svc.ListConnections(ctx, AzureScope{Scope: "member"})
	if err != nil || len(list.Connections) != 2 {
		t.Fatalf("member listing = %+v, %v", list, err)
	}
	if c := list.Connections[1]; !c.Shared || c.OrgID != "org-1" || c.OrgName != "Contoso" || c.CredentialUpdatedAt != "" || c.SuggestDiscovery || c.SharedByID != "admin" {
		t.Errorf("shared resource = %+v", c)
	}
	models, err := e.svc.ListModels(ctx, AzureScope{Scope: "member"})
	if err != nil || len(models.Models) != 2 {
		t.Fatalf("member models = %+v, %v", models, err)
	}
	// Purpose first (General before Review), personal before shared.
	if models.Models[0].Shared || !models.Models[1].Shared || models.Models[1].ID != om.ID || !models.Models[1].IsDefault || models.Models[1].OrgName != "Contoso" {
		t.Errorf("order / shared = %+v", models.Models)
	}
	// The member's server default is their own model.
	if models.ServerDefault != "m/"+models.Models[0].ID {
		t.Errorf("serverDefault = %q", models.ServerDefault)
	}
	for _, who := range []string{"viewer", "stranger"} {
		if l, _ := e.svc.ListConnections(ctx, AzureScope{Scope: who}); len(l.Connections) != 0 {
			t.Errorf("%s sees %d shared resources", who, len(l.Connections))
		}
	}
	inOrg, err := e.svc.ListModels(ctx, orgScope)
	if err != nil || len(inOrg.Models) != 1 || !inOrg.Models[0].Shared || inOrg.ServerDefault != "" {
		t.Errorf("org scope = %+v, %v", inOrg, err)
	}
	// A member without models of their own, in one organisation: the
	// organisation's default for members is their server default.
	e.orgs.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "member": auth.RoleMember, "newbie": auth.RoleMember})
	if l, _ := e.svc.ListModels(ctx, AzureScope{Scope: "newbie"}); l.ServerDefault != "m/"+om.ID {
		t.Errorf("newbie serverDefault = %q", l.ServerDefault)
	}
}

func TestAzureRegistryHTTPError(t *testing.T) {
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{ErrModelNotAvailable, 400, "MODEL_NOT_AVAILABLE"},
		{storageif.ErrAzureConnectionNotFound, 404, "CONNECTION_NOT_FOUND"},
		{storageif.ErrAzureModelNotFound, 404, "MODEL_NOT_FOUND"},
		{storageif.ErrAzureDuplicateConnectionName, 409, "DUPLICATE_CONNECTION_NAME"},
		{storageif.ErrAzureDuplicateModel, 409, "DUPLICATE_MODEL"},
		{storageif.ErrAzureTooManyConnections, 409, "TOO_MANY_CONNECTIONS"},
		{storageif.ErrAzureTooManyModels, 409, "TOO_MANY_MODELS"},
		{storageif.ErrAzureRegistryConflict, 409, "CONFLICT"},
		{ErrOrgCredentialConflict, 409, "CONFLICT"},
		{storageif.ErrAzureDuplicateDesignation, 409, "CONFLICT"},
		{storageif.ErrAzureInvalidDesignation, 400, "INVALID_DESIGNATION"},
		{storageif.ErrAzureRegistryReadOnly, 409, "REGISTRY_READ_ONLY"},
		{reindexRequired(), 409, "REINDEX_REQUIRED"},
		{ai.ErrAzureDiscoveryUnavailable, 400, "DISCOVERY_UNAVAILABLE"},
		{storage.ErrSecretStorageUnavailable, 503, "SECRET_STORAGE_UNAVAILABLE"},
		{ErrOrgKeyStoreUnavailable, 404, "ORG_KEYSTORE_UNAVAILABLE"},
		{ErrOrgCredentialUnreadable, 500, "ORG_CREDENTIAL_UNREADABLE"},
		{&ai.AzureConfigError{Code: ai.AzureCfgErrInvalidKind}, 400, "INVALID_KIND"},
		{&ai.AzureConfigError{Code: ai.AzureCfgErrHostNotAllowed}, 400, "HOST_NOT_ALLOWED"},
		{azureRegErr(400, AzureCodeKindRequiresProject, "x", nil, nil), 400, "KIND_REQUIRES_PROJECT"},
	}
	for _, tc := range cases {
		status, code, _, ok := AzureRegistryHTTPError(tc.err)
		if !ok || status != tc.status || code != tc.code {
			t.Errorf("%v: %d %s %v, want %d %s", tc.err, status, code, ok, tc.status, tc.code)
		}
	}
	if _, _, _, ok := AzureRegistryHTTPError(errors.New("db down")); ok {
		t.Error("an internal failure must not map")
	}
	if _, code, details, _ := AzureRegistryHTTPError(tooManyModels()); code != "TOO_MANY_MODELS" || details["limit"] != 50 {
		t.Errorf("limit details: %v", details)
	}
}

// The views carry exactly the contract's fields.
func TestAzureModels_ViewJSON(t *testing.T) {
	e := newAzureModelsEnv(t)
	c := e.mustConn(t, azmUser, "c", azmKeyConfig("k"))
	m := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "gpt-4o", ContextWindow: 64000})
	var got map[string]any
	b, _ := json.Marshal(c)
	_ = json.Unmarshal(b, &got)
	for _, k := range []string{"id", "name", "shared", "legacy", "config", "status", "canDiscover", "supportsAgents", "suggestDiscovery", "modelCount", "lastTest", "updatedAt"} {
		if _, ok := got[k]; !ok {
			t.Errorf("connection view lacks %q: %s", k, b)
		}
	}
	b, _ = json.Marshal(m)
	got = nil
	_ = json.Unmarshal(b, &got)
	for _, k := range []string{"id", "modelId", "connectionId", "connectionName", "kind", "deployment", "target", "displayName", "purpose", "contextWindow", "isDefault", "isKnowledgeEmbedding", "shared", "status", "lastTest", "updatedAt"} {
		if _, ok := got[k]; !ok {
			t.Errorf("model view lacks %q: %s", k, b)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, m.UpdatedAt); err != nil {
		t.Errorf("updatedAt %q: %v", m.UpdatedAt, err)
	}
}
