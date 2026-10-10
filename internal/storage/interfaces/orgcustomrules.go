package interfaces

import (
	"context"
	"encoding/json"
	"errors"
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

// ErrOrgCustomRuleRenamed is a save that names a stored row of the org under another rule id. A row keeps its rule
// id (findings, suppressions and settings entries carry it); a new id is a new rule.
var ErrOrgCustomRuleRenamed = errors.New("a saved custom rule's id cannot change")

// OrgCustomRuleStore covers per-org custom analyzer rules.
type OrgCustomRuleStore interface {
	// SaveOrgCustomRule upserts a rule; (org_id, rule_id) is unique — the same id under a different org is a distinct rule.
	// It sets rule.ID to the stored row's id. A row id of the org stored under another rule id is
	// ErrOrgCustomRuleRenamed and changes nothing; a row id of another org is ErrNotFound.
	SaveOrgCustomRule(ctx context.Context, rule *OrgCustomRule) error
	DeleteOrgCustomRule(ctx context.Context, orgID, id string) error
	// ListOrgCustomRules returns the org's rules; enabledOnly for the analysis path, which must not compile disabled rules.
	ListOrgCustomRules(ctx context.Context, orgID string, enabledOnly bool) ([]*OrgCustomRule, error)
}
