package service

import (
	"context"
	"fmt"
	"slices"

	"pad-analyzer/internal/ai"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// ProviderCodeNotAllowed is the client code for a provider the policy refused (ADR 0028): the deployment
// disabled it (AI.Providers[id].Enabled) or the organisation's allowed-provider list (AI.AllowedProviders of
// the org's settings) leaves it out. On a chat stream's error event, as ProviderTestResult.code, and 400 on
// the knowledge routes.
const ProviderCodeNotAllowed = "PROVIDER_NOT_ALLOWED"

// ProviderCodePolicyUnavailable is the client code for a policy that could not be read
// (ai.ErrProviderPolicyUnavailable): 503 on the knowledge routes, the code on a connection test's result.
const ProviderCodePolicyUnavailable = "PROVIDER_POLICY_UNAVAILABLE"

// OrgSettingsReader reads an organisation's settings document (the storage backend).
type OrgSettingsReader interface {
	LoadOrgSettings(ctx context.Context, orgID string) (*storageif.AppSettings, error)
}

// ProviderPolicy is the ai.ProviderPolicy the factory consults before any key is read: the deployment's
// per-provider Enabled flag for every build, and the organisation's allowed-provider list for a build that
// names an org (its shared credential, or a member's own key on the org's flow or corpus). A refusal wraps
// ai.ErrProviderNotAllowed with copy the client may show; an unreadable org settings document is reported as
// what it is (the chat path words it as the generic setup failure), never as "allowed".
type ProviderPolicy struct {
	settings SettingsProvider
	orgs     OrgSettingsReader
}

// NewProviderPolicy builds the policy. A nil settings provider applies no deployment rule; a nil org reader
// (the desktop: no org store) applies no org list.
func NewProviderPolicy(settings SettingsProvider, orgs OrgSettingsReader) *ProviderPolicy {
	return &ProviderPolicy{settings: settings, orgs: orgs}
}

// Allowed implements ai.ProviderPolicy.
func (p *ProviderPolicy) Allowed(ctx context.Context, _ string, orgID, providerID string) error {
	if p.settings != nil && providerDisabledByPolicy(p.settings.Get(), providerID) {
		return fmt.Errorf("%w: %s is disabled in this deployment's AI settings — pick another provider or ask your administrator to re-enable it", ai.ErrProviderNotAllowed, providerID)
	}
	if orgID == "" || p.orgs == nil {
		return nil
	}
	orgSettings, err := p.orgs.LoadOrgSettings(ctx, orgID)
	if err != nil {
		// The store's text (hosts, users) is the operator's; the client gets the sentinel's fixed copy.
		logger.Warn("provider policy: could not read the organisation's settings", "org_id", orgID, "provider", providerID, "error", err)
		return ai.ErrProviderPolicyUnavailable
	}
	if orgSettings == nil || orgAllowsProvider(orgSettings.AI.AllowedProviders, providerID) {
		return nil
	}
	return fmt.Errorf("%w: %s is not on your organisation's list of allowed AI providers — pick one that is, or ask an organisation admin to allow it", ai.ErrProviderNotAllowed, providerID)
}

// orgAllowsProvider applies an organisation's allowed-provider list: an empty list (unset, or saved empty —
// the wire cannot tell them apart) allows every provider the deployment offers.
func orgAllowsProvider(allowed []string, providerID string) bool {
	return len(allowed) == 0 || slices.Contains(allowed, providerID)
}

// providerDisabledByPolicy fires only on an unambiguous disable; absent entries read as enabled. The flag is a
// plain bool that no UI writes and the defaults ship false for every provider but the default active one, so
// a stored false is "unset" unless the defaults enable the provider.
func providerDisabledByPolicy(settings *models.AppSettings, providerID string) bool {
	if settings == nil {
		return false
	}
	pc, ok := settings.AI.Providers[providerID]
	if !ok || pc.Enabled {
		return false
	}
	return models.DefaultSettings().AI.Providers[providerID].Enabled
}
