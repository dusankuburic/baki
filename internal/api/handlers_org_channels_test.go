package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/testutil"
)

// newChannelTestEnv: JWT router (FakeBackend storage) + an org created through
// the real org API by "boss" (creator = owner/admin), plus a member added via
// the invite-free direct path (SaveOrg through the org service store is not
// reachable from here; use the org update API's member add as admin).
type channelEnv struct {
	rt      *Router
	admin   string
	orgID   string
	backend *testutil.FakeBackend
}

func newChannelEnv(t *testing.T) *channelEnv {
	t.Helper()
	backend := &testutil.FakeBackend{}
	rt := newTestRouter(backend, true)
	seedUserWithRole(t, rt, "boss", "boss@acme.io", auth.RoleMember)
	seedUserWithRole(t, rt, "peon", "peon@acme.io", auth.RoleMember)
	admin := jwtBearer(t, rt, "boss", "boss@acme.io")

	// Create the org through the API (creator becomes owner/admin; 200 + org JSON).
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs", admin, map[string]any{"name": "Acme"})
	checkStatus(t, rr, http.StatusOK)
	var createdOrg struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&createdOrg); err != nil || createdOrg.ID == "" {
		t.Fatalf("org create: %v (%s)", err, rr.Body.String())
	}
	return &channelEnv{rt: rt, admin: admin, orgID: createdOrg.ID, backend: backend}
}

// TestOrgChannels_AdminCRUDAndTest drives the full channel lifecycle: member
// forbidden, kind/URL validation, create→list→test(delivers exactly once
// to a local receiver)→delete.
func TestOrgChannels_AdminCRUDAndTest(t *testing.T) {
	env := newChannelEnv(t)
	rt, admin, orgID := env.rt, env.admin, env.orgID
	peon := jwtBearer(t, rt, "peon", "peon@acme.io")

	// A non-member cannot configure (404 — org invisible to them).
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/channels", peon, map[string]any{"kind": "webhook", "url": "https://h.example/x"})
	if rr.Code != http.StatusForbidden && rr.Code != http.StatusNotFound {
		t.Fatalf("non-member save: status %d", rr.Code)
	}

	// Bad kind → 400.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/channels", admin, map[string]any{"kind": "pager", "url": "https://h.example/x"})
	checkStatus(t, rr, http.StatusBadRequest)

	// Non-HTTPS URL → 400 (validateAlertURL).
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/channels", admin, map[string]any{"kind": "webhook", "url": "http://far.example/x"})
	checkStatus(t, rr, http.StatusBadRequest)

	// Local receiver counts deliveries.
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// Valid create.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/channels", admin,
		map[string]any{"name": "ops hook", "kind": "webhook", "url": srv.URL, "secret": "k"})
	checkStatus(t, rr, http.StatusOK)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil || created.ID == "" {
		t.Fatalf("channel create: %v (%s)", err, rr.Body.String())
	}

	// List shows it (URL visible; secret never serialized).
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/orgs/"+orgID+"/channels", admin, nil)
	checkStatus(t, rr, http.StatusOK)
	var list []map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil || len(list) != 1 {
		t.Fatalf("list: %v (%s)", err, rr.Body.String())
	}
	if _, hasSecret := list[0]["secret"]; hasSecret {
		t.Error("channel secret leaked in list response")
	}

	// Test delivery: synchronous, exactly one hit.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/channels/"+created.ID+"/test", admin, map[string]any{})
	checkStatus(t, rr, http.StatusOK)
	if n := atomic.LoadInt32(&hits); n != 1 {
		t.Fatalf("test delivery hits = %d, want 1", n)
	}

	// Delete + verify.
	rr = doRequestWithAuth(t, rt, http.MethodDelete, "/api/orgs/"+orgID+"/channels/"+created.ID, admin, nil)
	checkStatus(t, rr, http.StatusOK)
	channels, _ := env.backend.ListOrgChannels(context.Background(), orgID, false)
	if len(channels) != 0 {
		t.Fatalf("channel not deleted: %+v", channels)
	}

	// Testing a deleted channel → 404.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/channels/"+created.ID+"/test", admin, map[string]any{})
	checkStatus(t, rr, http.StatusNotFound)
}
