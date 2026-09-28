package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

// newAzureProviderHandler builds a ProviderHandler on an in-memory secret
// store (never the OS keyring) in local mode (empty key scope).
func newAzureProviderHandler(t *testing.T) (*ProviderHandler, *testutil.FakeSecretStore) {
	t.Helper()
	secrets := testutil.NewFakeSecretStore()
	factory := ai.NewProviderFactory(func(scope, provider string) (string, error) {
		v, _ := secrets.Get(scope, provider)
		return v, nil
	}, nil, nil, nil)
	svc := service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets)
	return NewProviderHandler(svc, &SecurityConfig{}, nil), secrets
}

func azureConfigRequest(t *testing.T, h *ProviderHandler, method string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, "/api/providers/azure-foundry/config", &buf)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	if method == http.MethodGet {
		h.handleGetAzureFoundryConfig(rr, req)
	} else {
		h.handleSaveAzureFoundryConfig(rr, req)
	}
	return rr
}

func azureDecode(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
	return out
}

func TestAzureFoundryConfigRoutes(t *testing.T) {
	h, secrets := newAzureProviderHandler(t)

	rr := azureConfigRequest(t, h, http.MethodGet, nil)
	checkStatus(t, rr, http.StatusOK)
	if got := azureDecode(t, rr); got["configured"] != false {
		t.Errorf("unconfigured GET = %v", got)
	}

	for _, tc := range []struct {
		name string
		body map[string]any
		code string
	}{
		{"http scheme", map[string]any{"endpoint": "http://my-res.openai.azure.com", "auth": map[string]any{"mode": "api_key", "key": "k"}}, ai.AzureCfgErrInvalidEndpoint},
		{"metadata ip", map[string]any{"endpoint": "https://169.254.169.254", "auth": map[string]any{"mode": "api_key", "key": "k"}}, ai.AzureCfgErrHostNotAllowed},
		{"foreign host", map[string]any{"endpoint": "https://evil.example", "auth": map[string]any{"mode": "api_key", "key": "k"}}, ai.AzureCfgErrHostNotAllowed},
		{"missing key", map[string]any{"endpoint": "https://my-res.openai.azure.com", "auth": map[string]any{"mode": "api_key"}}, ai.AzureCfgErrMissingSecret},
		{"bad deployment", map[string]any{"endpoint": "https://my-res.openai.azure.com", "auth": map[string]any{"mode": "api_key", "key": "k"}, "chatDeployment": "a b"}, ai.AzureCfgErrInvalidDeploy},
		{"bad additional deployment", map[string]any{"endpoint": "https://my-res.openai.azure.com", "auth": map[string]any{"mode": "api_key", "key": "k"}, "deployments": []string{"ok", "a b"}}, ai.AzureCfgErrInvalidDeploy},
		// An API key cannot list deployments: a chat deployment must be named.
		{"no chat deployment", map[string]any{"endpoint": "https://my-res.openai.azure.com", "auth": map[string]any{"mode": "api_key", "key": "k"}, "embeddingDeployment": "emb"}, ai.AzureCfgErrMissingDeploy},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := azureConfigRequest(t, h, http.MethodPost, tc.body)
			checkStatus(t, rr, http.StatusBadRequest)
			if got := azureDecode(t, rr); got["code"] != tc.code {
				t.Errorf("code = %v, want %s (body %s)", got["code"], tc.code, rr.Body.String())
			}
		})
	}
	if has, _ := secrets.Has("", "azure-foundry"); has {
		t.Fatal("a rejected save must not store anything")
	}

	rr = azureConfigRequest(t, h, http.MethodPost, map[string]any{
		"endpoint": "https://my-res.openai.azure.com", "auth": map[string]any{"mode": "api_key", "key": "secret-1"},
		"projectEndpoint": "https://my-res.services.ai.azure.com/api/projects/proj", "chatDeployment": "chat1",
		"deployments": []string{" gpt-4o ", "GPT-4O", "chat1", "phi-4"}})
	checkStatus(t, rr, http.StatusOK)

	rr = azureConfigRequest(t, h, http.MethodGet, nil)
	got := azureDecode(t, rr)
	if got["configured"] != true || got["hasSecret"] != true || got["authMode"] != "api_key" || got["chatDeployment"] != "chat1" {
		t.Errorf("configured GET = %v", got)
	}
	// The additional deployments come back normalised (trimmed, deduplicated,
	// without the chat deployment).
	if deps, _ := got["deployments"].([]any); len(deps) != 2 || deps[0] != "gpt-4o" || deps[1] != "phi-4" {
		t.Errorf("deployments = %v", got["deployments"])
	}
	if _, leaked := got["key"]; leaked {
		t.Error("GET must never return the secret")
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("secret-1")) {
		t.Error("secret leaked in the GET body")
	}

	// Editing without the key keeps it.
	rr = azureConfigRequest(t, h, http.MethodPost, map[string]any{
		"endpoint": "https://my-res.openai.azure.com", "auth": map[string]any{"mode": "api_key"}, "chatDeployment": "chat2"})
	checkStatus(t, rr, http.StatusOK)
	raw, _ := secrets.Get("", "azure-foundry")
	stored, _ := ai.ParseAzureFoundryConfig(raw)
	if stored.Auth.Key != "secret-1" || stored.ChatDeployment != "chat2" {
		t.Errorf("stored after edit = %+v", stored)
	}

	// Malformed JSON → 400.
	req := httptest.NewRequest(http.MethodPost, "/api/providers/azure-foundry/config", bytes.NewBufferString("{"))
	bad := httptest.NewRecorder()
	h.handleSaveAzureFoundryConfig(bad, req)
	checkStatus(t, bad, http.StatusBadRequest)
}

func TestSaveApiKey_RejectsAzureFoundry(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/keys/save", map[string]any{"provider": "azure-foundry", "key": "raw"})
	checkStatus(t, rr, http.StatusBadRequest)
	if got := azureDecode(t, rr); got["code"] != "USE_PROVIDER_CONFIG" {
		t.Errorf("code = %v", got["code"])
	}
}
