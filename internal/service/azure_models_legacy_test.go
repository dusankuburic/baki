package service

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// azmSettings is a user-settings loader holding each user's saved Azure
// default model.
type azmSettings struct {
	defaults map[string]string
	err      error
}

func (s azmSettings) LoadUserSettings(_ context.Context, userID string) (*storageif.AppSettings, error) {
	if s.err != nil {
		return nil, s.err
	}
	return &storageif.AppSettings{AI: storageif.AISettings{Providers: map[string]storageif.AIProviderConfig{
		"azure-foundry": {DefaultModel: s.defaults[userID]},
	}}}, nil
}

// azmEntries is owner's registry as "kind:deployment" strings, sorted.
func azmEntries(t *testing.T, e *azmEnv, owner storageif.AzureOwner) []string {
	t.Helper()
	models, err := e.reg.ListModels(context.Background(), owner)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, m := range models {
		s := m.Kind + ":" + m.Deployment
		if m.IsKnowledgeEmbedding {
			s += "+kb"
		}
		if m.IsDefault {
			s += "+default"
		}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// The import makes resource "default" (named after its host) and one entry
// per deployment the document names, plus the saved agent default; entry
// ids are deterministic; running it again (or concurrently) changes nothing.
func TestAzureModels_LegacyImport(t *testing.T) {
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) {
		d.UserSettings = azmSettings{defaults: map[string]string{"u1": "agent:helper"}}
	})
	ctx := context.Background()
	_ = e.secrets.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"chat1","deployments":["gpt-4o"],"claudeDeployments":["claude-sonnet-4-6"],"embeddingDeployment":"emb1"`))
	owner := storageif.AzureUserOwner("u1")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := e.svc.ListConnections(ctx, azmUser); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	conns, _ := e.reg.ListConnections(ctx, owner)
	if len(conns) != 1 || conns[0].ID != "default" || conns[0].Name != "unit-test" {
		t.Fatalf("resources = %+v", conns)
	}
	want := []string{"agent:helper", "chat:chat1", "chat:gpt-4o", "claude:claude-sonnet-4-6", "embedding:emb1+kb"}
	if got := azmEntries(t, e, owner); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("entries = %v, want %v", got, want)
	}
	models, _ := e.reg.ListModels(ctx, owner)
	for _, m := range models {
		if m.ID != legacyAzureEntryID(owner, storageif.AzureModelNamespace(m.Kind), m.Deployment) || m.ConnectionID != "default" || m.Purpose != "general" {
			t.Errorf("entry %+v: not the deterministic import", m)
		}
	}
	if _, ok, _ := e.reg.ImportedAt(ctx, owner); !ok {
		t.Error("no import marker")
	}
	// The document itself is untouched.
	raw, _ := e.secrets.Get("u1", "azure-foundry")
	if doc, _ := ai.ParseAzureFoundryConfig(raw); doc.ChatDeployment != "chat1" || len(doc.ClaudeDeployments) != 1 {
		t.Errorf("document changed: %s", raw)
	}
	// Deterministic across owners' ids and stable: the same input, the same id.
	if legacyAzureEntryID(owner, "chat", "GPT-4O") != legacyAzureEntryID(owner, "chat", "gpt-4o") ||
		legacyAzureEntryID(owner, "chat", "x") == legacyAzureEntryID(storageif.AzureUserOwner("u2"), "chat", "x") {
		t.Error("ids must be case-insensitive per name and distinct per owner")
	}
}

// A deleted "default" resource is never resurrected: the marker survives it.
func TestAzureModels_LegacyMarkerPreventsResurrection(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	_ = e.secrets.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"chat1"`))
	if l, _ := e.svc.ListConnections(ctx, azmUser); len(l.Connections) != 1 {
		t.Fatal("no import")
	}
	if _, err := e.svc.DeleteConnection(ctx, azmUser, "default", AzureDeleteOptions{Cascade: true}); err != nil {
		t.Fatal(err)
	}
	if has, _ := e.secrets.Has("u1", "azure-foundry"); has {
		t.Fatal("the legacy credential outlived its resource")
	}
	// A rolled-back binary writes the document again; a fresh process (no
	// memo) must still not import it.
	_ = e.secrets.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"chat1"`))
	fresh := NewAzureModelService(AzureModelServiceDeps{Registry: e.reg, Secrets: e.secrets, KeyFamilies: e.secrets, OrgKeys: e.orgKeys, PerUserOwners: true})
	if l, _ := fresh.ListConnections(ctx, azmUser); len(l.Connections) != 0 {
		t.Errorf("resurrected: %+v", l.Connections)
	}
}

// unreadable only for a decrypt or parse failure; any other failure skips
// the import without a marker, and the next read imports.
func TestAzureModels_LegacyUnreadableAndTransient(t *testing.T) {
	ctx := context.Background()
	t.Run("parse failure", func(t *testing.T) {
		e := newAzureModelsEnv(t)
		_ = e.secrets.Save("u1", "azure-foundry", `{"endpoint":`)
		l, err := e.svc.ListConnections(ctx, azmUser)
		if err != nil || len(l.Connections) != 1 || l.Connections[0].ID != "default" || l.Connections[0].Status != AzureStatusUnreadable {
			t.Fatalf("listing = %+v, %v", l, err)
		}
		if got := azmEntries(t, e, storageif.AzureUserOwner("u1")); len(got) != 0 {
			t.Errorf("entries from an unreadable document: %v", got)
		}
	})
	t.Run("decrypt failure", func(t *testing.T) {
		secrets := &failingSecrets{FakeSecretStore: testutil.NewFakeSecretStore(), getErr: map[string]error{"azure-foundry": storage.ErrSecretDecryptFailed}}
		e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) { d.Secrets = secrets })
		l, err := e.svc.ListConnections(ctx, azmUser)
		if err != nil || len(l.Connections) != 1 || l.Connections[0].Status != AzureStatusUnreadable {
			t.Fatalf("listing = %+v, %v", l, err)
		}
	})
	t.Run("transient failure", func(t *testing.T) {
		secrets := &failingSecrets{FakeSecretStore: testutil.NewFakeSecretStore(), getErr: map[string]error{"azure-foundry": errors.New("keystore: connection refused")}}
		e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) { d.Secrets = secrets })
		_ = secrets.FakeSecretStore.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"chat1"`))
		if _, err := e.svc.ListModels(ctx, azmUser); err == nil {
			// The listing itself reads the credential too and fails: fine
			// either way — what matters is the marker.
			t.Log("listing succeeded")
		}
		if _, ok, _ := e.reg.ImportedAt(ctx, storageif.AzureUserOwner("u1")); ok {
			t.Fatal("a transient failure wrote the marker")
		}
		delete(secrets.getErr, "azure-foundry")
		l, err := e.svc.ListConnections(ctx, azmUser)
		if err != nil || len(l.Connections) != 1 || l.Connections[0].Status != AzureStatusReady {
			t.Fatalf("retry = %+v, %v", l, err)
		}
	})
	t.Run("a settings failure waits too", func(t *testing.T) {
		e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) { d.UserSettings = azmSettings{err: errors.New("db down")} })
		_ = e.secrets.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"chat1"`))
		_, _ = e.svc.ListConnections(ctx, azmUser)
		if _, ok, _ := e.reg.ImportedAt(ctx, storageif.AzureUserOwner("u1")); ok {
			t.Error("imported without the saved default")
		}
	})
}

// The saved default becomes an entry when it is an agent or a deployment
// the document does not list — not when it is listed, nor an "m/" id.
func TestAzureModels_LegacySavedDefault(t *testing.T) {
	cases := map[string]string{
		"agent:Helper Bot": "agent:Helper Bot,chat:chat1",
		"gpt-4o":           "chat:chat1,chat:gpt-4o",
		"CHAT1":            "chat:chat1",
		"claude:sonnet":    "chat:chat1,claude:sonnet",
		"m/abcdefghij":     "chat:chat1",
		"not a/name":       "chat:chat1",
	}
	for saved, want := range cases {
		t.Run(saved, func(t *testing.T) {
			e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) {
				d.UserSettings = azmSettings{defaults: map[string]string{"u1": saved}}
			})
			_ = e.secrets.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"chat1"`))
			_, _ = e.svc.ListConnections(context.Background(), azmUser)
			if got := strings.Join(azmEntries(t, e, storageif.AzureUserOwner("u1")), ","); got != want {
				t.Errorf("entries = %s, want %s", got, want)
			}
		})
	}
}

// The import stops at the model limit, keeps what fits, and still marks the
// owner imported.
func TestAzureModels_LegacyImportStopsAtTheLimit(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	owner := storageif.AzureUserOwner("u1")
	if _, err := e.reg.CreateConnection(ctx, owner, storageif.AzureConnectionRecord{ID: "abcdefghij", Name: "early"}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < storageif.AzureMaxModels-2; i++ {
		id, _ := newAzureRegistryID()
		if _, err := e.reg.CreateModel(ctx, owner, storageif.AzureModelRecord{ID: id, ConnectionID: "abcdefghij", Kind: "chat", Deployment: "m" + strings.Repeat("x", i), DisplayName: "m", Purpose: "general"}); err != nil {
			t.Fatal(err)
		}
	}
	_ = e.secrets.Save("u1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"a","deployments":["b","c","d"]`))
	_ = e.secrets.Save("u1", "azure-foundry/abcdefghij", azmLegacyDoc(""))
	if _, err := e.svc.ListConnections(ctx, azmUser); err != nil {
		t.Fatal(err)
	}
	counts, _ := e.reg.Counts(ctx, owner)
	if counts.Models != storageif.AzureMaxModels || counts.Connections != 2 {
		t.Errorf("counts = %+v", counts)
	}
	if _, ok, _ := e.reg.ImportedAt(ctx, owner); !ok {
		t.Error("an import stopped at the limit must still be marked")
	}
}

// An organisation's chat deployment becomes its default for members.
func TestAzureModels_LegacyOrgImport(t *testing.T) {
	e := newAzureModelsEnv(t)
	_ = e.orgKeys.SaveOrg("org-1", "azure-foundry", azmLegacyDoc(`,"chatDeployment":"org-chat","deployments":["b"],"embeddingDeployment":"emb"`), "admin")
	if _, err := e.svc.ListConnections(context.Background(), AzureScope{Scope: "admin", OrgID: "org-1"}); err != nil {
		t.Fatal(err)
	}
	got := strings.Join(azmEntries(t, e, storageif.AzureOrgOwner("org-1")), ",")
	if got != "chat:b,chat:org-chat+default,embedding:emb+kb" {
		t.Errorf("org entries = %s", got)
	}
}

// The single-document routes act on resource "default": a save maps the
// deployment fields onto its entries and keeps the document's old fields,
// the GET builds the fields from the registry, /api/keys/delete removes the
// resource, /api/keys/has answers for any resource.
func TestAzureModels_DeprecatedAdapters(t *testing.T) {
	e := newAzureModelsEnv(t)
	ctx := context.Background()
	providers := newProviderServiceWithFakeSecrets(t, e.secrets)
	providers.orgKeys = e.orgKeys
	providers.azure = e.svc
	system := NewSystemService(nil, e.secrets, nil, nil, "", nil, nil)
	system.azure = e.svc
	owner := storageif.AzureUserOwner("u1")

	cfg := ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k1"}, ChatDeployment: "chat1", Deployments: []string{"gpt-4o"}}
	if err := providers.SaveAzureFoundryConfig("u1", cfg); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(azmEntries(t, e, owner), ","); got != "chat:chat1,chat:gpt-4o" {
		t.Fatalf("entries after the first save = %s", got)
	}
	// The route stores its document as saved, deployment fields included:
	// a rolled-back binary finds them, and the server default follows the
	// saved chat deployment.
	raw, _ := e.secrets.Get("u1", "azure-foundry")
	if doc, _ := ai.ParseAzureFoundryConfig(raw); doc.Auth.Key != "k1" || doc.ChatDeployment != "chat1" || strings.Join(doc.Deployments, ",") != "gpt-4o" {
		t.Errorf("a first save stores the document as saved: %s", raw)
	}
	listing, err := e.svc.PickerListing(ctx, "u1", "")
	if err != nil || listing.DefaultModel == "" {
		t.Fatalf("listing = %+v, %v", listing, err)
	}
	defaultName := ""
	for _, m := range listing.Models {
		if m.ID == listing.DefaultModel {
			defaultName = m.DisplayName
		}
	}
	if defaultName != "chat1" {
		t.Errorf("server default = %s (%q), want the saved chat deployment chat1", listing.DefaultModel, defaultName)
	}
	// A model added through the registry shows in the GET.
	e.mustModel(t, azmUser, AzureModelInput{ConnectionID: "default", Kind: "claude", Deployment: "claude-opus"})
	pub, err := providers.GetAzureFoundryConfig("u1")
	if err != nil || !pub.Configured || pub.HasSecret != true || strings.Join(pub.ClaudeDeployments, ",") != "claude-opus" || pub.ChatDeployment == "" {
		t.Fatalf("GET = %+v, %v", pub, err)
	}
	// A save that drops a deployment deletes its entry; one naming a new
	// one creates it; the omitted key is carried.
	edit := ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key"}, ChatDeployment: "chat1", Deployments: []string{"gpt-5"}, EmbeddingDeployment: "emb"}
	if err := providers.SaveAzureFoundryConfig("u1", edit); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(azmEntries(t, e, owner), ","); got != "chat:chat1,chat:gpt-5,embedding:emb+kb" {
		t.Errorf("entries after the edit = %s", got)
	}
	raw, _ = e.secrets.Get("u1", "azure-foundry")
	if doc, _ := ai.ParseAzureFoundryConfig(raw); doc.Auth.Key != "k1" {
		t.Errorf("key not carried: %s", raw)
	}
	pub, _ = providers.GetAzureFoundryConfig("u1")
	if pub.ChatDeployment != "chat1" || strings.Join(pub.Deployments, ",") != "gpt-5" || pub.EmbeddingDeployment != "emb" || len(pub.ClaudeDeployments) != 0 {
		t.Errorf("GET after the edit = %+v", pub)
	}
	// Validation stays the route's own.
	if err := providers.SaveAzureFoundryConfig("u1", ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k"}}); azmCode(err) != ai.AzureCfgErrMissingDeploy {
		t.Errorf("no deployment: %v", err)
	}

	other := e.mustConn(t, azmUser, "other", azmKeyConfig("k2"))
	if has, err := system.HasApiKey("u1", "azure-foundry"); err != nil || !has {
		t.Errorf("has = %v, %v", has, err)
	}
	if err := system.DeleteApiKey(ctx, "u1", "azure-foundry"); err != nil {
		t.Fatal(err)
	}
	conns, _ := e.reg.ListConnections(ctx, owner)
	if len(conns) != 1 || conns[0].ID != other.ID {
		t.Errorf("after /api/keys/delete: %+v", conns)
	}
	if has, _ := e.secrets.Has("u1", "azure-foundry"); has {
		t.Error("the default resource's credential survived")
	}
	if has, _ := system.HasApiKey("u1", "azure-foundry"); !has {
		t.Error("another resource still counts")
	}
	if has, _ := system.HasApiKey("u2", "azure-foundry"); has {
		t.Error("u2 has none")
	}
}

func TestAzureModels_DeprecatedOrgAdapters(t *testing.T) {
	e := newAzureModelsEnv(t)
	providers := newProviderServiceWithFakeSecrets(t, e.secrets)
	providers.orgKeys = e.orgKeys
	providers.azure = e.svc
	owner := storageif.AzureOrgOwner("org-1")
	doc := ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k"}, ChatDeployment: "c1", Deployments: []string{"c2"}}
	at, err := providers.SaveOrgAzureFoundryConfig("org-1", "admin-a", "", doc)
	if err != nil || at == "" {
		t.Fatalf("save = %q, %v", at, err)
	}
	if got := strings.Join(azmEntries(t, e, owner), ","); got != "chat:c1+default,chat:c2" {
		t.Errorf("org entries = %s", got)
	}
	pub, err := providers.GetOrgAzureFoundryConfig("org-1")
	if err != nil || !pub.Configured || pub.UpdatedAt != at || pub.SecretSetByID != "admin-a" || pub.ChatDeployment != "c1" || strings.Join(pub.Deployments, ",") != "c2" {
		t.Fatalf("org GET = %+v, %v", pub, err)
	}
	if _, err := providers.SaveOrgAzureFoundryConfig("org-1", "admin-b", "", doc); !errors.Is(err, ErrOrgCredentialConflict) {
		t.Errorf("token-less save over a document: %v", err)
	}
	cli := doc
	cli.Auth = ai.AzureFoundryAuth{Mode: ai.AzureAuthModeEntraCLI}
	if _, err := providers.SaveOrgAzureFoundryConfig("org-1", "admin-b", at, cli); azmCode(err) != ai.AzureCfgErrInvalidAuth {
		t.Errorf("CLI org save: %v", err)
	}
	if err := providers.DeleteOrgAzureFoundryConfig("org-1"); err != nil {
		t.Fatal(err)
	}
	if conns, _ := e.reg.ListConnections(context.Background(), owner); len(conns) != 0 {
		t.Errorf("org resource after delete: %+v", conns)
	}
	if has, _ := e.orgKeys.HasOrg("org-1", "azure-foundry"); has {
		t.Error("org credential after delete")
	}
	if pub, err := providers.GetOrgAzureFoundryConfig("org-1"); err != nil || pub.Configured {
		t.Errorf("GET after delete = %+v, %v", pub, err)
	}
}

// /api/providers/test tests resource "default" — else the first — as a
// resource: an authenticated probe, no model call.
func TestAzureModels_DeprecatedConnectionTest(t *testing.T) {
	e := newAzureModelsEnv(t)
	providers := newProviderServiceWithFakeSecrets(t, e.secrets)
	providers.azure = e.svc
	res, err := providers.TestProviderConnection(context.Background(), "u1", "azure-foundry")
	if err != nil || res.Ok || !strings.Contains(res.Error, "no Azure AI Foundry resource") {
		t.Fatalf("none = %+v, %v", res, err)
	}
	a := e.mustConn(t, azmUser, "a", azmKeyConfig("ka"))
	var probed []string
	e.svc.probeResource = func(_ context.Context, raw string) error {
		probed = append(probed, raw)
		return nil
	}
	e.svc.now = func() time.Time { return time.Now() }
	res, err = providers.TestProviderConnection(context.Background(), "u1", "azure-foundry")
	if err != nil || !res.Ok || len(probed) != 1 || !strings.Contains(probed[0], "ka") {
		t.Fatalf("first resource = %+v, %v, %v", res, err, probed)
	}
	if v, _ := e.svc.GetConnection(context.Background(), azmUser, a.ID); v.LastTest == nil || !v.LastTest.OK {
		t.Errorf("last test = %+v", v.LastTest)
	}
}
