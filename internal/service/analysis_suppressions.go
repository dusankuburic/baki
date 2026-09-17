package service

import (
	"context"
	"fmt"

	"pad-core/analyzer"
	"pad-core/models"
)

// ruleSetFor resolves which rules run, and with which settings, for doc: the
// owning org's rule profile when a resolver is wired (cloud), else the
// deployment settings plus custom rules. Shared by analyzeFlow and
// SuppressionInventory so the staleness audit judges a directive against the
// same rule set that produced the report — auditing with the global defaults
// would flag a directive for an org-disabled rule as live, or an org custom
// rule's directive as stale.
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

// SuppressionInventory lists every inline `# pad-ignore` directive in doc and
// marks the stale ones (masking nothing today). The analyzer has computed this
// since the CLI's `bakicli suppressions`, but no route exposed it, so the app
// could write suppressions into a flow and never list or audit them.
func (s *AnalysisService) SuppressionInventory(ctx context.Context, doc *models.FlowDocument) ([]analyzer.SuppressionEntry, error) {
	if doc == nil {
		return nil, fmt.Errorf("%w: no flow loaded", ErrInvalidInput)
	}
	settings, rules := s.ruleSetFor(ctx, doc)
	entries := analyzer.SuppressionInventory(doc, rules, settings)
	if entries == nil {
		// Encode as [] so clients never have to special-case null.
		entries = []analyzer.SuppressionEntry{}
	}
	return entries, nil
}
