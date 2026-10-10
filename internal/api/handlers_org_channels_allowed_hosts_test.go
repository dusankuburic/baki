package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// restrictHosts serves this env's channels under the deployment's host allow-list (PAD_NOTIFY_ALLOWED_HOSTS), with
// the loopback policy of a deployment without accounts so a delivery can reach a test receiver.
func (e *channelEnv) restrictHosts(hosts ...string) *channelEnv {
	org := moduleOf[*OrgHandler](e.t, e.rt)
	org.channels = service.NewChannelService(e.backend, service.ChannelOptions{Policy: egress.AllowLoopback, AllowedHosts: hosts})
	return e
}

// TestOrgChannels_AllowListRefusesSaveAndTest: a save to a host off the list is a 400 INVALID_CHANNEL on url whose
// message names the host and no address; a stored channel whose host left the list is refused by the test endpoint
// before anything is dialled.
func TestOrgChannels_AllowListRefusesSaveAndTest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	env := newChannelEnv(t).restrictHosts("localhost")

	rr := doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels", env.admin,
		map[string]any{"kind": "teams-workflow", "url": "http://127.0.0.1" + port + "/workflows/x?sig=SECRET"})
	checkStatus(t, rr, http.StatusBadRequest)
	var body struct {
		Code    string         `json:"code"`
		Details map[string]any `json:"details"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v (%s)", err, rr.Body.String())
	}
	if body.Code != "INVALID_CHANNEL" || body.Details["field"] != "url" {
		t.Errorf("refusal = %s, want INVALID_CHANNEL on url", rr.Body.String())
	}
	if text := rr.Body.String(); strings.Contains(text, "SECRET") || strings.Contains(text, "/workflows") || !strings.Contains(text, "PAD_NOTIFY_ALLOWED_HOSTS") {
		t.Errorf("refusal = %s, want the allow-list named and nothing of the URL past its host", text)
	}

	if err := env.backend.SaveOrgChannel(context.Background(), &storageif.OrgChannel{
		ID: "ch-old", OrgID: env.orgID, Name: "old", Kind: "webhook", URL: "http://127.0.0.1" + port + "/hook", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	rr = doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels/ch-old/test", env.admin, map[string]any{})
	checkStatus(t, rr, http.StatusBadRequest)
	if n := hits.Load(); n != 0 {
		t.Errorf("the off-list host was reached %d times", n)
	}

	rr = doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels", env.admin,
		map[string]any{"name": "room", "kind": "teams-workflow", "url": "http://localhost" + port + "/workflows/x"})
	checkStatus(t, rr, http.StatusOK)
}

// TestOrgChannels_TeamsWorkflowKind: an admin adds a Teams Workflows channel (no secret: one is refused), it lists
// redacted without a secret, and its test delivers an Adaptive Card message.
func TestOrgChannels_TeamsWorkflowKind(t *testing.T) {
	got := make(chan []byte, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		got <- body
		w.WriteHeader(http.StatusAccepted)
	}))
	defer srv.Close()
	env := newChannelEnv(t).allowLoopbackDelivery()
	url := srv.URL + "/workflows/abc/triggers/manual/paths/invoke?sig=SECRET"

	rr := doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels", env.admin,
		map[string]any{"kind": "teams-workflow", "url": url, "secret": "s"})
	checkStatus(t, rr, http.StatusBadRequest)

	rr = doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels", env.admin,
		map[string]any{"name": "room", "kind": "teams-workflow", "url": url})
	checkStatus(t, rr, http.StatusOK)
	var created channelOut
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Kind != "teams-workflow" || created.HasSecret || strings.Contains(created.URL, "SECRET") {
		t.Errorf("created = %+v", created)
	}

	rr = doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels/"+created.ID+"/test", env.admin, map[string]any{})
	checkStatus(t, rr, http.StatusOK)
	var msg struct {
		Type        string `json:"type"`
		Attachments []struct {
			ContentType string `json:"contentType"`
		} `json:"attachments"`
	}
	if err := json.Unmarshal(<-got, &msg); err != nil || msg.Type != "message" || len(msg.Attachments) != 1 ||
		msg.Attachments[0].ContentType != "application/vnd.microsoft.card.adaptive" {
		t.Errorf("delivered %+v (%v), want a message with an Adaptive Card", msg, err)
	}
}
