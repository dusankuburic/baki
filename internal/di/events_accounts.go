package di

import (
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
)

// SubscribeSettingsAndCredentialCaches registers the caches derived from settings, credentials, accounts,
// organisations and knowledge documents on the event bus (docs/adr/0009-domain-events.md). The deployment
// settings cache subscribes before the rule profiles, whose next resolve reads it. Only the cloud's settings
// provider caches (the desktop's file store reads through).
func SubscribeSettingsAndCredentialCaches(bus *eventbus.Bus, settings service.SettingsProvider, profiles *service.RuleProfileResolver, providers *service.ProviderService, knowledge *rag.KnowledgeService) {
	if p, ok := settings.(*service.BackendSettingsProvider); ok {
		service.SubscribeSettingsCache(bus, p)
	}
	service.SubscribeRuleProfiles(bus, profiles)
	service.SubscribeModelCache(bus, providers)
	rag.SubscribeCorpusCache(bus, knowledge)
	rag.SubscribeEmbedderCache(bus, knowledge)
}
