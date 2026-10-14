package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/cache"
	"pad-core/logger"
	"pad-core/models"
)

// deploymentSettingsCache is BackendSettingsProvider's cache (ADR 0017): one value, the deployment's settings row. Its
// storage is the provider's own (it keeps serving the last value read when a refresh fails), so the spec only
// counts: a hit is a fresh Get, a miss a Get that refreshed, a store a refresh no invalidation overlapped.
var deploymentSettingsCache = cache.Declare("settings.deployment",
	cache.Global("one deployment-wide settings row, the same for every caller"),
	cache.Shared(cache.MechanismInvalidation, "a write on any replica drops it on every replica once committed; the 30 s TTL bounds a lost message"))

// settingsToModel and settingsFromModel convert between the storage-layer and domain representations via a
// JSON round-trip: the structs are separate so storage does not depend on pad-core/models, and a hand-written
// mapper would silently drop any field added to one side only.
func settingsToModel(is *storageif.AppSettings) *models.AppSettings {
	if is == nil {
		return models.DefaultSettings()
	}
	data, err := json.Marshal(is)
	if err != nil {
		logger.Warn("settings: marshal storage settings", "error", err)
		return models.DefaultSettings()
	}
	var m models.AppSettings
	if err := json.Unmarshal(data, &m); err != nil {
		logger.Warn("settings: unmarshal into model settings", "error", err)
		return models.DefaultSettings()
	}
	return &m
}

func settingsFromModel(m *models.AppSettings) *storageif.AppSettings {
	if m == nil {
		return nil
	}
	data, err := json.Marshal(m)
	if err != nil {
		logger.Warn("settings: marshal model settings", "error", err)
		return nil
	}
	var is storageif.AppSettings
	if err := json.Unmarshal(data, &is); err != nil {
		logger.Warn("settings: unmarshal into storage settings", "error", err)
		return nil
	}
	return &is
}

const (
	// backendSettingsTTL bounds how stale a cached read can be: Get() is on hot paths with no context to thread,
	// so it cannot round-trip the database per call. A write through this provider invalidates the cache
	// outright, so the TTL only bounds how long another writer's change (direct DB edit, another replica) goes
	// unnoticed.
	backendSettingsTTL = 30 * time.Second

	// backendSettingsErrorTTL is the retry backoff after a failed load, so a database blip does not make every
	// Get() re-query and wait out the timeout.
	backendSettingsErrorTTL = 5 * time.Second

	// backendSettingsTimeout bounds a single refresh.
	backendSettingsTimeout = 2 * time.Second
)

// BackendSettingsProvider is the cloud-mode SettingsProvider: deployment-wide settings
// live in the storage backend (Postgres, ADR 0012), not on local disk — closing the
// split-brain the desktop-only store left behind (two writers, system settings → Postgres
// and rule config → a local store recreated from defaults on every container start, with
// every analysis reader on the local store, so an admin who raised the daily budget to
// $100 still had users cut off at the $5 default). Both writers and all readers now go
// through here. Recent-file tracking is deliberately not implemented: it records local
// filesystem paths, and every route that produces or consumes them already refuses in
// cloud mode.
type BackendSettingsProvider struct {
	backend storageif.StorageBackend

	mu      sync.RWMutex
	cached  *models.AppSettings
	expires time.Time

	// refreshMu serialises refreshes so a cache expiry under concurrent load produces one query.
	refreshMu sync.Mutex
	// gen fences refreshes against Invalidate: a refresh that read before a write committed must not store.
	gen eventbus.Gen[string]

	// now is overridable in tests.
	now func() time.Time
}

// NewBackendSettingsProvider builds the provider and primes its cache; a failed initial load is not fatal —
// the provider serves defaults and retries rather than turning a startup blip into a boot failure.
func NewBackendSettingsProvider(ctx context.Context, backend storageif.StorageBackend) *BackendSettingsProvider {
	p := &BackendSettingsProvider{
		backend: backend,
		cached:  models.DefaultSettings(),
		now:     time.Now,
	}
	if _, err := p.refresh(ctx); err != nil {
		logger.Warn("settings: initial load from backend failed, serving defaults until it succeeds", "error", err)
	}
	return p
}

// settingsGenKey is the one key of the provider's generation: the deployment settings are a singleton.
const settingsGenKey = ""

// refresh loads settings from the backend and replaces the cache, unless an invalidation ran since the read
// began (the value read may predate a write that has committed since); it returns what it read either way.
func (p *BackendSettingsProvider) refresh(ctx context.Context) (*models.AppSettings, error) {
	loadCtx, cancel := context.WithTimeout(ctx, backendSettingsTimeout)
	defer cancel()

	captured := p.gen.Load(settingsGenKey)
	// A detached context on purpose: a settings read must not depend on a request transaction that may still
	// roll back, and the settings tables carry no RLS policies, so nothing is lost by reading outside it.
	stored, err := p.backend.LoadSettings(loadCtx)
	if err != nil {
		p.mu.Lock()
		p.expires = p.now().Add(backendSettingsErrorTTL)
		p.mu.Unlock()
		return nil, err
	}

	read := settingsToModel(stored)
	p.mu.Lock()
	if p.gen.FillOK(ctx, settingsGenKey, captured) {
		p.cached = read
		p.expires = p.now().Add(backendSettingsTTL)
		deploymentSettingsCache.RecordStore()
	}
	p.mu.Unlock()
	return read, nil
}

// Invalidate forces the next Get to reload and refuses a refresh already in flight.
func (p *BackendSettingsProvider) Invalidate() {
	p.mu.Lock()
	p.gen.Bump(settingsGenKey)
	p.expires = time.Time{}
	p.mu.Unlock()
}

// SubscribeSettingsCache drops p's cached deployment settings on every deployment SettingsChanged: once when
// published, again when the write's transaction ends, so a refresh between the write and its commit cannot
// leave the old value cached.
func SubscribeSettingsCache(bus *eventbus.Bus, p *BackendSettingsProvider) {
	eventbus.Subscribe(bus, eventbus.Sync, "settings-cache", func(_ context.Context, e events.SettingsChanged) {
		if e.Scope == events.DeploymentSettings {
			p.Invalidate()
		}
	}, eventbus.Replicated(func(e events.SettingsChanged) []eventbus.Invalidation {
		if e.Scope != events.DeploymentSettings {
			return nil
		}
		return []eventbus.Invalidation{{Key: "settings.deployment"}}
	}, "settings.deployment"))
	// A peer's deployment write (or a resync after a gap): the same drop, whole (the settings are one row).
	bus.OnRemote("settings.deployment", func(eventbus.Scope) { p.Invalidate() })
}

// Get returns the current deployment settings: never blocks longer than backendSettingsTimeout, and on failure
// serves the last value read (or defaults) rather than failing the caller — every caller is on a path where no
// settings at all would be worse than slightly stale ones.
func (p *BackendSettingsProvider) Get() *models.AppSettings {
	p.mu.RLock()
	fresh := p.now().Before(p.expires)
	cached := p.cached
	p.mu.RUnlock()
	if fresh {
		deploymentSettingsCache.RecordHit()
		return cached.Clone()
	}

	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()

	// Re-check: another goroutine may have refreshed while we waited.
	p.mu.RLock()
	fresh = p.now().Before(p.expires)
	p.mu.RUnlock()
	if fresh {
		deploymentSettingsCache.RecordHit()
	} else {
		deploymentSettingsCache.RecordMiss()
		read, err := p.refresh(context.Background())
		if err != nil {
			logger.Warn("settings: backend load failed, serving last known values", "error", err)
		} else if read != nil {
			// What was read is current even when an invalidation kept it out of the cache.
			return read.Clone()
		}
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cached.Clone()
}

// Update persists settings through the caller's context, so the write joins the request's RLS transaction and
// rolls back with it (see postgres_settings.go).
// The cache is INVALIDATED rather than set to the written value, precisely because of that rollback:
// optimistically caching a write the transaction later discards would serve a never-committed value until TTL.
// Invalidating costs one extra read on the next Get — writes are rare administrative actions.
func (p *BackendSettingsProvider) Update(ctx context.Context, updated models.AppSettings) error {
	if err := p.backend.SaveSettings(ctx, settingsFromModel(&updated)); err != nil {
		return err
	}
	// The writer's SettingsChanged invalidates again once the transaction has ended.
	p.Invalidate()
	return nil
}

// AddRecentFile, RemoveRecentFile and ClearRecentFiles are no-ops in cloud mode: recent files are server-local
// paths, and every route that produces or consumes them already returns 403 there.
func (p *BackendSettingsProvider) AddRecentFile(string, int64) error { return nil }
func (p *BackendSettingsProvider) RemoveRecentFile(string) error     { return nil }
func (p *BackendSettingsProvider) ClearRecentFiles() error           { return nil }
