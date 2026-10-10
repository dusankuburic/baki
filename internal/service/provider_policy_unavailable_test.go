package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// Phase 4b review T2: an org settings document the policy cannot read is its own class of failure
// (ai.ErrProviderPolicyUnavailable) with fixed copy — the store's text (hosts, users) stays in the log — and the
// connection tests code it PROVIDER_POLICY_UNAVAILABLE instead of leaking it in a 200 body.
func TestProviderPolicy_UnreadableOrgSettingsIsItsOwnSentinel(t *testing.T) {
	const internal = "failed to connect to host=db.internal user=baki_app"
	policy := NewProviderPolicy(nil, orgSettingsStub{err: errors.New(internal)})
	err := policy.Allowed(context.Background(), "u1", "org-1", "claude")
	if !errors.Is(err, ai.ErrProviderPolicyUnavailable) {
		t.Fatalf("Allowed = %v, want ErrProviderPolicyUnavailable", err)
	}
	if strings.Contains(err.Error(), "host=") || strings.Contains(err.Error(), "user=") {
		t.Errorf("the policy's error carries the store's text: %q", err.Error())
	}
	if errors.Is(err, ai.ErrProviderNotAllowed) {
		t.Error("an unreadable policy must not read as a refusal")
	}

	stub := &countingChatProvider{FakeProvider: &testutil.FakeProvider{}, id: "policy-unavailable-stub"}
	ai.RegisterProvider(stub.id, func(string) ai.Provider { return stub })
	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()
	_ = orgKeys.SaveOrg("org-1", stub.id, "org-k", "u-admin")
	svc, _ := budgetedProviderService(t, NewAIBudget(&staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 100}}}, spendByOrg{}), secrets, orgKeys)
	svc.factory = ai.NewProviderFactory(func(string, string) (string, error) {
		t.Error("key read while the policy is unreadable")
		return "", nil
	}, nil, nil, nil,
		ai.WithProviderPolicy(policy))
	svc.factory.SetOrgKeyLookup(func(_, _ string) (string, error) {
		t.Error("org key read while the policy is unreadable")
		return "", nil
	})

	res, err := svc.TestOrgProviderConnection(context.Background(), "u-admin", "org-1", stub.id)
	if err != nil {
		t.Fatal(err)
	}
	if res.Ok || res.Code != ProviderCodePolicyUnavailable {
		t.Errorf("org connection test = %+v, want ok:false code %s", res, ProviderCodePolicyUnavailable)
	}
	if strings.Contains(res.Error, "host=") {
		t.Errorf("the test result carries the store's text: %q", res.Error)
	}
	if stub.chats != 0 {
		t.Errorf("Chat was called %d times while the policy could not be read", stub.chats)
	}
}
