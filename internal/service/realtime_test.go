package service

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/txhook"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// The settings writes name their audience: the user, the organisation (its roster resolved by the notifier), and
// for the deployment settings, written only on the desktop, the local user. Nothing is a broadcast.
func TestSystemService_SettingsChangedAudiences(t *testing.T) {
	n := &testutil.CountingNotifier{}
	// Settings rows reference their user and org.
	backend := &testutil.FakeBackend{
		Users: map[string]*storageif.User{"user-9": {ID: "user-9"}},
		Orgs:  map[string]*storageif.Organisation{"org-a": {ID: "org-a"}},
	}
	cloud := NewSystemService(newTestSettingsStore(t), nil, n, backend, config.ModeCloud, nil, nil)
	if err := cloud.UpdateUserSettings(context.Background(), "user-9", *models.DefaultSettings(), nil); err != nil {
		t.Fatal(err)
	}
	if err := cloud.UpdateOrgSettings(context.Background(), "org-a", *models.DefaultSettings(), OrgSettingsWrite{}); err != nil {
		t.Fatal(err)
	}
	desktop := NewSystemService(newTestSettingsStore(t), nil, n, nil, config.ModeLocal, nil, nil)
	if err := desktop.UpdateUserSettings(context.Background(), "local", *models.DefaultSettings(), nil); err != nil {
		t.Fatal(err)
	}
	want := []testutil.Emitted{
		{Audience: testutil.AudienceUser, Target: "user-9", Name: "settings:changed"},
		{Audience: testutil.AudienceOrg, Target: "org-a", Name: "settings:changed"},
		{Audience: testutil.AudienceUser, Target: LocalUser, Name: "settings:changed"},
	}
	got := n.Events()
	if len(got) != len(want) {
		t.Fatalf("emitted %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i].Audience != want[i].Audience || got[i].Target != want[i].Target || got[i].Name != want[i].Name {
			t.Errorf("emit %d = %s %q %s, want %s %q %s", i, got[i].Audience, got[i].Target, got[i].Name, want[i].Audience, want[i].Target, want[i].Name)
		}
		if _, ok := got[i].Data.(models.AppSettings); !ok {
			t.Errorf("emit %d carries %T, want the saved settings", i, got[i].Data)
		}
	}
}

// Each scope reaches exactly its audience, and nothing is ever a broadcast: a user's own clients, the
// organisation's members (resolved by the notifier), the clients of a deployment without accounts.
func TestNotifySettingsChanged_Audiences(t *testing.T) {
	cases := []struct {
		event events.SettingsChanged
		want  testutil.Emitted
	}{
		{events.SettingsChanged{Scope: events.UserSettings, ID: "user-9"}, testutil.Emitted{Audience: testutil.AudienceUser, Target: "user-9"}},
		{events.SettingsChanged{Scope: events.OrgSettings, ID: "org-a"}, testutil.Emitted{Audience: testutil.AudienceOrg, Target: "org-a"}},
		{events.SettingsChanged{Scope: events.DeploymentSettings}, testutil.Emitted{Audience: testutil.AudienceUser, Target: LocalUser}},
	}
	for _, c := range cases {
		n := &testutil.CountingNotifier{}
		NotifySettingsChanged(n)(context.Background(), c.event)
		got := n.Events()
		if len(got) != 1 || got[0].Audience != c.want.Audience || got[0].Target != c.want.Target || got[0].Name != "settings:changed" {
			t.Errorf("%+v: emitted %+v, want one settings:changed to %s %q", c.event, got, c.want.Audience, c.want.Target)
			continue
		}
		if data, _ := got[0].Data.(map[string]string); data["scope"] != string(c.event.Scope) {
			t.Errorf("%+v: payload %v names no scope", c.event, got[0].Data)
		}
	}

	n := &testutil.CountingNotifier{}
	NotifySettingsChanged(n)(context.Background(), events.SettingsChanged{Scope: events.UserSettings})
	if got := n.Events(); len(got) != 0 {
		t.Errorf("a user scope without a user emitted %+v; LocalUser is reached only through the deployment scope", got)
	}
}

// settings:changed leaves only once the write committed, and never for a rolled-back one.
func TestNotifySettingsChanged_OnlyAfterCommit(t *testing.T) {
	n := &testutil.CountingNotifier{}
	bus := eventbus.New(nil, time.Second)
	eventbus.Subscribe(bus, eventbus.Async, "sse-settings-changed", NotifySettingsChanged(n))

	ctx, reg := txhook.With(context.Background())
	bus.Publish(ctx, events.SettingsChanged{Scope: events.UserSettings, ID: "user-9"})
	if got := n.Count(); got != 0 {
		t.Fatalf("emitted %d before the commit", got)
	}
	reg.Commit(nil)
	if got := n.Events(); len(got) != 1 || got[0].Target != "user-9" {
		t.Fatalf("after the commit: %+v, want one emit to user-9", got)
	}

	n.Reset()
	ctx, reg = txhook.With(context.Background())
	bus.Publish(ctx, events.SettingsChanged{Scope: events.OrgSettings, ID: "org-a"})
	reg.Abort()
	if got := n.Count(); got != 0 {
		t.Fatalf("a rolled-back write emitted %d event(s)", got)
	}
}

type flowViewersProbe struct {
	flows    []string
	versions []int
}

func (p *flowViewersProbe) NotifyFlowChanged(flowID string, version int) {
	p.flows = append(p.flows, flowID)
	p.versions = append(p.versions, version)
}

// Only content updates reach a flow's viewers, always with a fresh millisecond stamp (never the stored version,
// which a viewer may already hold over stale content).
func TestNotifyFlowViewers_UpdatesOnly(t *testing.T) {
	p := &flowViewersProbe{}
	notify := NotifyFlowViewers(p)
	for _, kind := range []events.FlowChangeKind{events.FlowCreated, events.FlowDeleted, events.FlowSharingChanged} {
		notify(context.Background(), events.FlowChanged{FlowID: "f1", Kind: kind, Version: 3})
	}
	if len(p.flows) != 0 {
		t.Fatalf("non-update kinds notified viewers: %v", p.flows)
	}

	before := time.Now().UnixMilli()
	notify(context.Background(), events.FlowChanged{FlowID: "f1", Kind: events.FlowUpdated, Version: 7})
	notify(context.Background(), events.FlowChanged{FlowID: "f2", Kind: events.FlowUpdated})
	if len(p.flows) != 2 || p.flows[0] != "f1" || p.flows[1] != "f2" {
		t.Fatalf("notified %v, want f1 then f2", p.flows)
	}
	for i, v := range p.versions {
		if int64(v) < before {
			t.Errorf("notification %d sent version %d, want the time in milliseconds (>= %d)", i, v, before)
		}
	}
}

// flow.changed leaves only once the write committed: a viewer that refetched before the commit read the old row.
func TestNotifyFlowViewers_OnlyAfterCommit(t *testing.T) {
	p := &flowViewersProbe{}
	bus := eventbus.New(nil, time.Second)
	eventbus.Subscribe(bus, eventbus.Async, "ws-flow-changed", NotifyFlowViewers(p))

	ctx, reg := txhook.With(context.Background())
	bus.Publish(ctx, events.FlowChanged{FlowID: "f1", Kind: events.FlowUpdated, Version: 4})
	if len(p.flows) != 0 {
		t.Fatal("viewers notified before the commit")
	}
	reg.Commit(nil)
	if len(p.flows) != 1 || p.flows[0] != "f1" {
		t.Fatalf("after the commit: %v, want f1", p.flows)
	}

	ctx, reg = txhook.With(context.Background())
	bus.Publish(ctx, events.FlowChanged{FlowID: "f1", Kind: events.FlowUpdated, Version: 5})
	reg.Abort()
	if len(p.flows) != 1 {
		t.Fatalf("a rolled-back write notified viewers: %v", p.versions)
	}
}
