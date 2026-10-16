package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/notify"
	"pad-analyzer/internal/platform/apperr"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// The deployment's host allow-list (PAD_NOTIFY_ALLOWED_HOSTS) restricts an organisation's channels: refused at save,
// and again at delivery, for a stored channel whose host left the list.

func newAllowListedChannels(b *testutil.FakeBackend, policy egress.Policy, hosts ...string) *ChannelService {
	return NewChannelService(b, ChannelOptions{Policy: policy, AllowedHosts: hosts, Runner: (&lifecycle.Group{}).NewBounded("notify_test", 4)})
}

// TestChannelService_SaveRefusesAHostOffTheAllowList: 400 INVALID_CHANNEL on url, with a message that names the
// host as typed and no address; an on-list host saves.
func TestChannelService_SaveRefusesAHostOffTheAllowList(t *testing.T) {
	b := testutil.NewFakeBackend()
	b.SeedOrgs("o") // a channel needs its organisation
	svc := newAllowListedChannels(b, egress.AllowLoopback, "localhost", "*.logic.azure.com")
	ctx := context.Background()
	for _, save := range []ChannelSave{
		{Kind: "webhook", URL: "http://127.0.0.1:9/hook"},
		{Kind: "teams-workflow", URL: "https://evil.example/workflows/x?sig=SECRET"},
		{Kind: "slack", URL: "https://logic.azure.com.evil.example/x"},
	} {
		_, err := svc.Save(ctx, "o", save)
		ae, ok := apperr.As(err)
		if !ok || ae.Kind != apperr.KindInvalid || ae.Code != "INVALID_CHANNEL" || ae.Details["field"] != "url" {
			t.Errorf("Save(%s) = %v, want 400 INVALID_CHANNEL on url", save.URL, err)
			continue
		}
		if containsAny(ae.Message, "SECRET", "/workflows", "127.0.0.1:9/", "resolve") {
			t.Errorf("Save(%s) answered %q: it names more than the host", save.URL, ae.Message)
		}
	}
	if got, _ := b.ListOrgChannels(ctx, "o", false); len(got) != 0 {
		t.Fatalf("a refused save stored %+v", got)
	}
	if _, err := svc.Save(ctx, "o", ChannelSave{Kind: "webhook", URL: "http://localhost:9/hook"}); err != nil {
		t.Errorf("an on-list host: %v", err)
	}
	// Unset is today's behaviour.
	if _, err := newTestChannels(t, b, egress.AllowLoopback).Save(ctx, "o", ChannelSave{Kind: "webhook", URL: "http://127.0.0.1:9/hook"}); err != nil {
		t.Errorf("without an allow-list: %v", err)
	}
}

// TestChannelService_DeliveryObeysTheAllowList: a channel stored before its host left the list (or by the previous
// release) is refused by the test endpoint and left out of the scanner's notifiers, and nothing reaches its host.
func TestChannelService_DeliveryObeysTheAllowList(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	port := srv.URL[strings.LastIndex(srv.URL, ":"):]
	b := testutil.NewFakeBackend()
	b.SeedOrgs("o") // a channel needs its organisation
	ctx := context.Background()
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "off", OrgID: "o", Name: "left the list", Kind: "teams-workflow", URL: "http://127.0.0.1" + port + "/workflows/x", Enabled: true})
	_ = b.SaveOrgChannel(ctx, &storageif.OrgChannel{ID: "on", OrgID: "o", Name: "listed", Kind: "webhook", URL: "http://localhost" + port + "/hook", Enabled: true})
	svc := newAllowListedChannels(b, egress.AllowLoopback, "localhost")

	err := svc.Test(ctx, "o", "off")
	ae, ok := apperr.As(err)
	if !ok || ae.Kind != apperr.KindInvalid || ae.Details["field"] != "url" {
		t.Errorf("Test(off-list) = %v, want 400 naming url", err)
	}
	built, err := svc.OrgNotifiers(ctx, "o")
	if err != nil {
		t.Fatal(err)
	}
	if len(built) != 1 || built[0].Name() != "webhook" {
		t.Fatalf("OrgNotifiers = %v, want the listed channel only", built)
	}
	notify.Deliver(ctx, built[0], notify.Event{Type: notify.EventTest, Title: "t", Message: "m"})
	if n := hits.Load(); n != 1 {
		t.Errorf("hits = %d, want the listed channel's delivery only", n)
	}
	if err := svc.Test(ctx, "o", "on"); err != nil {
		t.Errorf("Test(listed) = %v", err)
	}

	// The list is the deployment's: the same rows, served without one, deliver to both.
	open := newTestChannels(t, b, egress.AllowLoopback)
	if built, _ := open.OrgNotifiers(ctx, "o"); len(built) != 2 {
		t.Errorf("without an allow-list OrgNotifiers = %d channels, want 2", len(built))
	}
}
