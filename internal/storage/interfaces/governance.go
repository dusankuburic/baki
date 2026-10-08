package interfaces

import (
	"context"
	"time"
)

// GovernanceAlertStore is the in-app inbox for continuous-governance scanner signals. Alerts are team-shared; the scanner writes without an RLS principal, users read/ack/dismiss through the API.
type GovernanceAlertStore interface {
	// RecordGovernanceAlert persists a new alert (ID and CreatedAt stamped by the scanner); a duplicate ID is a no-op (retry-safe).
	RecordGovernanceAlert(ctx context.Context, a *GovernanceAlert) error
	// ListGovernanceAlerts returns alerts visible to the caller, newest first, per the filter's pagination + include-dismissed flag; never nil.
	ListGovernanceAlerts(ctx context.Context, filter GovernanceAlertFilter) ([]*GovernanceAlert, error)
	// UnreadGovernanceAlertCount returns the number of visible unacknowledged alerts (read_at IS NULL and not dismissed).
	UnreadGovernanceAlertCount(ctx context.Context) (int, error)
	// UnreadGovernanceAlertCountFor scopes the badge count to one caller: team-wide plus their targeted alerts.
	UnreadGovernanceAlertCountFor(ctx context.Context, userID string) (int, error)
	// MarkGovernanceAlertRead stamps read_at = NOW() on one alert. Idempotent.
	MarkGovernanceAlertRead(ctx context.Context, userID, alertID string) error
	// MarkAllGovernanceAlertsRead stamps read_at = NOW() on every visible unread alert. Idempotent.
	MarkAllGovernanceAlertsRead(ctx context.Context, userID string) error
	// DismissGovernanceAlert stamps dismissed_at = NOW() on one alert (hidden from the default view, not deleted). Idempotent.
	DismissGovernanceAlert(ctx context.Context, userID, alertID string) error
	// ClearGovernanceAlerts permanently deletes the caller's visible dismissed alerts.
	ClearGovernanceAlerts(ctx context.Context, userID string) error
}

// GovernanceAlert is one in-app governance signal. Alerts are team-shared (RLS inherits flow visibility) and read/dismiss stamps are team-global — one ack clears all badges.
type GovernanceAlert struct {
	ID          string     `json:"id"`
	FlowID      string     `json:"flowId"`
	FlowName    string     `json:"flowName,omitempty"`
	OrgID       string     `json:"orgId,omitempty"`
	Type        string     `json:"type"` // "drift" | "health_regression" | "finding_assigned" | "comment_added"
	Title       string     `json:"title"`
	Message     string     `json:"message,omitempty"`
	Severity    string     `json:"severity"` // "error" | "warning"
	NewErrors   int        `json:"newErrors,omitempty"`
	NewWarnings int        `json:"newWarnings,omitempty"`
	HealthScore int        `json:"healthScore,omitempty"`
	PrevHealth  int        `json:"prevHealth,omitempty"`
	CreatedAt   time.Time  `json:"createdAt"`
	ReadAt      *time.Time `json:"readAt,omitempty"`
	DismissedAt *time.Time `json:"dismissedAt,omitempty"`
	// TargetUser is the delivery target for PERSONAL alerts (assignment/comment notifications): visible only to that user; empty = team-wide.
	TargetUser string `json:"targetUser,omitempty"`
}

// GovernanceAlertFilter narrows ListGovernanceAlerts: Limit/Offset paginate (<=0 ⇒ default cap); an alert matches UserID when TargetUser is empty (team-wide) OR equals it; empty = team-wide only.
type GovernanceAlertFilter struct {
	Limit            int
	Offset           int
	IncludeDismissed bool
	UserID           string
}
