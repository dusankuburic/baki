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
	if s.profiles != nil {
		profile := s.profiles.Resolve(ctx, doc.OrganizationID)
		return profile.Settings, profile.Rules
	}
	rules := analyzer.AllRules()
	if len(s.customRules) > 0 {
		rules = append(rules, s.customRules...)
	}
	return s.settings.Get(), rules
}

// SuppressionInventory lists every inline `# pad-ignore` directive in doc and marks the
// stale ones (masking nothing today); the analyzer computed it since the CLI's
// `bakicli suppressions`, but no route exposed it — flows could carry unaudited suppressions.
func (s *AnalysisService) SuppressionInventory(ctx context.Context, doc *models.FlowDocument) ([]analyzer.SuppressionEntry, error) {
	if doc == nil {
		return nil, fmt.Errorf("%w: no flow loaded", ErrInvalidInput)
	}
	settings, rules := s.ruleSetFor(ctx, doc)
	// Cached variant: the inventory is a pure function of (content, rule set, settings),
	// so repeat views are served from the analyzer's content-keyed cache instead of
	// re-running the entire rule set.
	entries := analyzer.SuppressionInventoryCached(doc, rules, settings)
	if entries == nil {
		// Encode as [] so clients never have to special-case null.
		entries = []analyzer.SuppressionEntry{}
	}
	return entries, nil
}
