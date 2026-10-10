package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// listChannels returns the org's channels as the list endpoint serves them to the admin.
func (e *channelEnv) listChannels() []map[string]any {
	e.t.Helper()
	rr := doRequestWithAuth(e.t, e.rt, http.MethodGet, "/api/orgs/"+e.orgID+"/channels", e.admin, nil)
	checkStatus(e.t, rr, http.StatusOK)
	var list []map[string]any
	if err := json.NewDecoder(rr.Body).Decode(&list); err != nil {
		e.t.Fatalf("list: %v (%s)", err, rr.Body.String())
	}
	return list
}

// stored returns the channel row as the store holds it.
func (e *channelEnv) stored(id string) *storageif.OrgChannel {
	e.t.Helper()
	channels, err := e.backend.ListOrgChannels(context.Background(), e.orgID, false)
	if err != nil {
		e.t.Fatal(err)
	}
	for _, c := range channels {
		if c.ID == id {
			return c
		}
	}
	e.t.Fatalf("channel %s not stored", id)
	return nil
}

// TestOrgChannels_EditKeepsSecretEnabledAndCreatedAt is the regression gate for B1, B2 and B10: the settings UI's
// pause toggle and its edit form re-send the channel without the secret (which the API never returns) and the edit
// form without enabled, and both used to wipe the HMAC secret, re-enable a paused channel and answer a createdAt of
// "now". The requests are the ones the released UI sends, the URL as the list shows it.
func TestOrgChannels_EditKeepsSecretEnabledAndCreatedAt(t *testing.T) {
	env := newChannelEnv(t).allowLoopbackDelivery()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()

	rr := doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels", env.admin,
		map[string]any{"name": "ops", "kind": "webhook", "url": srv.URL + "/hook", "secret": "k1"})
	checkStatus(t, rr, http.StatusOK)
	var created struct {
		ID        string    `json:"id"`
		CreatedAt time.Time `json:"createdAt"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil || created.ID == "" {
		t.Fatalf("create: %v (%s)", err, rr.Body.String())
	}
	createdAt := env.stored(created.ID).CreatedAt
	listed := env.listChannels()[0]

	// Pause, as the toggle does: {id, name, kind, url, enabled}, no secret.
	rr = doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels", env.admin,
		map[string]any{"id": created.ID, "name": "ops", "kind": "webhook", "url": listed["url"], "enabled": false})
	checkStatus(t, rr, http.StatusOK)
	if got := env.stored(created.ID); got.Secret != "k1" || got.Enabled || got.URL != srv.URL+"/hook" {
		t.Errorf("after a pause: secret %q, enabled %v, url %q; want the secret and url kept, paused", got.Secret, got.Enabled, got.URL)
	}

	// Edit, as the form does: {id, name, kind, url}, no enabled, no secret.
	time.Sleep(5 * time.Millisecond)
	rr = doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels", env.admin,
		map[string]any{"id": created.ID, "name": "ops renamed", "kind": "webhook", "url": listed["url"]})
	checkStatus(t, rr, http.StatusOK)
	var saved struct {
		CreatedAt time.Time `json:"createdAt"`
		Enabled   bool      `json:"enabled"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&saved); err != nil {
		t.Fatal(err)
	}
	got := env.stored(created.ID)
	if got.Secret != "k1" {
		t.Errorf("an edit without a secret wiped it (B1): %q", got.Secret)
	}
	if got.Enabled || saved.Enabled {
		t.Errorf("an edit without enabled re-enabled the paused channel (B2): stored %v, answered %v", got.Enabled, saved.Enabled)
	}
	if got.Name != "ops renamed" {
		t.Errorf("name = %q, want the edit applied", got.Name)
	}
	if !got.CreatedAt.Equal(createdAt) || !saved.CreatedAt.Equal(createdAt) {
		t.Errorf("createdAt: stored %v, answered %v; want the creation time %v kept (B9, B10)", got.CreatedAt, saved.CreatedAt, createdAt)
	}
}

// TestOrgChannels_ClearUnchangedAndRefusals drives the API contract the settings UI moves to: urlUnchanged with the
// listed url keeps the stored URL and with any other url is refused, clear removes the secret (hasSecret goes false),
// and a field the kind does not have is a 400 INVALID_CHANNEL naming it.
func TestOrgChannels_ClearUnchangedAndRefusals(t *testing.T) {
	env := newChannelEnv(t).allowLoopbackDelivery()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	post := func(body map[string]any) (int, map[string]any) {
		t.Helper()
		rr := doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels", env.admin, body)
		var out map[string]any
		_ = json.Unmarshal(rr.Body.Bytes(), &out)
		return rr.Code, out
	}
	code, created := post(map[string]any{"name": "ops", "kind": "slack", "url": srv.URL + "/services/T/B/XXXX", "secret": "k1"})
	if code != http.StatusOK || created["hasSecret"] != true || strings.Contains(created["url"].(string), "XXXX") {
		t.Fatalf("create: %d %v", code, created)
	}
	id := created["id"].(string)

	code, refused := post(map[string]any{"id": id, "urlUnchanged": true, "url": "https://evil.example/x"})
	if details, _ := refused["details"].(map[string]any); code != http.StatusBadRequest || refused["code"] != "INVALID_CHANNEL" || details["field"] != "url" {
		t.Errorf("urlUnchanged with a new url: %d %v", code, refused)
	}
	code, saved := post(map[string]any{"id": id, "urlUnchanged": true, "url": created["url"], "clear": []string{"secret"}})
	if code != http.StatusOK || saved["hasSecret"] != false {
		t.Fatalf("clear: %d %v", code, saved)
	}
	if got := env.stored(id); got.URL != srv.URL+"/services/T/B/XXXX" || got.Secret != "" || got.Kind != "slack" {
		t.Errorf("after urlUnchanged and clear: %+v", got)
	}

	code, teams := post(map[string]any{"name": "t", "kind": "teams", "url": srv.URL + "/t"})
	if code != http.StatusOK {
		t.Fatalf("teams create: %d %v", code, teams)
	}
	code, refused = post(map[string]any{"id": teams["id"], "secret": "s"})
	details, _ := refused["details"].(map[string]any)
	if code != http.StatusBadRequest || refused["code"] != "INVALID_CHANNEL" || details["field"] != "secret" {
		t.Errorf("a secret on teams: %d %v", code, refused)
	}
	if code, out := post(map[string]any{"kind": "webhook", "url": srv.URL, "bogus": 1}); code != http.StatusBadRequest {
		t.Errorf("an unknown field: %d %v", code, out)
	}
}

// TestOrgChannels_TestFailureIsCoarse is B13's API half: a failed test is a 502 whose details carry the endpoint's
// status, and the body never names the URL's path or query.
func TestOrgChannels_TestFailureIsCoarse(t *testing.T) {
	env := newChannelEnv(t).allowLoopbackDelivery()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusGone) }))
	defer srv.Close()
	if err := env.backend.SaveOrgChannel(context.Background(), &storageif.OrgChannel{
		ID: "ch-gone", OrgID: env.orgID, Name: "gone", Kind: "webhook", URL: srv.URL + "/hook?token=XXXXSECRET", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels/ch-gone/test", env.admin, map[string]any{})
	checkStatus(t, rr, http.StatusBadGateway)
	var body struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != "CHANNEL_DELIVERY_FAILED" || body.Details["status"] != float64(http.StatusGone) {
		t.Errorf("answer %s", rr.Body.String())
	}
	if strings.Contains(rr.Body.String(), "XXXXSECRET") || strings.Contains(rr.Body.String(), "127.0.0.1") {
		t.Errorf("the answer names the target: %s", rr.Body.String())
	}
}
