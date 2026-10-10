package service

import (
	"context"
	"errors"
	"fmt"
	"strings"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// GovernanceInboxStore is what the inbox reads and writes: the scanner's alerts and its "new rules" notices.
type GovernanceInboxStore interface {
	storageif.GovernanceAlertStore
	storageif.RuleNoticeStore
}

// GovernanceAlertService is the caller's side of the governance alerts inbox (the notifications
// bell): the continuous-governance scanner writes alerts and "new rules" notices, users read, mark
// and dismiss them. Every read and write is scoped to the caller: alerts to their visible flows and
// their own targeted alerts, notices to their organisations and themselves.
type GovernanceAlertService struct {
	store GovernanceInboxStore
}

func NewGovernanceAlertService(store GovernanceInboxStore) *GovernanceAlertService {
	return &GovernanceAlertService{store: store}
}

// listedNotices caps the notices a list shows above the alerts.
const listedNotices = 10

// noticeIDPrefix tells a notice's id (storageif.RuleNoticeID) from an alert's.
const noticeIDPrefix = storageif.RuleNoticeKindNewRules + "|"

// List returns the caller's alerts; the first page (offset 0) starts with their notices that are not
// dismissed, newest first, as "new_rules" entries without a flow.
func (s *GovernanceAlertService) List(ctx context.Context, filter storageif.GovernanceAlertFilter) ([]*storageif.GovernanceAlert, error) {
	alerts, err := s.store.ListGovernanceAlerts(ctx, filter)
	if err != nil || filter.Offset > 0 {
		return alerts, err
	}
	notices, err := s.store.ListRuleNotices(ctx, filter.UserID, listedNotices)
	if err != nil {
		return nil, err
	}
	if len(notices) == 0 {
		return alerts, nil
	}
	out := make([]*storageif.GovernanceAlert, 0, len(notices)+len(alerts))
	for _, n := range notices {
		out = append(out, noticeAlert(n))
	}
	return append(out, alerts...), nil
}

// noticeAlert is a notice as an inbox entry. The server writes its title and message, so a client that
// does not know the type still reads it.
func noticeAlert(n *storageif.RuleNotice) *storageif.GovernanceAlert {
	a := &storageif.GovernanceAlert{
		ID:          n.ID,
		Type:        n.Kind,
		Title:       fmt.Sprintf("%d new %s findings", len(n.RuleIDs), noticePlural(len(n.RuleIDs), "rule reports", "rules report")),
		Message:     noticeMessage(n),
		Severity:    "warning",
		CreatedAt:   n.CreatedAt,
		ReadAt:      n.ReadAt,
		DismissedAt: n.DismissedAt,
		RuleIDs:     n.RuleIDs,
		Scope:       n.Scope,
		FlowCount:   n.FlowCount,
	}
	if n.Scope == storageif.RuleNoticeScopeOrg {
		a.OrgID = n.ScopeID
	}
	return a
}

func noticeMessage(n *storageif.RuleNotice) string {
	return fmt.Sprintf("%s: findings in %d %s whose baseline or last recorded analysis predates %s. These findings "+
		"raise no drift or health alerts until the flows are baselined or analyzed again.",
		strings.Join(n.RuleIDs, ", "), n.FlowCount, noticePlural(n.FlowCount, "flow", "flows"),
		noticePlural(len(n.RuleIDs), "this rule", "these rules"))
}

func noticePlural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// UnreadCount is the badge count for userID: team-wide plus their targeted alerts, and their unread notices.
func (s *GovernanceAlertService) UnreadCount(ctx context.Context, userID string) (int, error) {
	alerts, err := s.store.UnreadGovernanceAlertCountFor(ctx, userID)
	if err != nil {
		return 0, err
	}
	notices, err := s.store.UnreadRuleNoticeCount(ctx, userID)
	if err != nil {
		return 0, err
	}
	return alerts + notices, nil
}

func (s *GovernanceAlertService) MarkAllRead(ctx context.Context, userID string) error {
	return errors.Join(s.store.MarkAllGovernanceAlertsRead(ctx, userID), s.store.MarkAllRuleNoticesRead(ctx, userID))
}

// Dismiss hides one alert, or one notice when id is a notice's.
func (s *GovernanceAlertService) Dismiss(ctx context.Context, userID, alertID string) error {
	if strings.HasPrefix(alertID, noticeIDPrefix) {
		return s.store.DismissRuleNotice(ctx, userID, alertID)
	}
	return s.store.DismissGovernanceAlert(ctx, userID, alertID)
}

// Clear permanently deletes the caller's visible dismissed alerts. Dismissed notices are kept, never listed: one
// deleted would be inserted and announced again by the next sweep that sees its rules (a restart re-scans every
// flow); the retention sweep removes them.
func (s *GovernanceAlertService) Clear(ctx context.Context, userID string) error {
	return s.store.ClearGovernanceAlerts(ctx, userID)
}
