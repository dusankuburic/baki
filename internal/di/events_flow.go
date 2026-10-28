package di

import (
	"path/filepath"

	"github.com/redis/go-redis/v9"

	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
)

// ProvideFlowService builds the FlowService announcing its flow changes on the bus and dropping the process
// analyzer's reports they make stale (options, not parameters: NewFlowService has about 55 test call sites).
func ProvideFlowService(cfg *config.Config, configDir string, notifier service.EventNotifier, settings service.SettingsProvider, docs service.DocumentProvider, backend storageif.StorageBackend, authz *service.AuthzService, astCache *service.ASTCache, bus *eventbus.Bus, a *analyzer.CachedAnalyzer, redisClient *redis.Client) *service.FlowService {
	opts := []service.FlowOption{service.WithFlowEvents(bus), service.WithAnalyzer(a)}
	// nil when PAD_REDIS_URL is unset: each replica remembers the flow ids of its own uploads.
	opts = append(opts, service.WithUploadIDs(redisClient))
	opts = append(opts, snapshotStoreOptions(capability.Of(cfg), backend)...)
	// A desktop flow's ring (a file or folder on this machine) is kept under the config directory, so the undo of a
	// fix outlives the process (R-approvals-c9); only desktop flows use it.
	if configDir != "" {
		opts = append(opts, service.WithDesktopSnapshotDir(filepath.Join(configDir, "snapshots")))
	}
	return service.NewFlowService(notifier, settings, docs, backend, authz, astCache, opts...)
}

// snapshotStoreOptions puts the undo ring of stored flows in the database where the deployment has a server store,
// so every replica sees the snapshots of the others; without one (the desktop) the service keeps its memory ring.
func snapshotStoreOptions(caps capability.Set, backend storageif.StorageBackend) []service.FlowOption {
	if !caps.Has(capability.ServerStore) || backend == nil {
		return nil
	}
	return []service.FlowOption{service.WithSnapshotStore(backend)}
}

// SubscribeFlowCaches keeps the caches derived from flows coherent with FlowChanged, on the sync tier: each drop
// runs when the change is published and again once its transaction has committed or rolled back.
func SubscribeFlowCaches(bus *eventbus.Bus, flows *service.FlowService, dashboard *service.DashboardService, chat *service.ChatService) {
	for _, s := range service.FlowCacheSubscribers(flows, dashboard, chat) {
		eventbus.Subscribe(bus, eventbus.Sync, s.Name, s.Fn, s.Options...)
	}
	// A peer's dashboard drop (a flow change or an analysis).
	service.SubscribeRemoteDashboard(bus, dashboard)
}
