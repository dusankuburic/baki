package service

import (
	"context"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// An invalidation another replica announced reaches the cache through its applier (Bus.OnRemote), the cache
// owner's own drop: these tests fill the cache, tell the bus what a peer's message says, and read again. An empty
// scope is the whole cache (the receiver's resync after a gap).

func TestSettingsCache_AppliesARemoteInvalidation(t *testing.T) {
	b := &countingBackend{}
	storedWithMarker(t, b, 100)
	p := NewBackendSettingsProvider(context.Background(), b)
	bus := eventbus.New(nil, time.Second)
	SubscribeSettingsCache(bus, p)

	storedWithMarker(t, b, 200) // another replica's write
	if got := p.Get().Parser.MaxFileSizeMB; got != 100 {
		t.Fatalf("premise: the cached value is served until the TTL, got %v", got)
	}
	if !bus.DeliverRemote("settings.deployment", nil) {
		t.Fatal("no applier for settings.deployment")
	}
	if got := p.Get().Parser.MaxFileSizeMB; got != 200 {
		t.Errorf("after the remote invalidation MaxFileSizeMB = %v, want the other replica's 200", got)
	}
}

func TestRuleProfiles_ApplyRemoteInvalidations(t *testing.T) {
	b := testutil.NewFakeBackend()
	storeRule(t, b, "org-a", "first", "^SET$", true)
	storeRule(t, b, "org-b", "second", "^SET$", true)
	r := NewRuleProfileResolver(b, deploymentSettings(nil), nil)
	bus := eventbus.New(nil, time.Second)
	SubscribeRuleProfiles(bus, r)
	ctx := context.Background()
	fill := func() { r.Resolve(ctx, "org-a"); r.Resolve(ctx, "org-b"); r.Resolve(ctx, "") }
	cached := func() []bool {
		return []bool{r.cachedFor("org-a") != nil, r.cachedFor("org-b") != nil, r.cachedFor("") != nil}
	}

	fill()
	if got := cached(); !slices.Equal(got, []bool{true, true, true}) {
		t.Fatalf("premise: all three profiles cached, got %v", got)
	}
	bus.DeliverRemote("rules.profiles", eventbus.NewScope("org", "org-a"))
	if got := cached(); !slices.Equal(got, []bool{false, true, true}) {
		t.Errorf("org scope: cached %v, want only org-a dropped", got)
	}
	fill()
	bus.DeliverRemote("rules.profiles", eventbus.NewScope("org", "org-a", "user", "u1"))
	if got := cached(); !slices.Equal(got, []bool{true, true, true}) {
		t.Errorf("org+user scope drops the user's own entry only (the org's own view stays), cached %v", got)
	}
	bus.DeliverRemote("rules.profiles", nil)
	if got := cached(); !slices.Equal(got, []bool{false, false, false}) {
		t.Errorf("empty scope: cached %v, want the whole cache dropped", got)
	}
	fill()
	bus.DeliverRemote("rules.deployment-profile", nil)
	if got := cached(); !slices.Equal(got, []bool{false, false, false}) {
		t.Errorf("deployment profile: cached %v, want every profile dropped (every org inherits the deployment layer)", got)
	}
}

func TestModelCache_AppliesRemoteInvalidations(t *testing.T) {
	svc := newProviderServiceWithFakeSecrets(t, testutil.NewFakeSecretStore())
	bus := eventbus.New(nil, time.Second)
	SubscribeModelCache(bus, svc)
	put := func(scope, provider string) {
		svc.modelMu.Lock()
		svc.modelCache[modelListingKey(scope, provider)] = modelCacheEntry{models: []ai.ModelInfo{{ID: "m"}}, expiresAt: time.Now().Add(time.Hour)}
		svc.modelMu.Unlock()
	}
	has := func(scope, provider string) bool {
		svc.modelMu.Lock()
		defer svc.modelMu.Unlock()
		_, ok := svc.modelCache[modelListingKey(scope, provider)]
		return ok
	}
	fill := func() { put("u1", "glm"); put("u1", "openai"); put("u2", "glm"); put("", "glm") }

	fill()
	bus.DeliverRemote("providers.model-listings", eventbus.NewScope("scope", "u1", "provider", "glm"))
	if has("u1", "glm") || !has("u1", "openai") || !has("u2", "glm") {
		t.Errorf("one provider of one scope: u1/glm=%v u1/openai=%v u2/glm=%v, want only u1/glm dropped", has("u1", "glm"), has("u1", "openai"), has("u2", "glm"))
	}
	fill()
	bus.DeliverRemote("providers.model-listings", eventbus.NewScope("scope", "u1"))
	if has("u1", "glm") || has("u1", "openai") || !has("u2", "glm") {
		t.Error("a scope without a provider: want both of u1's listings dropped and u2's kept")
	}
	fill()
	bus.DeliverRemote("providers.model-listings", eventbus.NewScope("scope", ""))
	if has("", "glm") || !has("u1", "glm") {
		t.Error("the deployment scope is named though empty: want its listing dropped and the others kept")
	}
	fill()
	bus.DeliverRemote("providers.model-listings", nil)
	if has("u1", "glm") || has("u1", "openai") || has("u2", "glm") || has("", "glm") {
		t.Error("empty scope: want every listing dropped")
	}
}

// A listing that started before a whole-cache drop must not be written back after it.
func TestModelCache_WholeDropFencesListingsInFlight(t *testing.T) {
	svc := newProviderServiceWithFakeSecrets(t, testutil.NewFakeSecretStore())
	svc.modelMu.Lock()
	gen := svc.modelGen
	svc.modelMu.Unlock()
	svc.InvalidateAllModels()
	svc.modelMu.Lock()
	defer svc.modelMu.Unlock()
	if !svc.invalidatedSinceLocked(modelListingKey("u1", "glm"), "u1", gen) {
		t.Error("a listing begun before InvalidateAllModels may still be written back")
	}
	if svc.invalidatedSinceLocked(modelListingKey("u1", "glm"), "u1", svc.modelGen) {
		t.Error("a listing begun after InvalidateAllModels is fenced off")
	}
}

func TestDashboard_AppliesRemoteInvalidations(t *testing.T) {
	store := &gatedDashStore{FakeBackend: testutil.NewFakeBackend()}
	svc := NewDashboardService(store, nil, nil, nil)
	ctx := context.Background()
	svc.BuildHome(ctx, "u1")
	svc.BuildHome(ctx, "u2")
	if store.builds.Load() != 2 {
		t.Fatalf("premise: two builds, got %d", store.builds.Load())
	}
	svc.ApplyRemoteViewInvalidation(eventbus.NewScope("owner", "u1", "org", "o1"))
	svc.BuildHome(ctx, "u1")
	svc.BuildHome(ctx, "u2")
	if store.builds.Load() != 3 {
		t.Errorf("owner scope: %d builds, want 3 (u1 rebuilt, u2 served from the cache)", store.builds.Load())
	}
	svc.ApplyRemoteViewInvalidation(nil)
	svc.BuildHome(ctx, "u1")
	svc.BuildHome(ctx, "u2")
	if store.builds.Load() != 5 {
		t.Errorf("empty scope: %d builds, want 5 (every view dropped)", store.builds.Load())
	}
	svc.ApplyRemoteViewInvalidation(eventbus.NewScope("unrelated", "x")) // names nobody: drops nothing
	svc.BuildHome(ctx, "u1")
	if store.builds.Load() != 5 {
		t.Errorf("a scope naming no owner or org rebuilt a view (%d builds)", store.builds.Load())
	}
}

// The two drops that are not a domain event's (an analysis recorded, an org's Azure credentials disabled) tell the
// other replicas themselves, through the bus, once the write is durable.
func TestRecordAnalysis_TellsTheOtherReplicasWhichViewsItDropped(t *testing.T) {
	bus, rec := replicatedBus(func(b *eventbus.Bus) {
		for _, s := range FlowCacheSubscribers(nil, nil, nil) {
			eventbus.Subscribe(b, eventbus.Sync, s.Name, s.Fn, s.Options...)
		}
	})
	backend := &captureDashBackend{FakeBackend: testutil.NewFakeBackend()}
	svc := NewDashboardService(backend, nil, nil, nil, WithDashboardEvents(bus))
	svc.RecordAnalysis(context.Background(), &models.FlowDocument{ID: "f1", OwnerID: "u1", OrganizationID: "o1"}, &models.AnalysisReport{})
	want := []string{"dashboard.home{org=o1,owner=u1}", "dashboard.analytics{org=o1,owner=u1}"}
	if got := rec.Published(); !slices.Equal(got, want) {
		t.Errorf("RecordAnalysis told the replicas %v, want %v", got, want)
	}
}

// The key changes that go through ProviderService's own methods (the GitHub and Copilot sign-ins, the local and Azure
// configurations, an organisation's Azure credentials) are not CredentialChanged events: they tell the other
// replicas themselves, with the provider when there is one.
func TestProviderService_DirectListingDropsAreAnnounced(t *testing.T) {
	bus, rec := replicatedBus(func(*eventbus.Bus) {})
	secrets := testutil.NewFakeSecretStore()
	getKey := func(scope, provider string) (string, error) { v, _ := secrets.Get(scope, provider); return v, nil }
	factory := ai.NewProviderFactory(getKey, ai.NewCopilotAuth(), nil, nil)
	svc := NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets, nil, nil, WithProviderEvents(bus))
	SubscribeModelCache(bus, svc) // declares the key the announcement needs

	if err := svc.RevokeGitHubAuth("u1"); err != nil {
		t.Fatal(err)
	}
	if got, want := rec.Published(), []string{"providers.model-listings{provider=" + ai.GitHubModelsProviderID + ",scope=u1}"}; !slices.Equal(got, want) {
		t.Errorf("RevokeGitHubAuth told the replicas %v, want %v", got, want)
	}
	rec.Reset()
	svc.invalidateScopeEverywhere(ai.OrgScope("o1"))
	if got, want := rec.Published(), []string{"providers.model-listings{scope=org:o1}"}; !slices.Equal(got, want) {
		t.Errorf("an org scope drop told the replicas %v, want %v", got, want)
	}

	// Without a bus (a test, the desktop) the same drops are local and nothing fails.
	plain := newProviderServiceWithFakeSecrets(t, secrets)
	if err := plain.RevokeGitHubAuth("u1"); err != nil {
		t.Fatal(err)
	}
	plain.invalidateScopeEverywhere(ai.OrgScope("o1"))
}
