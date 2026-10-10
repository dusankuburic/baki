package rag

import (
	"errors"
	"fmt"
	"testing"

	"pad-analyzer/internal/ai"
)

// Phase 4b review T6: azureEmbedderRefusal lets the policy's sentinels through as they are (the knowledge routes
// render them 400 PROVIDER_NOT_ALLOWED / 503 PROVIDER_POLICY_UNAVAILABLE) instead of folding them into the
// masked ErrEmbeddingSetupUnavailable.
func TestAzureEmbedderRefusal_KeepsThePolicySentinels(t *testing.T) {
	for _, sentinel := range []error{ai.ErrProviderNotAllowed, ai.ErrProviderPolicyUnavailable} {
		err := azureEmbedderRefusal(fmt.Errorf("%w: azure-foundry", sentinel))
		if !errors.Is(err, sentinel) || errors.Is(err, ErrEmbeddingSetupUnavailable) {
			t.Errorf("azureEmbedderRefusal(%v) = %v, want the sentinel kept", sentinel, err)
		}
	}
}
