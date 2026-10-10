package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// postChannel saves a channel as the env's admin and returns the status and the decoded answer.
func (e *channelEnv) postChannel(body map[string]any) (int, map[string]any) {
	e.t.Helper()
	rr := doRequestWithAuth(e.t, e.rt, http.MethodPost, "/api/orgs/"+e.orgID+"/channels", e.admin, body)
	var out map[string]any
	_ = json.Unmarshal(rr.Body.Bytes(), &out)
	return rr.Code, out
}

// TestOrgChannels_JiraAndEventsThroughTheAPI: an admin adds a Jira channel with its email, API token and project
// key and the events it receives; the answer and the list carry the email, the project, hasToken and the events,
// never the token; an edit keeps what it omits; a webhook channel lists the default events and no hasToken.
func TestOrgChannels_JiraAndEventsThroughTheAPI(t *testing.T) {
	env := newChannelEnv(t).allowLoopbackDelivery()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusCreated) }))
	defer srv.Close()

	code, refused := env.postChannel(map[string]any{"kind": "jira", "url": srv.URL, "email": "bot@acme.io", "project": "PAD"})
	if code != http.StatusBadRequest || refused["details"].(map[string]any)["field"] != "token" {
		t.Fatalf("jira without a token: %d %v, want 400 on token", code, refused)
	}
	code, created := env.postChannel(map[string]any{"name": "tickets", "kind": "jira", "url": srv.URL, "email": "bot@acme.io",
		"token": "XXXXTOKEN", "project": "PAD", "events": []string{"new_rules", "drift"}})
	if code != http.StatusOK {
		t.Fatalf("create: %d %v", code, created)
	}
	id := created["id"].(string)
	raw, _ := json.Marshal(created)
	if strings.Contains(string(raw), "XXXXTOKEN") || created["hasToken"] != true || created["email"] != "bot@acme.io" ||
		created["project"] != "PAD" || created["hasSecret"] != false {
		t.Errorf("created = %s", raw)
	}
	if ev := created["events"].([]any); len(ev) != 2 || ev[0] != "drift" || ev[1] != "new_rules" {
		t.Errorf("events = %v, want [drift new_rules]", ev)
	}

	code, edited := env.postChannel(map[string]any{"id": id, "kind": "jira", "url": created["url"], "urlUnchanged": true, "project": "OPS"})
	if code != http.StatusOK || edited["project"] != "OPS" || edited["hasToken"] != true || len(edited["events"].([]any)) != 2 {
		t.Errorf("edit without token or events: %d %v, want the token and events kept", code, edited)
	}
	if s := env.stored(id); s.Secrets["token"] != "XXXXTOKEN" || s.URL != srv.URL {
		t.Errorf("stored after the edit: %+v", s)
	}
	rr := doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels/"+id+"/test", env.admin, map[string]any{})
	checkStatus(t, rr, http.StatusOK)

	if code, hook := env.postChannel(map[string]any{"kind": "webhook", "url": srv.URL + "/hook"}); code != http.StatusOK ||
		len(hook["events"].([]any)) != 3 || hook["hasToken"] != nil {
		t.Errorf("a webhook channel: %d %v, want the default events and no hasToken", code, hook)
	}
	for _, listed := range env.listChannels() {
		if listed["id"] == id && (listed["hasToken"] != true || listed["project"] != "OPS") {
			t.Errorf("listed jira channel = %v", listed)
		}
	}
	if code, bad := env.postChannel(map[string]any{"id": id, "events": []string{"analysis_complete"}}); code != http.StatusBadRequest ||
		bad["details"].(map[string]any)["field"] != "events" {
		t.Errorf("an event org channels never get: %d %v, want 400 on events", code, bad)
	}
}

// recordingMailer is a mail sender that records the addresses it was asked to mail.
type recordingMailer struct {
	mu sync.Mutex
	to []string
}

func (m *recordingMailer) SendAlert(_ context.Context, to, _, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.to = append(m.to, to)
	return nil
}

func (m *recordingMailer) Enabled() bool { return true }

// TestOrgChannels_EmailRecipientsThroughTheAPI: an email channel names members by id; a user who is not a member
// (peon) is refused on recipients, and the test mails the member at their account's verified address.
func TestOrgChannels_EmailRecipientsThroughTheAPI(t *testing.T) {
	env := newChannelEnv(t)
	mailer := &recordingMailer{}
	moduleOf[*OrgHandler](t, env.rt).channels = service.NewChannelService(env.backend, service.ChannelOptions{
		Policy: egress.AllowLoopback, Mailer: mailer, Members: env.backend})
	for _, id := range []string{"boss", "peon"} {
		if err := env.backend.SetUserEmailVerified(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	}

	code, refused := env.postChannel(map[string]any{"kind": "email", "recipients": []string{"boss", "peon"}})
	if code != http.StatusBadRequest || refused["details"].(map[string]any)["field"] != "recipients" {
		t.Fatalf("a non-member recipient: %d %v, want 400 on recipients", code, refused)
	}
	if code, typed := env.postChannel(map[string]any{"kind": "email", "url": "mailto:victim@example.com", "recipients": []string{"boss"}}); code != http.StatusBadRequest {
		t.Errorf("an address on an email channel: %d %v", code, typed)
	}
	code, created := env.postChannel(map[string]any{"name": "leads", "kind": "email", "recipients": []string{"boss"}})
	if code != http.StatusOK || !slices.Equal(toStrings(created["recipients"]), []string{"boss"}) || created["url"] != "" {
		t.Fatalf("create: %d %v", code, created)
	}
	rr := doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels/"+created["id"].(string)+"/test", env.admin, map[string]any{})
	checkStatus(t, rr, http.StatusOK)
	mailer.mu.Lock()
	defer mailer.mu.Unlock()
	if !slices.Equal(mailer.to, []string{"boss@acme.io"}) {
		t.Errorf("the test mailed %v, want the member's account address", mailer.to)
	}
}

func toStrings(v any) []string {
	var out []string
	for _, s := range v.([]any) {
		out = append(out, s.(string))
	}
	return out
}

// TestOrgChannels_OptionsShowTheHostListOnlyWhenVisible: the form's options name the kinds this deployment delivers
// (no email without mail settings) and the events, and say a host list applies; the list itself only when the
// operator made it visible.
func TestOrgChannels_OptionsShowTheHostListOnlyWhenVisible(t *testing.T) {
	env := newChannelEnv(t).restrictHosts("hooks.slack.com")
	options := func() map[string]any {
		t.Helper()
		rr := doRequestWithAuth(t, env.rt, http.MethodGet, "/api/orgs/"+env.orgID+"/channels/options", env.admin, nil)
		checkStatus(t, rr, http.StatusOK)
		var out map[string]any
		if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	got := options()
	if got["hostsRestricted"] != true || got["allowedHosts"] != nil || slices.Contains(toStrings(got["kinds"]), "email") ||
		!slices.Equal(toStrings(got["events"]), []string{"drift", "health_regression", "new_rules"}) {
		t.Errorf("options with a hidden list = %v", got)
	}
	moduleOf[*OrgHandler](t, env.rt).channels = service.NewChannelService(env.backend, service.ChannelOptions{
		AllowedHosts: []string{"hooks.slack.com"}, AllowedHostsVisible: true})
	if got := options(); !slices.Equal(toStrings(got["allowedHosts"]), []string{"hooks.slack.com"}) {
		t.Errorf("options with a visible list = %v", got)
	}
}

// TestOrgChannels_UnreadableChannelThroughTheAPI: a channel whose stored URL cannot be decrypted is listed flagged
// and without a URL beside the others, its test answers 400 CHANNEL_UNREADABLE without dialling, and it can be
// removed.
func TestOrgChannels_UnreadableChannelThroughTheAPI(t *testing.T) {
	env := newChannelEnv(t).allowLoopbackDelivery()
	for _, id := range []string{"fine", "lost"} {
		if err := env.backend.SaveOrgChannel(context.Background(), &storageif.OrgChannel{ID: id, OrgID: env.orgID, Name: id,
			Kind: "webhook", URL: "http://127.0.0.1:1/" + id, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	env.backend.OrgChannels["lost"].Unreadable = true
	list := env.listChannels()
	if len(list) != 2 {
		t.Fatalf("listed %d channels, want both", len(list))
	}
	for _, ch := range list {
		lost := ch["id"] == "lost"
		if (ch["unreadable"] == true) != lost || (lost && ch["url"] != "") {
			t.Errorf("listed %v", ch)
		}
	}
	rr := doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/channels/lost/test", env.admin, map[string]any{})
	checkStatus(t, rr, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "CHANNEL_UNREADABLE") {
		t.Errorf("test of an unreadable channel = %s", rr.Body.String())
	}
	rr = doRequestWithAuth(t, env.rt, http.MethodDelete, "/api/orgs/"+env.orgID+"/channels/lost", env.admin, map[string]any{})
	checkStatus(t, rr, http.StatusOK)
	if list := env.listChannels(); len(list) != 1 {
		t.Errorf("after the removal: %v", list)
	}
}
