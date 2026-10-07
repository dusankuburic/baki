package service

import (
	"context"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// GovernanceAlertService is the caller's side of the governance alerts inbox (the notifications
// bell): the continuous-governance scanner writes alerts, users read, mark and dismiss them. Every
// read and write is RLS-scoped to the caller's visible flows and their own targeted alerts.
type GovernanceAlertService struct {
	store storageif.GovernanceAlertStore
}

func NewGovernanceAlertService(store storageif.GovernanceAlertStore) *GovernanceAlertService {
	return &GovernanceAlertService{store: store}
}

func (s *GovernanceAlertService) List(ctx context.Context, filter storageif.GovernanceAlertFilter) ([]*storageif.GovernanceAlert, error) {
	return s.store.ListGovernanceAlerts(ctx, filter)
}

// UnreadCount is the badge count for userID: team-wide plus their targeted alerts.
func (s *GovernanceAlertService) UnreadCount(ctx context.Context, userID string) (int, error) {
	return s.store.UnreadGovernanceAlertCountFor(ctx, userID)
}

func (s *GovernanceAlertService) MarkAllRead(ctx context.Context, userID string) error {
	return s.store.MarkAllGovernanceAlertsRead(ctx, userID)
}

func (s *GovernanceAlertService) Dismiss(ctx context.Context, userID, alertID string) error {
	return s.store.DismissGovernanceAlert(ctx, userID, alertID)
}

// Clear permanently deletes the caller's visible dismissed alerts.
func (s *GovernanceAlertService) Clear(ctx context.Context, userID string) error {
	return s.store.ClearGovernanceAlerts(ctx, userID)
}
