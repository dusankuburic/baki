package service

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// countingChatProvider answers every Chat and counts them.
type countingChatProvider struct {
	*testutil.FakeProvider
	id    string
	chats int
}

func (p *countingChatProvider) ID() string { return p.id }
func (p *countingChatProvider) PricePerMillionTokens() ai.Pricing {
	return ai.Pricing{InputCostPerM: 1, OutputCostPerM: 1}
}
func (p *countingChatProvider) Chat(context.Context, ai.Request) (*ai.Response, error) {
	p.chats++
	return &ai.Response{Content: "ok", TokensIn: 4, TokensOut: 2}, nil
}

// spendByOrg reads today's spend per org ("" = the user's own).
type spendByOrg map[string]float64

func (s spendByOrg) GetDailyUsage(_ context.Context, _, orgID string) (float64, error) {
	return s[orgID], nil
}

// budgetedProviderService builds a provider service whose factory runs on the given budget, with every usage
// row forwarded to the returned channel.
func budgetedProviderService(t *testing.T, budget *AIBudget, secrets *testutil.FakeSecretStore, orgKeys *testutil.FakeOrgSecretStore) (*ProviderService, <-chan *storageif.UsageMetric) {
	t.Helper()
	rows := make(chan *storageif.UsageMetric, 8)
	rec := func(_ context.Context, m *storageif.UsageMetric) error { rows <- m; return nil }
	getKey := func(scope, provider string) (string, error) {
		v, _ := secrets.Get(scope, provider)
		return v, nil
	}
	factory := ai.NewProviderFactory(getKey, ai.NewCopilotAuth(), rec, nil, ai.WithBudgetChecker(budget))
	if orgKeys != nil {
		factory.SetOrgKeyLookup(orgKeys.OrgKeyLookup)
	}
	return NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets, orgKeys, nil), rows
}

// ADR 0028: the connection test is a billed Chat like any other, so the wrapper's hook gates it on the caller's
// daily budget — nothing is sent, no usage row is written, and the result carries the budget code.
func TestProviderService_TestConnection_RefusedOverBudget(t *testing.T) {
	stub := &countingChatProvider{FakeProvider: &testutil.FakeProvider{}, id: "budget-test-stub"}
	ai.RegisterProvider(stub.id, func(string) ai.Provider { return stub })
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", stub.id, "k")
	spend := spendByOrg{"": 9}
	budget := NewAIBudget(&staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 5}}}, spend)
	svc, rows := budgetedProviderService(t, budget, secrets, nil)

	res, err := svc.TestProviderConnection(context.Background(), "u1", stub.id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ok || res.Code != ChatErrBudgetExceeded || res.Error == "" {
		t.Errorf("over budget: %+v, want Ok:false with code %s and the budget message", res, ChatErrBudgetExceeded)
	}
	if stub.chats != 0 {
		t.Errorf("Chat was called %d times over budget", stub.chats)
	}
	select {
	case m := <-rows:
		t.Errorf("a usage row was recorded for the refused test: %+v", m)
	case <-time.After(50 * time.Millisecond):
	}

	// With budget left the test runs and bills the caller.
	spend[""] = 1
	res, err = svc.TestProviderConnection(context.Background(), "u1", stub.id)
	if err != nil || !res.Ok || res.Code != "" {
		t.Fatalf("under budget: %+v (%v)", res, err)
	}
	select {
	case m := <-rows:
		if m.UserID != "u1" || m.OrgID != "" {
			t.Errorf("usage row = user %q org %q, want u1 personal", m.UserID, m.OrgID)
		}
	case <-time.After(2 * time.Second):
		t.Error("no usage row for the admitted test")
	}
}

// The org variant runs on the org's shared credential and bills the org, so it is gated on the ORG's budget:
// a caller with personal budget left is still refused when the org's is spent, and vice versa.
func TestProviderService_TestOrgConnection_IsGatedOnTheOrgBudget(t *testing.T) {
	stub := &countingChatProvider{FakeProvider: &testutil.FakeProvider{}, id: "budget-org-test-stub"}
	ai.RegisterProvider(stub.id, func(string) ai.Provider { return stub })
	orgKeys := testutil.NewFakeOrgSecretStore()
	_ = orgKeys.SaveOrg("org-1", stub.id, "org-k", "u-admin")
	spend := spendByOrg{"": 0, "org-1": 9}
	budget := NewAIBudget(&staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 5}}}, spend)
	svc, rows := budgetedProviderService(t, budget, testutil.NewFakeSecretStore(), orgKeys)

	res, err := svc.TestOrgProviderConnection(context.Background(), "u-admin", "org-1", stub.id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ok || res.Code != ChatErrBudgetExceeded {
		t.Errorf("org budget spent: %+v, want refused with %s", res, ChatErrBudgetExceeded)
	}
	if stub.chats != 0 {
		t.Errorf("Chat was called %d times on a spent org budget", stub.chats)
	}

	spend["org-1"], spend[""] = 1, 9
	res, err = svc.TestOrgProviderConnection(context.Background(), "u-admin", "org-1", stub.id)
	if err != nil || !res.Ok {
		t.Fatalf("org budget left, personal spent: %+v (%v), want the test run on the org's budget", res, err)
	}
	select {
	case m := <-rows:
		if m.OrgID != "org-1" || m.UserID != "u-admin" {
			t.Errorf("usage row = org %q user %q, want org-1 / u-admin", m.OrgID, m.UserID)
		}
	case <-time.After(2 * time.Second):
		t.Error("no usage row for the admitted org test")
	}

	// A refused provider (the policy) is coded too, before any key is read.
	svc.factory = ai.NewProviderFactory(func(string, string) (string, error) { t.Error("key read for a refused provider"); return "", nil }, nil, nil, nil,
		ai.WithProviderPolicy(NewProviderPolicy(&staticSettings{s: models.AppSettings{AI: models.AISettings{Providers: map[string]models.AIProviderConfig{"claude": {Enabled: false}}}}}, nil)))
	res, err = svc.TestProviderConnection(context.Background(), "u1", "claude")
	if err != nil || res.Ok || res.Code != ProviderCodeNotAllowed {
		t.Errorf("disabled provider: %+v (%v), want code %s", res, err, ProviderCodeNotAllowed)
	}
}
