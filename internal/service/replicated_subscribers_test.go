package service

import (
	"context"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/testutil"
)

// replicatedBus is a bus whose Replicas hook records what the subscribers registered by wire tell other replicas.
func replicatedBus(wire func(*eventbus.Bus)) (*eventbus.Bus, *eventbus.ReplicasRecorder) {
	bus := eventbus.New(nil, time.Second)
	rec := &eventbus.ReplicasRecorder{}
	bus.SetReplicas(rec)
	wire(bus)
	return bus, rec
}

// replicatedCases publishes each event on its own and checks what the other replicas are told.
func replicatedCases(t *testing.T, bus *eventbus.Bus, rec *eventbus.ReplicasRecorder, cases []replicatedCase) {
	t.Helper()
	for _, tc := range cases {
		rec.Reset()
		bus.Publish(context.Background(), tc.event)
		if got := rec.Published(); !slices.Equal(got, tc.want) {
			t.Errorf("%s: published %v, want %v", tc.name, got, tc.want)
		}
	}
}

type replicatedCase struct {
	name  string
	event eventbus.Event
	want  []string
}

func TestSettingsCache_ReplicatesDeploymentChangesOnly(t *testing.T) {
	bus, rec := replicatedBus(func(b *eventbus.Bus) { SubscribeSettingsCache(b, &BackendSettingsProvider{}) })
	replicatedCases(t, bus, rec, []replicatedCase{
		{"deployment", events.SettingsChanged{Scope: events.DeploymentSettings}, []string{"settings.deployment{}"}},
		{"user", events.SettingsChanged{Scope: events.UserSettings, ID: "u1"}, nil},
		{"org", events.SettingsChanged{Scope: events.OrgSettings, ID: "o1"}, nil},
	})
}

func TestRuleProfiles_ReplicateWhatTheyDrop(t *testing.T) {
	r := NewRuleProfileResolver(testutil.NewFakeBackend(), deploymentSettings(nil), nil)
	bus, rec := replicatedBus(func(b *eventbus.Bus) { SubscribeRuleProfiles(b, r) })
	replicatedCases(t, bus, rec, []replicatedCase{
		{"deployment settings drop both caches whole", events.SettingsChanged{Scope: events.DeploymentSettings},
			[]string{"rules.deployment-profile{}", "rules.profiles{}"}},
		{"org settings", events.SettingsChanged{Scope: events.OrgSettings, ID: "o1"}, []string{"rules.profiles{org=o1}"}},
		{"org settings without an id", events.SettingsChanged{Scope: events.OrgSettings}, nil},
		{"user settings", events.SettingsChanged{Scope: events.UserSettings, ID: "u1"}, nil},
		{"member added", events.OrgMemberChanged{OrgID: "o1", UserID: "u1", Kind: events.OrgMemberAdded}, []string{"rules.profiles{org=o1,user=u1}"}},
		{"member removed", events.OrgMemberChanged{OrgID: "o1", UserID: "u1", Kind: events.OrgMemberRemoved}, []string{"rules.profiles{org=o1,user=u1}"}},
		{"member added without a user drops the org", events.OrgMemberChanged{OrgID: "o1", Kind: events.OrgMemberAdded}, []string{"rules.profiles{org=o1}"}},
		{"role change", events.OrgMemberChanged{OrgID: "o1", UserID: "u1", Kind: events.OrgMemberRoleChanged}, nil},
		{"org deleted", events.OrgMemberChanged{OrgID: "o1", Kind: events.OrgDeleted}, []string{"rules.profiles{org=o1}"}},
		{"no org", events.OrgMemberChanged{UserID: "u1", Kind: events.OrgMemberAdded}, nil},
	})
}

func TestModelCache_ReplicatesTheScopeAndProviderItDrops(t *testing.T) {
	svc := newProviderServiceWithFakeSecrets(t, testutil.NewFakeSecretStore())
	bus, rec := replicatedBus(func(b *eventbus.Bus) { SubscribeModelCache(b, svc) })
	replicatedCases(t, bus, rec, []replicatedCase{
		{"one provider's key", events.CredentialChanged{Scope: "u1", ProviderID: "openai", Kind: events.CredentialSaved},
			[]string{"providers.model-listings{provider=openai,scope=u1}"}},
		{"every provider of the scope", events.CredentialChanged{Scope: "u1", Kind: events.CredentialDeleted},
			[]string{"providers.model-listings{scope=u1}"}},
		{"scope erased", events.CredentialChanged{Scope: "u1", ProviderID: "openai", Kind: events.CredentialScopeErased},
			[]string{"providers.model-listings{scope=u1}"}},
		{"the deployment scope is named though empty", events.CredentialChanged{ProviderID: "openai", Kind: events.CredentialSaved},
			[]string{"providers.model-listings{provider=openai,scope=}"}},
		{"account erased", events.AccountErased{UserID: "u1", KeyScope: "u1"}, []string{"providers.model-listings{scope=u1}"}},
		{"org deleted", events.OrgMemberChanged{OrgID: "o1", Kind: events.OrgDeleted}, []string{"providers.model-listings{scope=org:o1}"}},
		{"member removed", events.OrgMemberChanged{OrgID: "o1", UserID: "u1", Kind: events.OrgMemberRemoved}, nil},
	})
}

func TestFlowCaches_OnlyTheDashboardsReplicate(t *testing.T) {
	backend := testutil.NewFakeBackend()
	authz := NewAuthzService(backend, nil)
	flows := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(backend), backend, authz, nil)
	dashboard := NewDashboardService(&gatedDashStore{FakeBackend: backend}, nil, flows, nil)
	chat := &ChatService{chatCtxCache: newChatContextCache()}
	bus, rec := replicatedBus(func(b *eventbus.Bus) {
		for _, s := range FlowCacheSubscribers(flows, dashboard, chat) {
			eventbus.Subscribe(b, eventbus.Sync, s.Name, s.Fn, s.Options...)
		}
	})
	both := []string{"dashboard.home{org=o1,owner=u1}", "dashboard.analytics{org=o1,owner=u1}"}
	replicatedCases(t, bus, rec, []replicatedCase{
		{"updated", events.FlowChanged{FlowID: "f1", OwnerID: "u1", OrgID: "o1", Kind: events.FlowUpdated}, both},
		{"created", events.FlowChanged{FlowID: "f1", OwnerID: "u1", OrgID: "o1", Kind: events.FlowCreated}, both},
		{"deleted", events.FlowChanged{FlowID: "f1", OwnerID: "u1", OrgID: "o1", Kind: events.FlowDeleted}, both},
		{"personal flow", events.FlowChanged{FlowID: "f1", OwnerID: "u1", Kind: events.FlowUpdated},
			[]string{"dashboard.home{owner=u1}", "dashboard.analytics{owner=u1}"}},
		{"sharing touches no dashboard", events.FlowChanged{FlowID: "f1", OwnerID: "u1", OrgID: "o1", Kind: events.FlowSharingChanged}, nil},
		{"nobody named: the local drop ignores it too", events.FlowChanged{FlowID: "f1", Kind: events.FlowUpdated}, nil},
	})
}
