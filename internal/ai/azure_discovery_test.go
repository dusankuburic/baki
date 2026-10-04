package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
)

// Discovery: a saved resource's project deployments and agents as
// candidate registry entries — kinds, windows, "already added" — with
// partial failures as warnings, typed refusals, and nothing written to the
// process caches the chat path reads.

// discoveryTestProvider is the isolated Entra ID + project provider
// DiscoverAzureFoundryProject builds, on a local server with a fake token.
func discoveryTestProvider(t *testing.T, srv *azureTestServer) *AzureFoundryProvider {
	t.Helper()
	p := newAzureAgentProvider(t, srv)
	p.isolated = true
	return p
}

// discoveryProject serves a project whose deployments and agents listings
// answer with the given status (200: the fixture below).
func discoveryProject(t *testing.T, deploymentsStatus, agentsStatus int) *azureTestServer {
	t.Helper()
	return newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			if deploymentsStatus != 200 {
				writeJSON(w, deploymentsStatus, map[string]any{"error": map[string]any{"code": "Refused", "message": "Public network access is disabled for this project."}})
				return
			}
			writeJSON(w, 200, map[string]any{"value": []map[string]any{
				{"name": "gpt-4o", "modelName": "gpt-4o", "modelPublisher": "OpenAI", "capabilities": map[string]any{"chat_completion": "true"}},
				{"name": "sonnet", "modelName": "claude-sonnet-4-6", "modelPublisher": "Anthropic"},
				{"name": "haiku", "modelName": "claude-haiku-4-5"},
				{"name": "emb", "modelName": "text-embedding-3-small"},
				{"name": "vec", "modelName": "Cohere-v3-english", "capabilities": map[string]any{"embeddings": true}},
				{"name": "gw", "modelName": "gpt-4o", "connectionName": "gateway"},
				{"name": "model-router", "modelName": "model-router"},
				{"name": "mystery"},
				{"name": "Phi-4"},
				{"name": "prod-chat", "modelName": "gpt-4.1"},
			}})
		case "/api/projects/p1/agents":
			if agentsStatus != 200 {
				writeJSON(w, agentsStatus, map[string]any{"error": map[string]any{"code": "Refused", "message": "busy"}})
				return
			}
			writeJSON(w, 200, map[string]any{"data": []map[string]any{
				{"name": "Helper", "versions": map[string]any{"latest": map[string]any{"definition": map[string]any{"kind": "prompt", "model": "prod-chat"}}}},
				{"name": "Flow", "definition": map[string]any{"kind": "workflow", "model": "gpt-4o"}},
				{"name": "Hosted", "definition": map[string]any{"kind": "hosted"}},
				{"name": "Off", "state": "disabled", "definition": map[string]any{"kind": "prompt"}},
				{"name": "Mystery", "definition": map[string]any{"kind": "prompt", "model": "other-dep"}},
			}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
}

func TestDiscoverAzureFoundryProject_KindsWindowsAndAlreadyAdded(t *testing.T) {
	srv := discoveryProject(t, 200, 200)
	p := discoveryTestProvider(t, srv)
	registered := []AzureRegisteredModel{
		entry("aaaaaaaaa1", AzureModelKindChat, "GPT-4O"),
		entry("aaaaaaaaa2", AzureModelKindAgent, "helper"),
		entry("aaaaaaaaa3", AzureModelKindEmbedding, "vec"),
		entry("not-an-id", AzureModelKindClaude, "sonnet"),
	}
	got, err := p.discoverProject(context.Background(), registered)
	if err != nil {
		t.Fatalf("discoverProject: %v", err)
	}

	want := []AzureDiscoveredDeployment{
		{Name: "gpt-4o", Kind: AzureModelKindChat, Model: "gpt-4o", ContextWindow: 128000, AlreadyAdded: true, ModelID: "m/aaaaaaaaa1"},
		{Name: "sonnet", Kind: AzureModelKindClaude, Model: "claude-sonnet-4-6", ContextWindow: 1000000, AlreadyAdded: true},
		{Name: "haiku", Kind: AzureModelKindClaude, Model: "claude-haiku-4-5", ContextWindow: 200000},
		{Name: "emb", Kind: AzureModelKindEmbedding, Model: "text-embedding-3-small"},
		{Name: "vec", Kind: AzureModelKindEmbedding, Model: "Cohere-v3-english", AlreadyAdded: true, ModelID: "m/aaaaaaaaa3"},
		// gw comes from a project connection: not callable on /openai/v1.
		{Name: "model-router", Kind: AzureModelKindChat, Model: "model-router"},
		{Name: "mystery", Kind: AzureModelKindChat},
		{Name: "Phi-4", Kind: AzureModelKindChat, ContextWindow: 16384},
		{Name: "prod-chat", Kind: AzureModelKindChat, Model: "gpt-4.1", ContextWindow: 1047576},
	}
	if len(got.Deployments) != len(want) {
		t.Fatalf("deployments = %+v, want %d", got.Deployments, len(want))
	}
	for i := range want {
		if got.Deployments[i] != want[i] {
			t.Errorf("deployment %d = %+v, want %+v", i, got.Deployments[i], want[i])
		}
	}

	wantAgents := []AzureDiscoveredAgent{
		{Name: "Helper", Kind: AzureModelKindAgent, Deployment: "prod-chat", Model: "gpt-4.1", ContextWindow: 1047576, AlreadyAdded: true, ModelID: "m/aaaaaaaaa2"},
		{Name: "Flow", Kind: AzureModelKindAgent, Workflow: true, Deployment: "gpt-4o", Model: "gpt-4o", ContextWindow: 128000},
		{Name: "Mystery", Kind: AzureModelKindAgent, Deployment: "other-dep"},
	}
	if len(got.Agents) != len(wantAgents) {
		t.Fatalf("agents = %+v, want %d (hosted and disabled agents skipped)", got.Agents, len(wantAgents))
	}
	for i := range wantAgents {
		if got.Agents[i] != wantAgents[i] {
			t.Errorf("agent %d = %+v, want %+v", i, got.Agents[i], wantAgents[i])
		}
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "2 of the project's agents cannot chat in baki") {
		t.Errorf("warnings = %q, want the two skipped agents explained", got.Warnings)
	}

	// The view the API returns as is.
	raw, _ := json.Marshal(got)
	for _, frag := range []string{`"deployments":[`, `"name":"sonnet","kind":"claude","model":"claude-sonnet-4-6","contextWindow":1000000,"alreadyAdded":true}`,
		`"modelId":"m/aaaaaaaaa1"`, `"workflow":true`, `"warnings":[`} {
		if !strings.Contains(string(raw), frag) {
			t.Errorf("JSON %s lacks %s", raw, frag)
		}
	}
}

// Discovery needs the project data plane: an API key or a missing project
// endpoint is DISCOVERY_UNAVAILABLE (no request), an invalid document the
// config error.
func TestDiscoverAzureFoundryProject_Unavailable(t *testing.T) {
	_, err := DiscoverAzureFoundryProject(context.Background(),
		`{"endpoint":"https://my-res.services.ai.azure.com","projectEndpoint":"https://my-res.services.ai.azure.com/api/projects/p","auth":{"mode":"api_key","key":"k"}}`, nil)
	if !errors.Is(err, ErrAzureDiscoveryUnavailable) || !strings.Contains(err.Error(), "API key") {
		t.Errorf("API key: %v, want ErrAzureDiscoveryUnavailable naming the key", err)
	}
	_, err = DiscoverAzureFoundryProject(context.Background(),
		`{"endpoint":"https://my-res.services.ai.azure.com","auth":{"mode":"entra","tenantId":"`+entraTestTenant+`","clientId":"`+azureSeamClientID+`","clientSecret":"s"}}`, nil)
	if !errors.Is(err, ErrAzureDiscoveryUnavailable) || !strings.Contains(err.Error(), "project endpoint") {
		t.Errorf("no project endpoint: %v, want ErrAzureDiscoveryUnavailable naming it", err)
	}
	_, err = DiscoverAzureFoundryProject(context.Background(), `{"endpoint":"https://evil.example.com","auth":{"mode":"api_key","key":"k"}}`, nil)
	if !errors.Is(err, ErrProviderConfigInvalid) || azureCfgCode(t, err) != AzureCfgErrHostNotAllowed {
		t.Errorf("invalid document: %v, want ErrProviderConfigInvalid / HOST_NOT_ALLOWED", err)
	}
}

// Both listings refused: a typed refusal the API maps (401 and 403 are a
// permission denial with a fixed or Azure's explanation, 404 a missing
// project); a sign-in Entra ID refuses is its own error, before any request.
func TestDiscoverAzureFoundryProject_Refusals(t *testing.T) {
	for _, tc := range []struct {
		status int
		is     error
		msg    string
	}{
		{401, ErrProviderPermissionDenied, "not authorized on the Foundry project"},
		{403, ErrProviderPermissionDenied, "Public network access is disabled"},
		{404, ErrAzureProjectNotFound, "project was not found"},
	} {
		srv := discoveryProject(t, tc.status, tc.status)
		_, err := discoveryTestProvider(t, srv).discoverProject(context.Background(), nil)
		var refusal *AzureProjectRefusalError
		if !errors.As(err, &refusal) || refusal.Status != tc.status || !errors.Is(err, tc.is) || !strings.Contains(err.Error(), tc.msg) {
			t.Errorf("%d: err = %v, want an *AzureProjectRefusalError %d matching %v and saying %q", tc.status, err, tc.status, tc.is, tc.msg)
		}
	}

	srv := discoveryProject(t, 200, 200)
	p := discoveryTestProvider(t, srv)
	p.tokens = newEntraTokenSource(erroringCred{errors.New("AzureCLICredential: az not found")})
	if _, err := p.discoverProject(context.Background(), nil); !errors.Is(err, errEntraToken) || !errors.Is(err, ErrEntraTokenUnavailable) {
		t.Errorf("token failure: %v, want the sign-in's error", err)
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("%d project requests without a token, want none", n)
	}
}

// One listing failing while the other succeeds is a warning with the
// reason, not an error.
func TestDiscoverAzureFoundryProject_PartialFailureIsAWarning(t *testing.T) {
	got, err := discoveryTestProvider(t, discoveryProject(t, 200, 503)).discoverProject(context.Background(), nil)
	if err != nil || len(got.Deployments) == 0 || len(got.Agents) != 0 {
		t.Fatalf("agents failing = %+v, %v; want the deployments", got, err)
	}
	if len(got.Warnings) != 1 || !strings.Contains(got.Warnings[0], "agents could not be listed") {
		t.Errorf("warnings = %q", got.Warnings)
	}

	got, err = discoveryTestProvider(t, discoveryProject(t, 404, 200)).discoverProject(context.Background(), nil)
	if err != nil || len(got.Deployments) != 0 || len(got.Agents) != 3 {
		t.Fatalf("deployments failing = %+v, %v; want the agents", got, err)
	}
	// Without the deployments listing an agent's custom deployment names no
	// model; a catalogued deployment name still does.
	if got.Agents[0].Model != "" || got.Agents[1].Model != "gpt-4o" {
		t.Errorf("agents = %+v", got.Agents)
	}
	if len(got.Warnings) != 2 || !strings.Contains(got.Warnings[0], "deployments could not be listed: the Foundry project was not found") {
		t.Errorf("warnings = %q, want the deployments failure first, then the skipped agents", got.Warnings)
	}
}

// DiscoverAzureFoundryProject end to end (host validation, the saved
// credential's sign-in through the Azure client) writes none of the process
// caches the chat path reads — while the same listing through a chat
// provider does, which is what makes the assertion meaningful.
func TestDiscoverAzureFoundryProject_WritesNoProcessCache(t *testing.T) {
	seam := installAzureSeam(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			writeJSON(w, 200, map[string]any{"value": []map[string]any{{"name": "prod-chat", "modelName": "gpt-4.1"}}})
		case "/api/projects/p1/agents":
			writeJSON(w, 200, map[string]any{"data": []map[string]any{{"name": "Flow", "definition": map[string]any{"kind": "workflow", "model": "prod-chat"}}}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	raw := seamEntraDoc("disc")
	got, err := DiscoverAzureFoundryProject(context.Background(), raw, nil)
	if err != nil || len(got.Deployments) != 1 || len(got.Agents) != 1 || !got.Agents[0].Workflow {
		t.Fatalf("discovery = %+v, %v", got, err)
	}
	for _, r := range seam.requests("disc" + azureSeamSuffix) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer seam-token-1" {
			t.Errorf("project request %s %s with %q, want GETs with the signed-in token", r.Method, r.Path, r.Header.Get("Authorization"))
		}
	}
	assertNoProcessCacheWrites(t)

	// The same project listed for the chat path does fill them.
	cfg, _ := ParseAzureFoundryConfig(raw)
	if _, err := newAzureFoundryProvider(cfg, nil, azureHTTPClient()).fetchAgents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if agentKindFor(cfg.ProjectEndpoint, "Flow") != agentKindWorkflow {
		t.Error("control: a chat provider's agents listing remembers the workflow kind")
	}
}

// assertNoProcessCacheWrites fails when a listing, an agent kind, a served
// model or a learned window was recorded since the caches were last reset.
func assertNoProcessCacheWrites(t *testing.T) {
	t.Helper()
	azureModelsCache.mu.Lock()
	listings := len(azureModelsCache.m)
	azureModelsCache.mu.Unlock()
	agentKinds.Lock()
	kinds := len(agentKinds.m)
	agentKinds.Unlock()
	servedMu.Lock()
	served := len(servedMemo)
	servedMu.Unlock()
	learnedMu.Lock()
	learned := len(learnedWindows)
	learnedMu.Unlock()
	if listings+kinds+served+learned != 0 {
		t.Errorf("process caches written: %d listings, %d agent kinds, %d served models, %d learned windows", listings, kinds, served, learned)
	}
}
