package collaboration

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
)

// Every membership change and the deletion is announced once it is stored, with the organisation, the member
// and the role (the new one; the one held, for a removal); a refused change announces nothing. The owner's
// membership, which Create stores with the organisation, is announced like any other added member.
func TestOrgService_AnnouncesMembershipChanges(t *testing.T) {
	bus := eventbus.New(nil, 0)
	rec := eventbus.NewRecorder(bus)
	svc := NewOrgService(NewMemOrgStore(), WithEventBus(bus))
	ctx := context.Background()
	org, err := svc.Create(ctx, "Acme", "owner")
	if err != nil {
		t.Fatal(err)
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(svc.AddMember(ctx, org.ID, "u1", auth.RoleAdmin))
	must(svc.SetRole(ctx, org.ID, "u1", auth.RoleMember))
	must(svc.RemoveMember(ctx, org.ID, "u1"))
	// Refused: already a member, the last admin, an unknown member.
	if svc.AddMember(ctx, org.ID, "owner", auth.RoleMember) == nil || svc.RemoveMember(ctx, org.ID, "owner") == nil ||
		svc.SetRole(ctx, org.ID, "nobody", auth.RoleAdmin) == nil {
		t.Fatal("a refused change succeeded")
	}
	must(svc.Delete(ctx, org.ID))

	want := []events.OrgMemberChanged{
		{OrgID: org.ID, UserID: "owner", Role: string(auth.RoleAdmin), Kind: events.OrgMemberAdded},
		{OrgID: org.ID, UserID: "u1", Role: string(auth.RoleAdmin), Kind: events.OrgMemberAdded},
		{OrgID: org.ID, UserID: "u1", Role: string(auth.RoleMember), Kind: events.OrgMemberRoleChanged},
		{OrgID: org.ID, UserID: "u1", Role: string(auth.RoleMember), Kind: events.OrgMemberRemoved},
		{OrgID: org.ID, Kind: events.OrgDeleted},
	}
	got := eventbus.Recorded[events.OrgMemberChanged](rec)
	if len(got) != len(want) {
		t.Fatalf("announced %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("announcement %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

// Accepting an invite announces the new member with the invite's role.
func TestOrgService_AcceptInviteAnnouncesTheMember(t *testing.T) {
	bus := eventbus.New(nil, 0)
	rec := eventbus.NewRecorder(bus)
	svc := NewOrgService(NewMemOrgStore(), WithEventBus(bus))
	ctx := context.Background()
	org, _ := svc.Create(ctx, "Acme", "owner")
	_, token, err := svc.CreateInvite(ctx, org.ID, "new@example.com", auth.RoleViewer, "owner", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	rec.Reset() // the owner's announcement (TestOrgService_AnnouncesMembershipChanges)
	if _, err := svc.AcceptInvite(ctx, token, "u-new", "new@example.com", true); err != nil {
		t.Fatal(err)
	}
	got := eventbus.Recorded[events.OrgMemberChanged](rec)
	if len(got) != 1 || got[0] != (events.OrgMemberChanged{OrgID: org.ID, UserID: "u-new", Role: string(auth.RoleViewer), Kind: events.OrgMemberAdded}) {
		t.Errorf("announced %+v, want the new viewer", got)
	}
}
