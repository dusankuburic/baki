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
	"pad-analyzer/internal/platform/txhook"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// A resource's credential (the KeyStore) and its row (the registry) are two
// stores with no transaction spanning them. A write that changes both runs
// every check the registry write can fail on before the credential is
// saved; when the registry write still fails, the previous credential goes
// back — an organisation's with its lender and compare-and-swap token — and
// when even that fails the error says so, for the handler to audit.

// countingSecrets counts the personal keystore's saves and fails them from
// the failFrom-th on (0: never).
type countingSecrets struct {
	*testutil.FakeSecretStore
	mu       sync.Mutex
	saves    int
	failFrom int
}

func (s *countingSecrets) Save(scope, provider, key string) error {
	s.mu.Lock()
	s.saves++
	fail := s.failFrom > 0 && s.saves >= s.failFrom
	s.mu.Unlock()
	if fail {
		return errors.New("keystore: connection refused")
	}
	return s.FakeSecretStore.Save(scope, provider, key)
}

func (s *countingSecrets) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saves
}

// failingRestoreOrgKeys is the fake org keystore whose restores fail.
type failingRestoreOrgKeys struct {
	*testutil.FakeOrgSecretStore
}

func (failingRestoreOrgKeys) RestoreOrgIfUnchanged(string, string, time.Time, *storageif.OrgKeySnapshot) (bool, error) {
	return false, errors.New("org keystore: connection reset")
}

// collatingRegistry answers ConnectionNameTaken as Postgres does under
// en_US.UTF-8, where lower('İ') is 'i': "Foİ" and "Foi" are one name there,
// two under Go's case folding.
type collatingRegistry struct {
	*testutil.FakeAzureRegistry
}

func (r collatingRegistry) ConnectionNameTaken(ctx context.Context, owner storageif.AzureOwner, name, exceptID string) (bool, error) {
	fold := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), "i\u0307", "i") }
	conns, err := r.ListConnections(ctx, owner)
	if err != nil {
		return false, err
	}
	for _, c := range conns {
		if c.ID != exceptID && fold(c.Name) == fold(name) {
			return true, nil
		}
	}
	return false, nil
}

func (e *azmEnv) storedKey(t *testing.T, scope, provider string) string {
	t.Helper()
	raw, _ := e.secrets.Get(scope, provider)
	cfg, err := ai.ParseAzureFoundryConfig(raw)
	if err != nil {
		t.Fatalf("stored %s: %v", provider, err)
	}
	return cfg.Auth.Key
}

func (e *azmEnv) storedOrgKey(t *testing.T, orgID, provider string) string {
	t.Helper()
	raw, err := e.orgKeys.GetOrg(orgID, provider)
	if err != nil {
		t.Fatalf("stored org %s: %v", provider, err)
	}
	cfg, err := ai.ParseAzureFoundryConfig(raw)
	if err != nil {
		t.Fatalf("stored org %s: %v", provider, err)
	}
	return cfg.Auth.Key
}

// S1: a rename the registry store refuses by its OWN name rule (Postgres
// lower() under the database's collation, which Go's case folding cannot
// stand in for) is refused before the credential is saved: a PUT that also
// rotates the key writes nothing.
func TestAzureModels_UpdateConnectionNameCheckedByTheStoreRuleFirst(t *testing.T) {
	ctx := context.Background()
	secrets := &countingSecrets{FakeSecretStore: testutil.NewFakeSecretStore()}
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) {
		d.Registry = collatingRegistry{d.Registry.(*testutil.FakeAzureRegistry)}
		d.Secrets = secrets
	})
	e.secrets = secrets.FakeSecretStore
	e.mustConn(t, azmUser, "Foi", azmKeyConfig("k-foi"))
	other := e.mustConn(t, azmUser, "Other", azmKeyConfig("k-old"))
	saves := secrets.count()
	name, cfg := "Foİ", azmKeyConfig("k-NEW")
	_, err := e.svc.UpdateConnection(ctx, azmUser, other.ID, AzureConnectionPatch{Name: &name, Config: &cfg, UpdatedAt: other.UpdatedAt})
	if azmCode(err) != AzureCodeDuplicateConnectionName {
		t.Fatalf("rename onto a name the store folds together: %v (%s), want DUPLICATE_CONNECTION_NAME", err, azmCode(err))
	}
	if n := secrets.count(); n != saves {
		t.Errorf("the keystore was written %d time(s) before the refusal", n-saves)
	}
	if got := e.storedKey(t, "u1", "azure-foundry/"+other.ID); got != "k-old" {
		t.Errorf("stored key = %q, want the old one", got)
	}
	// A create is refused the same way, before its credential is saved.
	if _, err := e.svc.CreateConnection(ctx, azmUser, AzureConnectionInput{Name: "FOİ", Config: azmKeyConfig("k")}); azmCode(err) != AzureCodeDuplicateConnectionName {
		t.Errorf("create with a folded name: %v", err)
	}
	if n := secrets.count(); n != saves {
		t.Errorf("a refused create wrote the keystore")
	}
}

// S1: when the registry write fails after the credential was saved (a race
// on the compare-and-swap token, a store failure), the previous credential
// goes back: the personal key, and an organisation's document with its
// lender, writer and token — the admin can retry with the token they hold.
func TestAzureModels_UpdateConnectionUndoesTheCredentialOnAFailedWrite(t *testing.T) {
	ctx := context.Background()
	e := newAzureModelsEnv(t)
	c := e.mustConn(t, azmUser, "r1", azmKeyConfig("k1"))
	for _, fail := range []struct {
		method string
		err    error
		code   string
	}{
		{"UpdateConnection", storageif.ErrAzureRegistryConflict, AzureCodeConflict},
		{"UpdateConnection", errors.New("db: connection reset"), ""},
		{"SetDiscoveryDismissed", errors.New("db: connection reset"), ""},
		{"ListModels", errors.New("db: connection reset"), ""}, // the answer's view, read after the write
	} {
		e.reg.ClearFailures()
		e.reg.FailOn(fail.method, fail.err)
		name, cfg, dismiss := "r1-renamed", azmKeyConfig("k2-NEW"), true
		_, err := e.svc.UpdateConnection(ctx, azmUser, c.ID, AzureConnectionPatch{Name: &name, Config: &cfg, DismissDiscovery: &dismiss})
		if err == nil || azmCode(err) != fail.code {
			t.Errorf("%s failing: %v (%s), want an error coded %q", fail.method, err, azmCode(err), fail.code)
		}
		if _, changed := AzureCredentialChanged(err); changed {
			t.Errorf("%s failing: reported as a credential change, but the old one went back", fail.method)
		}
		if got := e.storedKey(t, "u1", "azure-foundry/"+c.ID); got != "k1" {
			t.Errorf("%s failing: stored key = %q, want k1 back", fail.method, got)
		}
	}
	e.reg.ClearFailures()

	// An organisation's credential: the lender, the writer and the token go
	// back with the document.
	e.orgs.add("o1", "Org", map[string]auth.Role{"a1": auth.RoleAdmin, "a2": auth.RoleAdmin})
	a1, a2 := AzureScope{Scope: "a1", OrgID: "o1"}, AzureScope{Scope: "a2", OrgID: "o1"}
	oc := e.mustConn(t, a1, "shared", azmKeyConfig("lender-a1"))
	key := "azure-foundry/" + oc.ID
	before, _, _ := e.orgKeys.OrgKeyMeta("o1", key)
	e.reg.FailOn("UpdateConnection", storageif.ErrAzureRegistryConflict)
	cfg := azmKeyConfig("a2-secret")
	_, err := e.svc.UpdateConnection(ctx, a2, oc.ID, AzureConnectionPatch{Config: &cfg, UpdatedAt: oc.UpdatedAt, CredentialUpdatedAt: oc.CredentialUpdatedAt})
	if azmCode(err) != AzureCodeConflict {
		t.Fatalf("org update: %v, want CONFLICT", err)
	}
	after, _, _ := e.orgKeys.OrgKeyMeta("o1", key)
	if got := e.storedOrgKey(t, "o1", key); got != "lender-a1" || after.SecretSetBy != "a1" || after.UpdatedBy != "a1" || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("org credential after the failed update: key %q, meta %+v; want %+v back", got, after, before)
	}
	e.reg.ClearFailures()
	if _, err := e.svc.UpdateConnection(ctx, a2, oc.ID, AzureConnectionPatch{Config: &cfg, CredentialUpdatedAt: oc.CredentialUpdatedAt}); err != nil {
		t.Errorf("the retry with the same token: %v", err)
	}
	if m, _, _ := e.orgKeys.OrgKeyMeta("o1", key); m.SecretSetBy != "a2" {
		t.Errorf("after the retry the lender is %q, want a2", m.SecretSetBy)
	}

	// A resource whose credential was gone (a departed lender's): the one a
	// failed update inserted goes again.
	gone := e.mustConn(t, a1, "gone", azmKeyConfig("k"))
	_ = e.orgKeys.DeleteOrg("o1", "azure-foundry/"+gone.ID)
	e.reg.FailOn("UpdateConnection", errors.New("db: connection reset"))
	if _, err := e.svc.UpdateConnection(ctx, a2, gone.ID, AzureConnectionPatch{Config: &cfg}); err == nil {
		t.Fatal("the update succeeded")
	}
	if has, _ := e.orgKeys.HasOrg("o1", "azure-foundry/"+gone.ID); has {
		t.Error("the credential a failed update inserted survived it")
	}
}

// S1: when the previous credential cannot go back either, the error says
// the new one is in effect (AzureCredentialChangedError, which the handler
// audits) — and still maps to the registry's refusal.
func TestAzureModels_UpdateConnectionReportsAnUndoneUndo(t *testing.T) {
	ctx := context.Background()
	secrets := &countingSecrets{FakeSecretStore: testutil.NewFakeSecretStore()}
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) {
		d.Secrets = secrets
		d.OrgKeys = failingRestoreOrgKeys{d.OrgKeys.(*testutil.FakeOrgSecretStore)}
	})
	e.secrets = secrets.FakeSecretStore
	c := e.mustConn(t, azmUser, "r1", azmKeyConfig("k1"))
	secrets.failFrom = secrets.count() + 2 // the rotation's save works, its undo does not
	e.reg.FailOn("UpdateConnection", storageif.ErrAzureRegistryConflict)
	cfg := azmKeyConfig("k2")
	_, err := e.svc.UpdateConnection(ctx, azmUser, c.ID, AzureConnectionPatch{Config: &cfg})
	if id, changed := AzureCredentialChanged(err); !changed || id != c.ID || azmCode(err) != AzureCodeConflict {
		t.Errorf("personal: %v (changed %v, id %q, code %s), want the change reported, coded CONFLICT", err, changed, id, azmCode(err))
	}
	if got := e.storedKey(t, "u1", "azure-foundry/"+c.ID); got != "k2" {
		t.Errorf("stored key = %q: the report must match the store", got)
	}

	e.reg.ClearFailures()
	e.orgs.add("o1", "Org", map[string]auth.Role{"a1": auth.RoleAdmin})
	a1 := AzureScope{Scope: "a1", OrgID: "o1"}
	oc := e.mustConn(t, a1, "shared", azmKeyConfig("old"))
	e.reg.FailOn("UpdateConnection", errors.New("db: connection reset"))
	ocfg := azmKeyConfig("new")
	_, err = e.svc.UpdateConnection(ctx, a1, oc.ID, AzureConnectionPatch{Config: &ocfg, CredentialUpdatedAt: oc.CredentialUpdatedAt})
	if id, changed := AzureCredentialChanged(err); !changed || id != oc.ID {
		t.Errorf("org: %v, want the change reported", err)
	}
}

// S1 for the single-document routes: everything the entry sync can be
// refused for is checked before the document is saved, and a sync that
// still fails puts the previous document back (an organisation's with its
// lender and token).
func TestAzureModels_LegacySavesUndoTheDocumentOnAFailedSync(t *testing.T) {
	ctx := context.Background()
	e := newAzureModelsEnv(t)
	providers := newProviderServiceWithFakeSecrets(t, e.secrets)
	providers.orgKeys = e.orgKeys
	providers.azure = e.svc
	doc := func(key, chat string, more ...string) ai.AzureFoundryConfig {
		return ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: key}, ChatDeployment: chat, Deployments: more}
	}

	// Personal: a first save whose sync fails leaves no document.
	e.reg.FailOn("CreateModel", errors.New("db: connection reset"))
	if err := providers.SaveAzureFoundryConfig("u1", doc("k1", "chat1")); err == nil {
		t.Fatal("the save succeeded")
	}
	if has, _ := e.secrets.Has("u1", "azure-foundry"); has {
		t.Error("a document whose entries failed to sync stayed")
	}
	e.reg.ClearFailures()
	if err := providers.SaveAzureFoundryConfig("u1", doc("k1", "chat1")); err != nil {
		t.Fatal(err)
	}
	e.reg.FailOn("CreateModel", errors.New("db: connection reset"))
	if err := providers.SaveAzureFoundryConfig("u1", doc("k2", "chat1", "gpt-5")); err == nil {
		t.Fatal("the edit succeeded")
	}
	if got := e.storedKey(t, "u1", "azure-foundry"); got != "k1" {
		t.Errorf("stored key after a failed sync = %q, want k1 back", got)
	}
	e.reg.ClearFailures()

	// Organisation: checked first — the model limit — nothing written.
	orgID := "org-1"
	org := AzureScope{Scope: "admin", OrgID: orgID}
	c := e.mustConn(t, org, "busy", azmKeyConfig("k"))
	for i := 0; i < storageif.AzureMaxModels-1; i++ {
		e.mustModel(t, org, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "d" + strings.Repeat("x", i)})
	}
	if _, err := providers.SaveOrgAzureFoundryConfig(orgID, "admin", "", doc("ok", "c1", "c2")); azmCode(err) != AzureCodeTooManyModels {
		t.Errorf("an org save over the model limit: %v, want TOO_MANY_MODELS before anything is written", err)
	}
	if has, _ := e.orgKeys.HasOrg(orgID, "azure-foundry"); has {
		t.Error("the refused org save wrote the credential")
	}
	if _, err := e.svc.DeleteConnection(ctx, org, c.ID, AzureDeleteOptions{Cascade: true}); err != nil {
		t.Fatal(err)
	}
	// …and a sync that fails after the save: the lender's document, lender
	// and token go back.
	at, err := providers.SaveOrgAzureFoundryConfig(orgID, "lender", "", doc("lender-key", "c1"))
	if err != nil {
		t.Fatal(err)
	}
	before, _, _ := e.orgKeys.OrgKeyMeta(orgID, "azure-foundry")
	e.reg.FailOn("CreateModel", errors.New("db: connection reset"))
	if _, err := providers.SaveOrgAzureFoundryConfig(orgID, "other", at, doc("other-key", "c1", "c2")); err == nil {
		t.Fatal("the org edit succeeded")
	}
	after, _, _ := e.orgKeys.OrgKeyMeta(orgID, "azure-foundry")
	if got := e.storedOrgKey(t, orgID, "azure-foundry"); got != "lender-key" || after.UpdatedBy != before.UpdatedBy || after.SecretSetBy != before.SecretSetBy || !after.UpdatedAt.Equal(before.UpdatedAt) {
		t.Errorf("org document after a failed sync: key %q, meta %+v; want %+v back", got, after, before)
	}
	e.reg.ClearFailures()
	if _, err := providers.SaveOrgAzureFoundryConfig(orgID, "other", at, doc("other-key", "c1", "c2")); err != nil {
		t.Errorf("the retry with the same token: %v", err)
	}
}

// lockHookRegistry runs a hook the first time the owner lock is taken —
// another request landing between a read and the lock.
type lockHookRegistry struct {
	storageif.AzureRegistryStore
	mu   sync.Mutex
	once func()
}

func (r *lockHookRegistry) WithOwnerLock(ctx context.Context, owner storageif.AzureOwner, fn func(ctx context.Context) error) error {
	r.mu.Lock()
	h := r.once
	r.once = nil
	r.mu.Unlock()
	if h != nil {
		h()
	}
	return r.AzureRegistryStore.WithOwnerLock(ctx, owner, fn)
}

func (r *lockHookRegistry) before(h func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.once = h
}

// S3: a designation that moved between an update's read and its owner
// lock is a conflict — never a clear of the NEW holder (which left no
// knowledge-base model, or no default for members).
func TestAzureModels_DesignationMovedBeforeTheLock(t *testing.T) {
	ctx := context.Background()
	hook := &lockHookRegistry{}
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) {
		d.KnowledgeEmbeddingEnabled = true
		hook.AzureRegistryStore = d.Registry
		d.Registry = hook
	})
	holders := func(owner storageif.AzureOwner, kb bool) []string {
		t.Helper()
		ms, err := e.reg.ListModels(ctx, owner)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, m := range ms {
			if (kb && m.IsKnowledgeEmbedding) || (!kb && m.IsDefault) {
				out = append(out, m.Deployment)
			}
		}
		return out
	}
	yes, no := true, false

	c := e.mustConn(t, azmUser, "r", azmKeyConfig("k"))
	x := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "embedding", Deployment: "emb-x", IsKnowledgeEmbedding: true})
	y := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "embedding", Deployment: "emb-y"})
	// Request A moves the designation to Y between request B's read of X
	// (still the holder) and B's lock; B meant to clear X's.
	hook.before(func() {
		if _, err := e.svc.UpdateModel(ctx, azmUser, y.ID, AzureModelPatch{IsKnowledgeEmbedding: &yes}); err != nil {
			t.Errorf("request A: %v", err)
		}
	})
	_, err := e.svc.UpdateModel(ctx, azmUser, x.ID, AzureModelPatch{IsKnowledgeEmbedding: &no})
	if azmCode(err) != AzureCodeConflict {
		t.Errorf("request B: %v (%s), want CONFLICT", err, azmCode(err))
	}
	if got := holders(storageif.AzureUserOwner("u1"), true); strings.Join(got, ",") != "emb-y" {
		t.Errorf("knowledge-base holders = %v, want exactly emb-y", got)
	}

	// The organisation's default for members, the same way.
	org := AzureScope{Scope: "admin", OrgID: "org-1"}
	oc := e.mustConn(t, org, "o", azmKeyConfig("k"))
	a := e.mustModel(t, org, AzureModelInput{ConnectionID: oc.ID, Kind: "chat", Deployment: "a", IsDefault: true})
	b := e.mustModel(t, org, AzureModelInput{ConnectionID: oc.ID, Kind: "chat", Deployment: "b"})
	hook.before(func() {
		if _, err := e.svc.UpdateModel(ctx, org, b.ID, AzureModelPatch{IsDefault: &yes}); err != nil {
			t.Errorf("request A (org): %v", err)
		}
	})
	if _, err := e.svc.UpdateModel(ctx, org, a.ID, AzureModelPatch{IsDefault: &no}); azmCode(err) != AzureCodeConflict {
		t.Errorf("request B (org): %v, want CONFLICT", err)
	}
	if got := holders(storageif.AzureOrgOwner("org-1"), false); strings.Join(got, ",") != "b" {
		t.Errorf("default holders = %v, want exactly b", got)
	}

	// A designation moved ONTO a model a request then deletes, without the
	// re-index confirmation: a conflict, not a silent delete of the corpus's
	// model.
	e.corpus.set("org-1", 7)
	emb := e.mustModel(t, org, AzureModelInput{ConnectionID: oc.ID, Kind: "embedding", Deployment: "e1"})
	hook.before(func() {
		if _, err := e.svc.UpdateModel(ctx, org, emb.ID, AzureModelPatch{IsKnowledgeEmbedding: &yes, Force: true}); err != nil {
			t.Errorf("request A (designate): %v", err)
		}
	})
	if err := e.svc.DeleteModel(ctx, org, emb.ID, false); azmCode(err) != AzureCodeConflict {
		t.Errorf("deleting a model that just became the knowledge-base model: %v, want CONFLICT", err)
	}
	if got := holders(storageif.AzureOrgOwner("org-1"), true); strings.Join(got, ",") != "e1" {
		t.Errorf("knowledge-base holders = %v, want e1 kept", got)
	}
}

// S4: the orphan sweep and a removed resource's post-commit delete decide on
// an earlier read; a key saved again in between is not theirs to delete.
func TestAzureModels_DeletesSpareAKeySavedAgain(t *testing.T) {
	ctx := context.Background()
	e := newAzureModelsEnv(t)
	now := time.Now()
	e.svc.now = func() time.Time { return now }

	// The post-commit delete of a removed resource.
	c := e.mustConn(t, azmUser, "r", azmKeyConfig("k1"))
	key := "azure-foundry/" + c.ID
	reqCtx, postCommit := txhook.With(ctx)
	if _, err := e.svc.DeleteConnection(reqCtx, azmUser, c.ID, AzureDeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // a later stamp for the save below
	_ = e.secrets.Save("u1", key, azmLegacyDoc(""))
	postCommit.Commit(nil)
	if has, _ := e.secrets.Has("u1", key); !has {
		t.Error("the post-commit delete took a key saved after its decision")
	}
	// Unchanged since the decision: deleted.
	c2 := e.mustConn(t, azmUser, "r2", azmKeyConfig("k2"))
	reqCtx, postCommit = txhook.With(ctx)
	if _, err := e.svc.DeleteConnection(reqCtx, azmUser, c2.ID, AzureDeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	postCommit.Commit(nil)
	if has, _ := e.secrets.Has("u1", "azure-foundry/"+c2.ID); has {
		t.Error("the removed resource's credential survived")
	}

	// The orphan sweep: a key listed as an old orphan, saved again before
	// the delete, survives.
	orphan := "azure-foundry/oldorphan1"
	_ = e.secrets.Save("u1", orphan, azmLegacyDoc(""))
	e.secrets.SetUpdatedAt("u1", orphan, now.Add(-2*time.Hour))
	e.svc.secrets = &resaveOnDelete{FakeSecretStore: e.secrets, key: orphan}
	if _, err := e.svc.ListConnections(ctx, azmUser); err != nil {
		t.Fatal(err)
	}
	if has, _ := e.secrets.Has("u1", orphan); !has {
		t.Error("the sweep deleted a key saved again after its listing")
	}
}

// resaveOnDelete saves key again just before its first conditional delete —
// a writer racing the sweep between its listing and its delete.
type resaveOnDelete struct {
	*testutil.FakeSecretStore
	key  string
	done bool
}

func (s *resaveOnDelete) DeleteIfUnchanged(scope, provider string, at time.Time) (bool, error) {
	if provider == s.key && !s.done {
		s.done = true
		time.Sleep(2 * time.Millisecond)
		_ = s.Save(scope, provider, azmLegacyDoc(""))
	}
	return s.FakeSecretStore.DeleteIfUnchanged(scope, provider, at)
}
