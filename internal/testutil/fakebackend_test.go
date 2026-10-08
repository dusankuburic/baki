package testutil

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// The first account becomes an email-verified admin only on the registration path (the contract cannot show it:
// it runs on a database that already has users).
func TestFakeBackend_CreateUserBootstrapsTheFirstAdmin(t *testing.T) {
	ctx := context.Background()
	b := NewFakeBackend()
	first := &interfaces.User{ID: "u1", Email: "u1@example.com", Role: auth.RoleViewer}
	if err := b.CreateUser(auth.WithAllowBootstrap(ctx, true), first); err != nil {
		t.Fatal(err)
	}
	if first.Role != auth.RoleAdmin || !first.EmailVerified {
		t.Errorf("first user = %+v, want a verified admin", first)
	}
	second := &interfaces.User{ID: "u2", Email: "u2@example.com", Role: auth.RoleViewer}
	if err := b.CreateUser(auth.WithAllowBootstrap(ctx, true), second); err != nil {
		t.Fatal(err)
	}
	if second.Role != auth.RoleViewer || second.EmailVerified {
		t.Errorf("second user = %+v, want the role it asked for", second)
	}
	if err := b.CreateUser(ctx, &interfaces.User{ID: "u1", Email: "other@example.com"}); !errors.Is(err, interfaces.ErrEmailExists) {
		t.Errorf("a taken id: %v, want ErrEmailExists", err)
	}
}

// Rows whose Postgres table has a foreign key to their parent are refused without it.
func TestFakeBackend_ForeignKeys(t *testing.T) {
	ctx := context.Background()
	b := NewFakeBackend()
	for name, err := range map[string]error{
		"user settings": b.SaveUserSettings(ctx, "nobody", &interfaces.AppSettings{}),
		"org settings":  b.SaveOrgSettings(ctx, "no-org", &interfaces.AppSettings{}),
		"collaborator":  b.AddCollaborator(ctx, "no-flow", &interfaces.Collaborator{UserID: "nobody"}),
		"comment":       b.AddFindingComment(ctx, &interfaces.FindingComment{FlowID: "no-flow", FindingKey: "k"}),
		"flow version":  b.SaveFlowVersion(ctx, &interfaces.FlowVersion{ID: "v", FlowID: "no-flow"}),
	} {
		if err == nil {
			t.Errorf("%s without its parent was stored", name)
		}
	}
}

// DeleteUser erases what Postgres erases of the rows the fake keeps.
func TestFakeBackend_DeleteUserCascades(t *testing.T) {
	ctx := context.Background()
	b := NewFakeBackend()
	for _, id := range []string{"gone", "other"} {
		if err := b.SaveUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, f := range []*interfaces.FlowDocument{{ID: "owned", OwnerID: "gone"}, {ID: "theirs", OwnerID: "other"}} {
		if err := b.SaveFlow(ctx, f); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.AddCollaborator(ctx, "theirs", &interfaces.Collaborator{UserID: "gone", Permission: "viewer"}); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveUserSettings(ctx, "gone", &interfaces.AppSettings{Appearance: interfaces.AppearanceSettings{Density: "compact"}}); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveOrg(ctx, &interfaces.Organisation{ID: "org", OwnerID: "other", Members: []interfaces.OrgMember{{UserID: "other"}, {UserID: "gone"}}}); err != nil {
		t.Fatal(err)
	}
	if err := b.StoreRefreshToken(ctx, "jti", "gone", time.Now().Add(time.Hour), "", ""); err != nil {
		t.Fatal(err)
	}
	if err := b.SetFindingStatus(ctx, &interfaces.FindingStatus{FlowID: "theirs", FindingKey: "k", Status: "resolved", UpdatedBy: "gone"}); err != nil {
		t.Fatal(err)
	}

	if err := b.DeleteUser(ctx, "gone"); err != nil {
		t.Fatal(err)
	}
	if _, err := b.LoadFlow(ctx, "owned"); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("the owned flow survived: %v", err)
	}
	if list, _ := b.ListCollaborators(ctx, "theirs", 0); len(list) != 0 {
		t.Errorf("the grant survived: %+v", list)
	}
	if s, _ := b.LoadUserSettings(ctx, "gone"); s.Appearance.Density == "compact" {
		t.Errorf("the settings survived")
	}
	if org, _ := b.LoadOrg(ctx, "org"); len(org.Members) != 1 {
		t.Errorf("the membership survived: %+v", org.Members)
	}
	if ok, _ := b.IsRefreshTokenValid(ctx, "jti"); ok {
		t.Errorf("the session survived")
	}
	if st, _ := b.ListFindingStatuses(ctx, "theirs"); len(st) != 1 || st[0].UpdatedBy != "" {
		t.Errorf("triage on another's flow = %+v, want it kept without the user's id", st)
	}
	if len(b.DeletedUsers) != 1 || b.DeletedUsers[0] != "gone" {
		t.Errorf("DeletedUsers = %v", b.DeletedUsers)
	}
	if err := b.DeleteUser(ctx, "gone"); err != nil || len(b.DeletedUsers) != 1 {
		t.Errorf("a second erasure: %v, DeletedUsers %v; want a no-op", err, b.DeletedUsers)
	}
}

func TestFakeBackend_PurgeExpiredData(t *testing.T) {
	ctx := context.Background()
	b := NewFakeBackend()
	past, future := time.Now().Add(-time.Hour), time.Now().Add(time.Hour)
	_ = b.StoreRefreshToken(ctx, "old", "u", past, "", "")
	_ = b.StoreRefreshToken(ctx, "live", "u", future, "", "")
	_ = b.CreateAPIToken(ctx, &interfaces.APIToken{ID: "t1", UserID: "u", TokenHash: "h1", ExpiresAt: &past})
	_ = b.CreateAPIToken(ctx, &interfaces.APIToken{ID: "t2", UserID: "u", TokenHash: "h2"})
	_ = b.CreateShareToken(ctx, &interfaces.ShareToken{FlowID: "f", TokenHash: "s1", ExpiresAt: &past})
	_ = b.SaveAuditEvent(ctx, &interfaces.AuditEvent{ID: "a1", CreatedAt: time.Now().AddDate(0, 0, -40)})
	_ = b.SaveAuditEvent(ctx, &interfaces.AuditEvent{ID: "a2", CreatedAt: time.Now()})
	_ = b.SaveOrg(ctx, &interfaces.Organisation{ID: "org", Name: "Org"})
	longAgo, lately := time.Now().AddDate(0, 0, -8), time.Now().AddDate(0, 0, -1)
	for _, inv := range []*interfaces.OrgInvite{
		{ID: "expired", OrgID: "org", Email: "a@example.com", ExpiresAt: past},
		{ID: "accepted-long-ago", OrgID: "org", Email: "b@example.com", ExpiresAt: future, AcceptedAt: &longAgo},
		{ID: "accepted-lately", OrgID: "org", Email: "c@example.com", ExpiresAt: future, AcceptedAt: &lately},
		{ID: "pending", OrgID: "org", Email: "d@example.com", ExpiresAt: future},
	} {
		if err := b.SaveOrgInvite(ctx, inv); err != nil {
			t.Fatalf("SaveOrgInvite %s: %v", inv.ID, err)
		}
	}

	res, err := b.PurgeExpiredData(ctx, 0)
	if err != nil || res.RefreshTokens != 1 || res.APITokens != 1 || res.ShareTokens != 1 || res.OrgInvites != 2 || res.AuditEvents != 0 {
		t.Fatalf("purge without audit retention = %+v, %v", res, err)
	}
	if left, _ := b.ListOrgInvites(ctx, "org"); len(left) != 2 {
		t.Errorf("invites left = %+v, want the pending one and the one accepted lately", left)
	}
	res, err = b.PurgeExpiredData(ctx, 30)
	if err != nil || res.AuditEvents != 1 || len(b.SavedAuditEvents()) != 1 {
		t.Fatalf("purge with 30 days of audit = %+v, %v", res, err)
	}
	if ok, _ := b.IsRefreshTokenValid(ctx, "live"); !ok {
		t.Errorf("the live session was purged")
	}
}

func TestFakeBackend_SearchKnowledgeRanksByCosineSimilarity(t *testing.T) {
	ctx := context.Background()
	b := NewFakeBackend()
	_ = b.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: "d1", OrgID: "org", Filename: "guide.md"})
	_ = b.SaveKnowledgeDocument(ctx, "", &interfaces.KnowledgeDocument{ID: "d2", OrgID: "other", Filename: "theirs.md"})
	_ = b.SaveKnowledgeChunks(ctx, "", []interfaces.KnowledgeChunk{
		{ID: "close", DocID: "d1", Content: "close", Embedding: []float32{1, 0.1}},
		{ID: "exact", DocID: "d1", Content: "exact", Embedding: []float32{1, 0}},
		{ID: "far", DocID: "d1", Content: "far", Embedding: []float32{0, 1}},
		{ID: "wide", DocID: "d1", Content: "other model", Embedding: []float32{1, 0, 0}},
		{ID: "foreign", DocID: "d2", Content: "other org", Embedding: []float32{1, 0}},
	})
	hits, err := b.SearchKnowledge(ctx, "org", []float32{1, 0}, 10)
	if err != nil || len(hits) != 2 || hits[0].ID != "exact" || hits[1].ID != "close" || hits[0].Filename != "guide.md" {
		t.Fatalf("hits = %+v, %v; want exact then close, from guide.md", hits, err)
	}
	if hits, _ := b.SearchKnowledge(ctx, "org", []float32{1, 0}, 1); len(hits) != 1 {
		t.Errorf("limit 1 returned %d hits", len(hits))
	}
}

// The methods are safe for concurrent use (run with -race).
func TestFakeBackend_ConcurrentUse(t *testing.T) {
	ctx := context.Background()
	b := &FakeBackend{}
	var wg sync.WaitGroup
	for i := range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := fmt.Sprint(i)
			_ = b.CreateUser(ctx, &interfaces.User{ID: "u" + id, Email: id + "@example.com"})
			_ = b.SaveFlow(ctx, &interfaces.FlowDocument{ID: "f" + id, OwnerID: "u" + id})
			_ = b.AddCollaborator(ctx, "f"+id, &interfaces.Collaborator{UserID: "u" + id})
			_ = b.SaveConversation(ctx, "f"+id, "s", []interfaces.ChatMessage{{ID: id}})
			_, _ = b.ListFlows(ctx, interfaces.FlowFilter{UserID: "u0"})
			_, _ = b.ListCollaborators(ctx, "f0", 0)
			_ = b.StoreRefreshToken(ctx, id, "u"+id, time.Now().Add(time.Hour), "", "")
		}()
	}
	wg.Wait()
	if n, _ := b.CountUsers(ctx); n != 20 {
		t.Errorf("%d users, want 20", n)
	}
	if n, _ := b.CountFlows(ctx, interfaces.FlowFilter{AllFlows: true}); n != 20 {
		t.Errorf("%d flows, want 20", n)
	}
}
