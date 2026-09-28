package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/notify"
	storageif "pad-analyzer/internal/storage/interfaces"
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

// allowLoopbackDelivery relaxes this env's egress policy to the desktop one.
//
// A test that asserts a channel actually DELIVERS needs a receiver, and a test
// receiver is necessarily loopback — which the cloud policy refuses, by design.
// So the lifecycle test opts in explicitly and the cloud refusal is asserted by
// its own test below. Reaching into the field is deliberate: the policy is a
// constructed dependency, and stating it here is clearer than a second router
// builder that differs in one invisible way.
func (e *channelEnv) allowLoopbackDelivery() *channelEnv {
	e.rt.handlers.Org.notifyPolicy = notify.AllowLoopbackTargets
	// The shared channel client (D-25) was built under the strict policy at
	// construction; swap it with the policy so the test delivery can dial
	// the loopback httptest server.
	e.rt.handlers.Org.channelClient = notify.NewChannelClient(notify.AllowLoopbackTargets)
	return e
}

// TestOrgChannels_AdminCRUDAndTest drives the full channel lifecycle: member
// forbidden, kind/URL validation, create→list→test(delivers exactly once
// to a local receiver)→delete.
func TestOrgChannels_AdminCRUDAndTest(t *testing.T) {
	env := newChannelEnv(t).allowLoopbackDelivery()
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

// TestOrgChannels_CloudModeRefusesInternalTargets is the regression gate for the
// SSRF chain.
//
// Any authenticated user can create an org and administer it (handleOrgCreate
// has no role gate), so a channel URL is TENANT-supplied input. The channel-test
// endpoint then dials it synchronously and reports whether the connection was
// refused, returned a non-2xx, or succeeded — which is a loopback port scanner,
// and a way to POST attacker-influenced JSON at internal listeners. Two layers
// have to refuse: URL validation at create time, and the dial guard at delivery
// time (the dial guard previously omitted IsLoopback entirely, so a literal
// 127.0.0.1 sailed through both).
func TestOrgChannels_CloudModeRefusesInternalTargets(t *testing.T) {
	env := newChannelEnv(t) // cloud policy — deliberately NOT relaxed
	internal := []string{
		"http://127.0.0.1:9200/_cluster/health",
		"http://localhost:2379/v2/keys/",
		"http://[::1]:8080/admin",
		"https://127.0.0.1/hook",
		"https://169.254.169.254/latest/meta-data/",
		"https://10.0.0.5/hook",
		"https://192.168.1.10/hook",
	}
	for _, u := range internal {
		t.Run(u, func(t *testing.T) {
			rr := doRequestWithAuth(t, env.rt, http.MethodPost,
				"/api/orgs/"+env.orgID+"/channels", env.admin,
				map[string]any{"kind": "webhook", "url": u})
			if rr.Code != http.StatusBadRequest {
				t.Errorf("internal target %q accepted with status %d — body: %s",
					u, rr.Code, rr.Body.String())
			}
		})
	}

	// Nothing was stored, so none of them can be reached by the test endpoint
	// later either.
	channels, _ := env.backend.ListOrgChannels(context.Background(), env.orgID, false)
	if len(channels) != 0 {
		t.Errorf("internal-target channels were persisted: %+v", channels)
	}
}

// TestOrgChannels_DeliveryRefusesInternalTargetAtDialTime covers the second
// layer. A channel row that predates the policy (or was written directly to
// storage) must still fail to deliver rather than being dialled.
func TestOrgChannels_DeliveryRefusesInternalTargetAtDialTime(t *testing.T) {
	env := newChannelEnv(t)
	ch := &storageif.OrgChannel{
		ID: "ch-legacy", OrgID: env.orgID, Name: "legacy", Kind: "webhook",
		URL: "http://127.0.0.1:9/never", Enabled: true,
	}
	if err := env.backend.SaveOrgChannel(context.Background(), ch); err != nil {
		t.Fatalf("seed channel: %v", err)
	}
	rr := doRequestWithAuth(t, env.rt, http.MethodPost,
		"/api/orgs/"+env.orgID+"/channels/"+ch.ID+"/test", env.admin, map[string]any{})
	if rr.Code == http.StatusOK {
		t.Errorf("delivery to a loopback target succeeded: %s", rr.Body.String())
	}
}
