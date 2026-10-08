package interfaces

import (
	"context"
	"encoding/json"
	"time"
)

// OrgCustomRule is one user-authored analyzer rule owned by an org. Config is a JSON-serialized analyzer.CustomRuleConfig carried opaquely; RuleID (the one on findings) is unique PER ORG.
type OrgCustomRule struct {
	ID        string          `json:"id"`
	OrgID     string          `json:"orgId"`
	RuleID    string          `json:"ruleId"`
	Config    json.RawMessage `json:"config"`
	Enabled   bool            `json:"enabled"`
	CreatedAt time.Time       `json:"createdAt"`
	UpdatedAt time.Time       `json:"updatedAt"`
}

// OrgCustomRuleStore covers per-org custom analyzer rules.
type OrgCustomRuleStore interface {
	// SaveOrgCustomRule upserts a rule; (org_id, rule_id) is unique — the same id under a different org is a distinct rule.
	SaveOrgCustomRule(ctx context.Context, rule *OrgCustomRule) error
	DeleteOrgCustomRule(ctx context.Context, orgID, id string) error
	// ListOrgCustomRules returns the org's rules; enabledOnly for the analysis path, which must not compile disabled rules.
	ListOrgCustomRules(ctx context.Context, orgID string, enabledOnly bool) ([]*OrgCustomRule, error)
}
