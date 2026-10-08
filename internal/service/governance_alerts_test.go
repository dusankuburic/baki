package service

import (
	"context"
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
