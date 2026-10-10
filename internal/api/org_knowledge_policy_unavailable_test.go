package api

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// brokenOrgSettings is an org settings reader whose store is down; its error text names internals.
type brokenOrgSettings struct{}

func (brokenOrgSettings) LoadOrgSettings(context.Context, string) (*storageif.AppSettings, error) {
	return nil, errors.New("load org settings: failed to connect to host=db.internal user=baki_app")
}

// Phase 4b review T2: a provider-policy read that fails inside an upload is not "not configured" and not the
// client's to fix — 503 PROVIDER_POLICY_UNAVAILABLE with fixed copy; the store's text never reaches the body.
func TestKnowledge_ProviderPolicyUnavailableRendered(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "k", nil }, nil, nil, nil,
		ai.WithProviderPolicy(service.NewProviderPolicy(nil, brokenOrgSettings{})))
	ks := rag.NewKnowledgeService(testutil.NewFakeBackend(), factory, nil)

	rr := f.doOrg(t, "u-admin", http.MethodPost, "/api/orgs/"+f.orgID+"/knowledge/upload", map[string]string{"filename": "a.md", "content": "text"}, ks)
	checkStatus(t, rr, http.StatusServiceUnavailable)
	got := azureDecode(t, rr)
	if got["code"] != service.ProviderCodePolicyUnavailable {
		t.Errorf("upload code = %v, want %s (body %s)", got["code"], service.ProviderCodePolicyUnavailable, rr.Body.String())
	}
	if body := rr.Body.String(); strings.Contains(body, "host=") || strings.Contains(body, "user=") || strings.Contains(body, "configure") {
		t.Errorf("body leaks the store's text or tells the admin to configure a provider: %s", body)
	}
}
