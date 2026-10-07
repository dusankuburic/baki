package service

import (
	"context"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// TriageService owns a flow's persistent, team-shared finding triage state and its accepted-findings
// baseline, plus what the triage and comment notifications read and write: the finding's assignee,
// the personal in-app alert, and the user lookups behind the alert and email copy. Authorization is
// the caller's. Its writes publish FindingStatusChanged, and a new assignment FindingAssigned.
type TriageService struct {
	store  storageif.TriageStore
	alerts storageif.GovernanceAlertStore
	users  storageif.UserStore
	bus    *eventbus.Bus
}

// NewTriageService builds the service; bus carries its events (nil publishes nothing).
func NewTriageService(store storageif.TriageStore, alerts storageif.GovernanceAlertStore, users storageif.UserStore, bus *eventbus.Bus) *TriageService {
	return &TriageService{store: store, alerts: alerts, users: users, bus: bus}
}

// FlowRef is the flow a triage or comment write belongs to, as the caller's authorization check read it: the
// events the write publishes name its owner, organisation and name from it, without a read of their own.
type FlowRef struct {
	ID      string
	Name    string
	OwnerID string
	OrgID   string
}

// FlowRefOf is the reference of a stored flow header; nil gives an empty one.
func FlowRefOf(h *storageif.FlowDocument) FlowRef {
	if h == nil {
		return FlowRef{}
	}
	return FlowRef{ID: h.ID, Name: h.Name, OwnerID: h.OwnerID, OrgID: h.OrganizationID}
}

// statusChanged announces that findings of the flow changed state (findingKey "": several, or the baseline).
func (s *TriageService) statusChanged(ctx context.Context, flow FlowRef, flowID, findingKey string) {
	s.bus.Publish(ctx, events.FindingStatusChanged{FlowID: flowID, FindingKey: findingKey, OwnerID: flow.OwnerID, OrgID: flow.OrgID})
}

// ListStatuses returns the flow's persisted triage states.
func (s *TriageService) ListStatuses(ctx context.Context, flowID string) ([]*storageif.FindingStatus, error) {
	return s.store.ListFindingStatuses(ctx, flowID)
}

// SetStatus upserts one finding's triage state. An assignee other than the finding's previous one is announced
// as FindingAssigned (st.UpdatedBy assigned it), which alerts and emails them once the write commits; re-sending
// the same assignee with a status change notifies no one.
func (s *TriageService) SetStatus(ctx context.Context, flow FlowRef, st *storageif.FindingStatus) error {
	prior := s.Assignee(ctx, st.FlowID, st.FindingKey)
	if err := s.store.SetFindingStatus(ctx, st); err != nil {
		return err
	}
	s.statusChanged(ctx, flow, st.FlowID, st.FindingKey)
	if st.AssigneeID != "" && st.AssigneeID != prior {
		s.bus.Publish(ctx, events.FindingAssigned{
			FlowID: st.FlowID, FlowName: flow.Name, OwnerID: flow.OwnerID, OrgID: flow.OrgID,
			FindingKey: st.FindingKey, RuleID: st.RuleID, AssigneeID: st.AssigneeID, AssignerID: st.UpdatedBy,
		})
	}
	return nil
}

// SetStatusBatch upserts many findings of one flow atomically: all items commit or none. A batch notifies no
// assignee.
func (s *TriageService) SetStatusBatch(ctx context.Context, flow FlowRef, userID string, items []*storageif.FindingStatus) error {
	if err := s.store.BatchSetFindingStatus(ctx, flow.ID, userID, items); err != nil {
		return err
	}
	s.statusChanged(ctx, flow, flow.ID, "")
	return nil
}

// ClearStatus resets one finding to "open".
func (s *TriageService) ClearStatus(ctx context.Context, flow FlowRef, findingKey string) error {
	if err := s.store.DeleteFindingStatus(ctx, flow.ID, findingKey); err != nil {
		return err
	}
	s.statusChanged(ctx, flow, flow.ID, findingKey)
	return nil
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
func (s *TriageService) SetBaseline(ctx context.Context, flow FlowRef, b *storageif.FlowBaseline) error {
	if err := s.store.SetFlowBaseline(ctx, b); err != nil {
		return err
	}
	s.statusChanged(ctx, flow, b.FlowID, "")
	return nil
}

// ClearBaseline removes the flow's baseline.
func (s *TriageService) ClearBaseline(ctx context.Context, flow FlowRef) error {
	if err := s.store.ClearFlowBaseline(ctx, flow.ID); err != nil {
		return err
	}
	s.statusChanged(ctx, flow, flow.ID, "")
	return nil
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
