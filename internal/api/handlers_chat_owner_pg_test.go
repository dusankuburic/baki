package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil/pgtest"
)

// TestChatOwner_Postgres is R-security-3's done-when on Postgres: users A and B are both members of the organisation
// that holds a flow. B's POST /api/chat/get with A's thread key — a thread uuid, the legacy "flow" key, a block id —
// answers an empty conversation at version 0, never A's messages; B's thread list does not name A's threads, A's
// names all three; B cannot clear them.
func TestChatOwner_Postgres(t *testing.T) {
	dsn := pgtest.NewDatabase(t, "")
	ctx := context.Background()
	pg, err := storagedb.New(ctx, storagedb.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { pg.Close() })
	rt, bearer := chatOwnerRouter(t, pg)
	for _, id := range []string{"alice", "bob"} {
		if err := pg.CreateUser(ctx, &storageif.User{ID: id, Email: id + "@example.com", Password: "hash", Role: "member"}); err != nil {
			t.Fatalf("CreateUser %s: %v", id, err)
		}
		bearer[id] = sessionBearer(t, rt, id, auth.RoleMember)
	}
	org := seedOrg(t, rt, "Acme", "owner")
	addOrgMember(t, rt, org, "alice", auth.RoleMember)
	addOrgMember(t, rt, org, "bob", auth.RoleMember)
	mirrorOrg(t, rt, org)
	const flowID = "f-org-chat"
	if err := pg.SaveFlow(ctx, &storageif.FlowDocument{ID: flowID, Name: "Org flow", OwnerID: "owner", OrganizationID: org, Content: json.RawMessage(`{}`)}); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	keys := []string{"44444444-0000-4000-8000-000000000001", "flow", "0b7c4c1e-6a65-4cf4-9a49-6f7d3b1f2a10"}
	for _, key := range keys {
		if _, err := pg.SaveConversationIf(ctx, flowID, key, "alice", 0, []storageif.ChatMessage{
			{ID: "a-" + key, Role: "user", Content: "alice's secret about " + key, Timestamp: "2026-10-27T10:00:00Z"},
		}); err != nil {
			t.Fatalf("seed alice's %s: %v", key, err)
		}
	}

	type getBody struct {
		Messages []json.RawMessage `json:"messages"`
		Version  *int64            `json:"version"`
	}
	for _, key := range keys {
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/get", bearer["bob"], map[string]any{"flowId": flowID, "provider": key})
		checkStatus(t, rr, http.StatusOK)
		if strings.Contains(rr.Body.String(), "alice's secret") {
			t.Fatalf("B read A's thread %s: %s", key, rr.Body)
		}
		var got getBody
		decodeJSON(t, rr, &got)
		if len(got.Messages) != 0 || got.Version == nil || *got.Version != 0 {
			t.Errorf("B's GET of %s = %d messages at version %v, want an empty conversation at 0", key, len(got.Messages), got.Version)
		}
		rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/get", bearer["alice"], map[string]any{"flowId": flowID, "provider": key})
		got = getBody{}
		decodeJSON(t, rr, &got)
		if len(got.Messages) != 1 {
			t.Errorf("A's GET of %s = %d messages, want 1", key, len(got.Messages))
		}
		// B's clear is the no-op of a missing conversation; the thread stays.
		rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/clear", bearer["bob"], map[string]any{"flowId": flowID, "provider": key})
		checkStatus(t, rr, http.StatusOK)
		if msgs, _, _ := pg.LoadConversationVersioned(ctx, flowID, key, ""); len(msgs) != 1 {
			t.Errorf("B's clear removed A's %s", key)
		}
	}

	var page chatThreadsPage
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/threads", bearer["bob"], map[string]any{"flowId": flowID})
	checkStatus(t, rr, http.StatusOK)
	decodeJSON(t, rr, &page)
	if len(page.Items) != 0 {
		t.Errorf("B's thread list names %+v, want none", page.Items)
	}
	page = chatThreadsPage{}
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/threads", bearer["alice"], map[string]any{"flowId": flowID})
	checkStatus(t, rr, http.StatusOK)
	decodeJSON(t, rr, &page)
	if len(page.Items) != len(keys) {
		t.Errorf("A's thread list = %+v, want A's %d threads", page.Items, len(keys))
	}
}
