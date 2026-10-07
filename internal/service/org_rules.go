package service

import (
	"context"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// OrgRuleService owns an organisation's custom analyzer rules (R4) and the org settings their
// export reads. A rule change drops the org's cached rule profile so the next analysis re-resolves;
// without it the change waits out the resolver's TTL.
type OrgRuleService struct {
	rules    storageif.OrgCustomRuleStore
	settings storageif.AppSettingsStore
	// profiles may be nil (tests); Invalidate is nil-safe.
	profiles *RuleProfileResolver
}

func NewOrgRuleService(rules storageif.OrgCustomRuleStore, settings storageif.AppSettingsStore, profiles *RuleProfileResolver) *OrgRuleService {
	return &OrgRuleService{rules: rules, settings: settings, profiles: profiles}
}

// List returns the org's rules; enabledOnly is what the analysis path compiles.
func (s *OrgRuleService) List(ctx context.Context, orgID string, enabledOnly bool) ([]*storageif.OrgCustomRule, error) {
	return s.rules.ListOrgCustomRules(ctx, orgID, enabledOnly)
}

// Save upserts rule and, once stored, invalidates its org's rule profile.
func (s *OrgRuleService) Save(ctx context.Context, rule *storageif.OrgCustomRule) error {
	if err := s.rules.SaveOrgCustomRule(ctx, rule); err != nil {
		return err
	}
	s.profiles.Invalidate(rule.OrgID)
	return nil
}

// Delete removes one of orgID's rules and, once removed, invalidates the org's rule profile.
func (s *OrgRuleService) Delete(ctx context.Context, orgID, id string) error {
	if err := s.rules.DeleteOrgCustomRule(ctx, orgID, id); err != nil {
		return err
	}
	s.profiles.Invalidate(orgID)
	return nil
}

// OrgSettings returns the org's settings layer (its per-rule profile lives in Analysis.Rules).
func (s *OrgRuleService) OrgSettings(ctx context.Context, orgID string) (*storageif.AppSettings, error) {
	return s.settings.LoadOrgSettings(ctx, orgID)
}
