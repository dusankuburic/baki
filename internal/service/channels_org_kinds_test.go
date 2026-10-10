package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/notify"
	"pad-analyzer/internal/platform/apperr"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// sentMail is an EmailSender that records the addresses it was asked to mail.
type sentMail struct {
	mu sync.Mutex
	to []string
}

func (m *sentMail) SendAlert(_ context.Context, to, _, _, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.to = append(m.to, to)
	return nil
}

func (m *sentMail) Enabled() bool { return true }

func (m *sentMail) sent() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.to)
}

// invalidField returns the field a 400 INVALID_CHANNEL names, or "" for another error.
func invalidField(err error) string {
	ae, ok := apperr.As(err)
	if !ok || ae.Kind != apperr.KindInvalid {
		return ""
	}
	f, _ := ae.Details["field"].(string)
	return f
}

// TestChannelService_UnreadableChannelListedSkippedAndReplaceable: a channel whose sealed values no configured key
// opens (the store lists it Unreadable) no longer takes the organisation's list or deliveries down with it. It is
// listed without a URL and flagged; the scanner's notifiers leave it out, logged by id; the test refuses it; an edit
// must enter the URL again (nothing sealed can be kept) and then makes it deliverable; it can be removed.
func TestChannelService_UnreadableChannelListedSkippedAndReplaceable(t *testing.T) {
	b := testutil.NewFakeBackend()
	svc := newTestChannels(t, b, egress.AllowLoopback)
	ctx := context.Background()
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "ok", OrgID: "o", Name: "fine", Kind: "webhook", URL: "http://127.0.0.1:9/ok", Enabled: true})
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "lost", OrgID: "o", Name: "lost", Kind: "webhook", URL: "http://127.0.0.1:9/XXXXSECRET",
		Secret: "s", Enabled: true})
	b.OrgChannels["lost"].Unreadable = true // what the Postgres store reports for a value sealed under an unknown key

	views, err := svc.List(ctx, "o")
	if err != nil || len(views) != 2 {
		t.Fatalf("List = %v, %v; want both channels", views, err)
	}
	for _, v := range views {
		if v.ID == "lost" && (!v.Unreadable || v.URL != "" || v.HasSecret || v.Name != "lost" || !v.Enabled) {
			t.Errorf("unreadable view = %+v, want it flagged, without a URL or secret, its name and state kept", v)
		}
		if v.ID == "ok" && v.Unreadable {
			t.Errorf("readable view flagged: %+v", v)
		}
	}

	var logs bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(prev)
	built, err := svc.OrgNotifiers(ctx, "o")
	if err != nil || len(built) != 1 {
		t.Fatalf("OrgNotifiers = %d, %v; want the readable channel delivered to", len(built), err)
	}
	if out := logs.String(); !strings.Contains(out, "channelId=lost") || strings.Contains(out, "XXXXSECRET") {
		t.Errorf("the skip is not logged by id alone:\n%s", out)
	}

	if err := svc.Test(ctx, "o", "lost"); invalidField(err) != "url" || !strings.Contains(err.Error(), "decrypted") {
		t.Errorf("Test(unreadable) = %v, want 400 on url saying why", err)
	}
	for _, edit := range []ChannelSave{
		{ID: "lost", Enabled: boolPtr(false)},
		{ID: "lost", URLUnchanged: true, Name: strPtr("renamed")},
	} {
		if _, err := svc.Save(ctx, "o", edit); invalidField(err) != "url" || !strings.Contains(err.Error(), "decrypted") {
			t.Errorf("Save(%+v) of an unreadable channel = %v, want 400 on url asking for it again", edit, err)
		}
	}
	saved, err := svc.Save(ctx, "o", ChannelSave{ID: "lost", URL: "http://127.0.0.1:9/new"})
	if err != nil || saved.Unreadable || saved.HasSecret || saved.Name != "lost" {
		t.Fatalf("re-saved = %+v, %v; want readable, the unreadable secret gone, the name kept", saved, err)
	}
	if built, _ := svc.OrgNotifiers(ctx, "o"); len(built) != 2 {
		t.Errorf("after the re-save %d channels are delivered to, want 2", len(built))
	}
	b.OrgChannels["lost"].Unreadable = true
	if err := svc.Delete(ctx, "o", "lost"); err != nil {
		t.Fatal(err)
	}
	if views, _ := svc.List(ctx, "o"); len(views) != 1 {
		t.Errorf("after removing the unreadable channel: %+v", views)
	}
}

// TestChannelService_JiraChannel: an organisation's Jira channel stores its base URL in the url column, the email and
// project as config and the API token sealed with the secrets; the token is write-only (kept when an edit omits it,
// replaced when one is sent, never cleared, never shown); a test files the issue with Basic auth.
func TestChannelService_JiraChannel(t *testing.T) {
	type issue struct{ path, auth string }
	got := make(chan issue, 4)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- issue{r.URL.Path, r.Header.Get("Authorization")}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	b := testutil.NewFakeBackend()
	svc := newTestChannels(t, b, egress.AllowLoopback)
	ctx := context.Background()

	for _, tc := range []struct {
		name   string
		values map[string]string
		field  string
	}{
		{"no token", map[string]string{"email": "bot@acme.io", "project": "PAD"}, "token"},
		{"no project", map[string]string{"email": "bot@acme.io", "token": "t"}, "project"},
		{"a project name", map[string]string{"email": "bot@acme.io", "token": "t", "project": "Power Automate"}, "project"},
		{"recipients on jira", map[string]string{"email": "bot@acme.io", "token": "t", "project": "PAD", "recipients": "u1"}, "recipients"},
	} {
		if _, err := svc.Save(ctx, "o", ChannelSave{Kind: "jira", URL: srv.URL, Values: tc.values}); invalidField(err) != tc.field {
			t.Errorf("%s: %v, want 400 on %s", tc.name, err, tc.field)
		}
	}
	if _, err := svc.Save(ctx, "o", ChannelSave{Kind: "jira", URL: srv.URL, Secret: "s",
		Values: map[string]string{"email": "bot@acme.io", "token": "t", "project": "PAD"}}); invalidField(err) != "secret" {
		t.Errorf("a signing secret on jira: %v, want 400 on secret", err)
	}

	created, err := svc.Save(ctx, "o", ChannelSave{Name: strPtr("tickets"), Kind: "jira", URL: srv.URL,
		Values: map[string]string{"email": " bot@acme.io ", "token": "XXXXTOKEN", "project": "PAD"}})
	if err != nil {
		t.Fatal(err)
	}
	if created.Values["email"] != "bot@acme.io" || created.Values["project"] != "PAD" || created.Values["token"] != "" ||
		!created.Secrets["token"] || created.HasSecret || !strings.HasSuffix(created.URL, "/…") {
		t.Errorf("created view = %+v", created)
	}
	stored := b.OrgChannels[created.ID]
	if stored.URL != srv.URL || stored.Secrets["token"] != "XXXXTOKEN" || stored.Config["email"] != "bot@acme.io" || stored.Config["token"] != "" {
		t.Errorf("stored %+v: want the token in Secrets only", stored)
	}

	if _, err := svc.Save(ctx, "o", ChannelSave{ID: created.ID, Values: map[string]string{"project": "OPS", "token": ""}, Enabled: boolPtr(false)}); err != nil {
		t.Fatal(err)
	}
	if s := b.OrgChannels[created.ID]; s.Secrets["token"] != "XXXXTOKEN" || s.Config["project"] != "OPS" || s.URL != srv.URL || s.Enabled {
		t.Errorf("after an edit without the token: %+v, want the token and url kept", s)
	}
	if _, err := svc.Save(ctx, "o", ChannelSave{ID: created.ID, Clear: []string{"token"}}); invalidField(err) != "token" {
		t.Errorf("clearing the token: %v, want 400 on token (it is required)", err)
	}
	if _, err := svc.Save(ctx, "o", ChannelSave{ID: created.ID, Values: map[string]string{"project": ""}}); invalidField(err) != "project" {
		t.Errorf("clearing the project: %v, want 400 on project", err)
	}
	if _, err := svc.Save(ctx, "o", ChannelSave{ID: created.ID, Values: map[string]string{"token": "NEWTOKEN"}, Enabled: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Test(ctx, "o", created.ID); err != nil {
		t.Fatalf("Test = %v", err)
	}
	want := "Basic " + base64.StdEncoding.EncodeToString([]byte("bot@acme.io:NEWTOKEN"))
	if is := <-got; is.path != "/rest/api/3/issue" || is.auth != want {
		t.Errorf("the test filed %+v, want the issue with the new token", is)
	}
}

// emailEnv is an organisation with two verified members, an unverified member and a verified outsider.
func emailEnv(t *testing.T, mailer notify.EmailSender) (*testutil.FakeBackend, *ChannelService) {
	t.Helper()
	b := testutil.NewFakeBackend()
	ctx := context.Background()
	for _, u := range []*storageif.User{
		{ID: "ann", Email: "ann@acme.io", EmailVerified: true},
		{ID: "bob", Email: "bob@acme.io", EmailVerified: true},
		{ID: "unverified", Email: "unverified@acme.io"},
		{ID: "outsider", Email: "victim@example.com", EmailVerified: true},
	} {
		u.Password = "x"
		if err := b.CreateUser(ctx, u); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.SaveOrg(ctx, &storageif.Organisation{ID: "o", Name: "Acme", OwnerID: "ann", Members: []storageif.OrgMember{
		{UserID: "ann", Role: auth.RoleAdmin}, {UserID: "bob", Role: auth.RoleMember}, {UserID: "unverified", Role: auth.RoleMember},
	}}); err != nil {
		t.Fatal(err)
	}
	return b, NewChannelService(b, ChannelOptions{Policy: egress.BlockInternal, Mailer: mailer, Members: b,
		Runner: (&lifecycle.Group{}).NewBounded("notify_test", 4)})
}

// TestChannelService_EmailRecipientsAreMembers: an organisation's email channel mails members the admin chose, at
// their verified addresses, never an address the admin types: an outsider or an unverified member is refused at
// save (alike), and a member who left since is skipped at delivery. Without mail settings no email channel is saved.
func TestChannelService_EmailRecipientsAreMembers(t *testing.T) {
	ctx := context.Background()
	mailer := &sentMail{}
	b, svc := emailEnv(t, mailer)

	for _, tc := range []struct {
		name string
		save ChannelSave
		want string
	}{
		{"an outsider", ChannelSave{Kind: "email", Values: map[string]string{"recipients": "ann,outsider"}}, "recipients"},
		{"an unverified member", ChannelSave{Kind: "email", Values: map[string]string{"recipients": "unverified"}}, "recipients"},
		{"an address", ChannelSave{Kind: "email", Values: map[string]string{"recipients": "victim@example.com"}}, "recipients"},
		{"no recipients", ChannelSave{Kind: "email", Values: map[string]string{"recipients": ""}}, "recipients"},
		{"a typed address field", ChannelSave{Kind: "email", Values: map[string]string{"to": "victim@example.com", "recipients": "ann"}}, "to"},
		{"a url", ChannelSave{Kind: "email", URL: "https://203.0.113.10/x", Values: map[string]string{"recipients": "ann"}}, "url"},
	} {
		_, err := svc.Save(ctx, "o", tc.save)
		if invalidField(err) != tc.want {
			t.Errorf("%s: %v, want 400 on %s", tc.name, err, tc.want)
		}
		if tc.name == "an outsider" && strings.Contains(err.Error(), "victim@example.com") {
			t.Errorf("%s: the refusal names the account's address: %v", tc.name, err)
		}
	}
	_, outsiderErr := svc.Save(ctx, "o", ChannelSave{Kind: "email", Values: map[string]string{"recipients": "outsider"}})
	_, unverifiedErr := svc.Save(ctx, "o", ChannelSave{Kind: "email", Values: map[string]string{"recipients": "unverified"}})
	if strings.ReplaceAll(outsiderErr.Error(), "outsider", "X") != strings.ReplaceAll(unverifiedErr.Error(), "unverified", "X") {
		t.Errorf("an outsider and an unverified member are told apart:\n%v\n%v", outsiderErr, unverifiedErr)
	}

	saved, err := svc.Save(ctx, "o", ChannelSave{Name: strPtr("leads"), Kind: "email", Values: map[string]string{"recipients": "ann,bob"}})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Values["recipients"] != "ann,bob" || saved.URL != "" || saved.HasSecret {
		t.Errorf("saved view = %+v", saved)
	}
	if err := svc.Test(ctx, "o", saved.ID); err != nil {
		t.Fatalf("Test = %v", err)
	}
	if got := mailer.sent(); !slices.Equal(got, []string{"ann@acme.io", "bob@acme.io"}) {
		t.Errorf("the test mailed %v, want each recipient at their account's address", got)
	}

	// Bob leaves: the channel still lists him (an admin's choice), the delivery skips him, and a pause still saves.
	if err := b.MutateOrg(ctx, "o", func(o *storageif.Organisation) error {
		o.Members = slices.DeleteFunc(o.Members, func(m storageif.OrgMember) bool { return m.UserID == "bob" })
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Save(ctx, "o", ChannelSave{ID: saved.ID, Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("a pause with a recipient who left: %v", err)
	}
	if _, err := svc.Save(ctx, "o", ChannelSave{ID: saved.ID, Enabled: boolPtr(true)}); err != nil {
		t.Fatal(err)
	}
	ns, err := svc.OrgNotifiers(ctx, "o")
	if err != nil || len(ns) != 1 {
		t.Fatalf("OrgNotifiers = %d, %v", len(ns), err)
	}
	before := len(mailer.sent())
	if err := ns[0].Notify(ctx, notify.Event{Type: notify.EventDrift, Title: "t"}); err != nil {
		t.Fatal(err)
	}
	if got := mailer.sent()[before:]; !slices.Equal(got, []string{"ann@acme.io"}) {
		t.Errorf("after bob left the delivery mailed %v, want ann only", got)
	}

	// Without mail settings an email channel is neither offered, saved nor delivered.
	noMail := NewChannelService(b, ChannelOptions{Members: b, Runner: (&lifecycle.Group{}).NewBounded("notify_test", 4)})
	if slices.Contains(noMail.Choices().Kinds, "email") || !slices.Contains(svc.Choices().Kinds, "email") {
		t.Errorf("kinds without mail %v, with %v", noMail.Choices().Kinds, svc.Choices().Kinds)
	}
	if _, err := noMail.Save(ctx, "o", ChannelSave{Kind: "email", Values: map[string]string{"recipients": "ann"}}); invalidField(err) != "kind" {
		t.Errorf("an email channel without mail settings: %v, want 400 on kind", err)
	}
	if ns, _ := noMail.OrgNotifiers(ctx, "o"); len(ns) != 0 {
		t.Errorf("without mail settings %d email channels are delivered to", len(ns))
	}
	if err := noMail.Test(ctx, "o", saved.ID); invalidField(err) != "kind" {
		t.Errorf("Test without mail settings: %v, want 400 on kind", err)
	}
}

// TestChannelService_EventSubscriptions: a channel receives the events it chose, the test event always; a create
// without a choice and every channel saved before subscriptions receive the default set; an edit without events
// keeps the choice; an unknown or empty choice is refused on events.
func TestChannelService_EventSubscriptions(t *testing.T) {
	b := testutil.NewFakeBackend()
	svc := newTestChannels(t, b, egress.AllowLoopback)
	ctx := context.Background()
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "old", OrgID: "o", Kind: "webhook", URL: "http://127.0.0.1:9/old", Enabled: true})

	chosen, err := svc.Save(ctx, "o", ChannelSave{Kind: "webhook", URL: "http://127.0.0.1:9/new", Events: []string{"new_rules"}})
	if err != nil || !slices.Equal(chosen.Events, []notify.EventType{notify.EventNewRules}) {
		t.Fatalf("created = %+v, %v", chosen, err)
	}
	if _, err := svc.Save(ctx, "o", ChannelSave{ID: chosen.ID, Name: strPtr("renamed")}); err != nil {
		t.Fatal(err)
	}
	if got := b.OrgChannels[chosen.ID].Events; !slices.Equal(got, []string{"new_rules"}) {
		t.Errorf("an edit without events stored %v, want the choice kept", got)
	}
	plain, err := svc.Save(ctx, "o", ChannelSave{Kind: "slack", URL: "http://127.0.0.1:9/plain"})
	if err != nil || b.OrgChannels[plain.ID].Events != nil || len(plain.Events) != 3 {
		t.Errorf("a create without events: view %v, stored %v; want the default set, stored as none", plain.Events, b.OrgChannels[plain.ID].Events)
	}
	for _, bad := range [][]string{{}, {"test"}, {"analysis_complete"}, {"drift", "nope"}} {
		if _, err := svc.Save(ctx, "o", ChannelSave{ID: chosen.ID, Events: bad}); invalidField(err) != "events" {
			t.Errorf("events %q: %v, want 400 on events", bad, err)
		}
	}

	ns, err := svc.OrgNotifiers(ctx, "o")
	if err != nil || len(ns) != 3 {
		t.Fatalf("OrgNotifiers = %d, %v", len(ns), err)
	}
	receiving := 0
	for _, n := range ns {
		if notify.Receives(n, notify.EventDrift) {
			receiving++
		}
		if !notify.Receives(n, notify.EventNewRules) || !notify.Receives(n, notify.EventTest) {
			t.Error("a channel does not receive new_rules or the test")
		}
	}
	if receiving != 2 {
		t.Errorf("%d channels receive drift, want the two without a choice", receiving)
	}
}

// TestChannelService_ChoicesShowTheAllowListOnlyWhenVisible: the form learns the kinds, the events and that a host
// list applies; the list itself only when the operator made it visible, since it may name other customers' hosts.
func TestChannelService_ChoicesShowTheAllowListOnlyWhenVisible(t *testing.T) {
	b := testutil.NewFakeBackend()
	hidden := NewChannelService(b, ChannelOptions{AllowedHosts: []string{"hooks.slack.com", "acme.atlassian.net"}})
	c := hidden.Choices()
	if !c.HostsRestricted || c.AllowedHosts != nil {
		t.Errorf("hidden list: %+v, want restricted without the hosts", c)
	}
	if !slices.Equal(c.Events, notify.OrgEvents()) || !slices.Equal(c.DefaultEvents, notify.DefaultOrgEvents()) ||
		!slices.Equal(c.Kinds, []string{"jira", "slack", "teams", "teams-workflow", "webhook"}) {
		t.Errorf("choices = %+v", c)
	}
	shown := NewChannelService(b, ChannelOptions{AllowedHosts: []string{"hooks.slack.com"}, AllowedHostsVisible: true}).Choices()
	if !shown.HostsRestricted || !slices.Equal(shown.AllowedHosts, []string{"hooks.slack.com"}) {
		t.Errorf("visible list: %+v", shown)
	}
	if open := NewChannelService(b, ChannelOptions{AllowedHostsVisible: true}).Choices(); open.HostsRestricted || open.AllowedHosts != nil {
		t.Errorf("no list: %+v", open)
	}
}
