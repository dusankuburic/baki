package service

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/notify"
	"pad-analyzer/internal/platform/apperr"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

func newTestChannels(t *testing.T, b *testutil.FakeBackend, policy egress.Policy) *ChannelService {
	t.Helper()
	return NewChannelService(b, ChannelOptions{Policy: policy, Runner: (&lifecycle.Group{}).NewBounded("notify_test", 4)})
}

func strPtr(s string) *string { return &s }
func boolPtr(b bool) *bool    { return &b }

func TestChannelService_OrgScoped(t *testing.T) {
	b := testutil.NewFakeBackend()
	b.SeedOrgs("org-a", "org-b") // a channel needs its organisation
	svc := newTestChannels(t, b, egress.BlockInternal)
	ctx := context.Background()

	for _, ch := range []*storageif.OrgChannel{
		{ID: "c1", OrgID: "org-a", Name: "ops", Kind: "webhook", URL: "https://hooks.example/1"},
		{ID: "c2", OrgID: "org-a", Name: "off", Kind: "slack", URL: "https://hooks.example/2"},
		{ID: "c3", OrgID: "org-b", Name: "theirs", Kind: "teams", URL: "https://hooks.example/3"},
	} {
		if err := b.SaveOrgChannel(ctx, ch); err != nil {
			t.Fatal(err)
		}
	}
	if got, err := svc.List(ctx, "org-a"); err != nil || len(got) != 2 {
		t.Fatalf("List(org-a) = %v, %v; want 2", got, err)
	}
	if got, err := svc.find(ctx, "org-a", "c3"); err != nil || got != nil {
		t.Errorf("find of another org's channel = %v, %v; want nil, nil", got, err)
	}
	if err := svc.Test(ctx, "org-a", "c3"); !errors.Is(err, apperr.NotFound("", "")) {
		t.Errorf("testing another org's channel: %v, want 404", err)
	}
	if err := svc.Delete(ctx, "org-a", "c1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.find(ctx, "org-a", "c1"); got != nil {
		t.Error("Delete left the channel")
	}
	if err := svc.Delete(ctx, "org-a", "c3"); err != nil {
		t.Fatal(err)
	}
	if got, _ := svc.find(ctx, "org-b", "c3"); got == nil {
		t.Error("Delete through another org removed the channel")
	}
}

// TestChannelService_ListRedactsForEveryone: the URL is write-only, for admins too (the list shows scheme and host);
// a secret is never shown, only that there is one.
func TestChannelService_ListRedactsForEveryone(t *testing.T) {
	b := testutil.NewFakeBackend()
	b.SeedOrgs("o") // a channel needs its organisation
	svc := newTestChannels(t, b, egress.BlockInternal)
	ctx := context.Background()
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "c1", OrgID: "o", Kind: "slack", URL: "https://hooks.slack.com/services/T/B/SECRET", Secret: "s", Enabled: true})
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "c2", OrgID: "o", Kind: "pagerduty", URL: "https://x.example/path?sig=SECRET"})
	// A Teams row with a secret column a release before the kind registry stored: Teams signs nothing.
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "c3", OrgID: "o", Kind: "teams", URL: "https://acme.webhook.office.com/webhookb2/SECRET", Secret: "legacy"})
	got, err := svc.List(ctx, "o")
	if err != nil || len(got) != 3 {
		t.Fatalf("List = %v, %v", got, err)
	}
	for _, v := range got {
		switch v.ID {
		case "c1":
			if v.URL != "hooks.slack.com/…" || !v.HasSecret {
				t.Errorf("slack view = %+v", v)
			}
		case "c2":
			if v.URL != "x.example/…" || v.HasSecret {
				t.Errorf("unknown-kind view = %+v", v)
			}
		case "c3":
			if v.URL != "acme.webhook.office.com/…" || v.HasSecret {
				t.Errorf("teams view with a legacy secret = %+v, want hasSecret false", v)
			}
		}
	}
}

// TestChannelService_SaveKeepsWhatAnEditOmits is the service half of B1, B2 and the stale-tab guard: an edit keeps
// the secret, the state, the name and the creation time it omits; the URL as the list showed it, or urlUnchanged,
// keeps the stored URL; clear removes the secret.
func TestChannelService_SaveKeepsWhatAnEditOmits(t *testing.T) {
	b := testutil.NewFakeBackend()
	b.SeedOrgs("o") // a channel needs its organisation
	svc := newTestChannels(t, b, egress.AllowLoopback)
	ctx := context.Background()
	const url = "http://127.0.0.1:9/services/T/B/SECRET"
	created, err := svc.Save(ctx, "o", ChannelSave{Name: strPtr("ops"), Kind: "slack", URL: url, Secret: "k1"})
	if err != nil {
		t.Fatal(err)
	}
	if created.URL != "127.0.0.1:9/…" || !created.HasSecret || !created.Enabled {
		t.Fatalf("created = %+v", created)
	}
	stored := func() *storageif.OrgChannel {
		ch, err := svc.find(ctx, "o", created.ID)
		if err != nil || ch == nil {
			t.Fatalf("find: %v %v", ch, err)
		}
		return ch
	}

	steps := []struct {
		name  string
		save  ChannelSave
		check func(ch *storageif.OrgChannel) bool
	}{
		{"pause with the listed URL", ChannelSave{Kind: "slack", URL: created.URL, Enabled: boolPtr(false)},
			func(ch *storageif.OrgChannel) bool {
				return ch.URL == url && ch.Secret == "k1" && !ch.Enabled && ch.Name == "ops"
			}},
		{"rename without url or enabled", ChannelSave{Name: strPtr("ops 2"), Kind: "slack"},
			func(ch *storageif.OrgChannel) bool {
				return ch.URL == url && ch.Secret == "k1" && !ch.Enabled && ch.Name == "ops 2"
			}},
		{"urlUnchanged with the listed url", ChannelSave{URLUnchanged: true, URL: created.URL, Enabled: boolPtr(true)},
			func(ch *storageif.OrgChannel) bool { return ch.URL == url && ch.Enabled && ch.Kind == "slack" }},
		{"urlUnchanged alone", ChannelSave{URLUnchanged: true, Name: strPtr("ops")},
			func(ch *storageif.OrgChannel) bool { return ch.URL == url && ch.Enabled && ch.Name == "ops" }},
		{"a new secret replaces it", ChannelSave{Kind: "slack", Secret: "k2"},
			func(ch *storageif.OrgChannel) bool { return ch.Secret == "k2" && ch.URL == url }},
		{"a new url replaces it", ChannelSave{Kind: "slack", URL: "http://127.0.0.1:9/services/T/B/NEW"},
			func(ch *storageif.OrgChannel) bool {
				return ch.URL == "http://127.0.0.1:9/services/T/B/NEW" && ch.Secret == "k2"
			}},
		{"clear removes the secret", ChannelSave{Kind: "slack", Clear: []string{"secret"}},
			func(ch *storageif.OrgChannel) bool {
				return ch.Secret == "" && ch.URL == "http://127.0.0.1:9/services/T/B/NEW"
			}},
	}
	createdAt := stored().CreatedAt
	for _, step := range steps {
		step.save.ID = created.ID
		view, err := svc.Save(ctx, "o", step.save)
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}
		ch := stored()
		if !step.check(ch) {
			t.Errorf("%s: stored %+v", step.name, ch)
		}
		if !ch.CreatedAt.Equal(createdAt) || !view.CreatedAt.Equal(createdAt) {
			t.Errorf("%s: createdAt %v / %v, want %v kept", step.name, ch.CreatedAt, view.CreatedAt, createdAt)
		}
	}
}

func TestChannelService_SaveRefuses(t *testing.T) {
	b := testutil.NewFakeBackend()
	b.SeedOrgs("o") // a channel needs its organisation
	svc := newTestChannels(t, b, egress.BlockInternal)
	ctx := context.Background()
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "t1", OrgID: "o", Kind: "teams", URL: "https://203.0.113.10/t", Enabled: true})
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "w1", OrgID: "o", Kind: "webhook", URL: "https://203.0.113.10/w", Secret: "s", Enabled: true})
	for _, tc := range []struct {
		name  string
		save  ChannelSave
		field string
	}{
		{"unknown kind", ChannelSave{Kind: "pager", URL: "https://203.0.113.10/x"}, "kind"},
		{"deployment kind", ChannelSave{Kind: "email", URL: "https://203.0.113.10/x"}, "kind"},
		{"create without url", ChannelSave{Kind: "webhook"}, "url"},
		{"create with urlUnchanged", ChannelSave{Kind: "webhook", URLUnchanged: true}, "url"},
		{"internal target", ChannelSave{Kind: "webhook", URL: "https://10.0.0.5/x"}, "url"},
		{"secret on teams", ChannelSave{ID: "t1", Kind: "teams", Secret: "s"}, "secret"},
		{"clear the url", ChannelSave{ID: "w1", Kind: "webhook", Clear: []string{"url"}}, "url"},
		{"clear an unknown field", ChannelSave{ID: "w1", Kind: "webhook", Clear: []string{"token"}}, "token"},
		{"set and clear", ChannelSave{ID: "w1", Kind: "webhook", Secret: "x", Clear: []string{"secret"}}, "secret"},
		{"urlUnchanged with a new url", ChannelSave{ID: "w1", URLUnchanged: true, URL: "https://203.0.113.20/w"}, "url"},
		{"create with a listed url", ChannelSave{Kind: "webhook", URL: "203.0.113.10/…"}, "url"},
		{"create with a redacted url", ChannelSave{Kind: "webhook", URL: "https://203.0.113.10/…"}, "url"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := svc.Save(ctx, "o", tc.save)
			ae, ok := apperr.As(err)
			if !ok || ae.Kind != apperr.KindInvalid || ae.Details["field"] != tc.field {
				t.Errorf("Save = %v, want 400 naming %s", err, tc.field)
			}
		})
	}
	if ch, _ := svc.find(ctx, "o", "w1"); ch.Secret != "s" {
		t.Errorf("a refused save changed the channel: %+v", ch)
	}
}

// TestChannelService_RedactedURLIsNeverStored is notify-1: the listing's placeholder used to be a valid https URL
// itself, so a tab listing the channel before another admin moved it to another host re-sent the placeholder, which
// passed validation and replaced the real URL. Only the current listing reads as unchanged; any other URL carrying
// the marker is refused, and the listed form has no scheme, so no release's validation accepts it.
func TestChannelService_RedactedURLIsNeverStored(t *testing.T) {
	b := testutil.NewFakeBackend()
	b.SeedOrgs("o") // a channel needs its organisation
	svc := newTestChannels(t, b, egress.BlockInternal)
	ctx := context.Background()
	const moved = "https://203.0.113.20/hook/NEW"
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "w1", OrgID: "o", Kind: "webhook", URL: moved, Secret: "s", Enabled: true})

	for _, stale := range []string{
		"https://203.0.113.10/…",           // the listing before this fix, of the URL before the move
		"203.0.113.10/…",                   // the listing now, ditto
		"https://203.0.113.10/%E2%80%A6",   // percent-encoded on the way
		"https://203.0.113.10/x/%e2%80%a6", // lower-case hex
		"…",
	} {
		_, err := svc.Save(ctx, "o", ChannelSave{ID: "w1", Kind: "webhook", URL: stale, Enabled: boolPtr(false)})
		ae, ok := apperr.As(err)
		if !ok || ae.Kind != apperr.KindInvalid || ae.Details["field"] != "url" || !strings.Contains(ae.Message, "redacted placeholder") {
			t.Errorf("Save(url %q) = %v, want 400 naming url", stale, err)
		}
	}
	if ch, _ := svc.find(ctx, "o", "w1"); ch.URL != moved || !ch.Enabled {
		t.Fatalf("a refused save changed the channel: %+v", ch)
	}

	listed := notify.ListedURL(moved)
	if _, err := svc.Save(ctx, "o", ChannelSave{ID: "w1", Kind: "webhook", URL: listed, Enabled: boolPtr(false)}); err != nil {
		t.Fatalf("the current listing: %v", err)
	}
	if ch, _ := svc.find(ctx, "o", "w1"); ch.URL != moved || ch.Enabled || ch.Secret != "s" {
		t.Errorf("after a save with the listed url: %+v, want the url and secret kept", ch)
	}
	// What the previous release's validation (https and a host) makes of the listing: refused, whatever the URL.
	for _, u := range []string{moved, "https://hooks.slack.com/services/T/B/X", "http://127.0.0.1:9/x", "https://localhost:8443/x"} {
		if err := notify.Validate(ctx, "webhook", notify.ScopeOrg, notify.Values{"url": notify.ListedURL(u)}, egress.AllowLoopback); err == nil {
			t.Errorf("the listing of %q validates as a channel URL", u)
		}
	}
}

// TestChannelService_TestAnswersCoarsely: a failed test is 502 with the endpoint's status or a fixed description,
// never the URL (B13).
func TestChannelService_TestAnswersCoarsely(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusNotFound)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(int(status.Load())) }))
	defer srv.Close()
	b := testutil.NewFakeBackend()
	b.SeedOrgs("o") // a channel needs its organisation
	svc := newTestChannels(t, b, egress.AllowLoopback)
	ctx := context.Background()
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "c1", OrgID: "o", Kind: "webhook", URL: srv.URL + "/hook?token=SECRET", Enabled: true})
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "c2", OrgID: "o", Kind: "webhook", URL: "http://127.0.0.1:1/hook?token=SECRET", Enabled: true})

	err := svc.Test(ctx, "o", "c1")
	ae, ok := apperr.As(err)
	if !ok || ae.Kind != apperr.KindBadGateway || ae.Code != "CHANNEL_DELIVERY_FAILED" || ae.Details["status"] != http.StatusNotFound {
		t.Fatalf("Test against a 404 = %v (%+v)", err, ae)
	}
	err = svc.Test(ctx, "o", "c2")
	// Windows words the refusal so that DescribeFailure files it under the generic description.
	if ae, ok = apperr.As(err); !ok || (ae.Details["reason"] != "the connection was refused" && ae.Details["reason"] != "the connection failed") {
		t.Fatalf("Test against a closed port = %v (%+v)", err, ae)
	}
	if text := err.Error(); containsAny(text, "SECRET", "token=") {
		t.Errorf("the error carries the URL: %s", text)
	}
	status.Store(http.StatusOK)
	if err := svc.Test(ctx, "o", "c1"); err != nil {
		t.Errorf("Test against a 200 = %v", err)
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
	}
	return false
}

// TestChannelService_EnqueueStampsAndSignalsDone: an org delivery carries the channel's org, an id and a time, and
// done runs once the delivery ends.
func TestChannelService_EnqueueStampsAndSignalsDone(t *testing.T) {
	got := make(chan *http.Request, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	b := testutil.NewFakeBackend()
	b.SeedOrgs("o") // a channel needs its organisation
	svc := newTestChannels(t, b, egress.AllowLoopback)
	ctx := context.Background()
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "c1", OrgID: "o", Kind: "webhook", URL: srv.URL, Enabled: true})
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "c2", OrgID: "o", Kind: "webhook", URL: srv.URL, Enabled: false})
	ns, err := svc.OrgNotifiers(ctx, "o")
	if err != nil || len(ns) != 1 {
		t.Fatalf("OrgNotifiers = %d, %v; want the enabled one", len(ns), err)
	}
	done := make(chan struct{})
	if !svc.Enqueue(ctx, ns[0], notify.Event{Type: notify.EventDrift, FlowID: "f"}, func() { close(done) }) {
		t.Fatal("Enqueue did not start")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("done never ran")
	}
	r := <-got
	if r.Header.Get(notify.HeaderDelivery) == "" || r.Header.Get("User-Agent") != notify.UserAgent(Version) {
		t.Errorf("headers %v", r.Header)
	}
}
