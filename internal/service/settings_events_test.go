package service

import (
	"context"
	"sync"
	"testing"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/txhook"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// txSettingsBackend models a settings row behind a transaction: SaveSettings stages the write, commit makes it
// what LoadSettings (a read on another connection) sees. onLoad, when set, runs inside a read after it has read.
type txSettingsBackend struct {
	testutil.FakeBackend

	mu        sync.Mutex
	committed *storageif.AppSettings
	pending   *storageif.AppSettings
	onLoad    func()
}

func (b *txSettingsBackend) LoadSettings(context.Context) (*storageif.AppSettings, error) {
	b.mu.Lock()
	read, hook := b.committed, b.onLoad
	b.onLoad = nil
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	return read, nil
}

func (b *txSettingsBackend) SaveSettings(_ context.Context, s *storageif.AppSettings) error {
	b.mu.Lock()
	b.pending = s
	b.mu.Unlock()
	return nil
}

func (b *txSettingsBackend) commit() {
	b.mu.Lock()
	b.committed, b.pending = b.pending, nil
	b.mu.Unlock()
}

func withBudget(budget float64) *models.AppSettings {
	s := models.DefaultSettings()
	s.AI.DailyBudget = budget
	return s
}

// A read between a settings write and its commit used to cache the old row for the whole TTL: the write had
// already invalidated, the reader then refilled from the committed (old) value. SettingsChanged invalidates again
// once the transaction has ended, so the next read sees the committed write.
func TestSettingsCache_RefreshBeforeCommitIsDroppedAfterIt(t *testing.T) {
	for _, outcome := range []string{"commit", "rollback"} {
		t.Run(outcome, func(t *testing.T) {
			b := &txSettingsBackend{committed: settingsFromModel(withBudget(5))}
			p := NewBackendSettingsProvider(context.Background(), b)
			bus := eventbus.New(nil, 0)
			SubscribeSettingsCache(bus, p)
			sys := NewSystemService(p, nil, NilNotifier{}, nil, "cloud", nil, bus)

			ctx, reg := txhook.With(context.Background())
			if err := sys.UpdateSettings(ctx, *withBudget(100)); err != nil {
				t.Fatal(err)
			}
			if got := p.Get().AI.DailyBudget; got != 5 {
				t.Fatalf("read before the commit = %v, want the committed 5", got)
			}
			want := 5.0
			if outcome == "commit" {
				b.commit()
				reg.Commit(nil)
				want = 100
			} else {
				reg.Abort()
			}
			if got := p.Get().AI.DailyBudget; got != want {
				t.Errorf("read after the %s = %v, want %v", outcome, got, want)
			}
		})
	}
}

// A refresh that read before an invalidation (a write committed while it was reading) must not cache what it
// read: the next Get reloads.
func TestSettingsCache_RefreshRacingAnInvalidationIsNotCached(t *testing.T) {
	b := &txSettingsBackend{committed: settingsFromModel(withBudget(5))}
	p := NewBackendSettingsProvider(context.Background(), b)
	p.Invalidate()
	b.onLoad = func() {
		b.mu.Lock()
		b.committed = settingsFromModel(withBudget(100))
		b.mu.Unlock()
		p.Invalidate()
	}
	if got := p.Get().AI.DailyBudget; got != 5 {
		t.Fatalf("the racing read = %v, want what it read (5)", got)
	}
	if got := p.Get().AI.DailyBudget; got != 100 {
		t.Errorf("after the racing read = %v, want the reload's 100 (the stale read was cached)", got)
	}
}

// orgReadHookBackend runs onOrgRead inside the org-settings read of a resolve, after it has read.
type orgReadHookBackend struct {
	*testutil.FakeBackend
	onOrgRead func()
}

func (b *orgReadHookBackend) LoadOrgSettings(ctx context.Context, orgID string) (*storageif.AppSettings, error) {
	s, err := b.FakeBackend.LoadOrgSettings(ctx, orgID)
	if hook := b.onOrgRead; hook != nil {
		b.onOrgRead = nil
		hook()
	}
	return s, err
}

// A resolve that read before an invalidation must not cache its profile (it would hold the old rules for the
// TTL); a deployment-wide invalidation fences every org's resolve.
func TestRuleProfileResolver_ResolveRacingAnInvalidationIsNotCached(t *testing.T) {
	for _, invalidate := range []string{"org-1", ""} {
		b := &orgReadHookBackend{FakeBackend: testutil.NewFakeBackend()}
		r := NewRuleProfileResolver(b, deploymentSettings(nil), nil)
		b.onOrgRead = func() { r.Invalidate(invalidate) }
		if p := r.Resolve(context.Background(), "org-1"); p == nil {
			t.Fatal("Resolve returned nil")
		}
		if r.cachedFor("org-1") != nil {
			t.Errorf("Invalidate(%q) during the resolve: its profile was cached", invalidate)
		}
		r.Resolve(context.Background(), "org-1")
		if r.cachedFor("org-1") == nil {
			t.Errorf("Invalidate(%q): the next resolve was not cached", invalidate)
		}
	}
}

// A transaction that announced a change of its own may read its uncommitted writes: its resolve is not cached.
func TestRuleProfileResolver_NoFillFromAPublishingTransaction(t *testing.T) {
	b := testutil.NewFakeBackend()
	r := NewRuleProfileResolver(b, deploymentSettings(nil), nil)
	bus := eventbus.New(nil, 0)
	ctx, reg := txhook.With(context.Background())
	bus.Publish(ctx, events.SettingsChanged{Scope: events.UserSettings, ID: "u1"})
	r.Resolve(ctx, "org-1")
	if r.cachedFor("org-1") != nil {
		t.Error("a profile resolved inside a publishing transaction was cached")
	}
	reg.Commit(nil)
}

// The deployment rule toggles announce SettingsChanged{DeploymentSettings}: every org's profile (they inherit
// the deployment settings) is dropped. They used to drop none, so a toggle waited out the TTL.
func TestDeploymentRuleToggles_InvalidateEveryProfile(t *testing.T) {
	b := testutil.NewFakeBackend()
	settings := deploymentSettings(nil)
	r := NewRuleProfileResolver(b, settings, nil)
	bus := eventbus.New(nil, 0)
	SubscribeRuleProfiles(bus, r)
	rec := eventbus.NewRecorder(bus)
	analysis, _ := NewAnalysisService(NilNotifier{}, settings, nil, AnalysisOptions{Profiles: r, Bus: bus})
	ctx := context.Background()

	for _, toggle := range []func() error{
		func() error { return analysis.SetRuleEnabled(ctx, "dead-code", false) },
		func() error {
			return analysis.UpdateRuleConfig(ctx, "dead-code", models.RuleConfig{Enabled: true, Severity: "error"})
		},
	} {
		r.Resolve(ctx, "")
		r.Resolve(ctx, "org-1")
		if err := toggle(); err != nil {
			t.Fatal(err)
		}
		if r.cachedFor("") != nil || r.cachedFor("org-1") != nil {
			t.Error("a deployment rule toggle left a cached profile")
		}
	}
	if got := eventbus.Recorded[events.SettingsChanged](rec); len(got) != 2 || got[0].Scope != events.DeploymentSettings || got[1].Scope != events.DeploymentSettings {
		t.Errorf("announced %+v, want two deployment SettingsChanged", got)
	}
	if r.Resolve(ctx, "org-1").Settings.Analysis.Rules["dead-code"].Severity != "error" {
		t.Error("the next resolve does not see the toggled rule")
	}
}

// SystemService announces each settings write with its scope and id; an org's settings change drops its rule
// profile and no other org's; a failed write announces nothing.
func TestSystemService_AnnouncesSettingsChanges(t *testing.T) {
	b := testutil.NewFakeBackend()
	// Settings rows reference their user and org.
	b.Users = map[string]*storageif.User{"u1": {ID: "u1"}}
	b.Orgs = map[string]*storageif.Organisation{"org-1": {ID: "org-1"}}
	r := NewRuleProfileResolver(b, deploymentSettings(nil), nil)
	bus := eventbus.New(nil, 0)
	SubscribeRuleProfiles(bus, r)
	rec := eventbus.NewRecorder(bus)
	sys := NewSystemService(deploymentSettings(nil), nil, NilNotifier{}, b, "cloud", nil, bus)
	ctx := context.Background()

	r.Resolve(ctx, "org-1")
	r.Resolve(ctx, "org-2")
	if err := sys.UpdateOrgSettings(ctx, "org-1", *models.DefaultSettings(), OrgSettingsWrite{}); err != nil {
		t.Fatal(err)
	}
	if r.cachedFor("org-1") != nil || r.cachedFor("org-2") == nil {
		t.Error("an org settings change must drop that org's profile and only that one")
	}
	if err := sys.UpdateUserSettings(ctx, "u1", *models.DefaultSettings(), nil); err != nil {
		t.Fatal(err)
	}
	if err := sys.UpdateSettings(ctx, *models.DefaultSettings()); err != nil {
		t.Fatal(err)
	}
	want := []events.SettingsChanged{
		{Scope: events.OrgSettings, ID: "org-1"},
		{Scope: events.UserSettings, ID: "u1"},
		{Scope: events.DeploymentSettings},
	}
	got := eventbus.Recorded[events.SettingsChanged](rec)
	if len(got) != len(want) {
		t.Fatalf("announced %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("announcement %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if r.cachedFor("org-2") != nil {
		t.Error("a deployment settings change must drop every org's profile")
	}

	rec.Reset()
	failing := NewSystemService(nil, nil, NilNotifier{}, nil, "cloud", nil, bus)
	if err := failing.UpdateSettings(ctx, *models.DefaultSettings()); err == nil {
		t.Fatal("want the missing-store error")
	}
	if got := rec.Events(); len(got) != 0 {
		t.Errorf("a failed write announced %v", got)
	}
}

// A deleted organisation's profile goes with it.
func TestRuleProfiles_OrgDeletedDropsTheProfile(t *testing.T) {
	r := NewRuleProfileResolver(testutil.NewFakeBackend(), deploymentSettings(nil), nil)
	bus := eventbus.New(nil, 0)
	SubscribeRuleProfiles(bus, r)
	ctx := context.Background()
	r.Resolve(ctx, "org-1")
	r.Resolve(ctx, "org-2")
	bus.Publish(ctx, events.OrgMemberChanged{OrgID: "org-2", UserID: "u1", Kind: events.OrgMemberRemoved})
	bus.Publish(ctx, events.OrgMemberChanged{OrgID: "org-1", Kind: events.OrgDeleted})
	if r.cachedFor("org-1") != nil || r.cachedFor("org-2") == nil {
		t.Error("only the deleted organisation's profile must go")
	}
}
