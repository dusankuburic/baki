package service

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// memoCountingOrgStore counts the membership reads behind OrgService.MemberRole.
type memoCountingOrgStore struct {
	collaboration.OrgStore
	roles atomic.Int64
}

func (s *memoCountingOrgStore) OrgMemberRole(ctx context.Context, orgID, userID string) (auth.Role, bool, error) {
	s.roles.Add(1)
	return s.OrgStore.OrgMemberRole(ctx, orgID, userID)
}

// memoCollabBackend is a FakeBackend whose collaborator read is counted and
// grants "bob" the editor tier on every flow.
type memoCollabBackend struct {
	*testutil.FakeBackend
	listCollabs atomic.Int64
}

func (b *memoCollabBackend) ListCollaborators(context.Context, string, int) ([]*storageif.Collaborator, error) {
	b.listCollabs.Add(1)
	return []*storageif.Collaborator{{UserID: "bob", Permission: "editor", GrantedAt: time.Now().UTC()}}, nil
}

// newMemoAuthz builds an AuthzService over the counting stores with an org
// whose admin is alice and in which bob is a viewer.
func newMemoAuthz(t *testing.T) (*AuthzService, *memoCountingOrgStore, *memoCollabBackend, string) {
	t.Helper()
	store := &memoCountingOrgStore{OrgStore: collaboration.NewMemOrgStore()}
	orgSvc := collaboration.NewOrgService(store)
	org, err := orgSvc.Create(context.Background(), "Acme", "alice")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), org.ID, "bob", auth.RoleViewer); err != nil {
		t.Fatalf("add member: %v", err)
	}
	backend := &memoCollabBackend{FakeBackend: testutil.NewFakeBackend()}
	store.roles.Store(0)
	return NewAuthzService(backend, orgSvc), store, backend, org.ID
}

// decide makes the three decisions a library PUT makes about one flow: the
// read gate, the write gate and the capability flags. bob is an org viewer
// (viewer rank, so the editor gate must go on to the collaborators) and an
// editor collaborator.
func decide(t *testing.T, a *AuthzService, ctx context.Context, orgID string) {
	t.Helper()
	if err := a.CheckFlowAccess(ctx, "f1", "alice", orgID, "bob", "viewer"); err != nil {
		t.Fatalf("viewer gate: %v", err)
	}
	if err := a.CheckFlowAccess(ctx, "f1", "alice", orgID, "bob", "editor"); err != nil {
		t.Fatalf("editor gate: %v", err)
	}
	canEdit, canDelete, canShare := a.FlowPermissions(ctx, "f1", "alice", orgID, "bob")
	if !canEdit || canDelete || canShare {
		t.Fatalf("flags = edit:%v delete:%v share:%v, want edit only", canEdit, canDelete, canShare)
	}
}

// TestFlowAuthzMemo_OneReadOfEach: with the request memo, the three
// decisions cost one role lookup and one collaborator read; without it (a
// plain context) they cost three and two, as before.
func TestFlowAuthzMemo_OneReadOfEach(t *testing.T) {
	a, store, backend, orgID := newMemoAuthz(t)

	decide(t, a, context.Background(), orgID)
	if roles, collabs := store.roles.Load(), backend.listCollabs.Load(); roles != 3 || collabs != 2 {
		t.Fatalf("plain context: roles=%d collabs=%d, want 3 and 2 (no memo, every decision reads)", roles, collabs)
	}

	store.roles.Store(0)
	backend.listCollabs.Store(0)
	decide(t, a, WithFlowAuthzMemo(context.Background()), orgID)
	if roles, collabs := store.roles.Load(), backend.listCollabs.Load(); roles != 1 || collabs != 1 {
		t.Fatalf("memo context: roles=%d collabs=%d, want 1 and 1", roles, collabs)
	}
}

// TestFlowAuthzMemo_NonMemberMemoised: "not a member" is a definitive
// answer and is memoised too, so a non-member's decisions do not re-ask.
func TestFlowAuthzMemo_NonMemberMemoised(t *testing.T) {
	a, store, _, orgID := newMemoAuthz(t)
	ctx := WithFlowAuthzMemo(context.Background())
	// carol is in no org; both gates fall through to the collaborators (none for her) and deny.
	for range 2 {
		if err := a.CheckFlowAccess(ctx, "f1", "alice", orgID, "carol", "viewer"); err == nil {
			t.Fatal("non-member, non-collaborator was allowed")
		}
	}
	if n := store.roles.Load(); n != 1 {
		t.Fatalf("non-member role asked %d times; want 1", n)
	}
}
