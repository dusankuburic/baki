package service

import (
	"context"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// OrgRuleService owns an organisation's custom analyzer rules (R4) and the org settings their
// export reads. A rule change is announced as a change of the org's analysis configuration
// (SettingsChanged{OrgSettings}), so its cached rule profile drops and the next analysis
// re-resolves; without it the change waits out the resolver's TTL.
type OrgRuleService struct {
	rules    storageif.OrgCustomRuleStore
	settings storageif.AppSettingsStore
	// bus may be nil (tests): nothing is announced.
	bus *eventbus.Bus
}

func NewOrgRuleService(rules storageif.OrgCustomRuleStore, settings storageif.AppSettingsStore, bus *eventbus.Bus) *OrgRuleService {
	return &OrgRuleService{rules: rules, settings: settings, bus: bus}
}

// List returns the org's rules; enabledOnly is what the analysis path compiles.
func (s *OrgRuleService) List(ctx context.Context, orgID string, enabledOnly bool) ([]*storageif.OrgCustomRule, error) {
	return s.rules.ListOrgCustomRules(ctx, orgID, enabledOnly)
}

// Save upserts rule and, once stored, announces the org's changed analysis configuration.
func (s *OrgRuleService) Save(ctx context.Context, rule *storageif.OrgCustomRule) error {
	if err := s.rules.SaveOrgCustomRule(ctx, rule); err != nil {
		return err
	}
	s.bus.Publish(ctx, events.SettingsChanged{Scope: events.OrgSettings, ID: rule.OrgID})
	return nil
}

// Delete removes one of orgID's rules and, once removed, announces the org's changed analysis configuration.
func (s *OrgRuleService) Delete(ctx context.Context, orgID, id string) error {
	if err := s.rules.DeleteOrgCustomRule(ctx, orgID, id); err != nil {
		return err
	}
	s.bus.Publish(ctx, events.SettingsChanged{Scope: events.OrgSettings, ID: orgID})
	return nil
}

// OrgSettings returns the org's settings layer (its per-rule profile lives in Analysis.Rules).
func (s *OrgRuleService) OrgSettings(ctx context.Context, orgID string) (*storageif.AppSettings, error) {
	return s.settings.LoadOrgSettings(ctx, orgID)
}
