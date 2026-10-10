package interfaces

import (
	"context"
	"time"
)

// RuleNoticeKindNewRules is the kind of a "new rules" notice: rules newer than the flows' recorded rule sets report
// findings, which the scanner announces once instead of as drift or regression alerts (ADR 0016).
const RuleNoticeKindNewRules = "new_rules"

// The scopes of a notice: an organisation's flows, a user's own flows, or every flow of a deployment without
// accounts (scope id "").
const (
	RuleNoticeScopeOrg  = "org"
	RuleNoticeScopeUser = "user"
	RuleNoticeScopeApp  = "app"
)

// RuleNoticeID is the id of the notice of a scope and rule-set digest: one notice each, decided by the primary key.
func RuleNoticeID(scope, scopeID, ruleSetDigest string) string {
	return RuleNoticeKindNewRules + "|" + scope + "|" + scopeID + "|" + ruleSetDigest
}

// RuleNotice is one scope's notice for one rule set (RuleSetDigest, the analyses' current set): the rules newer than
// the scope's baselines or health snapshots that report findings, and in how many flows. Read and dismissed stamps
// are scope-wide, like an alert's.
type RuleNotice struct {
	ID            string     `json:"id"`
	Kind          string     `json:"kind"`
	Scope         string     `json:"scope"`
	ScopeID       string     `json:"scopeId"`
	RuleSetDigest string     `json:"ruleSetDigest"`
	RuleIDs       []string   `json:"ruleIds"` // sorted, distinct
	FlowCount     int        `json:"flowCount"`
	CreatedAt     time.Time  `json:"createdAt"`
	UpdatedAt     time.Time  `json:"updatedAt"`
	ReadAt        *time.Time `json:"readAt,omitempty"`
	DismissedAt   *time.Time `json:"dismissedAt,omitempty"`
}

// RuleNoticeStore keeps the scanner's "new rules" notices. Visibility mirrors rls_rule_notices_visible: an org notice
// is its members', a user notice that user's, and an app notice (written only without accounts) every caller's.
type RuleNoticeStore interface {
	// UpsertRuleNotices records one page's notices in one statement, a system write: a new id is inserted; a known
	// one gains the rule ids it lacks, keeps the larger flow count and keeps its read and dismissed stamps. A notice
	// whose user or organisation does not exist is skipped. Returns the inserted ids, so each notice is announced
	// once across restarts and replicas.
	UpsertRuleNotices(ctx context.Context, notices []*RuleNotice) (inserted []string, err error)
	// ListRuleNotices returns the caller's visible notices that are not dismissed, newest first, at most limit
	// (<= 0: 10); never nil.
	ListRuleNotices(ctx context.Context, userID string, limit int) ([]*RuleNotice, error)
	// UnreadRuleNoticeCount counts the caller's visible notices that are neither read nor dismissed.
	UnreadRuleNoticeCount(ctx context.Context, userID string) (int, error)
	// MarkAllRuleNoticesRead stamps read_at on the caller's visible unread notices. Idempotent.
	MarkAllRuleNoticesRead(ctx context.Context, userID string) error
	// DismissRuleNotice stamps dismissed_at on one of the caller's visible notices. Idempotent; an id the caller
	// cannot see is a no-op.
	DismissRuleNotice(ctx context.Context, userID, id string) error
}
