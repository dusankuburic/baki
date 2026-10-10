package service

import (
	"errors"
	"fmt"
	"testing"

	"pad-analyzer/internal/ai"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// Phase 4b review T6: the policy's refusal of the Azure-registry embedder (deployment disable or off the org's
// list) is the admin's to act on and keeps its sentinel through the registry's classification — not a masked
// "try again in a moment" 503 with a WARN per upload. An unreadable policy keeps its own sentinel the same way.
func TestKnowledgeEmbedderResult_KeepsThePolicySentinels(t *testing.T) {
	owner := storageif.AzureOrgOwner("org-1")
	for _, sentinel := range []error{ai.ErrProviderNotAllowed, ai.ErrProviderPolicyUnavailable} {
		refusal := fmt.Errorf("%w: azure-foundry is not on the list", sentinel)
		_, err := knowledgeEmbedderResult(owner, storageif.AzureModelRecord{DisplayName: "emb"}, nil, refusal)
		if !errors.Is(err, sentinel) {
			t.Errorf("knowledgeEmbedderResult(%v) = %v, want the sentinel kept", sentinel, err)
		}
	}
}
