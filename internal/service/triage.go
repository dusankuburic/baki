package service

import (
	"context"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// TriageService owns a flow's persistent, team-shared finding triage state and its accepted-findings
// baseline, plus what the triage and comment notifications read and write: the finding's assignee,
// the personal in-app alert, and the user lookups behind the alert and email copy. Authorization is
// the caller's.
type TriageService struct {
	store  storageif.TriageStore
	alerts storageif.GovernanceAlertStore
	users  storageif.UserStore
}

func NewTriageService(store storageif.TriageStore, alerts storageif.GovernanceAlertStore, users storageif.UserStore) *TriageService {
	return &TriageService{store: store, alerts: alerts, users: users}
}

// ListStatuses returns the flow's persisted triage states.
func (s *TriageService) ListStatuses(ctx context.Context, flowID string) ([]*storageif.FindingStatus, error) {
	return s.store.ListFindingStatuses(ctx, flowID)
}

// SetStatus upserts one finding's triage state.
func (s *TriageService) SetStatus(ctx context.Context, st *storageif.FindingStatus) error {
	return s.store.SetFindingStatus(ctx, st)
}

// SetStatusBatch upserts many findings of one flow atomically: all items commit or none.
func (s *TriageService) SetStatusBatch(ctx context.Context, flowID, userID string, items []*storageif.FindingStatus) error {
	return s.store.BatchSetFindingStatus(ctx, flowID, userID, items)
}

// ClearStatus resets one finding to "open".
func (s *TriageService) ClearStatus(ctx context.Context, flowID, findingKey string) error {
	return s.store.DeleteFindingStatus(ctx, flowID, findingKey)
}

// Assignee returns the finding's current assignee, "" when it has no state or the lookup fails (the
// callers then notify, the safe direction). One primary-key row when the store offers it, else the
// flow's whole list.
func (s *TriageService) Assignee(ctx context.Context, flowID, findingKey string) string {
	if g, ok := s.store.(storageif.FindingStatusGetter); ok {
		if st, err := g.GetFindingStatus(ctx, flowID, findingKey); err == nil && st != nil {
			return st.AssigneeID
		}
		return ""
	}
	if statuses, err := s.store.ListFindingStatuses(ctx, flowID); err == nil {
		for _, st := range statuses {
			if st.FindingKey == findingKey {
				return st.AssigneeID
			}
		}
	}
	return ""
}

// Baseline returns the flow's baseline, nil when none is set.
func (s *TriageService) Baseline(ctx context.Context, flowID string) (*storageif.FlowBaseline, error) {
	return s.store.GetFlowBaseline(ctx, flowID)
}

// SetBaseline replaces the flow's baseline.
func (s *TriageService) SetBaseline(ctx context.Context, b *storageif.FlowBaseline) error {
	return s.store.SetFlowBaseline(ctx, b)
}

// ClearBaseline removes the flow's baseline.
func (s *TriageService) ClearBaseline(ctx context.Context, flowID string) error {
	return s.store.ClearFlowBaseline(ctx, flowID)
}

// RecordAlert persists a personal (targeted) in-app alert stamped now. The ID embeds the (flow, type,
// key, target) tuple, so a retry is a no-op.
func (s *TriageService) RecordAlert(ctx context.Context, a *storageif.GovernanceAlert) error {
	a.CreatedAt = time.Now().UTC()
	return s.alerts.RecordGovernanceAlert(ctx, a)
}

// DisplayName resolves a name for alert copy ("assigned by X"): the user's display name, else their
// email, else fallback (the claims email, no lookup), else "a teammate".
func (s *TriageService) DisplayName(ctx context.Context, userID, fallback string) string {
	if userID != "" {
		if u, err := s.users.LoadUserByID(ctx, userID); err == nil && u != nil {
			if u.DisplayName != "" {
				return u.DisplayName
			}
			if u.Email != "" {
				return u.Email
			}
		}
	}
	if fallback != "" {
		return fallback
	}
	return "a teammate"
}

// Recipient returns the user an email notification goes to, nil when the user is unknown, the
// lookup fails, or they have no email address.
func (s *TriageService) Recipient(ctx context.Context, userID string) *storageif.User {
	u, err := s.users.LoadUserByID(ctx, userID)
	if err != nil || u == nil || u.Email == "" {
		return nil
	}
	return u
}
