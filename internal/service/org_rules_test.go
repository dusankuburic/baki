package service

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// failingRuleStore refuses every rule write.
type failingRuleStore struct {
	*testutil.FakeBackend
	err error
}

func (s failingRuleStore) SaveOrgCustomRule(context.Context, *storageif.OrgCustomRule) error {
	return s.err
}

func (s failingRuleStore) DeleteOrgCustomRule(context.Context, string, string) error { return s.err }

// A stored rule change announces the org's changed configuration, which drops its cached rule profile (the
// next analysis re-resolves); a failed one leaves it, and other orgs' profiles are untouched.
func TestOrgRuleService_InvalidatesProfileOnChange(t *testing.T) {
	b := testutil.NewFakeBackend()
	b.SeedOrgs("org-a") // a rule needs its organisation
	resolver := NewRuleProfileResolver(b, deploymentSettings(nil), nil)
	bus := eventbus.New(nil, 0)
	SubscribeRuleProfiles(bus, resolver)
	svc := NewOrgRuleService(b, b, bus)
	ctx := context.Background()
	warm := func() {
		resolver.Resolve(ctx, "org-a")
		resolver.Resolve(ctx, "org-b")
	}

	warm()
	rule := &storageif.OrgCustomRule{ID: "r1", OrgID: "org-a", RuleID: "x", Config: []byte(`{}`), Enabled: true}
	if err := svc.save(ctx, rule); err != nil {
		t.Fatal(err)
	}
	if resolver.cachedFor("org-a") != nil {
		t.Error("Save left org-a's profile cached")
	}
	if resolver.cachedFor("org-b") == nil {
		t.Error("Save dropped another org's profile")
	}
	if got, err := svc.List(ctx, "org-a", false); err != nil || len(got) != 1 {
		t.Fatalf("List = %v, %v", got, err)
	}

	// Pausing and resuming (a toggle of the stored flag) change the profile too; a toggle of no such rule does not.
	for _, enabled := range []bool{false, true} {
		warm()
		if _, err := svc.SaveRule(ctx, "org-a", OrgRuleSave{RowID: "r1", Enabled: &enabled}); err != nil {
			t.Fatalf("toggle to enabled=%v: %v", enabled, err)
		}
		if resolver.cachedFor("org-a") != nil {
			t.Errorf("a toggle to enabled=%v left org-a's profile cached", enabled)
		}
		if resolver.cachedFor("org-b") == nil {
			t.Errorf("a toggle to enabled=%v dropped another org's profile", enabled)
		}
	}
	warm()
	off := false
	if _, err := svc.SaveRule(ctx, "org-a", OrgRuleSave{RowID: "no-such-row", Enabled: &off}); err == nil {
		t.Fatal("a toggle of no such rule succeeded")
	}
	if resolver.cachedFor("org-a") == nil {
		t.Error("a failed toggle invalidated the profile")
	}

	warm()
	if err := svc.Delete(ctx, "org-a", "r1"); err != nil {
		t.Fatal(err)
	}
	if resolver.cachedFor("org-a") != nil {
		t.Error("Delete left org-a's profile cached")
	}
	if got, _ := svc.List(ctx, "org-a", false); len(got) != 0 {
		t.Errorf("List after Delete = %v", got)
	}

	failing := NewOrgRuleService(failingRuleStore{FakeBackend: b, err: errors.New("db down")}, b, bus)
	warm()
	if err := failing.save(ctx, rule); err == nil {
		t.Fatal("Save swallowed the store error")
	}
	if err := failing.Delete(ctx, "org-a", "r1"); err == nil {
		t.Fatal("Delete swallowed the store error")
	}
	if resolver.cachedFor("org-a") == nil {
		t.Error("a failed write invalidated the profile")
	}

	// Without a bus (tests) writes still work.
	if err := NewOrgRuleService(b, b, nil).save(ctx, rule); err != nil {
		t.Fatal(err)
	}
}

func TestOrgRuleService_OrgSettings(t *testing.T) {
	b := testutil.NewFakeBackend()
	b.Orgs = map[string]*storageif.Organisation{"org-a": {ID: "org-a"}} // org settings reference their org
	want := &storageif.AppSettings{Version: 1}
	want.Analysis.Rules = map[string]storageif.RuleConfig{"deep-nesting": {Enabled: false}}
	if err := b.SaveOrgSettings(context.Background(), "org-a", want); err != nil {
		t.Fatal(err)
	}
	got, err := NewOrgRuleService(b, b, nil).OrgSettings(context.Background(), "org-a")
	if err != nil || got == nil || len(got.Analysis.Rules) != 1 {
		t.Fatalf("OrgSettings = %+v, %v", got, err)
	}
}
