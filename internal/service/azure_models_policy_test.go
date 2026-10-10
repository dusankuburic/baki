package service

import (
	"context"
	"fmt"
	"testing"

	"pad-analyzer/internal/ai"
)

// azureRefusingPolicy refuses azure-foundry and admits everything else.
type azureRefusingPolicy struct{}

func (azureRefusingPolicy) Allowed(_ context.Context, _, _, providerID string) error {
	if providerID == ai.AzureFoundryProviderID {
		return fmt.Errorf("%w: %s is disabled in this deployment's AI settings", ai.ErrProviderNotAllowed, providerID)
	}
	return nil
}

// Phase 4b review T3: the Azure AI Foundry probes (resource/model test, discovery) are built around the factory,
// so they must ask the provider policy themselves — a provider the deployment disabled or the organisation
// left off its list is not exercised with a real call; the test answers {ok:false, code: PROVIDER_NOT_ALLOWED}
// and discovery the same, with no probe made (the env fails the test on any network call).
func TestAzureModels_ProbesAskTheProviderPolicy(t *testing.T) {
	e := newAzureModelsEnv(t, func(d *AzureModelServiceDeps) {
		d.Factory = ai.NewProviderFactory(func(string, string) (string, error) { return "", nil }, nil, nil, nil, ai.WithProviderPolicy(azureRefusingPolicy{}))
	})
	ctx := context.Background()
	c := e.mustConn(t, azmUser, "c", azmKeyConfig("k"))

	res, err := e.svc.Test(ctx, azmUser, AzureTestInput{ConnectionID: c.ID})
	if err != nil {
		t.Fatalf("Test: %v", err)
	}
	if res.OK || res.Code != ProviderCodeNotAllowed || res.Error == "" {
		t.Errorf("resource test on a refused provider = %+v, want ok:false code %s with the policy's copy", res, ProviderCodeNotAllowed)
	}
	res, err = e.svc.Test(ctx, azmUser, AzureTestInput{ConnectionID: c.ID, Model: &AzureTestModelRef{Kind: "chat", Deployment: "draft"}})
	if err != nil || res.OK || res.Code != ProviderCodeNotAllowed {
		t.Errorf("draft model test on a refused provider = %+v, %v, want ok:false code %s", res, err, ProviderCodeNotAllowed)
	}

	view, err := e.svc.Discover(ctx, azmUser, c.ID)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if view.Code != ProviderCodeNotAllowed || view.Error == "" {
		t.Errorf("discovery on a refused provider = %+v, want code %s with the policy's copy", view, ProviderCodeNotAllowed)
	}
}
