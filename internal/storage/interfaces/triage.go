package interfaces

import (
	"context"
	"time"
)

// TriageStore covers finding triage state (status overrides) and per-flow accepted-findings baselines. The writes need FlowEdit, which the caller checks: row-level security scopes these rows to the flow's visibility only (ADR 0013).
type TriageStore interface {
	// SetFindingStatus upserts the triage state for one finding (keyed by FlowID + FindingKey).
	SetFindingStatus(ctx context.Context, st *FindingStatus) error
	// BatchSetFindingStatus upserts triage state for one flow's findings atomically (all or none); userID scopes RLS ("" only in non-authenticated local mode).
	BatchSetFindingStatus(ctx context.Context, flowID, userID string, items []*FindingStatus) error
	// ListFindingStatuses returns all persisted triage states for a flow; never nil.
	ListFindingStatuses(ctx context.Context, flowID string) ([]*FindingStatus, error)
	// GetFindingStatus reads one finding's triage state, or (nil, nil) when none is persisted (implicitly "open").
	GetFindingStatus(ctx context.Context, flowID, findingKey string) (*FindingStatus, error)
	// DeleteFindingStatus removes triage state for one finding, resetting it to "open"; missing records are not an error.
	DeleteFindingStatus(ctx context.Context, flowID, findingKey string) error
	// GetFlowBaseline returns the flow's accepted-findings baseline, or (nil, nil) when none is set.
	GetFlowBaseline(ctx context.Context, flowID string) (*FlowBaseline, error)
	// GetFlowBaselinesBatch resolves many flows' baselines in one query; map omits flows without one, never nil. A system read for the governance scanner, which runs without an RLS scope (callers pinned by TestArch_SystemReadsPinned).
	GetFlowBaselinesBatch(ctx context.Context, flowIDs []string) (map[string]*FlowBaseline, error)
	// SetFlowBaseline replaces the flow's baseline (one baseline per flow).
	SetFlowBaseline(ctx context.Context, b *FlowBaseline) error
	// ClearFlowBaseline removes the flow's baseline. Idempotent.
	ClearFlowBaseline(ctx context.Context, flowID string) error
}

// FindingStatus is the team-shared triage state keyed by flow + content-derived finding key ("ruleID:blockID"), so state survives re-analysis.
type FindingStatus struct {
	FlowID        string    `json:"flowId"`
	FindingKey    string    `json:"findingKey"`
	RuleID        string    `json:"ruleId,omitempty"`
	Status        string    `json:"status"` // open, acknowledged, in_progress, resolved, suppressed
	Justification string    `json:"justification,omitempty"`
	AssigneeID    string    `json:"assigneeId,omitempty"`
	UpdatedBy     string    `json:"updatedBy,omitempty"`
	UpdatedAt     time.Time `json:"updatedAt"`
}

// FlowBaseline is the set of finding keys accepted as a flow's baseline: in-set = "known", else "new since baseline" — the ratcheting/gating basis.
type FlowBaseline struct {
	FlowID    string    `json:"flowId"`
	Keys      []string  `json:"keys"`
	CreatedBy string    `json:"createdBy,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}
