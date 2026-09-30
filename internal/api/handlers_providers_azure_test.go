package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sort"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
)

// newAzureProviderHandler builds a ProviderHandler on an in-memory secret
// store (never the OS keyring) in local mode (empty key scope), with the Azure
// AI Foundry model registry wired as DI wires it — the desktop's single
// personal registry — so the single-document routes act on its resource
// "default" (h.providerSvc.AzureModels() reads it).
func newAzureProviderHandler(t *testing.T) (*ProviderHandler, *testutil.FakeSecretStore) {
	t.Helper()
	return newAzureProviderHandlerWithBackend(t, nil)
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
	// The deployment fields are the models of resource "default" (the GET
	// above is built from them), named after the endpoint host.
	if got := personalModelDeployments(t, h); !equalStrings(got, []string{"chat:chat1", "chat:gpt-4o", "chat:phi-4"}) {
		t.Errorf("registry models = %v, want chat1, gpt-4o and phi-4", got)
	}
	if conns := personalConnections(t, h); len(conns) != 1 || conns[0].ID != "default" || conns[0].Name != "my-res" || !conns[0].Legacy {
		t.Errorf("registry resources = %+v, want resource default named my-res", conns)
	}

	// Editing without the key keeps it; the renamed chat deployment replaces
	// the models — added and removed in the registry — and the document is
	// stored as saved (a rolled-back binary reads what the route last saved).
	rr = azureConfigRequest(t, h, http.MethodPost, map[string]any{
		"endpoint": "https://my-res.openai.azure.com", "auth": map[string]any{"mode": "api_key"}, "chatDeployment": "chat2"})
	checkStatus(t, rr, http.StatusOK)
	raw, _ := secrets.Get("", "azure-foundry")
	stored, _ := ai.ParseAzureFoundryConfig(raw)
	if stored.Auth.Key != "secret-1" || stored.ChatDeployment != "chat2" || len(stored.Deployments) != 0 {
		t.Errorf("stored after edit = %+v, want the kept key and chat2 alone", stored)
	}
	got = azureDecode(t, azureConfigRequest(t, h, http.MethodGet, nil))
	if got["chatDeployment"] != "chat2" || got["deployments"] != nil {
		t.Errorf("GET after edit = %v, want chat2 alone", got)
	}
	if got := personalModelDeployments(t, h); !equalStrings(got, []string{"chat:chat2"}) {
		t.Errorf("registry models after edit = %v, want chat2 alone", got)
	}

	// Malformed JSON → 400.
	req := httptest.NewRequest(http.MethodPost, "/api/providers/azure-foundry/config", bytes.NewBufferString("{"))
	bad := httptest.NewRecorder()
	h.handleSaveAzureFoundryConfig(bad, req)
	checkStatus(t, bad, http.StatusBadRequest)
}

// Rollback safety: resource "default"'s document always holds deployment
// fields a binary rolled back to the single-document release can read. The
// deprecated save stores its document as saved — its names also become the
// registry's models — and a registry credential rotation carries the
// fields over unchanged.
func TestAzureFoundryConfigRoutes_LegacyDocumentFields(t *testing.T) {
	h, secrets := newAzureProviderHandler(t)
	const legacy = `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"legacy-key"},` +
		`"chatDeployment":"legacy-chat","deployments":["legacy-extra"],"claudeDeployments":["legacy-claude"],"embeddingDeployment":"legacy-embed"}`
	if err := secrets.Save("", ai.AzureFoundryProviderID, legacy); err != nil {
		t.Fatal(err)
	}
	legacyFields := func(t *testing.T) ai.AzureFoundryConfig {
		t.Helper()
		raw, err := secrets.Get("", ai.AzureFoundryProviderID)
		if err != nil {
			t.Fatal(err)
		}
		cfg, err := ai.ParseAzureFoundryConfig(raw)
		if err != nil {
			t.Fatalf("stored document no longer parses: %v", err)
		}
		return cfg
	}
	assertNewChatOnly := func(t *testing.T, cfg ai.AzureFoundryConfig) {
		t.Helper()
		if cfg.ChatDeployment != "new-chat" || len(cfg.Deployments) != 0 || len(cfg.ClaudeDeployments) != 0 || cfg.EmbeddingDeployment != "" {
			t.Errorf("stored legacy fields = chat %q, deployments %v, claude %v, embedding %q; want the deprecated save's new-chat alone",
				cfg.ChatDeployment, cfg.Deployments, cfg.ClaudeDeployments, cfg.EmbeddingDeployment)
		}
	}

	// The first read imports the document: resource "default" and a model
	// per deployment field.
	got := azureDecode(t, azureConfigRequest(t, h, http.MethodGet, nil))
	if got["chatDeployment"] != "legacy-chat" || got["embeddingDeployment"] != "legacy-embed" {
		t.Errorf("GET of the imported document = %v", got)
	}
	if models := personalModelDeployments(t, h); !equalStrings(models, []string{"chat:legacy-chat", "chat:legacy-extra", "claude:legacy-claude", "embedding:legacy-embed"}) {
		t.Errorf("imported models = %v", models)
	}

	// The deprecated save: the document is stored as saved, and its names
	// become the models.
	checkStatus(t, azureConfigRequest(t, h, http.MethodPost, map[string]any{
		"endpoint": "https://my-res.openai.azure.com", "auth": map[string]any{"mode": "api_key"}, "chatDeployment": "new-chat"}), http.StatusOK)
	cfg := legacyFields(t)
	assertNewChatOnly(t, cfg)
	if cfg.Auth.Key != "legacy-key" {
		t.Errorf("the carried key = %q, want legacy-key", cfg.Auth.Key)
	}
	if models := personalModelDeployments(t, h); !equalStrings(models, []string{"chat:new-chat"}) {
		t.Errorf("models after the deprecated save = %v, want new-chat alone", models)
	}
	got = azureDecode(t, azureConfigRequest(t, h, http.MethodGet, nil))
	if got["chatDeployment"] != "new-chat" || got["deployments"] != nil || got["claudeDeployments"] != nil {
		t.Errorf("GET after the deprecated save = %v, want new-chat alone (from the registry, not the document)", got)
	}

	// A credential rotation through the registry (PUT /connections/default)
	// carries the old fields over too.
	azure := h.providerSvc.AzureModels()
	if _, err := azure.UpdateConnection(bgCtx(), service.AzureScope{}, "default", service.AzureConnectionPatch{
		Config: &service.AzureConnectionConfigInput{Endpoint: "https://my-res.openai.azure.com", Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "rotated-key"}},
	}); err != nil {
		t.Fatalf("rotate the default resource's key: %v", err)
	}
	cfg = legacyFields(t)
	assertNewChatOnly(t, cfg)
	if cfg.Auth.Key != "rotated-key" {
		t.Errorf("the rotated key = %q, want rotated-key", cfg.Auth.Key)
	}
}

// personalConnections lists the caller's own registry resources (local
// mode: the desktop's personal registry).
func personalConnections(t *testing.T, h *ProviderHandler) []service.AzureConnectionView {
	t.Helper()
	out, err := h.providerSvc.AzureModels().ListConnections(bgCtx(), service.AzureScope{})
	if err != nil {
		t.Fatalf("list the registry's resources: %v", err)
	}
	return out.Connections
}

// personalModelDeployments lists the caller's own registry models as sorted
// "kind:deployment" strings.
func personalModelDeployments(t *testing.T, h *ProviderHandler) []string {
	t.Helper()
	out, err := h.providerSvc.AzureModels().ListModels(bgCtx(), service.AzureScope{})
	if err != nil {
		t.Fatalf("list the registry's models: %v", err)
	}
	var got []string
	for _, m := range out.Models {
		got = append(got, m.Kind+":"+m.Deployment)
	}
	sort.Strings(got)
	return got
}

// equalStrings compares two string slices, nil and empty alike.
func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestSaveApiKey_RejectsAzureFoundry(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/keys/save", map[string]any{"provider": "azure-foundry", "key": "raw"})
	checkStatus(t, rr, http.StatusBadRequest)
	if got := azureDecode(t, rr); got["code"] != "USE_PROVIDER_CONFIG" {
		t.Errorf("code = %v", got["code"])
	}
}
