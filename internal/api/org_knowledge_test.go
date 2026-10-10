package api

import (
	"context"
	"fmt"
	"net/http"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

// refusingPolicy is an ai.ProviderPolicy that refuses every provider.
type refusingPolicy struct{}

func (refusingPolicy) Allowed(_ context.Context, _, _, providerID string) error {
	return fmt.Errorf("%w: %s is not on your organisation's list of allowed AI providers", ai.ErrProviderNotAllowed, providerID)
}

// ADR 0028: an embedding provider the policy refuses at the factory (disabled in the deployment, or off the
// organisation's allowed-provider list) is rendered 400 PROVIDER_NOT_ALLOWED on the knowledge routes — before
// the generic EMBEDDING_NOT_CONFIGURED the knowledge base wraps it in — and no key is read for it.
func TestKnowledge_ProviderNotAllowedRendered(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	factory := ai.NewProviderFactory(func(string, string) (string, error) {
		t.Error("a credential was read for a refused provider")
		return "", nil
	}, nil, nil, nil, ai.WithProviderPolicy(refusingPolicy{}))
	ks := rag.NewKnowledgeService(testutil.NewFakeBackend(), factory, nil)

	rr := f.doOrg(t, "u-admin", http.MethodPost, "/api/orgs/"+f.orgID+"/knowledge/upload", map[string]string{"filename": "a.md", "content": "text"}, ks)
	checkStatus(t, rr, http.StatusBadRequest)
	if got := azureDecode(t, rr); got["code"] != service.ProviderCodeNotAllowed {
		t.Errorf("upload code = %v, want %s (body %s)", got["code"], service.ProviderCodeNotAllowed, rr.Body.String())
	}
	rr = f.doOrg(t, "u-admin", http.MethodPost, "/api/orgs/"+f.orgID+"/knowledge/reindex", nil, ks)
	checkStatus(t, rr, http.StatusBadRequest)
	if got := azureDecode(t, rr); got["code"] != service.ProviderCodeNotAllowed {
		t.Errorf("reindex code = %v (body %s)", got["code"], rr.Body.String())
	}
}
