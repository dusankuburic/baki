package di

import (
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/cache"
)

// ProvideFlowService builds the FlowService announcing its flow changes on the bus (an option, not a parameter:
// NewFlowService has about 55 test call sites).
func ProvideFlowService(notifier service.EventNotifier, settings service.SettingsProvider, docs service.DocumentProvider, backend storageif.StorageBackend, authz *service.AuthzService, astCache cache.Cache, bus *eventbus.Bus) *service.FlowService {
	return service.NewFlowService(notifier, settings, docs, backend, authz, astCache, service.WithFlowEvents(bus))
}

// SubscribeFlowCaches keeps the caches derived from flows coherent with FlowChanged, on the sync tier: each drop
// runs when the change is published and again once its transaction has committed or rolled back.
func SubscribeFlowCaches(bus *eventbus.Bus, flows *service.FlowService, dashboard *service.DashboardService, chat *service.ChatService) {
	for _, s := range service.FlowCacheSubscribers(flows, dashboard, chat) {
		eventbus.Subscribe(bus, eventbus.Sync, s.Name, s.Fn)
	}
}
