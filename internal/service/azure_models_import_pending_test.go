package service

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/rag"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// While an owner's legacy import is pending — its legacy document exists
// (or the keystore cannot say) but a keystore or registry hiccup kept it out
// of the registry — the read paths must not answer as if the document did
// not exist: no fallback to the organisation's credential, no "model not
// available" that makes a client re-pin its threads, no embedding with the
// wrong model. The import then runs on a later read.

// legacyKeyFailures fails the reads of the legacy "azure-foundry" key of the
// personal keystore while armed (Get with getErr, Has with hasErr).
type legacyKeyFailures struct {
	KeyStore
	mu     sync.Mutex
	getErr error
	hasErr error
}

func (f *legacyKeyFailures) arm(getErr, hasErr error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr, f.hasErr = getErr, hasErr
}

func (f *legacyKeyFailures) Get(scope, provider string) (string, error) {
	f.mu.Lock()
	err := f.getErr
	f.mu.Unlock()
	if provider == ai.AzureFoundryProviderID && err != nil {
		return "", err
	}
	return f.KeyStore.Get(scope, provider)
}

func (f *legacyKeyFailures) Has(scope, provider string) (bool, error) {
	f.mu.Lock()
	err := f.hasErr
	f.mu.Unlock()
	if provider == ai.AzureFoundryProviderID && err != nil {
		return false, err
	}
	return f.KeyStore.Has(scope, provider)
}

// orgLegacyKeyFailures is legacyKeyFailures for the org keystore.
type orgLegacyKeyFailures struct {
	OrgKeyStore
	mu     sync.Mutex
	getErr error
}

func (f *orgLegacyKeyFailures) arm(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getErr = err
}

func (f *orgLegacyKeyFailures) GetOrg(orgID, provider string) (string, error) {
	f.mu.Lock()
	err := f.getErr
	f.mu.Unlock()
	if provider == ai.AzureFoundryProviderID && err != nil {
		return "", err
	}
	return f.OrgKeyStore.GetOrg(orgID, provider)
}

var errKeystoreHiccup = errors.New("keystore: context deadline exceeded")

// R2 (A): a member whose own legacy import failed transiently is never
// answered from the organisation — neither the server default ("") nor a
// raw legacy id — and the picker keeps them configured, lists none of the
// organisation's aliases and gives no organisation default. An explicit
// "m/" pick still resolves. Once the keystore recovers, the member's own
// document answers.
func TestAzureModels_ResolveAndPickerWhileLegacyImportPending(t *testing.T) {
	ctx := context.Background()
	var keys *legacyKeyFailures
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) {
		keys = &legacyKeyFailures{KeyStore: d.Secrets}
		d.Secrets = keys
	})
	e.orgs.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "u1": auth.RoleMember})
	_ = e.orgKeys.SaveOrg("org-1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"gpt-4.1"`), "admin")
	orgScope := AzureScope{Scope: "admin", OrgID: "org-1"}
	if _, err := e.svc.ListConnections(ctx, orgScope); err != nil { // the org's import runs fine
		t.Fatal(err)
	}
	orgEntry := "m/" + legacyAzureEntryID(storageif.AzureOrgOwner("org-1"), "chat", "gpt-4.1")
	_ = e.secrets.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"gpt-4.1"`))

	keys.arm(errKeystoreHiccup, nil)
	for _, id := range []string{"", "gpt-4.1", "GPT-4.1"} {
		res, err := e.svc.ResolveChat(ctx, "u1", "org-1", id)
		if err == nil || errors.Is(err, ErrModelNotAvailable) || !errors.Is(err, errAzureImportPending) {
			t.Errorf("%q while the import is pending: %+v, %v; want the pending error — never the org, never not-available", id, res, err)
		}
	}
	// An explicit pick of an existing entry is not the legacy document's to
	// answer.
	if res, err := e.svc.ResolveChat(ctx, "u1", "org-1", orgEntry); err != nil || res.ConnectionRef.OrgID != "org-1" {
		t.Errorf("m/ pick while pending = %+v, %v", res, err)
	}
	out, err := e.svc.PickerListing(ctx, "u1", "org-1")
	if err != nil {
		t.Fatalf("the picker must not fail while the import is pending: %v", err)
	}
	if !out.Configured || out.Shared {
		t.Errorf("configured/shared = %v/%v, want the member's own configuration kept", out.Configured, out.Shared)
	}
	for _, m := range out.Models {
		if len(m.Aliases) != 0 {
			t.Errorf("%s lists aliases %v: a client would re-pin the member's raw-id threads to it", m.ID, m.Aliases)
		}
	}
	if out.DefaultModel != "" {
		t.Errorf("default = %q while the member's own default is pending, want none (never the org's)", out.DefaultModel)
	}
	if ml, err := e.svc.ListModels(ctx, AzureScope{Scope: "u1"}); err != nil || ml.ServerDefault != "" {
		t.Errorf("GET /models serverDefault = %q, %v; want none while pending", ml.ServerDefault, err)
	}

	// The keystore cannot even say whether there is a legacy document, and
	// the marker cannot be read: still pending.
	e.reg.FailOn("ImportedAt", errors.New("db: connection reset"))
	keys.arm(errKeystoreHiccup, errKeystoreHiccup)
	if _, err := e.svc.ResolveChat(ctx, "u1", "org-1", ""); !errors.Is(err, errAzureImportPending) {
		t.Errorf("unknown legacy state: %v, want the pending error", err)
	}
	// …but a caller the keystore says has no legacy document is not pending.
	keys.arm(nil, nil)
	e.orgs.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "u1": auth.RoleMember, "fresh": auth.RoleMember})
	if res, err := e.svc.ResolveChat(ctx, "fresh", "org-1", ""); err != nil || res.ConnectionRef.OrgID != "org-1" {
		t.Errorf("a member without a legacy document = %+v, %v; want the org default", res, err)
	}
	e.reg.ClearFailures()

	// Recovered: the member's own document answers, member-first.
	res, err := e.svc.ResolveChat(ctx, "u1", "org-1", "gpt-4.1")
	if err != nil || res.ConnectionRef.OrgID != "" || res.ConnectionRef.ConnID != ai.AzureDefaultConnectionID {
		t.Errorf("after the import = %+v, %v; want the member's own resource", res, err)
	}
	if res, err := e.svc.ResolveChat(ctx, "u1", "org-1", ""); err != nil || res.ConnectionRef.OrgID != "" {
		t.Errorf("server default after the import = %+v, %v", res, err)
	}
}

// R2: an organisation whose own import is pending is shared (it has a
// resource: its legacy document) but gives members no default and no
// answer for a raw id yet — the pending error, not "not available".
func TestAzureModels_OrgImportPending(t *testing.T) {
	ctx := context.Background()
	var orgKeys *orgLegacyKeyFailures
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) {
		orgKeys = &orgLegacyKeyFailures{OrgKeyStore: d.OrgKeys}
		d.OrgKeys = orgKeys
	})
	e.orgs.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "member": auth.RoleMember})
	_ = e.orgKeys.SaveOrg("org-1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"gpt-4.1"`), "admin")
	orgKeys.arm(errors.New("org keystore: connection reset"))

	for _, id := range []string{"", "gpt-4.1"} {
		if _, err := e.svc.ResolveChat(ctx, "member", "org-1", id); !errors.Is(err, errAzureImportPending) {
			t.Errorf("%q: %v, want the pending error", id, err)
		}
	}
	out, err := e.svc.PickerListing(ctx, "member", "org-1")
	if err != nil || out.Configured || !out.Shared || out.DefaultModel != "" {
		t.Errorf("picker = %+v, %v; want shared, no default", out, err)
	}
	if has, err := e.svc.OrgHasResources(ctx, "org-1"); err != nil || !has {
		t.Errorf("OrgHasResources = %v, %v; want the pending document counted", has, err)
	}
	orgKeys.arm(nil)
	if res, err := e.svc.ResolveChat(ctx, "member", "org-1", "gpt-4.1"); err != nil || res.ConnectionRef.OrgID != "org-1" {
		t.Errorf("after the import = %+v, %v", res, err)
	}
}

// R2 (B): the knowledge base never embeds an organisation's corpus with a
// member's personal model while the organisation's import is pending (the
// org has a resource — its legacy document — and maybe a designation), nor
// treats a caller's pending import as "nothing designated" (which lets the
// knowledge base fall back to another provider): both are the transient
// rag.ErrEmbeddingSetupUnavailable.
func TestAzureModels_KnowledgeEmbedderWhileImportPending(t *testing.T) {
	ctx := context.Background()
	var orgKeys *orgLegacyKeyFailures
	var keys *legacyKeyFailures
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) {
		orgKeys = &orgLegacyKeyFailures{OrgKeyStore: d.OrgKeys}
		d.OrgKeys = orgKeys
		keys = &legacyKeyFailures{KeyStore: d.Secrets}
		d.Secrets = keys
		d.KnowledgeEmbeddingEnabled = true
	})
	e.orgs.add("org-1", "Contoso", map[string]auth.Role{"admin": auth.RoleAdmin, "u1": auth.RoleMember})
	_ = e.orgKeys.SaveOrg("org-1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"gpt-4.1","embeddingDeployment":"org-emb"`), "admin")
	mine := e.mustConn(t, azmUser, "mine", azmKeyConfig("personal-key"))
	e.mustModel(t, azmUser, AzureModelInput{ConnectionID: mine.ID, Kind: "embedding", Deployment: "personal-emb", IsKnowledgeEmbedding: true})

	orgKeys.arm(errors.New("org keystore: connection reset"))
	p, err := e.svc.KnowledgeEmbedder(ctx, "u1", "org-1")
	if p != nil || !errors.Is(err, rag.ErrEmbeddingSetupUnavailable) || errors.Is(err, ai.ErrEmbeddingDeploymentMissing) {
		t.Fatalf("org import pending: %v, %v; want the transient refusal — never the member's personal model", p, err)
	}
	orgKeys.arm(nil)
	p, err = e.svc.KnowledgeEmbedder(ctx, "u1", "org-1")
	if err != nil || !ai.UsedOrgCredential(p) {
		t.Fatalf("after the org's import = %v, %v; want its legacy embedding deployment", p, err)
	}

	// A caller whose own legacy document (it names an embedding
	// deployment) is not imported yet has no answer for "nothing
	// designated".
	_ = e.secrets.Save("u2", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"c","embeddingDeployment":"u2-emb"`))
	keys.arm(errKeystoreHiccup, nil)
	if p, err := e.svc.KnowledgeEmbedder(ctx, "u2", ""); p != nil || !errors.Is(err, rag.ErrEmbeddingSetupUnavailable) || errors.Is(err, ai.ErrEmbeddingDeploymentMissing) {
		t.Errorf("personal import pending: %v, %v; want the transient refusal, not a fallback", p, err)
	}
	keys.arm(nil, nil)
	if p, err := e.svc.KnowledgeEmbedder(ctx, "u2", ""); err != nil || p == nil || ai.UsedOrgCredential(p) {
		t.Errorf("after the import = %v, %v; want u2's own legacy embedding deployment", p, err)
	}
}

// S2: a legacy document whose resource "default" cannot be created (the
// owner reached the resource limit while its import kept failing) is NOT
// marked imported — the import waits for a free slot — and the orphan sweep
// never deletes the legacy key, the rollback credential, even for an owner
// that is imported and has no "default" row (it was deleted explicitly
// with its row, or never made it).
func TestAzureModels_LegacyImportWaitsForAFreeSlot(t *testing.T) {
	ctx := context.Background()
	settings := &azmSettingsToggle{err: errors.New("settings row unreadable")}
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) { d.UserSettings = settings })
	now := time.Now()
	e.svc.now = func() time.Time { return now }
	owner := storageif.AzureUserOwner("u1")
	_ = e.secrets.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"gpt-4o"`))
	// The import cannot run (the saved default is unreadable) while the user
	// creates resources up to the limit.
	for i := 0; i < storageif.AzureMaxConnections; i++ {
		e.mustConn(t, azmUser, "r"+strings.Repeat("x", i), azmKeyConfig("k"))
	}
	settings.set(nil)
	e.secrets.SetUpdatedAt("u1", "azure-foundry", now.Add(-48*time.Hour))
	list, err := e.svc.ListConnections(ctx, azmUser)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range list.Connections {
		if c.ID == ai.AzureDefaultConnectionID {
			t.Fatal("a default resource past the limit")
		}
	}
	if _, ok, _ := e.reg.ImportedAt(ctx, owner); ok {
		t.Error("marked imported without its resource \"default\": the document would never be imported")
	}
	if has, _ := e.secrets.Has("u1", "azure-foundry"); !has {
		t.Fatal("the legacy credential was deleted")
	}
	// Still pending: the member's own document is not answered from anyone
	// else's, and no raw id reads as "not available". The waiting import is
	// not retried on every read (a minute's backoff)…
	attempts := e.reg.Calls("ImportConnection")
	if _, err := e.svc.ResolveChat(ctx, "u1", "", "gpt-4o"); !errors.Is(err, errAzureImportPending) {
		t.Errorf("raw id while the import waits: %v", err)
	}
	if _, err := e.svc.ListConnections(ctx, azmUser); err != nil {
		t.Fatal(err)
	}
	if n := e.reg.Calls("ImportConnection"); n != attempts {
		t.Errorf("the waiting import was retried %d time(s) within its backoff", n-attempts)
	}
	// …but a removed resource frees a slot for the next read at once.
	// A slot frees up: the next read imports it.
	if _, err := e.svc.DeleteConnection(ctx, azmUser, list.Connections[0].ID, AzureDeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	list, err = e.svc.ListConnections(ctx, azmUser) // still inside the backoff's minute
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range list.Connections {
		found = found || c.ID == ai.AzureDefaultConnectionID
	}
	if _, ok, _ := e.reg.ImportedAt(ctx, owner); !ok || !found {
		t.Errorf("after a slot freed: imported=%v, default listed=%v", ok, found)
	}

	// The sweep never takes the bare legacy key, even with no row for it: an
	// imported owner whose "default" row is gone (here: removed behind the
	// service's back) keeps it.
	if _, err := e.reg.DeleteConnection(ctx, owner, ai.AzureDefaultConnectionID); err != nil {
		t.Fatal(err)
	}
	e.secrets.SetUpdatedAt("u1", "azure-foundry", now.Add(-48*time.Hour))
	now = now.Add(2 * time.Hour)
	if _, err := e.svc.ListConnections(ctx, azmUser); err != nil {
		t.Fatal(err)
	}
	if has, _ := e.secrets.Has("u1", "azure-foundry"); !has {
		t.Error("the orphan sweep deleted the legacy key")
	}
}

// azmSettingsToggle is a user-settings loader failing with err while set.
type azmSettingsToggle struct {
	mu  sync.Mutex
	err error
}

func (s *azmSettingsToggle) set(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

func (s *azmSettingsToggle) LoadUserSettings(context.Context, string) (*storageif.AppSettings, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return nil, s.err
	}
	return nil, storageif.ErrNotFound
}
