package service

import (
	"context"
	"errors"
	"testing"

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

// A stored rule change drops the org's cached rule profile (the next analysis re-resolves); a failed one
// leaves it, and other orgs' profiles are untouched.
func TestOrgRuleService_InvalidatesProfileOnChange(t *testing.T) {
	b := testutil.NewFakeBackend()
	resolver := NewRuleProfileResolver(b, deploymentSettings(nil), nil)
	svc := NewOrgRuleService(b, b, resolver)
	ctx := context.Background()
	warm := func() {
		resolver.Resolve(ctx, "org-a")
		resolver.Resolve(ctx, "org-b")
	}

	warm()
	rule := &storageif.OrgCustomRule{ID: "r1", OrgID: "org-a", RuleID: "x", Config: []byte(`{}`), Enabled: true}
	if err := svc.Save(ctx, rule); err != nil {
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

	failing := NewOrgRuleService(failingRuleStore{FakeBackend: b, err: errors.New("db down")}, b, resolver)
	warm()
	if err := failing.Save(ctx, rule); err == nil {
		t.Fatal("Save swallowed the store error")
	}
	if err := failing.Delete(ctx, "org-a", "r1"); err == nil {
		t.Fatal("Delete swallowed the store error")
	}
	if resolver.cachedFor("org-a") == nil {
		t.Error("a failed write invalidated the profile")
	}

	// Without a resolver (tests, desktop) writes still work.
	if err := NewOrgRuleService(b, b, nil).Save(ctx, rule); err != nil {
		t.Fatal(err)
	}
}

func TestOrgRuleService_OrgSettings(t *testing.T) {
	b := testutil.NewFakeBackend()
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
