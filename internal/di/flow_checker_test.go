package di

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	wshub "pad-analyzer/internal/websocket"
)

// sessionActor is a socket's actor for a session ticket: the unscoped user.
func sessionActor(userID string) *auth.Actor {
	return &auth.Actor{Kind: auth.ActorUser, UserID: userID}
}

// CanEdit answers AuthzService.Can's FlowEdit for the socket's actor: the owner and an org member edit, an org
// viewer and a stranger only read, and nobody edits a flow that does not exist.
func TestFlowAccessChecker_CanEditIsFlowEdit(t *testing.T) {
	t.Parallel()

	store := testutil.NewFakeBackend()
	ctx := context.Background()
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(ctx, "acme", "alice")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	for user, role := range map[string]auth.Role{"bob": auth.RoleViewer, "carol": auth.RoleMember} {
		if err := orgSvc.AddMember(ctx, org.ID, user, role); err != nil {
			t.Fatalf("add %s: %v", user, err)
		}
	}
	if err := store.SaveFlow(ctx, &storageif.FlowDocument{ID: "f1", Name: "f", OwnerID: "alice", OrganizationID: org.ID}); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	checker := ProvideFlowAccessChecker(service.NewAuthzService(store, orgSvc), store)

	for user, want := range map[string]bool{"alice": true, "carol": true, "bob": false, "mallory": false} {
		if got := checker.CanEdit(ctx, "f1", sessionActor(user)); got != want {
			t.Errorf("CanEdit(f1, %s) = %v, want %v", user, got, want)
		}
		if _, err := checker.CheckAccess(ctx, "f1", sessionActor(user)); (err == nil) != (user != "mallory") {
			t.Errorf("CheckAccess(f1, %s) = %v", user, err)
		}
	}
	if checker.CanEdit(ctx, "missing", sessionActor("alice")) {
		t.Error("CanEdit on a missing flow = true")
	}
}

// The socket's actor is its ticket's credential, scopes included (R7): the owner's read-scoped token joins but does
// not edit, a read+write token edits, and a chat-only token cannot join at all, whatever rank its owner holds.
func TestFlowAccessChecker_ActorScopes(t *testing.T) {
	t.Parallel()

	store := testutil.NewFakeBackend()
	ctx := context.Background()
	if err := store.SaveFlow(ctx, &storageif.FlowDocument{ID: "f1", Name: "f", OwnerID: "alice"}); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	checker := ProvideFlowAccessChecker(service.NewAuthzService(store, collaboration.NewOrgService(collaboration.NewMemOrgStore())), store)
	token := func(scopes ...string) *auth.Actor {
		return &auth.Actor{Kind: auth.ActorToken, UserID: "alice", TokenID: "tok1", Scopes: scopes}
	}

	read := token(auth.ScopeRead)
	if _, err := checker.CheckAccess(ctx, "f1", read); err != nil {
		t.Errorf("owner's read token cannot join: %v", err)
	}
	if checker.CanEdit(ctx, "f1", read) {
		t.Error("owner's read token may edit")
	}
	if rw := token(auth.ScopeRead, auth.ScopeWrite); !checker.CanEdit(ctx, "f1", rw) {
		t.Error("owner's read+write token may not edit")
	}
	if unscoped := token(); !checker.CanEdit(ctx, "f1", unscoped) {
		t.Error("owner's unscoped token may not edit")
	}
	if _, err := checker.CheckAccess(ctx, "f1", token(auth.ScopeChat)); !errors.Is(err, wshub.ErrAccessDenied) {
		t.Errorf("chat-only token joins: %v, want ErrAccessDenied", err)
	}
	if _, err := checker.CheckAccess(ctx, "f1", nil); !errors.Is(err, wshub.ErrAccessDenied) {
		t.Errorf("nil actor joins: %v, want ErrAccessDenied", err)
	}
}
