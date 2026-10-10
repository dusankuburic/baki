package service

import (
	"context"
	"fmt"

	"pad-core/analyzer"
	"pad-core/models"
)

// ruleSetFor resolves which rules run, and with which settings, for doc: the owning
// org's rule profile when a resolver is wired (cloud), else the deployment settings plus
// custom rules. Shared by analyzeFlow and SuppressionInventory so the staleness audit
// judges a directive against the same rule set that produced the report (auditing with
// the global defaults would flag an org-disabled rule's directive as live, an org custom
// rule's as stale).
func (s *AnalysisService) ruleSetFor(ctx context.Context, doc *models.FlowDocument) (*models.AppSettings, []analyzer.Rule) {
	p := s.profileFor(ctx, doc)
	return p.Settings, p.Rules
}

// profileFor is the rule profile ctx's reader analyzes doc under: the org's, resolved, or without a resolver the
// deployment settings and custom rules.
func (s *AnalysisService) profileFor(ctx context.Context, doc *models.FlowDocument) *RuleProfile {
	if s.profiles != nil {
		return s.profiles.Resolve(ctx, doc.OrganizationID)
	}
	rules := analyzer.AllRules()
	if len(s.customRules) > 0 {
		rules = append(rules, s.customRules...)
	}
	return &RuleProfile{Settings: s.settings.Get(), Rules: rules}
}

// SuppressionInventory lists every inline `# pad-ignore` directive in doc and marks the
// stale ones (masking nothing today); the analyzer computed it since the CLI's
// `bakicli suppressions`, but no route exposed it — flows could carry unaudited suppressions.
func (s *AnalysisService) SuppressionInventory(ctx context.Context, doc *models.FlowDocument) ([]analyzer.SuppressionEntry, error) {
	if doc == nil {
		return nil, fmt.Errorf("%w: no flow loaded", ErrInvalidInput)
	}
	settings, rules := s.ruleSetFor(ctx, doc)
	// Cached per tenant: the inventory is a function of the document and its rule profile, so repeat views are
	// served from the analyzer's cache instead of re-running the entire rule set.
	entries := s.analyzer.SuppressionInventory(analyzer.Input{Doc: doc, Rules: rules, Settings: settings, Tenant: s.cacheTenant(ctx, doc)})
	if entries == nil {
		// Encode as [] so clients never have to special-case null.
		entries = []analyzer.SuppressionEntry{}
	}
	return entries, nil
}
