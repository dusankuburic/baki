package service

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// G7: prepareTurn runs an org flow on the organisation's shared credential
// when the member has none of their own, keeps the member's when they have
// one, and never consults the org for a personal flow.
func TestPrepareTurn_UsesOrgCredentialWhenMemberHasNone(t *testing.T) {
	registerFakeProvider()
	memberKeys := map[string]string{"u-own": "own-key"}
	orgCalls := 0
	factory := ai.NewProviderFactory(func(scope, _ string) (string, error) { return memberKeys[scope], nil }, nil, nil, nil)
	factory.SetOrgKeyLookup(func(orgID, _ string) (string, error) {
		orgCalls++
		if orgID == "org-A" {
			return "org-key", nil
		}
		return "", nil
	})
	svc := &ChatService{
		factory:  factory,
		settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 100}}},
		backend:  testutil.NewFakeBackend(),
	}
	orgDoc := &models.FlowDocument{ID: "f1", OrganizationID: "org-A"}

	ts, err := svc.prepareTurn(context.Background(), "u-none", orgDoc, models.ChatRequest{Provider: "fake"}, &streamCtl{ownerID: "u-none", orgID: "org-A"})
	if err != nil {
		t.Fatalf("member without a key on an org flow: %v (want the org's credential)", err)
	}
	if ts.orgID != "org-A" || ts.provider == nil {
		t.Errorf("turn setup = %+v", ts)
	}
	if orgCalls != 1 {
		t.Errorf("org store consulted %d times, want 1", orgCalls)
	}

	orgCalls = 0
	if _, err := svc.prepareTurn(context.Background(), "u-own", orgDoc, models.ChatRequest{Provider: "fake"}, &streamCtl{ownerID: "u-own", orgID: "org-A"}); err != nil {
		t.Fatalf("member with their own key: %v", err)
	}
	if orgCalls != 0 {
		t.Errorf("member's own key must win without consulting the org (consulted %d times)", orgCalls)
	}

	orgCalls = 0
	personal := &models.FlowDocument{ID: "f2"}
	_, err = svc.prepareTurn(context.Background(), "u-none", personal, models.ChatRequest{Provider: "fake"}, &streamCtl{ownerID: "u-none"})
	if !errors.Is(err, ai.ErrKeyNotConfigured) {
		t.Errorf("personal flow without a key: err = %v, want ErrKeyNotConfigured", err)
	}
	if orgCalls != 0 {
		t.Errorf("personal flow consulted the org store %d times", orgCalls)
	}

	_, err = svc.prepareTurn(context.Background(), "u-none", &models.FlowDocument{ID: "f3", OrganizationID: "org-B"}, models.ChatRequest{Provider: "fake"}, &streamCtl{ownerID: "u-none", orgID: "org-B"})
	if !errors.Is(err, ai.ErrKeyNotConfigured) {
		t.Errorf("org without a credential: err = %v, want ErrKeyNotConfigured", err)
	}
}
