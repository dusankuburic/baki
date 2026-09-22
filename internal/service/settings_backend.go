package service

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// settingsToModel and settingsFromModel convert between the storage-layer and
// domain representations of AppSettings. The two structs are kept separate so
// the storage package does not depend on pad-core/models, and a JSON round-trip
// is the conversion: the field sets are the same shape by construction, and a
// hand-written mapper would silently drop any field added to one side only.
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
	// backendSettingsTTL bounds how stale a cached read can be. Get() is on hot
	// paths (every analyze, every batch, every chat budget check) and has no
	// context to thread, so it cannot do a database round-trip per call. A
	// single-row lookup twice a minute is the cost of keeping those paths fast.
	//
	// A write through this provider invalidates the cache outright, so the TTL
	// only bounds how long a change made by something else — a direct database
	// edit, or another replica once this deployment scales out — goes
	// unnoticed.
	backendSettingsTTL = 30 * time.Second

	// backendSettingsErrorTTL is the retry backoff after a failed load. Without
	// it a database blip would make every Get() on every hot path attempt a
	// fresh query and wait out the timeout.
	backendSettingsErrorTTL = 5 * time.Second

	// backendSettingsTimeout bounds a single refresh.
	backendSettingsTimeout = 2 * time.Second
)

// BackendSettingsProvider is the cloud-mode SettingsProvider: deployment-wide
// settings live in the storage backend (Postgres app_settings), not on local
// disk.
//
// This closes a split-brain that the desktop-only store left behind. Cloud
// deployments have two writers of "deployment settings" —
// POST /api/system/settings (SystemService, which wrote to Postgres) and
// POST /api/analysis/rule/{enabled,config} (AnalysisService, which wrote to the
// local store) — and every *reader* on the analysis path went to the local
// store. In a container that store is recreated from defaults on every start,
// so configured rule overrides, the AI daily budget and the embedding provider
// were all read back as defaults no matter what an administrator had saved: an
// admin who raised the daily budget to $100 still had users cut off at the $5
// default. Both writers and all readers now go through here.
//
// Recent-file tracking is deliberately not implemented: it records local
// filesystem paths, and every route that produces or consumes them already
// refuses in cloud mode ("recent files are not available in cloud mode",
// "loading from local paths is not supported in cloud mode").
type BackendSettingsProvider struct {
	backend storageif.StorageBackend

	mu      sync.RWMutex
	cached  *models.AppSettings
	expires time.Time

	// refreshMu serialises refreshes so a cache expiry under concurrent load
	// produces one query rather than one per in-flight request.
	refreshMu sync.Mutex

	// now is overridable in tests.
	now func() time.Time
}

// NewBackendSettingsProvider builds the provider and primes its cache. A failed
// initial load is not fatal: the provider serves defaults and retries, because
// refusing to construct would turn a transient database blip at startup into a
// boot failure that the readiness probe already covers more gracefully.
func NewBackendSettingsProvider(ctx context.Context, backend storageif.StorageBackend) *BackendSettingsProvider {
	p := &BackendSettingsProvider{
		backend: backend,
		cached:  models.DefaultSettings(),
		now:     time.Now,
	}
	if err := p.refresh(ctx); err != nil {
		logger.Warn("settings: initial load from backend failed, serving defaults until it succeeds", "error", err)
	}
	return p
}

// refresh loads settings from the backend and replaces the cache.
func (p *BackendSettingsProvider) refresh(ctx context.Context) error {
	loadCtx, cancel := context.WithTimeout(ctx, backendSettingsTimeout)
	defer cancel()

	// A detached context on purpose: b.query(ctx) joins the caller's RLS
	// transaction when one is open, and a settings read must not depend on a
	// request transaction that may still roll back. The settings tables carry
	// no RLS policies, so nothing is lost by reading outside it.
	stored, err := p.backend.LoadSettings(loadCtx)
	if err != nil {
		p.mu.Lock()
		p.expires = p.now().Add(backendSettingsErrorTTL)
		p.mu.Unlock()
		return err
	}

	p.mu.Lock()
	p.cached = settingsToModel(stored)
	p.expires = p.now().Add(backendSettingsTTL)
	p.mu.Unlock()
	return nil
}

// Get returns the current deployment settings. It never blocks on the database
// for longer than backendSettingsTimeout, and on failure serves the last value
// it did manage to read (or defaults, if it never has) rather than failing the
// caller — every caller is on a path where returning no settings at all would
// be worse than returning slightly stale ones.
func (p *BackendSettingsProvider) Get() *models.AppSettings {
	p.mu.RLock()
	fresh := p.now().Before(p.expires)
	cached := p.cached
	p.mu.RUnlock()
	if fresh {
		return cached.Clone()
	}

	p.refreshMu.Lock()
	defer p.refreshMu.Unlock()

	// Re-check: another goroutine may have refreshed while we waited.
	p.mu.RLock()
	fresh = p.now().Before(p.expires)
	p.mu.RUnlock()
	if !fresh {
		if err := p.refresh(context.Background()); err != nil {
			logger.Warn("settings: backend load failed, serving last known values", "error", err)
		}
	}

	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.cached.Clone()
}

// Update persists settings through the caller's context, so the write joins the
// request's RLS transaction and is rolled back with it if the request later
// fails (see the note at the top of internal/storage/database/postgres_settings.go).
//
// The cache is INVALIDATED rather than set to the written value, precisely
// because of that rollback: optimistically caching a write that the
// transaction later discards would serve a value that was never committed
// until the TTL expired. Invalidating costs one extra read on the next Get —
// settings writes are rare administrative actions — and is always consistent
// with what is actually in the database.
func (p *BackendSettingsProvider) Update(ctx context.Context, updated models.AppSettings) error {
	if err := p.backend.SaveSettings(ctx, settingsFromModel(&updated)); err != nil {
		return err
	}

	p.mu.Lock()
	p.expires = time.Time{} // force a reload on the next Get
	p.mu.Unlock()
	return nil
}

// AddRecentFile, RemoveRecentFile and ClearRecentFiles are no-ops in cloud
// mode. Recent files are server-local filesystem paths, which a browser client
// cannot act on; GET /api/flow/recent and POST /api/flow/load-path both already
// return 403 in cloud mode, so nothing reachable here produces or consumes
// them.
func (p *BackendSettingsProvider) AddRecentFile(string, int64) error { return nil }
func (p *BackendSettingsProvider) RemoveRecentFile(string) error     { return nil }
func (p *BackendSettingsProvider) ClearRecentFiles() error           { return nil }
