package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// newAzureProviderHandlerWithBackend is newAzureProviderHandler with an
// audit backend (nil for none), so logAudit records the save.
func newAzureProviderHandlerWithBackend(t *testing.T, backend storageif.StorageBackend) (*ProviderHandler, *testutil.FakeSecretStore) {
	t.Helper()
	secrets := testutil.NewFakeSecretStore()
	factory := ai.NewProviderFactory(func(scope, provider string) (string, error) {
		v, _ := secrets.Get(scope, provider)
		return v, nil
	}, nil, nil, nil)
	svc := service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets)
	azure, _ := newTestAzureModels(testAzureModelsDeps{Secrets: secrets, Factory: factory})
	svc.SetAzureModels(azure)
	return NewProviderHandler(svc, &SecurityConfig{}, backend), secrets
}

// D-52: the stored document is parsed strictly, so a misspelled field must
// fail the save (400 INVALID_CONFIG naming the field) rather than vanish;
// D-2 (V-cfg): a project endpoint on a foreign host is reported under the
// project endpoint's own code and wording.
func TestAzureFoundryConfigRoutes_StrictDecodeAndProjectHost(t *testing.T) {
	h, secrets := newAzureProviderHandler(t)

	t.Run("unknown field", func(t *testing.T) {
		body := `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"c","embeddingDeploymnt":"text-embedding-3-small"}`
		req := httptest.NewRequest(http.MethodPost, "/api/providers/azure-foundry/config", bytes.NewBufferString(body))
		rr := httptest.NewRecorder()
		h.handleSaveAzureFoundryConfig(rr, req)
		checkStatus(t, rr, http.StatusBadRequest)
		got := azureDecode(t, rr)
		if got["code"] != ai.AzureCfgErrInvalidConfig {
			t.Errorf("code = %v, want %s (body %s)", got["code"], ai.AzureCfgErrInvalidConfig, rr.Body.String())
		}
		if msg, _ := got["message"].(string); !strings.Contains(msg, `unknown field "embeddingDeploymnt"`) {
			t.Errorf("error = %q, want the unknown field named", msg)
		}
		if has, _ := secrets.Has("", "azure-foundry"); has {
			t.Error("a rejected save must not store anything")
		}
	})

	t.Run("oversized body", func(t *testing.T) {
		body := `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"chatDeployment":"` + strings.Repeat("a", 70<<10) + `"}`
		req := httptest.NewRequest(http.MethodPost, "/api/providers/azure-foundry/config", bytes.NewBufferString(body))
		rr := httptest.NewRecorder()
		h.handleSaveAzureFoundryConfig(rr, req)
		checkStatus(t, rr, http.StatusBadRequest)
		if got := azureDecode(t, rr); got["code"] != ai.AzureCfgErrInvalidConfig {
			t.Errorf("code = %v (body %s)", got["code"], rr.Body.String())
		}
	})

	t.Run("project host not allowed", func(t *testing.T) {
		rr := azureConfigRequest(t, h, http.MethodPost, map[string]any{
			"endpoint": "https://my-res.openai.azure.com",
			"auth": map[string]any{"mode": "entra", "tenantId": "11111111-1111-1111-1111-111111111111",
				"clientId": "22222222-2222-2222-2222-222222222222", "clientSecret": "s"},
			"projectEndpoint": "https://apim.contoso.com/api/projects/p"})
		checkStatus(t, rr, http.StatusBadRequest)
		got := azureDecode(t, rr)
		if got["code"] != ai.AzureCfgErrInvalidProject {
			t.Errorf("code = %v, want %s (body %s)", got["code"], ai.AzureCfgErrInvalidProject, rr.Body.String())
		}
		if msg, _ := got["message"].(string); !strings.Contains(msg, "project endpoint host") {
			t.Errorf("error = %q, want the project endpoint host named", msg)
		}
	})
}

// audit#11: the audit row carries the normalised auth mode, not the
// client's case/whitespace variant.
func TestSaveAzureFoundryConfig_AuditsNormalisedAuthMode(t *testing.T) {
	prevCh := auditCh
	auditCh = make(chan *storageif.AuditEvent, 1)
	auditClosed.Store(false)
	t.Cleanup(func() {
		auditCh = prevCh
		auditClosed.Store(false)
	})
	h, _ := newAzureProviderHandlerWithBackend(t, testutil.NewFakeBackend())
	rr := azureConfigRequest(t, h, http.MethodPost, map[string]any{
		"endpoint": "https://my-res.openai.azure.com",
		"auth": map[string]any{"mode": " ENTRA ", "tenantId": "11111111-1111-1111-1111-111111111111",
			"clientId": "22222222-2222-2222-2222-222222222222", "clientSecret": "s"},
		"chatDeployment": "c"})
	checkStatus(t, rr, http.StatusOK)
	select {
	case ev := <-auditCh:
		if ev.Action != AuditActionKeysSave || ev.ResourceID != ai.AzureFoundryProviderID || ev.Meta["authMode"] != "entra" || ev.Meta["op"] != "save" {
			t.Errorf("audit event = %+v (meta %v), want authMode entra", ev, ev.Meta)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no audit event was enqueued")
	}
}
