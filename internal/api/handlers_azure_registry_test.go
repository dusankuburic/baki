package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/api/route"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// The Azure AI Foundry model registry's HTTP API: the routes through the production router, the
// org role matrix, strict bodies, the error envelope, audit rows, /test and the outbound routes'
// rate-limit/RLS classification — against a fake Foundry.

// --- Harness ---

// azRegHostSuffix is the allow-listed domain of the fake Foundry (RFC 6761
// .invalid: it never resolves, so nothing escapes even if the redirect broke).
const azRegHostSuffix = ".unit-test.invalid"

// azRegAADHost is the Entra ID authority the sign-ins go to.
const azRegAADHost = "login.microsoftonline.com"

// The service principal of the fake's Entra ID resources.
const (
	azRegTenant = "11111111-2222-3333-4444-555555555555"
	azRegClient = "66666666-7777-8888-9999-000000000000"
)

// azRegBase is the caller's own registry.
const azRegBase = "/api/providers/azure-foundry"

// azRegEnv is the production router (JWT auth, /api/v1 alias, the real route table) with the
// registry wired as DI wires it in cloud mode, and the organisation "Acme" (u-admin, u-member,
// u-viewer, u-guest; u-stranger belongs to none). Every Azure request goes to the fake Foundry.
type azRegEnv struct {
	t        *testing.T
	rt       *Router
	fs       *filesystem.LocalStorageBackend
	secrets  *testutil.FakeSecretStore
	orgKeys  *testutil.FakeOrgSecretStore
	registry *testutil.FakeAzureRegistry
	factory  *ai.ProviderFactory
	svc      *service.ProviderService
	sys      *service.SystemService
	azure    *service.AzureModelService
	foundry  *azRegFoundry
	orgID    string
}

func newAzRegEnv(t *testing.T) *azRegEnv {
	t.Helper()
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("filesystem backend: %v", err)
	}
	e := &azRegEnv{
		t: t, rt: newTestRouter(fs, true), fs: fs, foundry: newAzRegFoundry(t),
		secrets: testutil.NewFakeSecretStore(), orgKeys: testutil.NewFakeOrgSecretStore(), registry: testutil.NewFakeAzureRegistry(),
	}
	e.factory = ai.NewProviderFactory(func(scope, provider string) (string, error) {
		v, _ := e.secrets.Get(scope, provider)
		return v, nil
	}, nil, nil, nil)
	e.factory.SetOrgKeyLookup(e.orgKeys.OrgKeyLookup)
	e.wire(nil)
	e.orgID = seedOrg(t, e.rt, "Acme", "u-admin")
	addOrgMember(t, e.rt, e.orgID, "u-member", auth.RoleMember)
	addOrgMember(t, e.rt, e.orgID, "u-viewer", auth.RoleViewer)
	addOrgMember(t, e.rt, e.orgID, "u-guest", auth.RoleGuest)
	return e
}

// wire (re)builds the registry service on the env's registry rows and the provider and system
// services on it, as DI does; adjust changes its dependencies.
func (e *azRegEnv) wire(adjust func(*testAzureModelsDeps)) {
	d := testAzureModelsDeps{
		Registry: e.registry, Secrets: e.secrets, OrgKeys: e.orgKeys, Factory: e.factory,
		Orgs: e.rt.security.OrgSvc, Users: e.fs, PerUser: true,
	}
	if adjust != nil {
		adjust(&d)
	}
	e.azure, _ = newTestAzureModels(d)
	e.install(e.azure, e.azure)
}

// install builds the provider and system services on their registries (nil for none) and hands them
// to the handlers, which read their services per request.
func (e *azRegEnv) install(providerAzure, systemAzure *service.AzureModelService) {
	e.svc = service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), e.factory, e.secrets, e.orgKeys, providerAzure)
	e.sys = service.NewSystemService(storage.NewInMemorySettingsStore(), e.secrets, &mockNotifier{}, e.fs, config.ModeCloud, systemAzure)
	e.sys.OnApiKeyChanged(e.svc.InvalidateModels)
	moduleOf[*ProviderHandler](e.t, e.rt).providerSvc = e.svc
	moduleOf[*OrgHandler](e.t, e.rt).providers = e.svc
	moduleOf[*SystemHandler](e.t, e.rt).sysSvc = e.sys
}

// orgBase is the organisation's registry.
func (e *azRegEnv) orgBase() string { return "/api/orgs/" + e.orgID + "/providers/azure-foundry" }

// do sends a request as userID through the router. body is nil (none), a
// string (sent as is) or a value (JSON).
func (e *azRegEnv) do(userID, method, path string, body any) *httptest.ResponseRecorder {
	e.t.Helper()
	var buf bytes.Buffer
	switch b := body.(type) {
	case nil:
	case string:
		buf.WriteString(b)
	default:
		if err := json.NewEncoder(&buf).Encode(b); err != nil {
			e.t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Authorization", jwtBearer(e.t, e.rt, userID, userID+"@example.com"))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	e.rt.ServeHTTP(rr, req)
	return rr
}

// createConnection adds a resource through base's POST /connections (201).
func (e *azRegEnv) createConnection(userID, base string, body map[string]any) service.AzureConnectionView {
	e.t.Helper()
	rr := e.do(userID, http.MethodPost, base+"/connections", body)
	if rr.Code != http.StatusCreated {
		e.t.Fatalf("create resource as %s: %d %s", userID, rr.Code, rr.Body.String())
	}
	var out service.AzureConnectionEnvelope
	azRegDecode(e.t, rr, &out)
	return out.Connection
}

// createModel adds a model through base's POST /models (201).
func (e *azRegEnv) createModel(userID, base string, body map[string]any) service.AzureModelView {
	e.t.Helper()
	rr := e.do(userID, http.MethodPost, base+"/models", body)
	if rr.Code != http.StatusCreated {
		e.t.Fatalf("create model as %s: %d %s", userID, rr.Code, rr.Body.String())
	}
	var out service.AzureModelEnvelope
	azRegDecode(e.t, rr, &out)
	return out.Model
}

// connection reads one resource through base's GET /connections/{id}.
func (e *azRegEnv) connection(userID, base, id string) service.AzureConnectionView {
	e.t.Helper()
	rr := e.do(userID, http.MethodGet, base+"/connections/"+id, nil)
	checkStatus(e.t, rr, http.StatusOK)
	var out service.AzureConnectionEnvelope
	azRegDecode(e.t, rr, &out)
	return out.Connection
}

// apiKeyResource is a POST /connections body with an API key.
func apiKeyResource(name, host, key string) map[string]any {
	return map[string]any{"name": name, "config": map[string]any{
		"endpoint": "https://" + host,
		"auth":     map[string]any{"mode": "api_key", "key": key},
	}}
}

// entraResource is a POST /connections body with a Microsoft Entra ID
// service principal and the project proj1 (discovery, agents).
func entraResource(name, host, secret string) map[string]any {
	return map[string]any{"name": name, "config": map[string]any{
		"endpoint":        "https://" + host,
		"auth":            map[string]any{"mode": "entra", "tenantId": azRegTenant, "clientId": azRegClient, "clientSecret": secret},
		"projectEndpoint": "https://" + host + "/api/projects/proj1",
	}}
}

func azRegDecode(t *testing.T, rr *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rr.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %q: %v", rr.Body.String(), err)
	}
}

// azRegWantError checks an error answer's status and code and returns its
// envelope.
func azRegWantError(t *testing.T, rr *httptest.ResponseRecorder, status int, code string) render.ErrorResponse {
	t.Helper()
	var out render.ErrorResponse
	if rr.Code != status {
		t.Errorf("status = %d, want %d (body %s)", rr.Code, status, rr.Body.String())
		return out
	}
	azRegDecode(t, rr, &out)
	if out.Code != code {
		t.Errorf("code = %q, want %q (body %s)", out.Code, code, rr.Body.String())
	}
	return out
}

// isAzRegAudit matches an audit row of action.
func isAzRegAudit(action string) func(*storageif.AuditEvent) bool {
	return func(ev *storageif.AuditEvent) bool { return ev.Action == action }
}

// assertAzRegAudit checks an audit row's resource and meta (every key of
// want), and that its meta carries no secret and no description.
func assertAzRegAudit(t *testing.T, ev *storageif.AuditEvent, resourceType, resourceID string, want map[string]string) {
	t.Helper()
	if ev.ResourceType != resourceType || ev.ResourceID != resourceID {
		t.Errorf("%s row on %s/%s, want %s/%s", ev.Action, ev.ResourceType, ev.ResourceID, resourceType, resourceID)
	}
	for k, v := range want {
		if ev.Meta[k] != v {
			t.Errorf("%s meta[%s] = %q, want %q (meta %v)", ev.Action, k, ev.Meta[k], v, ev.Meta)
		}
	}
	for k, v := range ev.Meta {
		if low := strings.ToLower(v); strings.Contains(low, "secret") || strings.Contains(low, "private note") {
			t.Errorf("%s meta %s = %q leaks a secret or a description", ev.Action, k, v)
		}
	}
	if _, has := ev.Meta["description"]; has {
		t.Errorf("%s meta carries a description: %v", ev.Action, ev.Meta)
	}
}

// azRegCorpus is a knowledge base of a fixed size (every organisation's).
type azRegCorpus struct{ chunks int }

func (c azRegCorpus) CountKnowledgeChunks(context.Context, string) (int, int, error) {
	return c.chunks, c.chunks, nil
}

// azRegFailingSecrets is a keystore whose saves fail with err.
type azRegFailingSecrets struct {
	*testutil.FakeSecretStore
	err error
}

func (s azRegFailingSecrets) Save(string, string, string) error { return s.err }

// --- The fake Azure AI Foundry ---

// azRegCall is one request the fake Azure side received.
type azRegCall struct {
	Method string
	Host   string
	Path   string
	Query  url.Values
	APIKey string
	Bearer string
	Body   map[string]any
}

func (c azRegCall) String() string { return c.Method + " " + c.Host + c.Path }

// azRegFoundry is a fake Azure AI Foundry — every *.unit-test.invalid resource, its project and
// the Entra ID authority — reached through the ai package's test seam, which refuses every other
// destination. It records every request, so a test proves which calls were made and which never were.
type azRegFoundry struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls []azRegCall
	// modelsStatus is what GET /openai/v1/models answers (0: 200).
	modelsStatus int
}

func newAzRegFoundry(t *testing.T) *azRegFoundry {
	t.Helper()
	f := &azRegFoundry{}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	base := &http.Transport{}
	t.Cleanup(base.CloseIdleConnections)
	ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{
		Egress:            egress.AllowLoopback,
		ExtraAllowedHosts: []string{"*" + azRegHostSuffix},
		Transport:         &azRegTransport{target: f.srv.Listener.Addr().String(), base: base},
	})
	ai.ResetAzureFoundryCaches()
	t.Cleanup(func() {
		ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{})
		ai.ResetAzureFoundryCaches()
	})
	return f
}

// azRegTransport sends https://<x>.unit-test.invalid and the Entra ID authority to the fake's
// listener (original host kept in the Host header) and refuses every other destination.
type azRegTransport struct {
	target string
	base   *http.Transport
}

func (tr *azRegTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	if req.URL.Scheme != "https" || (!strings.HasSuffix(host, azRegHostSuffix) && host != azRegAADHost) {
		return nil, fmt.Errorf("azure registry test transport: refusing %s", req.URL.Redacted())
	}
	out := req.Clone(req.Context())
	out.URL.Scheme, out.URL.Host, out.Host = "http", tr.target, req.URL.Host
	return tr.base.RoundTrip(out)
}

func (f *azRegFoundry) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	host := strings.ToLower(r.Host)
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	call := azRegCall{Method: r.Method, Host: host, Path: r.URL.Path, Query: r.URL.Query(),
		APIKey: r.Header.Get("api-key"), Bearer: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")}
	_ = json.Unmarshal(raw, &call.Body)
	f.mu.Lock()
	f.calls = append(f.calls, call)
	modelsStatus := f.modelsStatus
	f.mu.Unlock()

	const project = "/api/projects/proj1"
	w.Header().Set("Content-Type", "application/json")
	switch {
	case host == azRegAADHost:
		azRegServeEntra(w, r)
	case r.Method == http.MethodGet && r.URL.Path == "/openai/v1/models":
		if modelsStatus != 0 && modelsStatus != http.StatusOK {
			w.WriteHeader(modelsStatus)
			_, _ = io.WriteString(w, `{"error":{"code":"401","message":"Access denied due to invalid subscription key or wrong API endpoint."}}`)
			return
		}
		_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"gpt-4.1","object":"model"}]}`)
	case r.Method == http.MethodPost && r.URL.Path == "/openai/v1/chat/completions":
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4.1","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	case r.Method == http.MethodPost && r.URL.Path == "/openai/v1/embeddings":
		_, _ = io.WriteString(w, `{"object":"list","data":[{"object":"embedding","index":0,"embedding":[0.1,0.2,0.3]}],"model":"text-emb","usage":{"prompt_tokens":1,"total_tokens":1}}`)
	case r.Method == http.MethodGet && r.URL.Path == project+"/deployments":
		_, _ = io.WriteString(w, `{"value":[`+
			`{"name":"gpt-4.1","modelName":"gpt-4.1","modelPublisher":"OpenAI","capabilities":{"chat_completion":"true"}},`+
			`{"name":"text-emb","modelName":"text-embedding-3-small","modelPublisher":"OpenAI","capabilities":{"embeddings":"true"}}]}`)
	case r.Method == http.MethodGet && r.URL.Path == project+"/agents":
		_, _ = io.WriteString(w, `{"data":[{"name":"helper","state":"enabled","versions":{"latest":{"status":"active","definition":{"kind":"prompt","model":"gpt-4.1"}}}}],"has_more":false}`)
	case r.Method == http.MethodGet && r.URL.Path == project+"/agents/helper":
		_, _ = io.WriteString(w, `{"name":"helper","state":"enabled","versions":{"latest":{"status":"active","definition":{"kind":"prompt","model":"gpt-4.1"}}}}`)
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"NotFound","message":"no such route"}}`)
	}
}

// azRegServeEntra answers the client-credentials sign-in azidentity (MSAL)
// performs: instance discovery, the tenant's OpenID configuration, the token.
func azRegServeEntra(w http.ResponseWriter, r *http.Request) {
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
	switch {
	case strings.HasSuffix(r.URL.Path, "/discovery/instance"):
		tenant := "common"
		if ae := r.URL.Query().Get("authorization_endpoint"); ae != "" {
			if u, err := url.Parse(ae); err == nil {
				tenant = strings.Split(strings.Trim(u.Path, "/"), "/")[0]
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tenant_discovery_endpoint": "https://" + azRegAADHost + "/" + tenant + "/v2.0/.well-known/openid-configuration",
			"api-version":               "1.1",
			"metadata": []map[string]any{{
				"preferred_network": azRegAADHost, "preferred_cache": "login.windows.net",
				"aliases": []string{azRegAADHost, "login.windows.net", "login.microsoft.com", "sts.windows.net"},
			}},
		})
	case strings.HasSuffix(r.URL.Path, "/v2.0/.well-known/openid-configuration") && len(parts) > 0:
		base := "https://" + azRegAADHost + "/" + parts[0]
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_endpoint":                        base + "/oauth2/v2.0/token",
			"authorization_endpoint":                base + "/oauth2/v2.0/authorize",
			"issuer":                                base + "/v2.0",
			"jwks_uri":                              base + "/discovery/v2.0/keys",
			"token_endpoint_auth_methods_supported": []string{"client_secret_post", "private_key_jwt", "client_secret_basic"},
			"response_types_supported":              []string{"code", "id_token", "code id_token", "id_token token"},
			"tenant_region_scope":                   "EU",
			"cloud_instance_name":                   "microsoftonline.com",
		})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type": "Bearer", "expires_in": 3599, "ext_expires_in": 3599, "access_token": "fake-entra-token",
		})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found"}`)
	}
}

// reset forgets the calls so far.
func (f *azRegFoundry) reset() {
	f.mu.Lock()
	f.calls = nil
	f.mu.Unlock()
}

// callsTo returns the calls to host so far, in order.
func (f *azRegFoundry) callsTo(host string) []azRegCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []azRegCall
	for _, c := range f.calls {
		if c.Host == host {
			out = append(out, c)
		}
	}
	return out
}

// azRegTestResult posts a /test body to base as userID and reads the 200.
func (e *azRegEnv) test(userID, base string, body map[string]any) service.AzureTestResult {
	e.t.Helper()
	rr := e.do(userID, http.MethodPost, base+"/test", body)
	if rr.Code != http.StatusOK {
		e.t.Fatalf("test %v: %d %s", body, rr.Code, rr.Body.String())
	}
	var out service.AzureTestResult
	azRegDecode(e.t, rr, &out)
	return out
}

// --- Routes ---

// Every registry route is registered, the caller's own and the
// organisation's, with the same sub-paths.
func TestAzureRegistryRoutes_Registered(t *testing.T) {
	rt := newJWTTestRouter(t)
	registered := map[string]bool{}
	if err := chi.Walk(rt.mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+strings.TrimSuffix(route, "/")] = true
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"/api/providers/azure-foundry", "/api/orgs/{id}/providers/azure-foundry"} {
		for _, r := range []string{
			"GET /connections", "POST /connections",
			"GET /connections/{connectionId}", "PUT /connections/{connectionId}", "DELETE /connections/{connectionId}",
			"POST /connections/{connectionId}/discover",
			"GET /models", "POST /models", "GET /models/{modelId}", "PUT /models/{modelId}", "DELETE /models/{modelId}",
			"POST /test",
		} {
			method, sub, _ := strings.Cut(r, " ")
			if !registered[method+" "+base+sub] {
				t.Errorf("route %s %s%s is not registered", method, base, sub)
			}
		}
	}
}

// Without a wired registry every registry route answers 503 AZURE_REGISTRY_UNAVAILABLE; without org
// registries the org routes answer 404 ORG_KEYSTORE_UNAVAILABLE to everyone, before any membership check.
func TestAzureRegistry_Unavailable(t *testing.T) {
	t.Run("no registry", func(t *testing.T) {
		e := newAzRegEnv(t)
		e.install(nil, e.azure)
		const code = "AZURE_REGISTRY_UNAVAILABLE"
		azRegWantError(t, e.do("u-admin", http.MethodGet, azRegBase+"/connections", nil), http.StatusServiceUnavailable, code)
		azRegWantError(t, e.do("u-admin", http.MethodPost, azRegBase+"/models", map[string]any{}), http.StatusServiceUnavailable, code)
		azRegWantError(t, e.do("u-admin", http.MethodPost, azRegBase+"/test", map[string]any{"connectionId": "default"}), http.StatusServiceUnavailable, code)
		azRegWantError(t, e.do("u-admin", http.MethodGet, e.orgBase()+"/models", nil), http.StatusServiceUnavailable, code)
		azRegWantError(t, e.do("u-admin", http.MethodPost, e.orgBase()+"/test", map[string]any{"connectionId": "default"}), http.StatusServiceUnavailable, code)
	})
	t.Run("no organisation registries", func(t *testing.T) {
		e := newAzRegEnv(t)
		e.wire(func(d *testAzureModelsDeps) { d.OrgKeys = nil })
		for _, caller := range []string{"u-admin", "u-member", "u-viewer", "u-stranger"} {
			azRegWantError(t, e.do(caller, http.MethodGet, e.orgBase()+"/connections", nil), http.StatusNotFound, "ORG_KEYSTORE_UNAVAILABLE")
			azRegWantError(t, e.do(caller, http.MethodPost, e.orgBase()+"/models", map[string]any{}), http.StatusNotFound, "ORG_KEYSTORE_UNAVAILABLE")
			azRegWantError(t, e.do(caller, http.MethodDelete, e.orgBase()+"/connections/abcdefghij", nil), http.StatusNotFound, "ORG_KEYSTORE_UNAVAILABLE")
		}
		checkStatus(t, e.do("u-admin", http.MethodGet, azRegBase+"/connections", nil), http.StatusOK)
	})
}

// --- The caller's own registry ---

// The personal CRUD round trip through the real router (and its /api/v1 alias): envelopes, the stored
// credential, compare-and-swap, CONNECTION_IN_USE, cascade — and an audit row for every write, no secret.
func TestAzureRegistry_PersonalLifecycle(t *testing.T) {
	e := newAzRegEnv(t)
	ch := captureAudit(t)
	const user = "u-stranger" // any authenticated user has a registry of their own

	rr := e.do(user, http.MethodGet, "/api/v1/providers/azure-foundry/connections", nil)
	checkStatus(t, rr, http.StatusOK)
	var listing service.AzureConnectionsListing
	azRegDecode(t, rr, &listing)
	if len(listing.Connections) != 0 || listing.RegistryState != "ok" || listing.KnowledgeEmbeddingEnabled ||
		listing.Limits != (service.AzureRegistryLimits{Connections: 10, Models: 50}) {
		t.Errorf("empty listing = %+v", listing)
	}

	rr = e.do(user, http.MethodPost, azRegBase+"/connections", apiKeyResource("", "contoso-prod.openai.azure.com", "sk-secret-1"))
	checkStatus(t, rr, http.StatusCreated)
	if strings.Contains(rr.Body.String(), "sk-secret-1") {
		t.Errorf("the create answer echoes the secret: %s", rr.Body.String())
	}
	var created service.AzureConnectionEnvelope
	azRegDecode(t, rr, &created)
	conn := created.Connection
	if len(conn.ID) != 10 || conn.Name != "contoso-prod" || conn.Legacy || conn.Shared || conn.Status != service.AzureStatusReady ||
		conn.Config.AuthMode != "api_key" || !conn.Config.HasSecret || conn.Config.Endpoint != "https://contoso-prod.openai.azure.com" {
		t.Errorf("created resource = %+v", conn)
	}
	if raw, _ := e.secrets.Get(user, "azure-foundry/"+conn.ID); !strings.Contains(raw, "sk-secret-1") {
		t.Errorf("stored credential = %q, want the key under azure-foundry/%s", raw, conn.ID)
	}
	assertAzRegAudit(t, awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureConnectionSave)), "azure_connection", conn.ID,
		map[string]string{"op": "create", "connectionId": conn.ID, "authMode": "api_key"})

	if got := e.connection(user, azRegBase, conn.ID); got.ID != conn.ID || got.Name != "contoso-prod" {
		t.Errorf("GET /connections/%s = %+v", conn.ID, got)
	}

	rr = e.do(user, http.MethodPut, azRegBase+"/connections/"+conn.ID, map[string]any{"name": "Contoso", "updatedAt": conn.UpdatedAt})
	checkStatus(t, rr, http.StatusOK)
	var renamed service.AzureConnectionEnvelope
	azRegDecode(t, rr, &renamed)
	if renamed.Connection.Name != "Contoso" || renamed.Connection.UpdatedAt == conn.UpdatedAt {
		t.Errorf("renamed = %+v", renamed.Connection)
	}
	assertAzRegAudit(t, awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureConnectionSave)), "azure_connection", conn.ID,
		map[string]string{"op": "update", "connectionId": conn.ID, "renamed": "true"})
	azRegWantError(t, e.do(user, http.MethodPut, azRegBase+"/connections/"+conn.ID, map[string]any{"name": "Other", "updatedAt": conn.UpdatedAt}),
		http.StatusConflict, "CONFLICT")

	model := e.createModel(user, azRegBase, map[string]any{
		"connectionId": conn.ID, "kind": "chat", "deployment": "gpt-4.1", "displayName": "Docs writer",
		"purpose": "documentation", "description": "Writes flow docs (private note)", "contextWindow": 32768, "maxOutputTokens": 4096,
	})
	if model.ModelID != "m/"+model.ID || model.Target != "gpt-4.1" || model.ConnectionID != conn.ID || model.ConnectionName != "Contoso" ||
		model.DisplayName != "Docs writer" || model.Purpose != "documentation" || model.ContextWindow != 32768 ||
		model.MaxOutputTokens != 4096 || model.Status != service.AzureStatusReady || model.Shared {
		t.Errorf("created model = %+v", model)
	}
	assertAzRegAudit(t, awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureModelSave)), "azure_model", model.ID, map[string]string{
		"op": "create", "modelId": model.ID, "connectionId": conn.ID, "kind": "chat", "deployment": "gpt-4.1",
		"isDefault": "false", "isKnowledgeEmbedding": "false",
	})

	rr = e.do(user, http.MethodGet, azRegBase+"/models", nil)
	checkStatus(t, rr, http.StatusOK)
	var models service.AzureModelsListing
	azRegDecode(t, rr, &models)
	if len(models.Models) != 1 || models.Models[0].ID != model.ID || models.ServerDefault != model.ModelID {
		t.Errorf("GET /models = %+v, want the model and it as the server default", models)
	}
	rr = e.do(user, http.MethodGet, azRegBase+"/models/"+model.ID, nil)
	checkStatus(t, rr, http.StatusOK)
	var one service.AzureModelEnvelope
	azRegDecode(t, rr, &one)
	if one.Model.ID != model.ID || one.Model.Description != "Writes flow docs (private note)" {
		t.Errorf("GET /models/%s = %+v", model.ID, one.Model)
	}

	env := azRegWantError(t, e.do(user, http.MethodDelete, azRegBase+"/connections/"+conn.ID, nil), http.StatusConflict, "CONNECTION_IN_USE")
	if env.Details["modelCount"] != float64(1) {
		t.Errorf("CONNECTION_IN_USE details = %v, want modelCount 1", env.Details)
	}

	rr = e.do(user, http.MethodDelete, azRegBase+"/models/"+model.ID, nil)
	checkStatus(t, rr, http.StatusOK)
	if got := azureDecode(t, rr); got["status"] != "ok" {
		t.Errorf("DELETE /models answer = %v", got)
	}
	assertAzRegAudit(t, awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureModelDelete)), "azure_model", model.ID,
		map[string]string{"op": "delete", "modelId": model.ID})

	e.createModel(user, azRegBase, map[string]any{"connectionId": conn.ID, "kind": "claude", "deployment": "claude-sonnet"})
	_ = awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureModelSave))
	rr = e.do(user, http.MethodDelete, azRegBase+"/connections/"+conn.ID+"?cascade=true", nil)
	checkStatus(t, rr, http.StatusOK)
	var deleted service.AzureConnectionDeleteResult
	azRegDecode(t, rr, &deleted)
	if deleted != (service.AzureConnectionDeleteResult{Status: "ok", DeletedModels: 1}) {
		t.Errorf("DELETE /connections answer = %+v", deleted)
	}
	assertAzRegAudit(t, awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureConnectionDelete)), "azure_connection", conn.ID,
		map[string]string{"op": "delete", "connectionId": conn.ID, "deletedModels": "1", "clearedKnowledgeEmbedding": "false"})
	if has, _ := e.secrets.Has(user, "azure-foundry/"+conn.ID); has {
		t.Error("the removed resource's credential is still stored")
	}
	azRegWantError(t, e.do(user, http.MethodGet, azRegBase+"/connections/"+conn.ID, nil), http.StatusNotFound, "CONNECTION_NOT_FOUND")
}

// Bodies decode strictly (unknown fields, trailing data, the size caps) with INVALID_CONFIG; a
// wrong-typed number names its field's code; nothing a refused body asked for happens.
func TestAzureRegistry_StrictDecode(t *testing.T) {
	e := newAzRegEnv(t)
	const user = "u-stranger"
	conn := e.createConnection(user, azRegBase, apiKeyResource("res", "res.openai.azure.com", "k"))
	modelBody := func(extra string) string {
		return `{"connectionId":"` + conn.ID + `","kind":"chat","deployment":"d"` + extra + `}`
	}
	for _, c := range []struct {
		name, method, path, body string
		status                   int
		code, msg                string
	}{
		{"unknown field", http.MethodPost, "/connections",
			`{"name":"x","config":{"endpoint":"https://x.openai.azure.com","auth":{"mode":"api_key","key":"k"}},"deployments":["gpt-4o"]}`,
			http.StatusBadRequest, "INVALID_CONFIG", `unknown field "deployments"`},
		{"unknown nested field", http.MethodPost, "/connections",
			`{"config":{"endpoint":"https://x.openai.azure.com","auth":{"mode":"api_key","key":"k","keey":"k"}}}`,
			http.StatusBadRequest, "INVALID_CONFIG", `unknown field "keey"`},
		{"trailing data", http.MethodPut, "/connections/" + conn.ID, `{"name":"a"}{"name":"b"}`,
			http.StatusBadRequest, "INVALID_CONFIG", "unexpected data after the JSON object"},
		{"not JSON", http.MethodPost, "/models", `not-json`, http.StatusBadRequest, "INVALID_CONFIG", ""},
		{"empty body", http.MethodPost, "/test", ``, http.StatusBadRequest, "INVALID_CONFIG", "empty"},
		{"unknown model field", http.MethodPost, "/models", modelBody(`,"contextWindw":4096`),
			http.StatusBadRequest, "INVALID_CONFIG", `unknown field "contextWindw"`},
		{"window not a number", http.MethodPost, "/models", modelBody(`,"contextWindow":"big"`),
			http.StatusBadRequest, "INVALID_CONTEXT_WINDOW", ""},
		{"output cap not whole", http.MethodPost, "/models", modelBody(`,"maxOutputTokens":1.5`),
			http.StatusBadRequest, "INVALID_MAX_OUTPUT", ""},
		{"patch window not a number", http.MethodPut, "/models/abcdefghij", `{"contextWindow":true}`,
			http.StatusBadRequest, "INVALID_CONTEXT_WINDOW", ""},
		{"model body over 16 KiB", http.MethodPost, "/models", modelBody(`,"description":"` + strings.Repeat("a", 17<<10) + `"`),
			http.StatusBadRequest, "INVALID_CONFIG", "exceeds 16 KiB"},
		{"resource body over 64 KiB", http.MethodPost, "/connections", `{"name":"` + strings.Repeat("a", 65<<10) + `"}`,
			http.StatusBadRequest, "INVALID_CONFIG", "exceeds 64 KiB"},
		{"test body over 64 KiB", http.MethodPost, "/test", `{"connectionId":"` + strings.Repeat("a", 65<<10) + `"}`,
			http.StatusBadRequest, "INVALID_CONFIG", "exceeds 64 KiB"},
		{"cascade not a boolean", http.MethodDelete, "/connections/" + conn.ID + "?cascade=maybe", "",
			http.StatusBadRequest, "BAD_REQUEST", "cascade"},
		{"force not a boolean", http.MethodDelete, "/models/abcdefghij?force=2", "",
			http.StatusBadRequest, "BAD_REQUEST", "force"},
	} {
		t.Run(c.name, func(t *testing.T) {
			env := azRegWantError(t, e.do(user, c.method, azRegBase+c.path, c.body), c.status, c.code)
			if c.msg != "" && !strings.Contains(env.Message, c.msg) {
				t.Errorf("message = %q, want it to name %q", env.Message, c.msg)
			}
		})
	}
	// Under the resource cap a 20 KiB body is read — and refused for what it
	// says (the model cap is 16 KiB).
	azRegWantError(t, e.do(user, http.MethodPost, azRegBase+"/connections",
		`{"name":"`+strings.Repeat("a", 20<<10)+`","config":{"endpoint":"https://y.openai.azure.com","auth":{"mode":"api_key","key":"k"}}}`),
		http.StatusBadRequest, "INVALID_CONNECTION_NAME")

	// Nothing a refused body asked for happened.
	rr := e.do(user, http.MethodGet, azRegBase+"/connections", nil)
	var listing service.AzureConnectionsListing
	azRegDecode(t, rr, &listing)
	if len(listing.Connections) != 1 || listing.Connections[0].Name != "res" || listing.Connections[0].ModelCount != 0 {
		t.Errorf("resources after the refused bodies = %+v", listing.Connections)
	}
}

// PUT /models/{id} tells an absent override (kept) from null (cleared) and from a value; "" clears the description.
func TestAzureRegistry_ModelPatchAbsentVersusNull(t *testing.T) {
	e := newAzRegEnv(t)
	const user = "u-stranger"
	conn := e.createConnection(user, azRegBase, apiKeyResource("res", "res.openai.azure.com", "k"))
	m := e.createModel(user, azRegBase, map[string]any{
		"connectionId": conn.ID, "kind": "chat", "deployment": "distilled",
		"contextWindow": 32768, "maxOutputTokens": 4096, "description": "d1",
	})
	put := func(t *testing.T, body string) map[string]any {
		t.Helper()
		rr := e.do(user, http.MethodPut, azRegBase+"/models/"+m.ID, body)
		checkStatus(t, rr, http.StatusOK)
		var out struct {
			Model map[string]any `json:"model"`
		}
		azRegDecode(t, rr, &out)
		return out.Model
	}
	absent := func(model map[string]any, key string) bool {
		_, has := model[key]
		return !has
	}

	got := put(t, `{"displayName":"Distilled"}`)
	if got["displayName"] != "Distilled" || got["contextWindow"] != float64(32768) || got["maxOutputTokens"] != float64(4096) || got["description"] != "d1" {
		t.Errorf("absent overrides must be kept: %v", got)
	}
	got = put(t, `{"contextWindow":null}`)
	if !absent(got, "contextWindow") || got["maxOutputTokens"] != float64(4096) {
		t.Errorf("contextWindow null must clear it and keep maxOutputTokens: %v", got)
	}
	got = put(t, `{"maxOutputTokens":8192}`)
	if !absent(got, "contextWindow") || got["maxOutputTokens"] != float64(8192) {
		t.Errorf("a value sets it, the cleared one stays cleared: %v", got)
	}
	got = put(t, `{"contextWindow":65536,"maxOutputTokens":null}`)
	if got["contextWindow"] != float64(65536) || !absent(got, "maxOutputTokens") {
		t.Errorf("set one, clear the other: %v", got)
	}
	got = put(t, `{"description":""}`)
	if !absent(got, "description") || got["contextWindow"] != float64(65536) || got["displayName"] != "Distilled" {
		t.Errorf(`description "" must clear it and keep the rest: %v`, got)
	}
	if got["id"] != m.ID || got["modelId"] != m.ModelID {
		t.Errorf("the id must be kept: %v", got)
	}
	azRegWantError(t, e.do(user, http.MethodPut, azRegBase+"/models/"+m.ID, `{"contextWindow":10}`), http.StatusBadRequest, "INVALID_CONTEXT_WINDOW")
	azRegWantError(t, e.do(user, http.MethodPut, azRegBase+"/models/"+m.ID, `{"displayName":"x","updatedAt":"`+m.UpdatedAt+`"}`),
		http.StatusConflict, "CONFLICT")
}

// The registry's refusals keep the {code, message, details?} envelope — the codes and fields the modals route.
func TestAzureRegistry_ErrorCodes(t *testing.T) {
	e := newAzRegEnv(t)
	const user = "u-stranger"
	conn := e.createConnection(user, azRegBase, apiKeyResource("res", "res.openai.azure.com", "k"))
	model := e.createModel(user, azRegBase, map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": "gpt-4.1"})
	post := func(path string, body any) *httptest.ResponseRecorder {
		return e.do(user, http.MethodPost, azRegBase+path, body)
	}
	entra := func(tenant string) map[string]any {
		b := entraResource("entra", "entra.services.ai.azure.com", "entra-secret")
		b["config"].(map[string]any)["auth"].(map[string]any)["tenantId"] = tenant
		return b
	}

	t.Run("settings codes name their field", func(t *testing.T) {
		env := azRegWantError(t, post("/connections", apiKeyResource("nokey", "nokey.openai.azure.com", "")), http.StatusBadRequest, "MISSING_SECRET")
		if env.Details["field"] != "key" {
			t.Errorf("MISSING_SECRET details = %v, want field key", env.Details)
		}
		env = azRegWantError(t, post("/connections", entra("not a tenant!")), http.StatusBadRequest, "INVALID_AUTH")
		if env.Details["field"] != "tenantId" {
			t.Errorf("INVALID_AUTH details = %v, want field tenantId", env.Details)
		}
		azRegWantError(t, post("/connections", apiKeyResource("evil", "evil.example", "k")), http.StatusBadRequest, "HOST_NOT_ALLOWED")
		// A zero-width space: what lets a shared name impersonate another.
		azRegWantError(t, post("/connections", apiKeyResource("bad"+string(rune(0x200b))+"name", "ok.openai.azure.com", "k")), http.StatusBadRequest, "INVALID_CONNECTION_NAME")
	})
	t.Run("resources", func(t *testing.T) {
		azRegWantError(t, post("/connections", apiKeyResource("RES", "other.openai.azure.com", "k")), http.StatusConflict, "DUPLICATE_CONNECTION_NAME")
		azRegWantError(t, e.do(user, http.MethodGet, azRegBase+"/connections/zzzzzzzzzz", nil), http.StatusNotFound, "CONNECTION_NOT_FOUND")
		azRegWantError(t, e.do(user, http.MethodGet, azRegBase+"/connections/not-an-id", nil), http.StatusNotFound, "CONNECTION_NOT_FOUND")
		for i := 2; i <= 10; i++ {
			e.createConnection(user, azRegBase, apiKeyResource(fmt.Sprintf("res-%d", i), fmt.Sprintf("res%d.openai.azure.com", i), "k"))
		}
		env := azRegWantError(t, post("/connections", apiKeyResource("res-11", "res11.openai.azure.com", "k")), http.StatusConflict, "TOO_MANY_CONNECTIONS")
		if env.Details["limit"] != float64(10) {
			t.Errorf("TOO_MANY_CONNECTIONS details = %v, want limit 10", env.Details)
		}
	})
	t.Run("models", func(t *testing.T) {
		env := azRegWantError(t, post("/models", map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": "GPT-4.1"}), http.StatusConflict, "DUPLICATE_MODEL")
		if env.Details["existingModelId"] != model.ID {
			t.Errorf("DUPLICATE_MODEL details = %v, want existingModelId %s", env.Details, model.ID)
		}
		azRegWantError(t, post("/models", map[string]any{"connectionId": conn.ID, "kind": "agent", "deployment": "helper"}), http.StatusBadRequest, "KIND_REQUIRES_PROJECT")
		azRegWantError(t, post("/models", map[string]any{"connectionId": conn.ID, "kind": "bogus", "deployment": "d"}), http.StatusBadRequest, "INVALID_KIND")
		azRegWantError(t, post("/models", map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": "a b"}), http.StatusBadRequest, "INVALID_DEPLOYMENT")
		azRegWantError(t, post("/models", map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": "d", "purpose": "bogus"}), http.StatusBadRequest, "INVALID_PURPOSE")
		azRegWantError(t, post("/models", map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": "d", "description": strings.Repeat("x", 201)}), http.StatusBadRequest, "INVALID_DESCRIPTION")
		azRegWantError(t, post("/models", map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": "d", "displayName": "a" + string(rune(0x202e)) + "b"}), http.StatusBadRequest, "INVALID_DISPLAY_NAME")
		azRegWantError(t, post("/models", map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": "d", "maxOutputTokens": 10}), http.StatusBadRequest, "INVALID_MAX_OUTPUT")
		azRegWantError(t, post("/models", map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": "d", "isDefault": true}), http.StatusBadRequest, "INVALID_DESIGNATION")
		azRegWantError(t, post("/models", map[string]any{"connectionId": "zzzzzzzzzz", "kind": "chat", "deployment": "d"}), http.StatusNotFound, "CONNECTION_NOT_FOUND")
		azRegWantError(t, e.do(user, http.MethodGet, azRegBase+"/models/zzzzzzzzzz", nil), http.StatusNotFound, "MODEL_NOT_FOUND")
		azRegWantError(t, e.do(user, http.MethodDelete, azRegBase+"/models/zzzzzzzzzz", nil), http.StatusNotFound, "MODEL_NOT_FOUND")
		for i := 2; i <= 50; i++ {
			e.createModel(user, azRegBase, map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": fmt.Sprintf("d-%02d", i)})
		}
		env = azRegWantError(t, post("/models", map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": "d-51"}), http.StatusConflict, "TOO_MANY_MODELS")
		if env.Details["limit"] != float64(50) {
			t.Errorf("TOO_MANY_MODELS details = %v, want limit 50", env.Details)
		}
	})
	t.Run("store refusals", func(t *testing.T) {
		e.registry.FailOn("CreateConnection", storageif.ErrAzureRegistryReadOnly)
		// The limit is reached: use a second user.
		azRegWantError(t, e.do("u-guest", http.MethodPost, azRegBase+"/connections", apiKeyResource("ro", "ro.openai.azure.com", "k")),
			http.StatusConflict, "REGISTRY_READ_ONLY")
		e.registry.ClearFailures()
		if left, _ := e.secrets.ListFamily("u-guest", "azure-foundry"); len(left) != 0 {
			t.Errorf("a refused create left credentials behind: %v", left)
		}
		e.registry.FailOn("ListConnections", errors.New("the database is down"))
		env := azRegWantError(t, e.do(user, http.MethodGet, azRegBase+"/connections", nil), http.StatusInternalServerError, "INTERNAL_ERROR")
		if env.Message != "internal server error" {
			t.Errorf("a store failure's message = %q, want the generic copy", env.Message)
		}
		e.registry.ClearFailures()
		e.wire(func(d *testAzureModelsDeps) {
			d.Secrets = azRegFailingSecrets{FakeSecretStore: e.secrets, err: storage.ErrSecretStorageUnavailable}
		})
		azRegWantError(t, e.do("u-guest", http.MethodPost, azRegBase+"/connections", apiKeyResource("ks", "ks.openai.azure.com", "k")),
			http.StatusServiceUnavailable, "SECRET_STORAGE_UNAVAILABLE")
	})
}

// registryState reports the desktop list file's state (ok, read_only, recovered with the copy's path).
func TestAzureRegistry_RegistryState(t *testing.T) {
	e := newAzRegEnv(t)
	listing := func() service.AzureConnectionsListing {
		rr := e.do("u-member", http.MethodGet, azRegBase+"/connections", nil)
		checkStatus(t, rr, http.StatusOK)
		var out service.AzureConnectionsListing
		azRegDecode(t, rr, &out)
		return out
	}
	if got := listing(); got.RegistryState != "ok" || got.RecoveredCopyPath != "" {
		t.Errorf("default state = %q (%q)", got.RegistryState, got.RecoveredCopyPath)
	}
	e.registry.SetStatus(storageif.AzureRegistryStatus{State: storageif.AzureRegistryReadOnly})
	if got := listing(); got.RegistryState != "read_only" {
		t.Errorf("state = %q, want read_only", got.RegistryState)
	}
	e.registry.SetStatus(storageif.AzureRegistryStatus{State: storageif.AzureRegistryRecovered, QuarantinePath: "/cfg/azure-models.json.corrupt-1"})
	if got := listing(); got.RegistryState != "recovered" || got.RecoveredCopyPath != "/cfg/azure-models.json.corrupt-1" {
		t.Errorf("state = %q (%q), want recovered with the copy's path", got.RegistryState, got.RecoveredCopyPath)
	}
}

// The knowledge-base designation: on a non-empty corpus a change is 409 REINDEX_REQUIRED until force confirms it.
func TestAzureRegistry_KnowledgeBaseReindex(t *testing.T) {
	e := newAzRegEnv(t)
	e.wire(func(d *testAzureModelsDeps) {
		d.KnowledgeEmbedding = true
		d.Corpus = azRegCorpus{chunks: 5}
	})
	const user = "u-admin" // administers Acme, which has no resources: their model embeds its corpus
	rr := e.do(user, http.MethodGet, azRegBase+"/connections", nil)
	var listing service.AzureConnectionsListing
	azRegDecode(t, rr, &listing)
	if !listing.KnowledgeEmbeddingEnabled {
		t.Error("knowledgeEmbeddingEnabled = false, want true")
	}
	conn := e.createConnection(user, azRegBase, apiKeyResource("res", "res.openai.azure.com", "k"))
	emb := e.createModel(user, azRegBase, map[string]any{"connectionId": conn.ID, "kind": "embedding", "deployment": "text-emb"})

	azRegWantError(t, e.do(user, http.MethodPut, azRegBase+"/models/"+emb.ID, map[string]any{"isKnowledgeEmbedding": true}), http.StatusConflict, "REINDEX_REQUIRED")
	rr = e.do(user, http.MethodPut, azRegBase+"/models/"+emb.ID, map[string]any{"isKnowledgeEmbedding": true, "force": true})
	checkStatus(t, rr, http.StatusOK)
	var updated service.AzureModelEnvelope
	azRegDecode(t, rr, &updated)
	if !updated.Model.IsKnowledgeEmbedding {
		t.Errorf("forced designation = %+v", updated.Model)
	}
	azRegWantError(t, e.do(user, http.MethodDelete, azRegBase+"/models/"+emb.ID, nil), http.StatusConflict, "REINDEX_REQUIRED")
	azRegWantError(t, e.do(user, http.MethodDelete, azRegBase+"/connections/"+conn.ID+"?cascade=true", nil), http.StatusConflict, "REINDEX_REQUIRED")
	rr = e.do(user, http.MethodDelete, azRegBase+"/connections/"+conn.ID+"?cascade=true&force=true", nil)
	checkStatus(t, rr, http.StatusOK)
	var deleted service.AzureConnectionDeleteResult
	azRegDecode(t, rr, &deleted)
	if deleted.DeletedModels != 1 || !deleted.ClearedKnowledgeEmbedding {
		t.Errorf("forced delete = %+v, want the knowledge-base model cleared", deleted)
	}
}

// --- Organisations ---

// The org role matrix: reads need an admin or a member, writes/tests/discovery an admin; viewers,
// guests and non-members get 403 — and a refused write changes nothing.
func TestAzureRegistry_OrgRoleMatrix(t *testing.T) {
	e := newAzRegEnv(t)
	base := e.orgBase()
	conn := e.createConnection("u-admin", base, apiKeyResource("shared", "shared-res.openai.azure.com", "org-key"))
	model := e.createModel("u-admin", base, map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": "gpt-4.1"})

	reads := []string{base + "/connections", base + "/connections/" + conn.ID, base + "/models", base + "/models/" + model.ID}
	writes := []struct {
		method, path string
		body         any
	}{
		{http.MethodPost, base + "/connections", apiKeyResource("other", "other.openai.azure.com", "k")},
		{http.MethodPut, base + "/connections/" + conn.ID, map[string]any{"name": "renamed"}},
		{http.MethodDelete, base + "/connections/" + conn.ID + "?cascade=true", nil},
		{http.MethodPost, base + "/connections/" + conn.ID + "/discover", nil},
		{http.MethodPost, base + "/models", map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": "other"}},
		{http.MethodPut, base + "/models/" + model.ID, map[string]any{"displayName": "renamed"}},
		{http.MethodDelete, base + "/models/" + model.ID, nil},
		{http.MethodPost, base + "/test", map[string]any{"connectionId": conn.ID}},
		{http.MethodPost, base + "/test", nil},
	}
	for _, caller := range []string{"u-member", "u-viewer", "u-guest", "u-stranger"} {
		for _, w := range writes {
			if rr := e.do(caller, w.method, w.path, w.body); rr.Code != http.StatusForbidden {
				t.Errorf("%s: %s %s = %d, want 403 (body %s)", caller, w.method, w.path, rr.Code, rr.Body.String())
			}
		}
	}
	for caller, want := range map[string]int{"u-admin": 200, "u-member": 200, "u-viewer": 403, "u-guest": 403, "u-stranger": 403} {
		for _, path := range reads {
			if rr := e.do(caller, http.MethodGet, path, nil); rr.Code != want {
				t.Errorf("%s: GET %s = %d, want %d (body %s)", caller, path, rr.Code, want, rr.Body.String())
			}
		}
	}
	// The refusal's shape is the org config routes': 403 FORBIDDEN.
	azRegWantError(t, e.do("u-viewer", http.MethodGet, base+"/models", nil), http.StatusForbidden, "FORBIDDEN")
	// An unknown organisation: a read is refused like any non-member's, a
	// write is 404 (canOrg's OrgAdmin answer, as on the org config routes).
	azRegWantError(t, e.do("u-admin", http.MethodGet, "/api/orgs/nope/providers/azure-foundry/connections", nil), http.StatusForbidden, "FORBIDDEN")
	checkStatus(t, e.do("u-admin", http.MethodPost, "/api/orgs/nope/providers/azure-foundry/connections", apiKeyResource("x", "x.openai.azure.com", "k")), http.StatusNotFound)

	// Nothing a refused write asked for happened.
	rr := e.do("u-admin", http.MethodGet, base+"/connections", nil)
	var listing service.AzureConnectionsListing
	azRegDecode(t, rr, &listing)
	if len(listing.Connections) != 1 || listing.Connections[0].Name != "shared" || listing.Connections[0].ModelCount != 1 {
		t.Errorf("org resources after the refused writes = %+v", listing.Connections)
	}

	// The admin does all of it (discovery is answered — an API key cannot
	// list a project — not refused).
	checkStatus(t, e.do("u-admin", http.MethodPut, base+"/connections/"+conn.ID, map[string]any{"name": "renamed"}), http.StatusOK)
	azRegWantError(t, e.do("u-admin", http.MethodPost, base+"/connections/"+conn.ID+"/discover", nil), http.StatusBadRequest, "DISCOVERY_UNAVAILABLE")
	checkStatus(t, e.do("u-admin", http.MethodPut, base+"/models/"+model.ID, map[string]any{"isDefault": true}), http.StatusOK)
	checkStatus(t, e.do("u-admin", http.MethodDelete, base+"/models/"+model.ID, nil), http.StatusOK)
	checkStatus(t, e.do("u-admin", http.MethodDelete, base+"/connections/"+conn.ID, nil), http.StatusOK)

	for _, caller := range []string{"u-admin", "u-member", "u-viewer", "u-guest", "u-stranger"} {
		checkStatus(t, e.do(caller, http.MethodGet, azRegBase+"/connections", nil), http.StatusOK)
	}
}

// In user scope the listings carry the shared entries of every org where the caller is an admin or
// member (read-only, with orgId/orgName); viewers, guests and outsiders see none.
func TestAzureRegistry_SharedWithYou(t *testing.T) {
	e := newAzRegEnv(t)
	orgConn := e.createConnection("u-admin", e.orgBase(), apiKeyResource("shared", "shared-res.openai.azure.com", "org-key"))
	orgModel := e.createModel("u-admin", e.orgBase(), map[string]any{"connectionId": orgConn.ID, "kind": "chat", "deployment": "gpt-4.1", "isDefault": true})
	if !orgModel.IsDefault || !orgModel.Shared || orgModel.OrgID != e.orgID || orgModel.OrgName != "Acme" {
		t.Errorf("org model = %+v, want shared, the default for members", orgModel)
	}
	own := e.createConnection("u-member", azRegBase, apiKeyResource("mine", "mine.openai.azure.com", "k"))

	for caller, wantShared := range map[string]bool{"u-admin": true, "u-member": true, "u-viewer": false, "u-guest": false, "u-stranger": false} {
		rr := e.do(caller, http.MethodGet, azRegBase+"/connections", nil)
		var conns service.AzureConnectionsListing
		azRegDecode(t, rr, &conns)
		var shared []service.AzureConnectionView
		for _, c := range conns.Connections {
			if c.Shared {
				shared = append(shared, c)
			}
		}
		if wantShared != (len(shared) == 1) {
			t.Errorf("%s: shared resources = %+v, want shared=%v", caller, shared, wantShared)
		}
		if wantShared && (shared[0].ID != orgConn.ID || shared[0].OrgID != e.orgID || shared[0].OrgName != "Acme" || shared[0].SuggestDiscovery) {
			t.Errorf("%s: shared resource = %+v", caller, shared[0])
		}
		rr = e.do(caller, http.MethodGet, azRegBase+"/models", nil)
		var models service.AzureModelsListing
		azRegDecode(t, rr, &models)
		sawShared := false
		for _, m := range models.Models {
			sawShared = sawShared || (m.Shared && m.ID == orgModel.ID && m.OrgName == "Acme")
		}
		if sawShared != wantShared {
			t.Errorf("%s: shared model listed = %v, want %v (%+v)", caller, sawShared, wantShared, models.Models)
		}
	}
	rr := e.do("u-member", http.MethodGet, azRegBase+"/connections", nil)
	var conns service.AzureConnectionsListing
	azRegDecode(t, rr, &conns)
	if len(conns.Connections) != 2 || conns.Connections[0].ID != own.ID || conns.Connections[0].Shared {
		t.Errorf("member's listing = %+v, want their own resource then the shared one", conns.Connections)
	}
	rr = e.do("u-member", http.MethodGet, e.orgBase()+"/connections", nil)
	azRegDecode(t, rr, &conns)
	if len(conns.Connections) != 1 || !conns.Connections[0].Shared || conns.Connections[0].CredentialUpdatedAt == "" || conns.Connections[0].SharedBy != "u-admin" {
		t.Errorf("org listing = %+v", conns.Connections)
	}
	// A member cannot reach the shared resource through their own scope.
	azRegWantError(t, e.do("u-member", http.MethodPut, azRegBase+"/connections/"+orgConn.ID, map[string]any{"name": "mine now"}), http.StatusNotFound, "CONNECTION_NOT_FOUND")
}

// Every org write is audited on the organisation, with the ids in the meta and no secret; a demoted
// lender's resource credential is disabled per key and the resource then reads needs_credentials.
func TestAzureRegistry_OrgWritesAuditedAndLenderDisable(t *testing.T) {
	e := newAzRegEnv(t)
	addOrgMember(t, e.rt, e.orgID, "u-admin2", auth.RoleAdmin)
	ch := captureAudit(t)
	base := e.orgBase()

	conn := e.createConnection("u-admin2", base, entraResource("lent", "lent.services.ai.azure.com", "lender-secret"))
	assertAzRegAudit(t, awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureConnectionSave)), "org", e.orgID,
		map[string]string{"op": "create", "connectionId": conn.ID, "authMode": "entra"})
	model := e.createModel("u-admin2", base, map[string]any{"connectionId": conn.ID, "kind": "agent", "deployment": "helper", "description": "private note"})
	assertAzRegAudit(t, awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureModelSave)), "org", e.orgID,
		map[string]string{"op": "create", "modelId": model.ID, "connectionId": conn.ID, "kind": "agent", "deployment": "helper"})
	rr := e.do("u-admin", http.MethodPut, base+"/connections/"+conn.ID, map[string]any{"dismissDiscovery": true})
	checkStatus(t, rr, http.StatusOK)
	assertAzRegAudit(t, awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureConnectionSave)), "org", e.orgID,
		map[string]string{"op": "update", "connectionId": conn.ID, "dismissDiscovery": "true"})

	// The lender is demoted: their resource's credential goes (one keys.save disable row naming the key),
	// the reply says so, and the resource and its model then read needs_credentials.
	rr = e.do("u-admin", http.MethodPut, "/api/orgs/"+e.orgID+"/members/u-admin2/role", map[string]string{"role": "member"})
	checkStatus(t, rr, http.StatusOK)
	if !decodeMutationFlag(t, rr) {
		t.Error("the lender's demotion did not report the shared credential disabled")
	}
	ev := awaitAudit(t, ch, func(ev *storageif.AuditEvent) bool {
		return ev.Action == audit.ActionKeysSave && ev.Meta["op"] == "disable"
	})
	if ev.ResourceID != e.orgID || ev.Meta["provider"] != "azure-foundry/"+conn.ID || ev.Meta["connectionId"] != conn.ID ||
		ev.Meta["reason"] != lenderReasonDemoted || ev.Meta["user"] != "u-admin2" {
		t.Errorf("disable row = %+v (meta %v)", ev, ev.Meta)
	}
	got := e.connection("u-member", base, conn.ID)
	if got.Status != service.AzureStatusNeedsCredentials || got.StatusReason == "" || got.ModelCount != 1 {
		t.Errorf("the disabled resource = %+v, want needs_credentials with its model", got)
	}

	checkStatus(t, e.do("u-admin", http.MethodDelete, base+"/models/"+model.ID, nil), http.StatusOK)
	assertAzRegAudit(t, awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureModelDelete)), "org", e.orgID, map[string]string{"op": "delete", "modelId": model.ID})
	checkStatus(t, e.do("u-admin", http.MethodDelete, base+"/connections/"+conn.ID, nil), http.StatusOK)
	assertAzRegAudit(t, awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureConnectionDelete)), "org", e.orgID,
		map[string]string{"op": "delete", "connectionId": conn.ID, "deletedModels": "0"})
}

// --- The /test route and discovery (a fake Azure AI Foundry) ---

// The one /test route across saved and draft resources and models: a resource test never calls a
// model, a model test makes exactly one call, an agent test only looks the agent up (never a run);
// every outcome is a 200, saved targets record their last test, drafts nothing.
func TestAzureRegistry_TestRoute(t *testing.T) {
	e := newAzRegEnv(t)
	f := e.foundry
	const user = "u-stranger"
	const res1, res2 = "res1" + azRegHostSuffix, "res2" + azRegHostSuffix
	keyRes := e.createConnection(user, azRegBase, apiKeyResource("res1", res1, "res1-key"))
	entraRes := e.createConnection(user, azRegBase, entraResource("res2", res2, "res2-secret"))
	chat := e.createModel(user, azRegBase, map[string]any{"connectionId": keyRes.ID, "kind": "chat", "deployment": "gpt-4.1"})
	agent := e.createModel(user, azRegBase, map[string]any{"connectionId": entraRes.ID, "kind": "agent", "deployment": "helper"})
	postsTo := func(host string) []string {
		var out []string
		for _, c := range f.callsTo(host) {
			if c.Method != http.MethodGet {
				out = append(out, c.String())
			}
		}
		return out
	}

	t.Run("saved resource: an authenticated probe, no model call", func(t *testing.T) {
		f.reset()
		res := e.test(user, azRegBase, map[string]any{"connectionId": keyRes.ID})
		if !res.OK || res.Code != "" || res.LatencyMs < 0 {
			t.Errorf("result = %+v, want ok", res)
		}
		calls := f.callsTo(res1)
		if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/openai/v1/models" || calls[0].APIKey != "res1-key" {
			t.Errorf("calls = %v, want one GET /openai/v1/models with the stored key", calls)
		}
		if lt := e.connection(user, azRegBase, keyRes.ID).LastTest; lt == nil || !lt.OK || lt.At == "" {
			t.Errorf("lastTest = %+v, want the passed test recorded", lt)
		}
	})
	t.Run("saved resource refused: 200 with the code, recorded", func(t *testing.T) {
		f.reset()
		f.mu.Lock()
		f.modelsStatus = http.StatusUnauthorized
		f.mu.Unlock()
		defer func() { f.mu.Lock(); f.modelsStatus = 0; f.mu.Unlock() }()
		res := e.test(user, azRegBase, map[string]any{"connectionId": keyRes.ID})
		if res.OK || res.Code != "INVALID_API_KEY" || res.Error == "" {
			t.Errorf("result = %+v, want INVALID_API_KEY", res)
		}
		if lt := e.connection(user, azRegBase, keyRes.ID).LastTest; lt == nil || lt.OK || lt.Error == "" {
			t.Errorf("lastTest = %+v, want the failed test recorded", lt)
		}
	})
	t.Run("draft resource: the draft's key, nothing recorded", func(t *testing.T) {
		f.reset()
		before := e.connection(user, azRegBase, keyRes.ID).LastTest
		body := apiKeyResource("", res1, "draft-key")
		res := e.test(user, azRegBase, map[string]any{"config": body["config"]})
		if !res.OK {
			t.Errorf("result = %+v", res)
		}
		if calls := f.callsTo(res1); len(calls) != 1 || calls[0].APIKey != "draft-key" || calls[0].Path != "/openai/v1/models" {
			t.Errorf("calls = %v, want one probe with the draft's key", calls)
		}
		if after := e.connection(user, azRegBase, keyRes.ID).LastTest; after == nil || before == nil || after.At != before.At {
			t.Errorf("a draft test changed the saved resource's lastTest: %+v → %+v", before, after)
		}
	})
	t.Run("draft over a saved resource: the stored secret carries over", func(t *testing.T) {
		f.reset()
		res := e.test(user, azRegBase, map[string]any{"connectionId": keyRes.ID,
			"config": map[string]any{"endpoint": "https://" + res1, "auth": map[string]any{"mode": "api_key"}}})
		if !res.OK {
			t.Errorf("result = %+v", res)
		}
		if calls := f.callsTo(res1); len(calls) != 1 || calls[0].APIKey != "res1-key" {
			t.Errorf("calls = %v, want the probe with the stored key", calls)
		}
		// Another host: the stored key does not follow — MISSING_SECRET, no call.
		f.reset()
		env := azRegWantError(t, e.do(user, http.MethodPost, azRegBase+"/test", map[string]any{"connectionId": keyRes.ID,
			"config": map[string]any{"endpoint": "https://elsewhere" + azRegHostSuffix, "auth": map[string]any{"mode": "api_key"}}}),
			http.StatusBadRequest, "MISSING_SECRET")
		if env.Details["field"] != "key" {
			t.Errorf("MISSING_SECRET details = %v, want field key", env.Details)
		}
		if calls := f.callsTo("elsewhere" + azRegHostSuffix); len(calls) != 0 {
			t.Errorf("a refused draft reached Azure: %v", calls)
		}
	})
	t.Run("saved chat model: one tiny chat call, recorded", func(t *testing.T) {
		f.reset()
		res := e.test(user, azRegBase, map[string]any{"model": map[string]any{"id": chat.ID}})
		if !res.OK {
			t.Errorf("result = %+v", res)
		}
		calls := f.callsTo(res1)
		if len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != "/openai/v1/chat/completions" || calls[0].Body["model"] != "gpt-4.1" {
			t.Fatalf("calls = %v, want one POST /openai/v1/chat/completions for gpt-4.1", calls)
		}
		if mt, ok := calls[0].Body["max_completion_tokens"]; !ok || mt != float64(16) {
			if mt2 := calls[0].Body["max_tokens"]; mt2 != float64(16) {
				t.Errorf("chat body = %v, want the output capped at 16 tokens", calls[0].Body)
			}
		}
		rr := e.do(user, http.MethodGet, azRegBase+"/models/"+chat.ID, nil)
		var got service.AzureModelEnvelope
		azRegDecode(t, rr, &got)
		if got.Model.LastTest == nil || !got.Model.LastTest.OK {
			t.Errorf("model lastTest = %+v, want the passed test recorded", got.Model.LastTest)
		}
	})
	t.Run("draft embedding model: one embedding call", func(t *testing.T) {
		f.reset()
		res := e.test(user, azRegBase, map[string]any{"connectionId": keyRes.ID, "model": map[string]any{"kind": "embedding", "deployment": "text-emb"}})
		if !res.OK {
			t.Errorf("result = %+v", res)
		}
		if calls := f.callsTo(res1); len(calls) != 1 || calls[0].Method != http.MethodPost || calls[0].Path != "/openai/v1/embeddings" {
			t.Errorf("calls = %v, want one POST /openai/v1/embeddings", calls)
		}
	})
	t.Run("agent: one GET of the agent, never a run", func(t *testing.T) {
		f.reset()
		res := e.test(user, azRegBase, map[string]any{"model": map[string]any{"id": agent.ID}})
		if !res.OK {
			t.Errorf("result = %+v", res)
		}
		calls := f.callsTo(res2)
		if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/api/projects/proj1/agents/helper" ||
			calls[0].Query.Get("api-version") != "v1" || calls[0].Bearer != "fake-entra-token" {
			t.Errorf("calls = %v, want exactly one signed-in GET {project}/agents/helper", calls)
		}
		if posts := postsTo(res2); len(posts) != 0 {
			t.Errorf("an agent test ran something: %v", posts)
		}
		// An agent the project does not have: 200, AGENT_NOT_FOUND, still no run.
		f.reset()
		res = e.test(user, azRegBase, map[string]any{"connectionId": entraRes.ID, "model": map[string]any{"kind": "agent", "deployment": "ghost"}})
		if res.OK || res.Code != "AGENT_NOT_FOUND" {
			t.Errorf("result = %+v, want AGENT_NOT_FOUND", res)
		}
		if posts := postsTo(res2); len(posts) != 0 {
			t.Errorf("an agent test ran something: %v", posts)
		}
	})
	t.Run("Entra ID resource: sign-in, the probe and the project — no model call", func(t *testing.T) {
		f.reset()
		res := e.test(user, azRegBase, map[string]any{"connectionId": entraRes.ID})
		if !res.OK {
			t.Errorf("result = %+v", res)
		}
		var paths []string
		for _, c := range f.callsTo(res2) {
			paths = append(paths, c.String())
			if c.Bearer != "fake-entra-token" {
				t.Errorf("%s went out without the Entra ID token", c)
			}
		}
		if len(paths) != 2 || paths[0] != "GET "+res2+"/openai/v1/models" || paths[1] != "GET "+res2+"/api/projects/proj1/deployments" {
			t.Errorf("calls = %v, want the models probe then the project probe", paths)
		}
		if len(f.callsTo(azRegAADHost)) == 0 {
			t.Error("no Entra ID sign-in happened")
		}
	})
	t.Run("a resource without credentials: 200 NEEDS_CREDENTIALS, no call", func(t *testing.T) {
		gone := e.createConnection(user, azRegBase, apiKeyResource("gone", "gone"+azRegHostSuffix, "k"))
		if err := e.secrets.Delete(user, "azure-foundry/"+gone.ID); err != nil {
			t.Fatal(err)
		}
		f.reset()
		res := e.test(user, azRegBase, map[string]any{"connectionId": gone.ID})
		if res.OK || res.Code != "NEEDS_CREDENTIALS" || res.Error == "" {
			t.Errorf("result = %+v, want NEEDS_CREDENTIALS", res)
		}
		if calls := f.callsTo("gone" + azRegHostSuffix); len(calls) != 0 {
			t.Errorf("calls = %v, want none", calls)
		}
	})
	t.Run("invalid requests: 4xx", func(t *testing.T) {
		f.reset()
		azRegWantError(t, e.do(user, http.MethodPost, azRegBase+"/test", map[string]any{"connectionId": "zzzzzzzzzz"}), http.StatusNotFound, "CONNECTION_NOT_FOUND")
		azRegWantError(t, e.do(user, http.MethodPost, azRegBase+"/test", map[string]any{"model": map[string]any{"id": "zzzzzzzzzz"}}), http.StatusNotFound, "MODEL_NOT_FOUND")
		azRegWantError(t, e.do(user, http.MethodPost, azRegBase+"/test", map[string]any{"connectionId": entraRes.ID, "model": map[string]any{"id": chat.ID}}), http.StatusNotFound, "MODEL_NOT_FOUND")
		azRegWantError(t, e.do(user, http.MethodPost, azRegBase+"/test", map[string]any{"config": apiKeyResource("", "evil.example", "k")["config"]}), http.StatusBadRequest, "HOST_NOT_ALLOWED")
		azRegWantError(t, e.do(user, http.MethodPost, azRegBase+"/test", map[string]any{"connectionId": keyRes.ID, "model": map[string]any{"kind": "bogus", "deployment": "d"}}), http.StatusBadRequest, "INVALID_KIND")
		azRegWantError(t, e.do(user, http.MethodPost, azRegBase+"/test", map[string]any{}), http.StatusBadRequest, "INVALID_CONFIG")
		for _, host := range []string{res1, res2, "evil.example"} {
			if calls := f.callsTo(host); len(calls) != 0 {
				t.Errorf("an invalid request reached %s: %v", host, calls)
			}
		}
	})
	t.Run("another user's saved targets are not found", func(t *testing.T) {
		azRegWantError(t, e.do("u-member", http.MethodPost, azRegBase+"/test", map[string]any{"connectionId": keyRes.ID}), http.StatusNotFound, "CONNECTION_NOT_FOUND")
		azRegWantError(t, e.do("u-member", http.MethodPost, azRegBase+"/test", map[string]any{"model": map[string]any{"id": chat.ID}}), http.StatusNotFound, "MODEL_NOT_FOUND")
	})
}

// An organisation's test runs on the organisation's credential and is audited (providers.test); the
// deprecated body-less org test probes resource "default" — no chat call — and answers the legacy shape.
func TestAzureRegistry_OrgTest(t *testing.T) {
	e := newAzRegEnv(t)
	f := e.foundry
	ch := captureAudit(t)
	const orgHost, legacyHost = "orgres" + azRegHostSuffix, "legacy" + azRegHostSuffix
	conn := e.createConnection("u-admin", e.orgBase(), apiKeyResource("orgres", orgHost, "org-key"))
	_ = awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureConnectionSave))

	res := e.test("u-admin", e.orgBase(), map[string]any{"connectionId": conn.ID})
	if !res.OK {
		t.Errorf("org resource test = %+v", res)
	}
	if calls := f.callsTo(orgHost); len(calls) != 1 || calls[0].APIKey != "org-key" || calls[0].Path != "/openai/v1/models" {
		t.Errorf("calls = %v, want one probe with the org's key", calls)
	}
	ev := awaitAudit(t, ch, isAzRegAudit(audit.ActionProvidersTest))
	assertAzRegAudit(t, ev, "org", e.orgID, map[string]string{"provider": "azure-foundry", "ok": "true", "connectionId": conn.ID})

	// A draft: its auth mode and model are audited, never its settings.
	res = e.test("u-admin", e.orgBase(), map[string]any{"config": apiKeyResource("", orgHost, "draft-secret-key")["config"],
		"model": map[string]any{"kind": "chat", "deployment": "gpt-4.1"}})
	if !res.OK {
		t.Errorf("org draft test = %+v", res)
	}
	ev = awaitAudit(t, ch, isAzRegAudit(audit.ActionProvidersTest))
	assertAzRegAudit(t, ev, "org", e.orgID, map[string]string{"draft": "true", "authMode": "api_key", "kind": "chat", "deployment": "gpt-4.1"})
	if _, has := ev.Meta["endpoint"]; has {
		t.Errorf("the audit row carries the draft's endpoint: %v", ev.Meta)
	}

	// The deprecated body-less test: resource "default" probed as a resource.
	checkStatus(t, e.do("u-admin", http.MethodPost, e.orgBase()+"/config", map[string]any{
		"endpoint": "https://" + legacyHost, "auth": map[string]any{"mode": "api_key", "key": "legacy-key"}, "chatDeployment": "org-chat",
	}), http.StatusOK)
	f.reset()
	rr := e.do("u-admin", http.MethodPost, e.orgBase()+"/test", nil)
	checkStatus(t, rr, http.StatusOK)
	var legacy map[string]any
	azRegDecode(t, rr, &legacy)
	if legacy["ok"] != true {
		t.Errorf("deprecated org test = %v", legacy)
	}
	if _, has := legacy["code"]; has {
		t.Errorf("the deprecated form answers the legacy shape, got %v", legacy)
	}
	calls := f.callsTo(legacyHost)
	if len(calls) != 1 || calls[0].Method != http.MethodGet || calls[0].Path != "/openai/v1/models" || calls[0].APIKey != "legacy-key" {
		t.Errorf("calls = %v, want one resource probe of the default resource (no chat call)", calls)
	}
	ev = awaitAudit(t, ch, func(ev *storageif.AuditEvent) bool {
		return ev.Action == audit.ActionProvidersTest && ev.Meta["ok"] == "true"
	})
	if ev.ResourceID != e.orgID || ev.Meta["provider"] != "azure-foundry" {
		t.Errorf("deprecated test audit = %+v (meta %v)", ev, ev.Meta)
	}
}

// Discovery lists a saved Entra ID resource's project — deployments with their kind, the runnable
// agents — marking what the resource already registers; an API-key resource cannot (no call).
func TestAzureRegistry_Discover(t *testing.T) {
	e := newAzRegEnv(t)
	f := e.foundry
	const user = "u-stranger"
	const host = "disc" + azRegHostSuffix
	conn := e.createConnection(user, azRegBase, entraResource("disc", host, "disc-secret"))
	added := e.createModel(user, azRegBase, map[string]any{"connectionId": conn.ID, "kind": "chat", "deployment": "gpt-4.1"})

	rr := e.do(user, http.MethodPost, "/api/v1/providers/azure-foundry/connections/"+conn.ID+"/discover", nil)
	checkStatus(t, rr, http.StatusOK)
	var view service.AzureDiscoveryView
	azRegDecode(t, rr, &view)
	if view.Error != "" || len(view.Deployments) != 2 || len(view.Agents) != 1 {
		t.Fatalf("discovery = %+v", view)
	}
	byName := map[string]service.AzureDiscoveryItemView{}
	for _, d := range view.Deployments {
		byName[d.Name] = d
	}
	if d := byName["gpt-4.1"]; d.Kind != "chat" || !d.AlreadyAdded || d.ModelID != added.ModelID {
		t.Errorf("gpt-4.1 = %+v, want an added chat deployment", d)
	}
	if d := byName["text-emb"]; d.Kind != "embedding" || d.AlreadyAdded {
		t.Errorf("text-emb = %+v, want an embedding deployment not added", d)
	}
	if a := view.Agents[0]; a.Name != "helper" || a.Kind != "agent" || a.AlreadyAdded {
		t.Errorf("agent = %+v", a)
	}
	for _, c := range f.callsTo(host) {
		if c.Method != http.MethodGet {
			t.Errorf("discovery sent %s — it only lists", c)
		}
	}

	keyConn := e.createConnection(user, azRegBase, apiKeyResource("key", "keyres"+azRegHostSuffix, "k"))
	f.reset()
	azRegWantError(t, e.do(user, http.MethodPost, azRegBase+"/connections/"+keyConn.ID+"/discover", nil), http.StatusBadRequest, "DISCOVERY_UNAVAILABLE")
	azRegWantError(t, e.do(user, http.MethodPost, azRegBase+"/connections/zzzzzzzzzz/discover", nil), http.StatusNotFound, "CONNECTION_NOT_FOUND")
	if calls := f.callsTo("keyres" + azRegHostSuffix); len(calls) != 0 {
		t.Errorf("an unavailable discovery reached Azure: %v", calls)
	}
}

// --- The deprecated key routes on the registry ---

// /api/keys/has answers "any resource of the caller's own"; /api/keys/delete removes resource
// "default" with its models and credential, leaving the other resources; /api/providers/test probes it.
func TestAzureRegistry_DeprecatedKeyRoutes(t *testing.T) {
	e := newAzRegEnv(t)
	f := e.foundry
	const user = "u-stranger"
	const legacyHost = "legacy" + azRegHostSuffix
	has := func() bool {
		t.Helper()
		rr := e.do(user, http.MethodPost, "/api/keys/has", map[string]string{"provider": "azure-foundry"})
		checkStatus(t, rr, http.StatusOK)
		var out map[string]bool
		azRegDecode(t, rr, &out)
		return out["has"]
	}
	if has() {
		t.Fatal("has = true before any resource")
	}
	other := e.createConnection(user, azRegBase, apiKeyResource("other", "other.openai.azure.com", "k"))
	if !has() {
		t.Error("has = false with a resource of the caller's own")
	}

	checkStatus(t, e.do(user, http.MethodPost, azRegBase+"/config", map[string]any{
		"endpoint": "https://" + legacyHost, "auth": map[string]any{"mode": "api_key", "key": "legacy-key"}, "chatDeployment": "legacy-chat",
	}), http.StatusOK)
	if def := e.connection(user, azRegBase, "default"); !def.Legacy || def.ModelCount != 1 || def.Name != "legacy" {
		t.Errorf("resource default = %+v", def)
	}

	f.reset()
	rr := e.do(user, http.MethodPost, "/api/providers/test", map[string]string{"provider": "azure-foundry"})
	checkStatus(t, rr, http.StatusOK)
	if got := azureDecode(t, rr); got["ok"] != true {
		t.Errorf("deprecated provider test = %v", got)
	}
	if calls := f.callsTo(legacyHost); len(calls) != 1 || calls[0].Path != "/openai/v1/models" {
		t.Errorf("calls = %v, want one resource probe", calls)
	}

	checkStatus(t, e.do(user, http.MethodPost, "/api/keys/delete", map[string]string{"provider": "azure-foundry"}), http.StatusOK)
	azRegWantError(t, e.do(user, http.MethodGet, azRegBase+"/connections/default", nil), http.StatusNotFound, "CONNECTION_NOT_FOUND")
	if stored, _ := e.secrets.Has(user, "azure-foundry"); stored {
		t.Error("resource default's credential is still stored")
	}
	rr = e.do(user, http.MethodGet, azRegBase+"/models", nil)
	var models service.AzureModelsListing
	azRegDecode(t, rr, &models)
	if len(models.Models) != 0 {
		t.Errorf("models after the delete = %+v, want none", models.Models)
	}
	if got := e.connection(user, azRegBase, other.ID); got.ID != other.ID {
		t.Errorf("the other resource = %+v, want it kept", got)
	}
	if !has() {
		t.Error("has = false while another resource remains")
	}
}

// --- Rate-limit buckets and the RLS exemption ---

// The outbound registry routes — /test (the chat bucket) and /connections/{id}/discover (the
// analysis bucket), the caller's own and an organisation's — are classified whatever spelling
// reaches the router (the /api/v1 alias, dot segments, doubled slashes) and run outside the RLS
// transaction; the registry's CRUD routes stay on the general bucket and inside it.
func TestAzureOutboundRoute_Classification(t *testing.T) {
	outbound := []struct {
		path, group string
	}{
		{"/api/providers/azure-foundry/test", rlGroupChat},
		{"/api/v1/providers/azure-foundry/test", rlGroupChat},
		{"/api/providers/./azure-foundry/test", rlGroupChat},
		{"/api/providers//azure-foundry/test", rlGroupChat},
		{"/api/providers/azure-foundry/x/../test", rlGroupChat},
		{"/api/orgs/o1/providers/azure-foundry/test", rlGroupChat},
		{"/api/v1/orgs/o1/providers/azure-foundry/test", rlGroupChat},
		{"/api/orgs/o1/../o1/providers/azure-foundry/test", rlGroupChat},
		{"/api/providers/azure-foundry/connections/abcdefghij/discover", rlGroupAnalysis},
		{"/api/v1/providers/azure-foundry/connections/abcdefghij/discover", rlGroupAnalysis},
		{"/api/providers/azure-foundry/connections/./abcdefghij/discover", rlGroupAnalysis},
		{"/api/orgs/o1/providers/azure-foundry/connections/default/discover", rlGroupAnalysis},
		{"/api/v1/orgs/o1/providers/azure-foundry/connections/default/discover", rlGroupAnalysis},
		{"/api/orgs/o1/./providers/azure-foundry/connections/default/discover", rlGroupAnalysis},
	}
	for _, c := range outbound {
		if g := rateLimitGroup(http.MethodPost, &url.URL{Path: c.path}); g != c.group {
			t.Errorf("rateLimitGroup(POST %s) = %q, want %q", c.path, g, c.group)
		}
		// The RLS middleware runs on the canonical path (the router has
		// cleaned it and stripped the alias by then).
		if !rlsExemptPath(route.CanonicalPath(c.path)) {
			t.Errorf("rlsExemptPath(%s) = false, want the outbound route exempt", route.CanonicalPath(c.path))
		}
	}
	notOutbound := []struct{ method, path string }{
		{http.MethodGet, "/api/providers/azure-foundry/connections"},
		{http.MethodPost, "/api/providers/azure-foundry/connections"},
		{http.MethodPut, "/api/providers/azure-foundry/connections/abcdefghij"},
		{http.MethodDelete, "/api/providers/azure-foundry/connections/abcdefghij"},
		{http.MethodPost, "/api/providers/azure-foundry/models"},
		{http.MethodPut, "/api/providers/azure-foundry/models/abcdefghij"},
		{http.MethodPost, "/api/providers/azure-foundry/config"},
		{http.MethodPost, "/api/orgs/o1/providers/azure-foundry/connections"},
		{http.MethodDelete, "/api/orgs/o1/providers/azure-foundry/models/abcdefghij"},
		// Near misses.
		{http.MethodPost, "/api/providers/azure-foundry/connections/discover"},
		{http.MethodPost, "/api/providers/azure-foundry/connections/a/b/discover"},
		{http.MethodPost, "/api/providers/azure-foundry/test/x"},
		{http.MethodPost, "/api/orgs//providers/azure-foundry/test"},
		{http.MethodPost, "/api/orgs/o1/x/providers/azure-foundry/connections/a/discover"},
		{http.MethodPost, "/api/providers/azure-foundryX/test"},
	}
	for _, c := range notOutbound {
		if g := rateLimitGroup(c.method, &url.URL{Path: c.path}); g != rlGroupGeneral {
			t.Errorf("rateLimitGroup(%s %s) = %q, want general", c.method, c.path, g)
		}
		if p := route.CanonicalPath(c.path); rlsExemptPath(p) {
			t.Errorf("rlsExemptPath(%s) = true, want the route inside the RLS transaction", p)
		}
	}
	for _, p := range []string{"/api/providers/azure-foundry/test", "/api/orgs/o1/providers/azure-foundry/connections/abcdefghij/discover"} {
		if _, ok := azureOutboundRoute(p); !ok {
			t.Errorf("azureOutboundRoute(%s) = false", p)
		}
	}
}

// azRegRestoreFailingOrgKeys is the fake org keystore whose restores fail.
type azRegRestoreFailingOrgKeys struct {
	*testutil.FakeOrgSecretStore
}

func (azRegRestoreFailingOrgKeys) RestoreOrgIfUnchanged(string, string, time.Time, *storageif.OrgKeySnapshot) (bool, error) {
	return false, errors.New("org keystore: connection reset")
}

// A PUT whose registry write fails after the resource's new credential was saved: when the previous
// credential goes back nothing changed and nothing is audited; when it cannot, the change stands —
// the new secret and its new lender — and is audited, marked as a failed request.
func TestAzureRegistry_FailedUpdateThatKeptTheNewCredentialIsAudited(t *testing.T) {
	e := newAzRegEnv(t)
	ch := captureAudit(t)
	host := "shared" + azRegHostSuffix
	conn := e.createConnection("u-admin", e.orgBase(), apiKeyResource("team", host, "k1"))
	_ = awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureConnectionSave))
	body := map[string]any{
		"config":              map[string]any{"endpoint": "https://" + host, "auth": map[string]any{"mode": "api_key", "key": "k2"}},
		"credentialUpdatedAt": conn.CredentialUpdatedAt,
	}
	key := ai.AzureConnectionKey(conn.ID)

	e.registry.FailOn("UpdateConnection", storageif.ErrAzureRegistryConflict)
	azRegWantError(t, e.do("u-admin", http.MethodPut, e.orgBase()+"/connections/"+conn.ID, body), http.StatusConflict, service.AzureCodeConflict)
	noAudit(t, ch, isAzRegAudit(audit.ActionAzureConnectionSave))
	if raw, _ := e.orgKeys.GetOrg(e.orgID, key); !strings.Contains(raw, `"key":"k1"`) {
		t.Errorf("the credential did not go back: %s", raw)
	}

	e.wire(func(d *testAzureModelsDeps) { d.OrgKeys = azRegRestoreFailingOrgKeys{e.orgKeys} })
	azRegWantError(t, e.do("u-admin", http.MethodPut, e.orgBase()+"/connections/"+conn.ID, body), http.StatusConflict, service.AzureCodeConflict)
	ev := awaitAudit(t, ch, isAzRegAudit(audit.ActionAzureConnectionSave))
	assertAzRegAudit(t, ev, "org", e.orgID, map[string]string{
		"op": "update", "connectionId": conn.ID, "credentialChanged": "true",
		"outcome": "failed_after_credential_change", "authMode": "api_key",
	})
	if raw, _ := e.orgKeys.GetOrg(e.orgID, key); !strings.Contains(raw, `"key":"k2"`) {
		t.Errorf("the audit row must describe the store: %s", raw)
	}
}

// azRegDeleteFailingSecrets is the fake personal keystore whose deletes fail.
type azRegDeleteFailingSecrets struct {
	*testutil.FakeSecretStore
}

func (azRegDeleteFailingSecrets) Delete(string, string) error {
	return errors.New("keystore: connection refused")
}

// The single-document saves: a save whose entry sync fails after the document was stored, when the
// previous state cannot be put back, is audited (keys.save, marked failed) — the new document is in effect.
func TestAzureRegistry_FailedLegacySaveThatKeptTheNewDocumentIsAudited(t *testing.T) {
	e := newAzRegEnv(t)
	ch := captureAudit(t)
	e.wire(func(d *testAzureModelsDeps) {
		d.OrgKeys = azRegRestoreFailingOrgKeys{e.orgKeys}
		d.Secrets = azRegDeleteFailingSecrets{e.secrets}
	})
	e.registry.FailOn("CreateModel", errors.New("db: connection reset"))
	body := map[string]any{
		"endpoint":       "https://legacy" + azRegHostSuffix,
		"auth":           map[string]any{"mode": "api_key", "key": "k1"},
		"chatDeployment": "gpt-4o",
	}
	isFailedSave := func(resourceType string) func(*storageif.AuditEvent) bool {
		return func(ev *storageif.AuditEvent) bool {
			return ev.Action == audit.ActionKeysSave && ev.ResourceType == resourceType && ev.Meta["outcome"] == "failed_after_credential_change"
		}
	}

	rr := e.do("u-admin", http.MethodPost, "/api/orgs/"+e.orgID+"/providers/azure-foundry/config", body)
	if rr.Code < 400 {
		t.Fatalf("org save: %d %s, want the sync's failure", rr.Code, rr.Body.String())
	}
	ev := awaitAudit(t, ch, isFailedSave("org"))
	assertAzRegAudit(t, ev, "org", e.orgID, map[string]string{"provider": ai.AzureFoundryProviderID, "authMode": "api_key"})
	if has, _ := e.orgKeys.HasOrg(e.orgID, ai.AzureFoundryProviderID); !has {
		t.Error("the audit row must describe the store: no org document")
	}

	rr = e.do("u-member", http.MethodPost, "/api/providers/azure-foundry/config", body)
	if rr.Code < 400 {
		t.Fatalf("personal save: %d %s, want the sync's failure", rr.Code, rr.Body.String())
	}
	ev = awaitAudit(t, ch, isFailedSave("provider"))
	assertAzRegAudit(t, ev, "provider", ai.AzureFoundryProviderID, map[string]string{"authMode": "api_key"})

	// A failed save whose previous state went back is not audited.
	e.wire(nil)
	rr = e.do("u-stranger", http.MethodPost, "/api/providers/azure-foundry/config", body)
	if rr.Code < 400 {
		t.Fatalf("stranger save: %d", rr.Code)
	}
	noAudit(t, ch, func(ev *storageif.AuditEvent) bool { return ev.Action == audit.ActionKeysSave })
	if has, _ := e.secrets.Has("u-stranger", ai.AzureFoundryProviderID); has {
		t.Error("the document of a failed save stayed")
	}
}
