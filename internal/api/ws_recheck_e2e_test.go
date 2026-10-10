package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil/pgtest"
	wshub "pad-analyzer/internal/websocket"
)

// recheckBound is how soon an access change must reach an open socket: well under the 2-minute periodic pass the
// socket otherwise waits for.
const recheckBound = 3 * time.Second

// wireSocketRechecks subscribes the socket access rechecks on rt's bus, as internal/di.SubscribeRealtime does.
func wireSocketRechecks(t *testing.T, rt *Router) {
	t.Helper()
	wshub.SubscribeAccessRechecks(busOf(t, rt), rt.hub)
}

// recheckRouter is a router with accounts whose sockets are checked as internal/di checks them and re-checked on
// access changes, served over HTTP for the sockets; seed saves a flow of an owner.
func recheckRouter(t *testing.T) (rt *Router, srv *httptest.Server, seed func(id, ownerID string)) {
	t.Helper()
	rt, seed = newLibraryTestRouter(t)
	rt.flowChecker = socketChecker{authz: service.NewAuthzService(rt.security.Backend, rt.security.OrgSvc)}
	wireSocketRechecks(t, rt)
	srv = httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	return rt, srv, seed
}

// memberSession seeds id as a member (unless seeded) and returns the bearer of a fresh session of theirs.
func memberSession(t *testing.T, rt *Router, id string) string {
	t.Helper()
	ensureUser(t, rt, id)
	pair, err := rt.security.AuthMgr.Issue(id, id+"@example.com", auth.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	return "Bearer " + pair.AccessToken
}

// grant makes userID a collaborator on flowID with permission.
func grant(t *testing.T, rt *Router, flowID, userID, permission string) {
	t.Helper()
	ensureUser(t, rt, userID)
	if err := rt.security.Backend.AddCollaborator(context.Background(), flowID, &storageif.Collaborator{
		UserID: userID, Email: userID + "@example.com", Permission: permission, GrantedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
}

// awaitClosed fails unless the server closes the socket within d.
func (ws *wsFrames) awaitClosed(t *testing.T, d time.Duration) {
	t.Helper()
	deadline := time.After(d)
	for {
		select {
		case _, open := <-ws.frames:
			if !open {
				return
			}
		case <-deadline:
			t.Fatalf("socket still open %v after the access change (the periodic pass would take up to 2 minutes)", d)
		}
	}
}

// assertOpen fails unless the socket still answers a ping.
func (ws *wsFrames) assertOpen(t *testing.T) {
	t.Helper()
	ws.send(t, `{"type":"ping"}`)
	ws.await(t, wshub.EventPong, 2*time.Second)
}

// TestWS_Recheck_RemovedCollaboratorIsDisconnected: removing a collaborator closes their open socket on the flow
// at once, instead of at its next 2-minute pass; the owner's socket stays.
func TestWS_Recheck_RemovedCollaboratorIsDisconnected(t *testing.T) {
	rt, srv, seed := recheckRouter(t)
	owner, collab := memberSession(t, rt, "u-owner"), memberSession(t, rt, "u-collab")
	seed("f1", "u-owner")
	grant(t, rt, "f1", "u-collab", "viewer")
	ownerWS, collabWS := dialFlow(t, srv, owner, "f1"), dialFlow(t, srv, collab, "f1")

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodDelete, "/api/flows/f1/collaborators/u-collab", owner, nil), http.StatusOK)

	collabWS.awaitClosed(t, recheckBound)
	ownerWS.assertOpen(t)
}

// TestWS_Recheck_DemotedEditorLosesWriteRank: an editor demoted to viewer keeps the socket but can no longer relay
// block mutations, at once.
func TestWS_Recheck_DemotedEditorLosesWriteRank(t *testing.T) {
	rt, srv, seed := recheckRouter(t)
	owner, collab := memberSession(t, rt, "u-owner"), memberSession(t, rt, "u-collab")
	seed("f1", "u-owner")
	grant(t, rt, "f1", "u-collab", "editor")
	ownerWS, collabWS := dialFlow(t, srv, owner, "f1"), dialFlow(t, srv, collab, "f1")
	ownerWS.await(t, wshub.EventPresenceJoin, 2*time.Second) // the collaborator's
	const mutation = `{"type":"block.update","payload":{"blockId":"BLOCK-1"}}`
	collabWS.send(t, mutation)
	ownerWS.await(t, wshub.EventBlockUpdate, 2*time.Second)

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPut, "/api/flows/f1/collaborators/u-collab", owner, map[string]string{"permission": "viewer"}), http.StatusOK)

	deadline := time.Now().Add(recheckBound)
	for {
		collabWS.send(t, mutation)
		if env, ok := collabWS.next(wshub.EventError, 200*time.Millisecond); ok {
			if p, _ := env.Payload.(map[string]any); p["code"] != "permission_denied" {
				t.Fatalf("refused mutation answered %+v, want permission_denied", env.Payload)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a demoted editor still relays block mutations %v after the demotion", recheckBound)
		}
	}
	collabWS.assertOpen(t)
}

// TestWS_Recheck_RemovedOrgMemberIsDisconnected: removing a member from an organisation closes their sockets on
// its flows at once.
func TestWS_Recheck_RemovedOrgMemberIsDisconnected(t *testing.T) {
	rt, srv, _ := recheckRouter(t)
	admin, member := memberSession(t, rt, "u-admin"), memberSession(t, rt, "u-member")
	org := seedOrg(t, rt, "acme", "u-admin")
	addOrgMember(t, rt, org, "u-member", auth.RoleMember)
	seedOrgFlow(t, rt, "f-org", "u-admin", org)
	adminWS, memberWS := dialFlow(t, srv, admin, "f-org"), dialFlow(t, srv, member, "f-org")

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodDelete, "/api/orgs/"+org+"/members/u-member", admin, nil), http.StatusOK)

	memberWS.awaitClosed(t, recheckBound)
	adminWS.assertOpen(t)
}

// TestWS_Recheck_DeletedOrgDisconnectsItsMembers: deleting an organisation closes its members' sockets on its
// flows at once; the flow's owner keeps theirs (the flow is now their own).
func TestWS_Recheck_DeletedOrgDisconnectsItsMembers(t *testing.T) {
	rt, srv, _ := recheckRouter(t)
	admin, member := memberSession(t, rt, "u-admin"), memberSession(t, rt, "u-member")
	org := seedOrg(t, rt, "acme", "u-admin")
	addOrgMember(t, rt, org, "u-member", auth.RoleMember)
	seedOrgFlow(t, rt, "f-org", "u-admin", org)
	adminWS, memberWS := dialFlow(t, srv, admin, "f-org"), dialFlow(t, srv, member, "f-org")

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodDelete, "/api/orgs/"+org, admin, nil), http.StatusOK)

	memberWS.awaitClosed(t, recheckBound)
	adminWS.assertOpen(t)
}

// TestWS_Recheck_DeletedFlowClosesItsRoom: deleting a flow closes every socket on it at once.
func TestWS_Recheck_DeletedFlowClosesItsRoom(t *testing.T) {
	rt, srv, seed := recheckRouter(t)
	owner, collab := memberSession(t, rt, "u-owner"), memberSession(t, rt, "u-collab")
	seed("f1", "u-owner")
	grant(t, rt, "f1", "u-collab", "viewer")
	ownerWS, collabWS := dialFlow(t, srv, owner, "f1"), dialFlow(t, srv, collab, "f1")

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodDelete, "/api/library/f1", owner, nil), http.StatusOK)

	collabWS.awaitClosed(t, recheckBound)
	ownerWS.awaitClosed(t, recheckBound)
}

// TestWS_Recheck_ErasedAccountIsDisconnected: erasing an account closes its sockets, and the sockets other people
// hold on the flows it owned (erased with it), at once.
func TestWS_Recheck_ErasedAccountIsDisconnected(t *testing.T) {
	rt, srv, seed := recheckRouter(t)
	moduleOf[*AuthHandler](t, rt).accounts = service.NewAccountLifecycleService(rt.security.Backend, nil, busOf(t, rt))
	owner, collab := memberSession(t, rt, "u-owner"), memberSession(t, rt, "u-collab")
	seed("f1", "u-owner")
	grant(t, rt, "f1", "u-collab", "viewer")
	ownerWS, collabWS := dialFlow(t, srv, owner, "f1"), dialFlow(t, srv, collab, "f1")

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/account", owner, map[string]string{"confirmEmail": "u-owner@example.com"}), http.StatusOK)

	ownerWS.awaitClosed(t, recheckBound)
	collabWS.awaitClosed(t, recheckBound)
}

// TestWS_Recheck_RevokedTokenIsDisconnected: deleting an API token closes the sockets it opened at once; the
// owner's session socket stays.
func TestWS_Recheck_RevokedTokenIsDisconnected(t *testing.T) {
	rt, srv, seed := recheckRouter(t)
	session := memberSession(t, rt, "u-owner")
	seed("f1", "u-owner")
	pat := mintPAT(t, rt, session, []string{auth.ScopeRead, auth.ScopeWrite})
	sessionWS, tokenWS := dialFlow(t, srv, session, "f1"), dialFlow(t, srv, pat, "f1")

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/auth/tokens", session, nil)
	checkStatus(t, rr, http.StatusOK)
	var tokens []storageif.APIToken
	if err := json.NewDecoder(rr.Body).Decode(&tokens); err != nil || len(tokens) != 1 {
		t.Fatalf("tokens: %v, %v", tokens, err)
	}
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/tokens/"+tokens[0].ID, session, nil), http.StatusOK)

	tokenWS.awaitClosed(t, recheckBound)
	sessionWS.assertOpen(t)
}

// TestWS_Recheck_LogoutDisconnectsThatSession: logging out closes the sockets of that session at once; the same
// user's other session keeps its socket.
func TestWS_Recheck_LogoutDisconnectsThatSession(t *testing.T) {
	rt, srv, seed := recheckRouter(t)
	first, second := memberSession(t, rt, "u-owner"), memberSession(t, rt, "u-owner")
	seed("f1", "u-owner")
	firstWS, secondWS := dialFlow(t, srv, first, "f1"), dialFlow(t, srv, second, "f1")

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/auth/logout", first, map[string]string{}), http.StatusOK)

	firstWS.awaitClosed(t, recheckBound)
	secondWS.assertOpen(t)
}

// TestWS_Recheck_Postgres: on Postgres the removal runs in the request's row-level-security transaction, and the
// socket is re-checked only after that commits, by its own read outside the request: the removed collaborator's
// socket closes, the owner's stays. A fresh database migrated to this binary's schema.
func TestWS_Recheck_Postgres(t *testing.T) {
	dsn := pgtest.NewDatabase(t, "")
	ctx := context.Background()
	pg, err := storagedb.New(ctx, storagedb.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { pg.Close() })
	rt := newTestRouter(pg, true)
	rt.flowChecker = socketChecker{authz: service.NewAuthzService(pg, rt.security.OrgSvc)}
	wireSocketRechecks(t, rt)
	srv := httptest.NewServer(rt)
	t.Cleanup(srv.Close)
	for _, id := range []string{"ws-owner", "ws-collab"} {
		if err := pg.CreateUser(ctx, &storageif.User{ID: id, Email: id + "@example.com", Password: "hash", Role: auth.RoleMember}); err != nil {
			t.Fatalf("CreateUser: %v", err)
		}
	}
	if err := pg.SaveFlow(ctx, &storageif.FlowDocument{ID: "ws-flow", Name: "ws-flow", OwnerID: "ws-owner"}); err != nil {
		t.Fatalf("SaveFlow: %v", err)
	}
	grant(t, rt, "ws-flow", "ws-collab", "viewer")
	owner, collab := sessionBearer(t, rt, "ws-owner", auth.RoleMember), sessionBearer(t, rt, "ws-collab", auth.RoleMember)
	ownerWS, collabWS := dialFlow(t, srv, owner, "ws-flow"), dialFlow(t, srv, collab, "ws-flow")

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodDelete, "/api/flows/ws-flow/collaborators/ws-collab", owner, nil), http.StatusOK)

	collabWS.awaitClosed(t, recheckBound)
	ownerWS.assertOpen(t)
}
