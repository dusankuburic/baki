package di

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/invalidation"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/redisreplica"
	"pad-core/analyzer"
	"pad-core/cache"
	"pad-core/models"
)

// Two replicas of the app's cache stack over one database and one Redis: each has its own bus with the production
// subscribers (the same registrars the fx graph invokes), its own services and the real invalidation fan-out. A
// write on one replica must not leave the other serving the old value until the cache's TTL.

type stackReplica struct {
	bus      *eventbus.Bus
	settings *service.BackendSettingsProvider
	profiles *service.RuleProfileResolver
	dash     *service.DashboardService
}

func startStack(t *testing.T, r *redisreplica.Replica, backend *testutil.FakeBackend) *stackReplica {
	t.Helper()
	ctx := context.Background()
	s := &stackReplica{bus: eventbus.New(nil, time.Second)}
	s.settings = service.NewBackendSettingsProvider(ctx, backend)
	s.profiles = service.NewRuleProfileResolver(backend, s.settings, nil)
	s.dash = service.NewDashboardService(backend, nil, nil, nil)
	SubscribeFlowCaches(s.bus, nil, s.dash, nil)
	SubscribeSettingsAndCredentialCaches(s.bus, s.settings, s.profiles, nil, nil)
	stop := invalidation.Install(r.Client, r.Health, s.bus, r.ID, invalidation.WithHeartbeat(time.Hour))
	t.Cleanup(func() {
		sctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		stop(sctx)
	})
	return s
}

func startStacks(t *testing.T, backend *testutil.FakeBackend) (*redisreplica.Cluster, *stackReplica, *stackReplica) {
	t.Helper()
	c := redisreplica.New(t, 2)
	resyncs := resyncCount("subscribe")
	a, b := startStack(t, c.Replicas[0], backend), startStack(t, c.Replicas[1], backend)
	channel := "baki:inval"
	redisreplica.Eventually(t, 5*time.Second, "both replicas subscribed", func() bool { return c.Redis.PubSubNumSub(channel)[channel] == 2 })
	// The resync every subscription starts with drops the replicas' caches: let both finish before the test fills any.
	redisreplica.Eventually(t, 5*time.Second, "the first resync of each replica", func() bool { return resyncCount("subscribe")-resyncs >= 2 })
	time.Sleep(50 * time.Millisecond) // the appliers run just after the counter moves
	return c, a, b
}

// resyncCount reads pad_inval_resync_total{reason}, which internal/invalidation owns.
func resyncCount(reason string) float64 {
	families, err := metrics.Gather()
	if err != nil {
		return -1
	}
	for _, f := range families {
		if f.GetName() != "pad_inval_resync_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "reason" && l.GetValue() == reason {
					return m.GetCounter().GetValue()
				}
			}
		}
	}
	return 0
}

func budgetOf(s *service.BackendSettingsProvider) float64 { return s.Get().AI.DailyBudget }

// An admin lowers a deployment setting on A: B enforces it as soon as the message lands, not 30 s later; a write
// that rolled back changes nothing on B.
func TestTwoReplicas_ADeploymentSettingChangeReachesThePeer(t *testing.T) {
	backend := testutil.NewFakeBackend()
	seed := models.DefaultSettings()
	seed.AI.DailyBudget = 100
	if err := backend.SaveSettings(context.Background(), settingsToStorage(t, seed)); err != nil {
		t.Fatal(err)
	}
	_, a, b := startStacks(t, backend)
	if budgetOf(b.settings) != 100 {
		t.Fatalf("premise: B reads the seeded budget, got %v", budgetOf(b.settings))
	}

	// A change that rolls back: the writer's own cache drops it (rollback pass), the peer is told nothing.
	next := models.DefaultSettings()
	next.AI.DailyBudget = 5
	ctx, reg := txhook.With(context.Background())
	if err := a.settings.Update(ctx, *next); err != nil {
		t.Fatal(err)
	}
	a.bus.Publish(ctx, events.SettingsChanged{Scope: events.DeploymentSettings})
	reg.Abort()
	// The database kept the committed row; the fake backend has no transaction, so undo the write by hand.
	if err := backend.SaveSettings(context.Background(), settingsToStorage(t, seed)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(200 * time.Millisecond)
	if got := budgetOf(b.settings); got != 100 {
		t.Errorf("B reads %v after a rolled-back change, want 100", got)
	}

	// The committed change reaches B well inside the 30 s TTL.
	ctx, reg = txhook.With(context.Background())
	if err := a.settings.Update(ctx, *next); err != nil {
		t.Fatal(err)
	}
	a.bus.Publish(ctx, events.SettingsChanged{Scope: events.DeploymentSettings})
	if got := budgetOf(b.settings); got != 100 {
		t.Fatalf("B reads %v before the commit, want the old 100", got)
	}
	reg.Commit(nil)
	redisreplica.Eventually(t, 5*time.Second, "B to read the new budget", func() bool { return budgetOf(b.settings) == 5 })
}

func settingsToStorage(t *testing.T, s *models.AppSettings) *storageif.AppSettings {
	t.Helper()
	raw, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var out storageif.AppSettings
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

// An org admin adds a custom rule on A: B's cached profile of the org is dropped, so its next analysis runs the
// rule.
func TestTwoReplicas_AnOrgRuleChangeReachesThePeer(t *testing.T) {
	backend := testutil.NewFakeBackend()
	backend.SeedOrgs("org-a") // a rule needs its organisation
	_, a, b := startStacks(t, backend)
	ctx := context.Background()
	hasRule := func(r *service.RuleProfileResolver) bool {
		for _, rule := range r.Resolve(ctx, "org-a").Rules {
			if rule.ID() == "late-rule" {
				return true
			}
		}
		return false
	}
	if hasRule(b.profiles) {
		t.Fatal("premise: the rule does not exist yet")
	}
	cfg, _ := json.Marshal(analyzer.CustomRuleConfig{ID: "late-rule", Name: "late", Severity: "warning", Category: "Style", RawTypeMatch: "^SET$"})
	if err := backend.SaveOrgCustomRule(ctx, &storageif.OrgCustomRule{ID: "org-a-late", OrgID: "org-a", RuleID: "late-rule", Config: cfg, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if hasRule(b.profiles) {
		t.Fatal("premise: B serves its cached profile (30 s TTL) until told")
	}
	a.bus.Publish(ctx, events.SettingsChanged{Scope: events.OrgSettings, ID: "org-a"})
	redisreplica.Eventually(t, 5*time.Second, "B to resolve the org's new rule", func() bool { return hasRule(b.profiles) })
}

// A flow change on A drops the owner's cached home view on B.
func TestTwoReplicas_AFlowChangeDropsThePeersHomeView(t *testing.T) {
	backend := testutil.NewFakeBackend()
	_, a, b := startStacks(t, backend)
	ctx := context.Background()
	misses := func() uint64 {
		for _, s := range cache.Specs() {
			if s.Name == "dashboard.home" {
				return s.Stats.Misses
			}
		}
		t.Fatal("dashboard.home is not declared")
		return 0
	}
	b.dash.BuildHome(ctx, "u1")
	before := misses()
	b.dash.BuildHome(ctx, "u1")
	if misses() != before {
		t.Fatal("premise: the second load of the home view is served from B's cache")
	}

	a.bus.Publish(ctx, events.FlowChanged{FlowID: "f1", OwnerID: "u1", Kind: events.FlowUpdated})

	redisreplica.Eventually(t, 5*time.Second, "B to rebuild u1's home view", func() bool {
		b.dash.BuildHome(ctx, "u1")
		return misses() > before
	})
}
