package service

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

func TestGovernanceAlertService_Inbox(t *testing.T) {
	b := testutil.NewFakeBackend()
	ctx := context.Background()
	// Alerts are about a flow, and a user sees those of the flows they can see.
	if err := b.SaveFlow(ctx, &storageif.FlowDocument{ID: "f-1", Name: "F", OwnerID: "u-1"}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	for _, a := range []*storageif.GovernanceAlert{
		{ID: "team", FlowID: "f-1", Title: "drift", CreatedAt: now},
		{ID: "mine", FlowID: "f-1", Title: "assigned", TargetUser: "u-1", CreatedAt: now.Add(time.Second)},
		{ID: "theirs", FlowID: "f-1", Title: "assigned", TargetUser: "u-2", CreatedAt: now.Add(2 * time.Second)},
	} {
		if err := b.RecordGovernanceAlert(ctx, a); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewGovernanceAlertService(b)

	got, err := svc.List(ctx, storageif.GovernanceAlertFilter{UserID: "u-1"})
	if err != nil || len(got) != 2 || got[0].ID != "mine" || got[1].ID != "team" {
		t.Fatalf("List = %v, %v; want [mine team]", got, err)
	}
	if n, err := svc.UnreadCount(ctx, "u-1"); err != nil || n != 2 {
		t.Fatalf("UnreadCount = %d, %v; want 2", n, err)
	}
	if err := svc.MarkAllRead(ctx, "u-1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.UnreadCount(ctx, "u-1"); n != 0 {
		t.Errorf("UnreadCount after MarkAllRead = %d", n)
	}

	if err := svc.Dismiss(ctx, "u-1", "team"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.List(ctx, storageif.GovernanceAlertFilter{UserID: "u-1"}); len(got) != 1 {
		t.Errorf("List after Dismiss = %d alerts, want 1", len(got))
	}
	if err := svc.Clear(ctx, "u-1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.List(ctx, storageif.GovernanceAlertFilter{UserID: "u-1", IncludeDismissed: true}); len(got) != 1 || got[0].ID != "mine" {
		t.Errorf("List after Clear = %v, want only mine", got)
	}
}

// TestGovernanceAlertService_Notices: the caller's "new rules" notices head the first page of the inbox as entries
// without a flow, count towards the badge, are marked read with the alerts, are dismissed by their id, and survive a
// Clear (a deleted notice would be announced again by the next sweep).
func TestGovernanceAlertService_Notices(t *testing.T) {
	b := testutil.NewFakeBackend()
	ctx := context.Background()
	for _, u := range []string{"u-1", "u-2"} {
		if err := b.CreateUser(ctx, &storageif.User{ID: u, Email: u + "@example.com", Password: "hash"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.SaveOrg(ctx, &storageif.Organisation{ID: "org-1", Name: "One", OwnerID: "u-1",
		Members: []storageif.OrgMember{{UserID: "u-1"}}}); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveFlow(ctx, &storageif.FlowDocument{ID: "f-1", Name: "F", OwnerID: "u-1"}); err != nil {
		t.Fatal(err)
	}
	if err := b.RecordGovernanceAlert(ctx, &storageif.GovernanceAlert{ID: "team", FlowID: "f-1", Title: "drift", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	notice := func(scope, id string, rules ...string) *storageif.RuleNotice {
		return &storageif.RuleNotice{ID: storageif.RuleNoticeID(scope, id, "rs1:d"), Kind: storageif.RuleNoticeKindNewRules,
			Scope: scope, ScopeID: id, RuleSetDigest: "rs1:d", RuleIDs: rules, FlowCount: 3}
	}
	orgNotice, userNotice := notice("org", "org-1", "rule-b", "rule-a"), notice("user", "u-2", "rule-a")
	if _, err := b.UpsertRuleNotices(ctx, []*storageif.RuleNotice{orgNotice, userNotice}); err != nil {
		t.Fatal(err)
	}
	svc := NewGovernanceAlertService(b)

	got, err := svc.List(ctx, storageif.GovernanceAlertFilter{UserID: "u-1"})
	if err != nil || len(got) != 2 || got[0].ID != orgNotice.ID || got[1].ID != "team" {
		t.Fatalf("List = %v, %v; want [org notice, team]", got, err)
	}
	n := got[0]
	if n.Type != "new_rules" || n.FlowID != "" || n.OrgID != "org-1" || n.Scope != "org" || n.FlowCount != 3 ||
		!slices.Equal(n.RuleIDs, []string{"rule-a", "rule-b"}) || n.Title != "2 new rules report findings" ||
		n.Severity != "warning" || !strings.HasPrefix(n.Message, "rule-a, rule-b: findings in 3 flows") {
		t.Errorf("notice entry = %+v", n)
	}
	if got, _ := svc.List(ctx, storageif.GovernanceAlertFilter{UserID: "u-1", Offset: 1}); len(got) != 0 {
		t.Errorf("a later page lists %v; notices head the first page only", got)
	}
	if got, _ := svc.List(ctx, storageif.GovernanceAlertFilter{UserID: "u-2"}); len(got) != 1 || got[0].ID != userNotice.ID ||
		got[0].Title != "1 new rule reports findings" {
		t.Errorf("u-2 lists %v; want their own notice only", got)
	}

	if n, err := svc.UnreadCount(ctx, "u-1"); err != nil || n != 2 {
		t.Fatalf("UnreadCount = %d, %v; want the alert and the notice", n, err)
	}
	if err := svc.MarkAllRead(ctx, "u-1"); err != nil {
		t.Fatal(err)
	}
	if n, _ := svc.UnreadCount(ctx, "u-1"); n != 0 {
		t.Errorf("UnreadCount after MarkAllRead = %d", n)
	}
	if n, _ := svc.UnreadCount(ctx, "u-2"); n != 1 {
		t.Errorf("u-1 marking read cleared u-2's badge: %d", n)
	}

	if err := svc.Dismiss(ctx, "u-1", orgNotice.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.List(ctx, storageif.GovernanceAlertFilter{UserID: "u-1", IncludeDismissed: true}); len(got) != 1 || got[0].ID != "team" {
		t.Errorf("after dismissing the notice: %v; want the alert only (a dismissed notice is not listed)", got)
	}
	if err := svc.Clear(ctx, "u-1"); err != nil {
		t.Fatal(err)
	}
	if stored := b.RuleNoticesSnapshot()[orgNotice.ID]; stored == nil || stored.DismissedAt == nil {
		t.Errorf("Clear deleted the dismissed notice: %+v", stored)
	}
}
