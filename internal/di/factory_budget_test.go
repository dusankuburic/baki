package di

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// diBudgetStub answers Chat and counts the calls that reached it.
type diBudgetStub struct {
	*testutil.FakeProvider
	chats int
}

func (p *diBudgetStub) ID() string { return "di-budget-stub" }
func (p *diBudgetStub) Chat(context.Context, ai.Request) (*ai.Response, error) {
	p.chats++
	return &ai.Response{Content: "ok", TokensIn: 1, TokensOut: 1}, nil
}

// ADR 0028 wiring: the factory ProvideAI builds runs its chains on the ProvideAIBudget instance (the one chat
// turns and the knowledge base consult) and on the ProvideProviderPolicy instance, so a call on a spent
// budget, or on a disabled provider, is refused in the wrapper and at the factory — with the service's own
// sentinels. A factory built without them (the di-only test graphs, a nil budget) gates nothing.
func TestProvideAI_FactoryRunsOnTheSharedBudget(t *testing.T) {
	stub := &diBudgetStub{FakeProvider: &testutil.FakeProvider{}}
	ai.RegisterProvider("di-budget-stub", func(string) ai.Provider { return stub })
	backend := testutil.NewFakeBackend()
	backend.DailyUsage = 10
	settings := budgetSettings{budget: 5}
	cfg := config.Default()
	keys := testutil.NewFakeSecretStore()
	_ = keys.Save("u1", "di-budget-stub", "k")
	budget := ProvideAIBudget(settings, backend)
	policy := ProvideProviderPolicy(settings, backend)
	ctx := context.Background()

	_, _, factory, err := ProvideAI("", backend, cfg, keys, nil, budget, policy)
	if err != nil {
		t.Fatal(err)
	}
	p, err := factory.For(ctx, "u1", "di-budget-stub")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Chat(ctx, ai.Request{Model: "m"}); !errors.Is(err, service.ErrAIBudgetExceeded) {
		t.Errorf("Chat on a spent budget: %v, want ErrAIBudgetExceeded from the shared budget", err)
	}
	if stub.chats != 0 {
		t.Errorf("the provider was called %d times over budget", stub.chats)
	}
	backend.DailyUsage = 1
	if _, err := p.Chat(ctx, ai.Request{Model: "m"}); err != nil || stub.chats != 1 {
		t.Errorf("Chat with budget left: %v (calls %d)", err, stub.chats)
	}

	// The policy: an org whose list leaves the provider out refuses the build before the key is read.
	backend.Orgs = map[string]*storageif.Organisation{"org-1": {ID: "org-1"}}
	_ = backend.SaveOrgSettings(ctx, "org-1", &storageif.AppSettings{AI: storageif.AISettings{AllowedProviders: []string{"openai"}}})
	if _, err := factory.ForOrg(ctx, "u1", "org-1", "di-budget-stub"); !errors.Is(err, ai.ErrProviderNotAllowed) {
		t.Errorf("ForOrg off the org's list: %v, want ErrProviderNotAllowed", err)
	}
	if _, err := factory.ForOrg(ctx, "u1", "org-2", "di-budget-stub"); err != nil {
		t.Errorf("ForOrg for an org without a list: %v", err)
	}

	// Without the budget and the policy nothing is gated (and no typed-nil interface panics).
	_, _, bare, err := ProvideAI("", backend, cfg, keys, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend.DailyUsage = 10
	bp, err := bare.ForOrg(ctx, "u1", "org-1", "di-budget-stub")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bp.Chat(ctx, ai.Request{Model: "m"}); err != nil {
		t.Errorf("ungated factory: %v", err)
	}
}
