package ai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/egress"
)

// Part 9 resource and model tests: an authenticated probe with no model call,
// one tiny request per chat/Claude/embedding model, a read-only lookup per
// agent — isolated, with a throwaway Entra ID token source, recording no
// usage and teaching no memo.

// probeTestProvider is the isolated registry provider the tests run on,
// built for cfg on the server's client (validation bypassed: httptest hosts
// are loopback), with a fake Entra ID token in Entra ID mode.
func probeTestProvider(t *testing.T, srv *azureTestServer, cfg AzureFoundryConfig, models []AzureRegisteredModel, embedding string) *AzureFoundryProvider {
	t.Helper()
	ResetAzureFoundryCaches()
	t.Cleanup(ResetAzureFoundryCaches)
	p := newAzureRegistryProvider(cfg, nil, models, embedding, srv.Client(), throwawayEntraSources)
	if p.configErr != nil {
		t.Fatalf("probe provider: %v", p.configErr)
	}
	p.isolated = true
	if p.tokens != nil {
		p.tokens = newEntraTokenSource(&fakeCred{})
	}
	return p
}

// methodLog records the method of every request a handler sees.
type methodLog struct {
	mu sync.Mutex
	m  []string
}

func (l *methodLog) add(r *http.Request) {
	l.mu.Lock()
	l.m = append(l.m, r.Method+" "+r.URL.EscapedPath()+"?"+r.URL.RawQuery)
	l.mu.Unlock()
}

func (l *methodLog) all() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]string(nil), l.m...)
}

// The resource probe with an API key: one GET of the model route with the
// key, no model call, and each answer mapped to what the user must fix.
func TestProbeAzureFoundryResource_APIKeyAnswers(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   any
		check  func(error) bool
		text   string
	}{
		{"ok", 200, map[string]any{"data": []map[string]any{{"id": "dall-e-3-3.0"}}}, func(err error) bool { return err == nil }, ""},
		{"bad key", 401, map[string]any{"error": map[string]any{"code": "401", "message": "Access denied due to invalid subscription key."}},
			func(err error) bool {
				return errors.Is(err, ErrApiKeyInvalid) && !errors.Is(err, ErrProviderPermissionDenied)
			}, ""},
		{"permission denied", 401, map[string]any{"error": map[string]any{"code": "PermissionDenied", "message": "The principal lacks the data action."}},
			func(err error) bool { return errors.Is(err, ErrProviderPermissionDenied) }, "lacks the data action"},
		{"forbidden", 403, map[string]any{"error": map[string]any{"code": "AuthenticationTypeDisabled", "message": "Key based authentication is disabled for this resource."}},
			func(err error) bool { return errors.Is(err, ErrProviderPermissionDenied) }, "Key based authentication is disabled"},
		{"forbidden, no body", 403, nil, func(err error) bool { return errors.Is(err, ErrProviderPermissionDenied) }, "networking rules"},
		{"wrong endpoint", 404, map[string]any{"error": map[string]any{"code": "404", "message": "Resource not found"}},
			func(err error) bool { return errors.Is(err, ErrAzureEndpointNotFound) }, "use the resource endpoint"},
		{"throttled", 429, map[string]any{"error": map[string]any{"code": "429", "message": "slow down"}}, func(err error) bool { return errors.Is(err, ErrRateLimited) }, ""},
		{"outage", 503, map[string]any{"error": map[string]any{"message": "busy"}}, func(err error) bool { return errors.Is(err, ErrProviderDown) }, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var methods methodLog
			srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
				methods.add(r)
				if tc.body == nil {
					w.WriteHeader(tc.status)
					return
				}
				writeJSON(w, tc.status, tc.body)
			})
			p := probeTestProvider(t, srv, azureKeyConfig(srv.URL), nil, "")
			err := p.probeResource(context.Background())
			if !tc.check(err) || (tc.text != "" && !strings.Contains(err.Error(), tc.text)) {
				t.Errorf("probe = %v, want the %s mapping (%q)", err, tc.name, tc.text)
			}
			reqs := srv.requests()
			if got := methods.all(); len(got) != 1 || got[0] != "GET /openai/v1/models?" {
				t.Fatalf("requests = %v, want one GET of the model route", got)
			}
			if reqs[0].Headers.Get("api-key") != "secret-key" || reqs[0].Headers.Get("Authorization") != "" || reqs[0].Body != nil {
				t.Errorf("probe request headers %v body %v, want the api-key header only", reqs[0].Headers, reqs[0].Body)
			}
		})
	}
}

// With Entra ID: a token first, the model route with it, then the project's
// first deployments page — its refusals typed.
func TestProbeAzureFoundryResource_EntraProbesTheProject(t *testing.T) {
	for _, tc := range []struct {
		status int
		check  func(error) bool
		text   string
	}{
		{200, func(err error) bool { return err == nil }, ""},
		{401, func(err error) bool { return errors.Is(err, ErrProviderPermissionDenied) }, "not authorized on the Foundry project"},
		{403, func(err error) bool { return errors.Is(err, ErrProviderPermissionDenied) }, "Public network access is disabled"},
		{404, func(err error) bool { return errors.Is(err, ErrAzureProjectNotFound) }, "project was not found"},
	} {
		var methods methodLog
		srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
			methods.add(r)
			switch r.URL.Path {
			case "/openai/v1/models":
				writeJSON(w, 200, map[string]any{"data": []map[string]any{}})
			case "/api/projects/p1/deployments":
				if tc.status != 200 {
					writeJSON(w, tc.status, map[string]any{"error": map[string]any{"code": "Denied", "message": "Public network access is disabled."}})
					return
				}
				writeJSON(w, 200, map[string]any{"value": []map[string]any{}})
			default:
				t.Errorf("unexpected path %s", r.URL.Path)
			}
		})
		p := probeTestProvider(t, srv, registryEntraConfig(srv), nil, "")
		err := p.probeResource(context.Background())
		var refusal *AzureProjectRefusalError
		if !tc.check(err) || (tc.text != "" && (!strings.Contains(err.Error(), tc.text) || !errors.As(err, &refusal) || refusal.Status != tc.status)) {
			t.Errorf("project %d: probe = %v, want %q", tc.status, err, tc.text)
		}
		got := methods.all()
		if len(got) < 2 || got[0] != "GET /openai/v1/models?" || got[1] != "GET /api/projects/p1/deployments?api-version=v1" {
			t.Errorf("project %d: requests = %v, want the model route then the deployments page", tc.status, got)
		}
		for i, r := range srv.requests() {
			if !strings.HasPrefix(r.Headers.Get("Authorization"), "Bearer tok-") || r.Headers.Get("api-key") != "" {
				t.Errorf("project %d: request %d auth = %v", tc.status, i, r.Headers)
			}
		}
	}
}

// A rejected Entra ID token gets one fresh one (withTokenRetry); a refusal
// that survives it is the token's; a sign-in Entra ID refuses sends nothing.
func TestProbeAzureFoundryResource_EntraTokens(t *testing.T) {
	refuseFirst := true
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		if r.Header.Get("Authorization") == "Bearer tok-1" || !refuseFirst {
			writeJSON(w, 401, map[string]any{"error": map[string]any{"code": "401", "message": "Access token has expired or is not yet valid."}})
			return
		}
		writeJSON(w, 200, map[string]any{"data": []map[string]any{}})
	})
	cfg := azureKeyConfig(srv.URL)
	cfg.Auth = AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: entraTestTenant, ClientID: azureSeamClientID, ClientSecret: "s"}
	p := probeTestProvider(t, srv, cfg, nil, "")
	if err := p.probeResource(context.Background()); err != nil {
		t.Errorf("a revoked cached token must be replaced once: %v", err)
	}
	if n := len(srv.requests()); n != 2 {
		t.Errorf("%d requests, want the refused one and its retry", n)
	}

	refuseFirst = false
	p = probeTestProvider(t, srv, cfg, nil, "")
	if err := p.probeResource(context.Background()); !errors.Is(err, ErrEntraTokenRejected) || !strings.Contains(err.Error(), "has expired") {
		t.Errorf("a refusal that survives a fresh token = %v, want ErrEntraTokenRejected with Azure's reason", err)
	}

	before := len(srv.requests())
	p = probeTestProvider(t, srv, cfg, nil, "")
	p.tokens = newEntraTokenSource(erroringCred{errors.New("AzureCLICredential: please run 'az login'")})
	if err := p.probeResource(context.Background()); !errors.Is(err, errEntraToken) || !errors.Is(err, ErrEntraTokenUnavailable) {
		t.Errorf("a failed sign-in = %v, want the sign-in's error", err)
	}
	if n := len(srv.requests()) - before; n != 0 {
		t.Errorf("%d requests without a token, want none", n)
	}
}

// A chat or Claude model is tested with one "ping" capped at 16 tokens; a
// reply cut at the cap passes; nothing is memoised from the answer — not the
// served model, not a context-length rejection's window.
func TestProbeAzureFoundryModel_OneTinyChat(t *testing.T) {
	var methods methodLog
	contextRejection := false
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		methods.add(r)
		switch r.URL.Path {
		case "/openai/v1/chat/completions":
			if contextRejection {
				writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "context_length_exceeded",
					"message": "This model's maximum context length is 4096 tokens. However, your messages resulted in 5000 tokens."}})
				return
			}
			writeJSON(w, 200, chatCompletionJSON("pong", "gpt-4o-2024-11-20"))
		case "/anthropic/v1/messages":
			// A thinking model that spent the whole cap before any text.
			writeJSON(w, 200, map[string]any{"type": "message", "model": "claude-sonnet-4-6", "stop_reason": "max_tokens",
				"content": []map[string]any{}, "usage": map[string]any{"input_tokens": 8, "output_tokens": 16}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	})
	chat := AzureRegisteredModel{EntryID: "aaaaaaaaa1", Kind: AzureModelKindChat, Deployment: "gpt-4o"}
	p := probeTestProvider(t, srv, azureKeyConfig(srv.URL), []AzureRegisteredModel{chat}, "")
	if err := p.probeModel(context.Background(), chat); err != nil {
		t.Fatalf("chat test: %v", err)
	}
	body := srv.requests()[0].Body
	msgs, _ := body["messages"].([]any)
	first, _ := msgs[0].(map[string]any)
	if body["model"] != "gpt-4o" || body["max_completion_tokens"] != float64(16) || len(msgs) != 1 || first["content"] != "ping" {
		t.Errorf("chat test body = %v, want one ping to the deployment capped at 16 tokens", body)
	}
	if _, set := body["temperature"]; set {
		t.Error("no temperature: reasoning deployments refuse it")
	}
	assertNoProcessCacheWrites(t)

	contextRejection = true
	if err := p.probeModel(context.Background(), chat); !errors.Is(err, ErrContextLimit) {
		t.Errorf("a context-length rejection = %v, want ErrContextLimit", err)
	}
	assertNoProcessCacheWrites(t)

	claude := AzureRegisteredModel{EntryID: "aaaaaaaaa2", Kind: AzureModelKindClaude, Deployment: "sonnet"}
	p = probeTestProvider(t, srv, azureKeyConfig(srv.URL), []AzureRegisteredModel{claude}, "")
	if err := p.probeModel(context.Background(), claude); err != nil {
		t.Errorf("a Claude reply cut at the cap = %v, want a pass", err)
	}
	last := srv.requests()[len(srv.requests())-1]
	if last.Path != "/anthropic/v1/messages" || last.Body["model"] != "sonnet" || last.Body["max_tokens"] != float64(16) {
		t.Errorf("Claude test = %s %v, want the Anthropic route, the deployment, 16 tokens", last.Path, last.Body)
	}
	for _, m := range methods.all() {
		if !strings.HasPrefix(m, "POST ") {
			t.Errorf("model test request %s, want the model call only", m)
		}
	}
	assertNoProcessCacheWrites(t)
}

// An embedding model embeds "ping" on its deployment.
func TestProbeAzureFoundryModel_Embedding(t *testing.T) {
	srv := azureEmbedServer(t, nil)
	emb := AzureRegisteredModel{EntryID: "aaaaaaaaa1", Kind: AzureModelKindEmbedding, Deployment: "kb-embed"}
	p := probeTestProvider(t, srv, azureKeyConfig(srv.URL), []AzureRegisteredModel{emb}, emb.Deployment)
	if err := p.probeModel(context.Background(), emb); err != nil {
		t.Fatalf("embedding test: %v", err)
	}
	reqs := srv.requests()
	input, _ := reqs[0].Body["input"].([]any)
	if len(reqs) != 1 || reqs[0].Body["model"] != "kb-embed" || len(input) != 1 || input[0] != "ping" {
		t.Errorf("embedding test = %d requests, body %v", len(reqs), reqs[0].Body)
	}
}

// An agent is looked up with one GET — never run — and must exist and be
// runnable in baki; a resource without a project and Entra ID cannot have
// one.
func TestProbeAzureFoundryModel_AgentIsOnlyLookedUp(t *testing.T) {
	var methods methodLog
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		methods.add(r)
		if r.Method != http.MethodGet || r.URL.Query().Get("api-version") != "v1" {
			t.Errorf("agent test sent %s %s", r.Method, r.URL.String())
		}
		name, ok := strings.CutPrefix(r.URL.Path, "/api/projects/p1/agents/")
		if !ok {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		switch name {
		case "Helper", "my agent":
			writeJSON(w, 200, map[string]any{"object": "agent", "id": "asst_1", "name": name, "state": "enabled",
				"versions": map[string]any{"latest": map[string]any{"status": "active", "definition": map[string]any{"kind": "prompt", "model": "gpt-4o"}}}})
		case "Flow":
			writeJSON(w, 200, map[string]any{"name": name, "definition": map[string]any{"kind": "workflow"}})
		case "Hosted":
			writeJSON(w, 200, map[string]any{"name": name, "versions": map[string]any{"latest": map[string]any{"definition": map[string]any{"kind": "hosted"}}}})
		case "Off":
			writeJSON(w, 200, map[string]any{"name": name, "state": "disabled", "definition": map[string]any{"kind": "prompt"}})
		case "Building":
			writeJSON(w, 200, map[string]any{"name": name, "versions": map[string]any{"latest": map[string]any{"status": "creating", "definition": map[string]any{"kind": "prompt"}}}})
		default:
			writeJSON(w, 404, map[string]any{"error": map[string]any{"code": "NotFound", "message": "Agent not found"}})
		}
	})
	for _, tc := range []struct {
		name  string
		check func(error) bool
	}{
		{"Helper", func(err error) bool { return err == nil }},
		{"my agent", func(err error) bool { return err == nil }},
		{"Flow", func(err error) bool { return err == nil }},
		{"Hosted", func(err error) bool {
			return errors.Is(err, ErrAzureAgentNotRunnable) && strings.Contains(err.Error(), "hosted")
		}},
		{"Off", func(err error) bool {
			return errors.Is(err, ErrAzureAgentNotRunnable) && strings.Contains(err.Error(), "disabled")
		}},
		{"Building", func(err error) bool {
			return errors.Is(err, ErrAzureAgentNotRunnable) && strings.Contains(err.Error(), "creating")
		}},
		{"Gone", func(err error) bool {
			return errors.Is(err, ErrAzureAgentNotFound) && !errors.Is(err, ErrAzureProjectNotFound) && strings.Contains(err.Error(), `"Gone"`)
		}},
	} {
		agent := AzureRegisteredModel{EntryID: "aaaaaaaaa1", Kind: AzureModelKindAgent, Deployment: tc.name}
		p := probeTestProvider(t, srv, registryEntraConfig(srv), []AzureRegisteredModel{agent}, "")
		if err := p.probeModel(context.Background(), agent); !tc.check(err) {
			t.Errorf("agent %q: %v", tc.name, err)
		}
		assertNoProcessCacheWrites(t)
	}
	got := methods.all()
	if len(got) != 7 {
		t.Errorf("requests = %v, want one lookup per agent", got)
	}
	for _, m := range got {
		if !strings.HasPrefix(m, "GET /api/projects/p1/agents/") {
			t.Errorf("agent test request %s, want GET lookups only — an agent is never run", m)
		}
	}
	if got[1] != "GET /api/projects/p1/agents/my%20agent?api-version=v1" {
		t.Errorf("the agent name must be path-escaped: %s", got[1])
	}

	before := len(methods.all())
	agent := AzureRegisteredModel{EntryID: "aaaaaaaaa1", Kind: AzureModelKindAgent, Deployment: "Helper"}
	p := probeTestProvider(t, srv, azureKeyConfig(srv.URL), []AzureRegisteredModel{agent}, "")
	if err := p.probeModel(context.Background(), agent); !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("an agent on an API-key resource = %v, want ErrProviderConfigInvalid", err)
	}
	if n := len(methods.all()) - before; n != 0 {
		t.Errorf("%d requests for an agent the resource cannot run, want none", n)
	}
}

// An invalid model is refused before anything is built or sent.
func TestProbeAzureFoundryModel_InvalidModel(t *testing.T) {
	const doc = `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"}}`
	if err := ProbeAzureFoundryModel(context.Background(), doc, AzureRegisteredModel{Kind: "vision", Deployment: "x"}); azureCfgCode(t, err) != AzureCfgErrInvalidKind {
		t.Errorf("unknown kind: %v", err)
	}
	if err := ProbeAzureFoundryModel(context.Background(), doc, AzureRegisteredModel{Kind: AzureModelKindChat, Deployment: "a b"}); azureCfgCode(t, err) != AzureCfgErrInvalidDeploy {
		t.Errorf("invalid deployment: %v", err)
	}
	if err := ProbeAzureFoundryResource(context.Background(), `{"endpoint":"https://evil.example.com","auth":{"mode":"api_key","key":"k"}}`); !errors.Is(err, ErrProviderConfigInvalid) || azureCfgCode(t, err) != AzureCfgErrHostNotAllowed {
		t.Errorf("invalid document: %v, want ErrProviderConfigInvalid / HOST_NOT_ALLOWED", err)
	}
}

// A test's Entra ID credential never enters the process-wide token source
// registries (a draft's secret may never be saved), and no test is served a
// token another provider cached: each gets its own source.
func TestProbeDrafts_UseAThrowawayTokenSource(t *testing.T) {
	saveAzureRuntime(t, AzureFoundryOptions{Egress: egress.AllowLoopback})
	ResetAzureFoundryCaches()
	t.Cleanup(ResetAzureFoundryCaches)
	const entraDoc = `{"endpoint":"https://my-res.services.ai.azure.com","auth":{"mode":"entra","tenantId":"` + entraTestTenant +
		`","clientId":"` + azureSeamClientID + `","clientSecret":"draft-secret"}}`
	const cliDoc = `{"endpoint":"https://my-res.services.ai.azure.com","auth":{"mode":"entra_cli","tenantId":"contoso.onmicrosoft.com"}}`

	a := newAzureProbeProvider(entraDoc, nil, "")
	b := newAzureProbeProvider(entraDoc, nil, "")
	cli := newAzureProbeProvider(cliDoc, nil, "")
	for name, p := range map[string]*AzureFoundryProvider{"entra": a, "cli": cli} {
		if p.configErr != nil || p.tokens == nil || !p.isolated {
			t.Fatalf("%s probe provider: err %v tokens %v isolated %v", name, p.configErr, p.tokens != nil, p.isolated)
		}
	}
	if a.tokens == b.tokens {
		t.Error("two tests of one credential must not share a token source")
	}
	entraSources.mu.Lock()
	shared := len(entraSources.m)
	entraSources.mu.Unlock()
	entraCLISources.mu.Lock()
	sharedCLI := len(entraCLISources.m)
	entraCLISources.mu.Unlock()
	if shared != 0 || sharedCLI != 0 {
		t.Errorf("process-wide registries hold %d / %d sources after the tests, want none", shared, sharedCLI)
	}
	if src, _ := entraSourceFor(entraTestTenant, azureSeamClientID, "draft-secret"); src == a.tokens || src == b.tokens {
		t.Error("the process-wide source for the tuple must be a different one")
	}
}

// End to end on the in-process Azure, host validation and the Azure client
// included: an API-key probe is one GET with the key; every Entra ID test
// signs in afresh (no shared token cache) and leaves the process-wide
// registries and caches empty; an agent test only reads.
func TestProbeAzureFoundry_EndToEnd(t *testing.T) {
	seam := installAzureSeam(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openai/v1/models":
			writeJSON(w, 200, map[string]any{"object": "list", "data": []map[string]any{}})
		case "/api/projects/p1/deployments":
			writeJSON(w, 200, map[string]any{"value": []map[string]any{}})
		case "/api/projects/p1/agents/Helper":
			writeJSON(w, 200, map[string]any{"name": "Helper", "definition": map[string]any{"kind": "prompt"}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	})
	ctx := context.Background()

	if err := ProbeAzureFoundryResource(ctx, seamKeyDoc("probe-key")); err != nil {
		t.Fatalf("API key probe: %v", err)
	}
	keyReqs := seam.requests("probe-key" + azureSeamSuffix)
	if len(keyReqs) != 1 || keyReqs[0].Method != http.MethodGet || keyReqs[0].Path != "/openai/v1/models" || keyReqs[0].Header.Get("api-key") != "seam-key" {
		t.Errorf("API key probe requests = %+v", keyReqs)
	}

	raw := seamEntraDoc("probe-entra")
	for i := range 2 {
		if err := ProbeAzureFoundryResource(ctx, raw); err != nil {
			t.Fatalf("Entra ID probe %d: %v", i, err)
		}
	}
	if n := seam.tokenRequests(); n != 2 {
		t.Errorf("sign-ins = %d, want one per test (a throwaway source each)", n)
	}
	if err := ProbeAzureFoundryModel(ctx, raw, AzureRegisteredModel{Kind: AzureModelKindAgent, Deployment: "Helper"}); err != nil {
		t.Fatalf("agent test: %v", err)
	}
	entraReqs := seam.requests("probe-entra" + azureSeamSuffix)
	var got []string
	for _, r := range entraReqs {
		got = append(got, r.Method+" "+r.Path+" "+r.Header.Get("Authorization"))
	}
	want := []string{
		"GET /openai/v1/models Bearer seam-token-1", "GET /api/projects/p1/deployments Bearer seam-token-1",
		"GET /openai/v1/models Bearer seam-token-2", "GET /api/projects/p1/deployments Bearer seam-token-2",
		"GET /api/projects/p1/agents/Helper Bearer seam-token-3",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("Entra ID test requests:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	entraSources.mu.Lock()
	shared := len(entraSources.m)
	entraSources.mu.Unlock()
	if shared != 0 {
		t.Errorf("%d process-wide token sources after the tests, want none", shared)
	}
	assertNoProcessCacheWrites(t)
}
