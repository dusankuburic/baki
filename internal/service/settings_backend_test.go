package service

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// countingBackend records how often settings are read and written, and can be
// made to fail on demand.
type countingBackend struct {
	testutil.FakeBackend

	mu       sync.Mutex
	loads    atomic.Int32
	saves    atomic.Int32
	loadErr  error
	saveErr  error
	lastSave context.Context
}

func (b *countingBackend) LoadSettings(ctx context.Context) (*storageif.AppSettings, error) {
	b.loads.Add(1)
	b.mu.Lock()
	err := b.loadErr
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return b.FakeBackend.LoadSettings(ctx)
}

func (b *countingBackend) SaveSettings(ctx context.Context, s *storageif.AppSettings) error {
	b.saves.Add(1)
	b.mu.Lock()
	err := b.saveErr
	b.lastSave = ctx
	b.mu.Unlock()
	if err != nil {
		return err
	}
	return b.FakeBackend.SaveSettings(ctx, s)
}

func (b *countingBackend) setLoadErr(err error) {
	b.mu.Lock()
	b.loadErr = err
	b.mu.Unlock()
}

// storedWithBudget seeds the backend with settings carrying a distinctive
// daily budget, so a read that returns the DEFAULT value is unmistakable.
func storedWithBudget(t *testing.T, b *countingBackend, budget float64) {
	t.Helper()
	s := models.DefaultSettings()
	s.AI.DailyBudget = budget
	if err := b.FakeBackend.SaveSettings(context.Background(), settingsFromModel(s)); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
}

func TestBackendSettingsProvider_ReadsFromBackend(t *testing.T) {
	t.Parallel()

	b := &countingBackend{}
	storedWithBudget(t, b, 100)

	p := NewBackendSettingsProvider(context.Background(), b)
	got := p.Get()
	if got.AI.DailyBudget != 100 {
		t.Fatalf("DailyBudget = %v, want 100 (the stored value, not the %v default)",
			got.AI.DailyBudget, models.DefaultSettings().AI.DailyBudget)
	}
}

func TestBackendSettingsProvider_CachesReads(t *testing.T) {
	t.Parallel()

	b := &countingBackend{}
	storedWithBudget(t, b, 100)

	p := NewBackendSettingsProvider(context.Background(), b)
	before := b.loads.Load() // the constructor primes the cache
	for range 50 {
		_ = p.Get()
	}
	if after := b.loads.Load(); after != before {
		t.Errorf("Get() hit the backend %d extra times; it is called on every analyze and must serve from cache", after-before)
	}
}

func TestBackendSettingsProvider_ReloadsAfterTTL(t *testing.T) {
	t.Parallel()

	b := &countingBackend{}
	storedWithBudget(t, b, 100)

	now := time.Now()
	p := NewBackendSettingsProvider(context.Background(), b)
	p.now = func() time.Time { return now }

	_ = p.Get()
	loadsAfterFirst := b.loads.Load()

	// Something else changes the row; the cache must not hold the old value
	// forever.
	storedWithBudget(t, b, 250)
	now = now.Add(backendSettingsTTL + time.Second)

	if got := p.Get().AI.DailyBudget; got != 250 {
		t.Errorf("after TTL expiry DailyBudget = %v, want 250", got)
	}
	if b.loads.Load() <= loadsAfterFirst {
		t.Error("expected a reload after the TTL expired")
	}
}

func TestBackendSettingsProvider_UpdatePersistsAndIsVisible(t *testing.T) {
	t.Parallel()

	b := &countingBackend{}
	storedWithBudget(t, b, 5)
	p := NewBackendSettingsProvider(context.Background(), b)

	updated := *p.Get()
	updated.AI.DailyBudget = 100
	if err := p.Update(context.Background(), updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	if b.saves.Load() == 0 {
		t.Fatal("Update did not write to the backend")
	}
	// This is the bug in one assertion: an administrator raises the budget and
	// the value the analysis path reads back must be the new one, not the
	// default it used to serve from a local file.
	if got := p.Get().AI.DailyBudget; got != 100 {
		t.Errorf("DailyBudget after Update = %v, want 100", got)
	}
}

func TestBackendSettingsProvider_UpdateUsesCallerContext(t *testing.T) {
	t.Parallel()

	b := &countingBackend{}
	p := NewBackendSettingsProvider(context.Background(), b)

	type ctxKey struct{}
	ctx := context.WithValue(context.Background(), ctxKey{}, "request-scoped")
	if err := p.Update(ctx, *models.DefaultSettings()); err != nil {
		t.Fatalf("Update: %v", err)
	}

	b.mu.Lock()
	got := b.lastSave
	b.mu.Unlock()
	// The caller's context must reach the backend: in cloud mode it carries the
	// request's RLS transaction, so a request that writes settings and then
	// fails has the write rolled back with it.
	if got == nil || got.Value(ctxKey{}) != "request-scoped" {
		t.Error("SaveSettings did not receive the caller's context")
	}
}

func TestBackendSettingsProvider_UpdateInvalidatesRatherThanCaching(t *testing.T) {
	t.Parallel()

	b := &countingBackend{}
	storedWithBudget(t, b, 5)
	p := NewBackendSettingsProvider(context.Background(), b)

	loadsBefore := b.loads.Load()
	updated := *p.Get()
	updated.AI.DailyBudget = 100
	if err := p.Update(context.Background(), updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	_ = p.Get()

	// Caching the written value optimistically would serve a value the
	// transaction may still roll back. Re-reading is the price of always
	// agreeing with what is actually committed.
	if b.loads.Load() <= loadsBefore {
		t.Error("Update should invalidate the cache so the next Get re-reads from the backend")
	}
}

func TestBackendSettingsProvider_UpdateErrorIsReported(t *testing.T) {
	t.Parallel()

	b := &countingBackend{saveErr: errors.New("boom")}
	p := NewBackendSettingsProvider(context.Background(), b)

	if err := p.Update(context.Background(), *models.DefaultSettings()); err == nil {
		t.Error("expected Update to surface the backend error")
	}
}

func TestBackendSettingsProvider_ServesStaleOnBackendFailure(t *testing.T) {
	t.Parallel()

	b := &countingBackend{}
	storedWithBudget(t, b, 100)

	now := time.Now()
	p := NewBackendSettingsProvider(context.Background(), b)
	p.now = func() time.Time { return now }
	_ = p.Get()

	b.setLoadErr(errors.New("database unavailable"))
	now = now.Add(backendSettingsTTL + time.Second)

	// Every caller is on a path where no settings at all is worse than
	// slightly stale ones — analysis must not start failing because the
	// settings row was briefly unreadable.
	if got := p.Get().AI.DailyBudget; got != 100 {
		t.Errorf("DailyBudget during backend outage = %v, want the last known 100", got)
	}
}

func TestBackendSettingsProvider_InitialLoadFailureServesDefaults(t *testing.T) {
	t.Parallel()

	b := &countingBackend{loadErr: errors.New("database unavailable")}

	// A transient database problem at startup must not prevent construction:
	// readiness already gates traffic, and failing here would turn a blip into
	// a boot failure.
	p := NewBackendSettingsProvider(context.Background(), b)
	if got := p.Get(); got == nil {
		t.Fatal("Get returned nil after a failed initial load")
	}
}

func TestBackendSettingsProvider_RecentFilesAreNoOps(t *testing.T) {
	t.Parallel()

	b := &countingBackend{}
	p := NewBackendSettingsProvider(context.Background(), b)

	// Recent files are server-local filesystem paths; the routes that produce
	// and consume them already return 403 in cloud mode.
	if err := p.AddRecentFile("/srv/flow.txt", 10); err != nil {
		t.Errorf("AddRecentFile: %v", err)
	}
	if err := p.RemoveRecentFile("/srv/flow.txt"); err != nil {
		t.Errorf("RemoveRecentFile: %v", err)
	}
	if err := p.ClearRecentFiles(); err != nil {
		t.Errorf("ClearRecentFiles: %v", err)
	}
	if b.saves.Load() != 0 {
		t.Error("recent-file calls must not write deployment settings to the backend")
	}
}

func TestBackendSettingsProvider_ConcurrentGetRefreshesOnce(t *testing.T) {
	t.Parallel()

	b := &countingBackend{}
	storedWithBudget(t, b, 100)

	now := time.Now()
	p := NewBackendSettingsProvider(context.Background(), b)
	p.now = func() time.Time { return now }
	_ = p.Get()

	loadsBefore := b.loads.Load()
	now = now.Add(backendSettingsTTL + time.Second)

	var wg sync.WaitGroup
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = p.Get()
		}()
	}
	wg.Wait()

	// An expiry under load must collapse into one query, not one per in-flight
	// request.
	if extra := b.loads.Load() - loadsBefore; extra != 1 {
		t.Errorf("expiry under concurrent load produced %d reloads, want 1", extra)
	}
}

// TestSystemServiceAndAnalysisAgreeOnDeploymentSettings is the regression test for the
// split-brain itself: settings saved through SystemService (which backs
// POST /api/system/settings) must be what the analysis path reads back. Before the fix
// these were two different stores in cloud mode — SystemService wrote to Postgres while
// every analysis reader consulted a local file recreated from defaults on each container
// start — so an administrator's saved configuration had no effect on analysis at all.
func TestSystemServiceAndAnalysisAgreeOnDeploymentSettings(t *testing.T) {
	t.Parallel()

	b := &countingBackend{}
	provider := NewBackendSettingsProvider(context.Background(), b)
	sys := NewSystemService(provider, nil, newTargetingNotifier(), b, "cloud")

	updated := *models.DefaultSettings()
	updated.AI.DailyBudget = 100
	if updated.Analysis.Rules == nil {
		updated.Analysis.Rules = map[string]models.RuleConfig{}
	}
	updated.Analysis.Rules["hardcoded-credential"] = models.RuleConfig{Enabled: false}

	if err := sys.UpdateSettings(context.Background(), updated); err != nil {
		t.Fatalf("UpdateSettings: %v", err)
	}

	// The reader used by AnalysisService, RuleProfileResolver, ChatService and
	// the knowledge base.
	got := provider.Get()
	if got.AI.DailyBudget != 100 {
		t.Errorf("analysis path sees DailyBudget %v, admin saved 100", got.AI.DailyBudget)
	}
	if rc, ok := got.Analysis.Rules["hardcoded-credential"]; !ok || rc.Enabled {
		t.Errorf("analysis path does not see the disabled rule the admin saved: %+v", got.Analysis.Rules)
	}

	// And SystemService reads back the same values it wrote.
	readBack, err := sys.GetSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSettings: %v", err)
	}
	if readBack.AI.DailyBudget != 100 {
		t.Errorf("GetSettings returned DailyBudget %v, want 100", readBack.AI.DailyBudget)
	}
}
