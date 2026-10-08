package api

// Test-only harness for TestAzureFoundryE2E: the real baki HTTP API (JWT / cloud mode, the production Router, EventManager SSE hub, ChatService, ProviderService and ai.ProviderFactory) served on an httptest server, with every outbound Azure AI Foundry (and Microsoft Entra ID) request redirected to an in-process fake that records the exact wire request.
// Redirect seam: ai.AzureFoundryOptions.Transport (tests only) makes the Foundry runtime client — and the Entra ID credential, which uses the same client — send every request through the harness transport; every other provider keeps the real shared client. The endpoint must still be https on port 443 of an allow-listed DNS name, so the test hosts live under the allow-listed .unit-test.invalid domain; the transport rewrites https://<x>.unit-test.invalid and https://login.microsoftonline.com to the fake's plain-HTTP listener, preserving the original Host header, and refuses every other destination.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/egress"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/parser"
)

// azE2EHostSuffix is the allow-listed test domain (RFC 6761 .invalid: it can
// never resolve, so nothing escapes the harness even if the redirect broke).
const azE2EHostSuffix = ".unit-test.invalid"

// azE2EAADHost is the Entra ID authority azidentity signs in against.
const azE2EAADHost = "login.microsoftonline.com"

// azE2EEntraToken is the access token the fake Entra ID issues.
const azE2EEntraToken = "eyJ0eXAiOiJKV1QiLCJhbGciOiJSUzI1NiJ9.fake-entra-access-token.sig"

// azE2EFlowSource is the PAD source every seeded flow carries.
const azE2EFlowSource = "#Region \"Main\"\nSET X TO %X%\nSET Y TO %Y%\n#EndRegion\n"

// installAzureE2ETransport points the Azure AI Foundry runtime client at rt
// for the rest of the test (see the file comment).
func installAzureE2ETransport(t *testing.T, rt http.RoundTripper) {
	t.Helper()
	ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{
		Egress:            egress.AllowLoopback,
		ExtraAllowedHosts: []string{"*" + azE2EHostSuffix},
		Transport:         rt,
	})
	ai.ResetAzureFoundryCaches()
	t.Cleanup(func() {
		ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{})
		ai.ResetAzureFoundryCaches()
	})
}

// azE2ETransport rewrites allowed https destinations onto the fake's
// listener. The original host travels in the Host header.
type azE2ETransport struct {
	target string // fake listener host:port
	base   *http.Transport
}

func (tr *azE2ETransport) RoundTrip(req *http.Request) (*http.Response, error) {
	host := strings.ToLower(req.URL.Hostname())
	if req.URL.Scheme != "https" {
		return nil, fmt.Errorf("azure e2e transport: refusing non-https request to %s", req.URL.Redacted())
	}
	if !strings.HasSuffix(host, azE2EHostSuffix) && host != azE2EAADHost {
		return nil, fmt.Errorf("azure e2e transport: unexpected outbound host %q", host)
	}
	out := req.Clone(req.Context())
	out.URL.Scheme = "http"
	out.URL.Host = tr.target
	out.Host = req.URL.Host
	return tr.base.RoundTrip(out)
}

// azE2ERequest is one request the fake received, exactly as sent.
type azE2ERequest struct {
	Method   string
	Host     string
	Path     string
	RawQuery string
	Header   http.Header
	Body     []byte
	At       time.Time
}

// JSON decodes the body as a JSON object (nil when it is not one).
func (r azE2ERequest) JSON() map[string]any {
	var m map[string]any
	if err := json.Unmarshal(r.Body, &m); err != nil {
		return nil
	}
	return m
}

// azE2EHandler answers one request; n is the 1-based count of requests the
// fake has seen for that host (this one included).
type azE2EHandler func(w http.ResponseWriter, r *http.Request, rec azE2ERequest, n int)

// azE2EFake is the fake Azure AI Foundry resource(s) + Entra ID authority.
type azE2EFake struct {
	srv      *httptest.Server
	mu       sync.Mutex
	handlers map[string]azE2EHandler // by lower-case host
	reqs     []azE2ERequest
	perHost  map[string]int
	// signIns counts the access tokens the fake Entra ID issued: the first is azE2EEntraToken, later ones carry a "-<n>" suffix, so a refreshed token is observable on the wire.
	signIns int
}

func newAzureE2EFake(t *testing.T) *azE2EFake {
	t.Helper()
	f := &azE2EFake{handlers: map[string]azE2EHandler{}, perHost: map[string]int{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// transport is the RoundTripper to install (installAzureE2ETransport).
func (f *azE2EFake) transport(t *testing.T) http.RoundTripper {
	base := &http.Transport{}
	t.Cleanup(base.CloseIdleConnections)
	return &azE2ETransport{target: f.srv.Listener.Addr().String(), base: base}
}

// handle registers the handler for one resource host.
func (f *azE2EFake) handle(host string, h azE2EHandler) {
	f.mu.Lock()
	f.handlers[strings.ToLower(host)] = h
	f.mu.Unlock()
}

// requests returns the requests one host received, in order.
func (f *azE2EFake) requests(host string) []azE2ERequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []azE2ERequest
	for _, r := range f.reqs {
		if strings.EqualFold(r.Host, host) {
			out = append(out, r)
		}
	}
	return out
}

func (f *azE2EFake) serve(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	host := strings.ToLower(r.Host)
	if h, _, found := strings.Cut(host, ":"); found {
		host = h
	}
	rec := azE2ERequest{Method: r.Method, Host: host, Path: r.URL.Path, RawQuery: r.URL.RawQuery, Header: r.Header.Clone(), Body: body, At: time.Now()}
	f.mu.Lock()
	f.reqs = append(f.reqs, rec)
	f.perHost[host]++
	n := f.perHost[host]
	h := f.handlers[host]
	f.mu.Unlock()
	if host == azE2EAADHost {
		f.serveEntraID(w, r)
		return
	}
	if h == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"code":"DeploymentNotFound","message":"The API deployment for this resource does not exist."}}`)
		return
	}
	h(w, r, rec, n)
}

// serveEntraID answers the client-credentials sign-in azidentity (MSAL) performs against login.microsoftonline.com: the tenant's OpenID configuration, then the token endpoint; instance discovery is answered too in case MSAL asks.
func (f *azE2EFake) serveEntraID(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
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
			"tenant_discovery_endpoint": "https://" + azE2EAADHost + "/" + tenant + "/v2.0/.well-known/openid-configuration",
			"api-version":               "1.1",
			"metadata": []map[string]any{{
				"preferred_network": azE2EAADHost, "preferred_cache": "login.windows.net",
				"aliases": []string{azE2EAADHost, "login.windows.net", "login.microsoft.com", "sts.windows.net"},
			}},
		})
	case strings.HasSuffix(r.URL.Path, "/v2.0/.well-known/openid-configuration") && len(parts) > 0:
		tenant := parts[0]
		base := "https://" + azE2EAADHost + "/" + tenant
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_endpoint":                        base + "/oauth2/v2.0/token",
			"authorization_endpoint":                base + "/oauth2/v2.0/authorize",
			"issuer":                                base + "/v2.0",
			"jwks_uri":                              "https://" + azE2EAADHost + "/" + tenant + "/discovery/v2.0/keys",
			"token_endpoint_auth_methods_supported": []string{"client_secret_post", "private_key_jwt", "client_secret_basic"},
			"response_types_supported":              []string{"code", "id_token", "code id_token", "id_token token"},
			"tenant_region_scope":                   "EU",
			"cloud_instance_name":                   "microsoftonline.com",
		})
	case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/oauth2/v2.0/token"):
		f.mu.Lock()
		f.signIns++
		tok := azE2EEntraToken
		if f.signIns > 1 {
			tok += "-" + strconv.Itoa(f.signIns)
		}
		f.mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"token_type":     "Bearer",
			"expires_in":     3599,
			"ext_expires_in": 3599,
			"access_token":   tok,
		})
	default:
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":"not_found","error_description":"AADSTS90002: fake authority has no such route"}`)
	}
}

// azE2EWriteSSE streams frames with a flush after each (text/event-stream).
func azE2EWriteSSE(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("apim-request-id", uuid.NewString())
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for _, fr := range frames {
		_, _ = io.WriteString(w, fr)
		if fl != nil {
			fl.Flush()
		}
	}
}

// azE2EData is one OpenAI-style SSE frame ("data: <json>").
func azE2EData(v any) string {
	if s, ok := v.(string); ok {
		return "data: " + s + "\n\n"
	}
	b, _ := json.Marshal(v)
	return "data: " + string(b) + "\n\n"
}

// azE2EEvent is one Anthropic/Responses-style SSE frame ("event:" + "data:").
func azE2EEvent(name string, v any) string {
	b, _ := json.Marshal(v)
	return "event: " + name + "\ndata: " + string(b) + "\n\n"
}

// azE2EWriteJSON answers status with a JSON body.
func azE2EWriteJSON(w http.ResponseWriter, status int, body string, headers ...string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("apim-request-id", uuid.NewString())
	for i := 0; i+1 < len(headers); i += 2 {
		w.Header().Set(headers[i], headers[i+1])
	}
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

// --- the baki side ---

// azE2EOrgStore is the collaboration OrgStore on top of the fake backend's organisations, so the OrgService (org routes, flow authz) and the ChatService's role lookup (backend.OrgMemberRole) see the same rosters — as they do on Postgres. Invites stay in memory (not exercised).
type azE2EOrgStore struct {
	collaboration.OrgStore
	store *testutil.FakeBackend
	mu    *sync.Mutex // serializes MutateOrg
}

func (s azE2EOrgStore) SaveOrg(ctx context.Context, org *storageif.Organisation) error {
	return s.store.SaveOrg(ctx, org)
}

func (s azE2EOrgStore) LoadOrg(ctx context.Context, id string) (*storageif.Organisation, error) {
	return s.store.LoadOrg(ctx, id)
}

func (s azE2EOrgStore) ListOrgsForUser(ctx context.Context, userID string) ([]*storageif.Organisation, error) {
	return s.store.ListOrgsForUser(ctx, userID)
}

func (s azE2EOrgStore) DeleteOrg(ctx context.Context, id string) error {
	return s.store.DeleteOrg(ctx, id)
}

// MutateOrg is load, fn, save under one lock, as Postgres holds the row lock across them.
func (s azE2EOrgStore) MutateOrg(ctx context.Context, id string, fn func(*storageif.Organisation) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	org, err := s.store.LoadOrg(ctx, id)
	if err != nil {
		return err
	}
	if err := fn(org); err != nil {
		return err
	}
	return s.store.SaveOrg(ctx, org)
}

func (s azE2EOrgStore) OrgMemberRole(ctx context.Context, orgID, userID string) (auth.Role, bool, error) {
	return s.store.OrgMemberRole(ctx, orgID, userID)
}

// azE2EEnv is the running baki API plus the fake Azure side.
type azE2EEnv struct {
	rt      *Router
	api     *httptest.Server
	fake    *azE2EFake
	backend *testutil.FakeBackend
	secrets *testutil.FakeSecretStore
	orgKeys *testutil.FakeOrgSecretStore
	orgSvc  *collaboration.OrgService
}

// newAzureE2EEnv wires the router the way DI does in cloud mode (JWT auth, cloud document provider, backend conversations, org credential store, the EventManager as the services' notifier, the Azure AI Foundry model registry on the provider, system and chat services) on in-memory secrets, an in-memory registry and the fake backend, and serves it over real HTTP.
func newAzureE2EEnv(t *testing.T) *azE2EEnv {
	t.Helper()
	fake := newAzureE2EFake(t)
	installAzureE2ETransport(t, fake.transport(t))

	// Cloud chat reads the caller's preferences through backend.LoadUserSettings; the fake, like Postgres, answers a
	// user without a row with models.DefaultSettings() ("Save conversation history" on).
	store := testutil.NewFakeBackend()
	cfg := config.Default()
	cfg.Auth.Enabled = true
	cfg.Auth.Secret = testToken
	routePolicyFromEnv(cfg)

	shutdown := make(chan struct{})
	events := NewEventManager(shutdown)
	settings := storage.NewInMemorySettingsStore()
	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()

	orgSvc := collaboration.NewOrgService(azE2EOrgStore{OrgStore: collaboration.NewMemOrgStore(), store: store, mu: &sync.Mutex{}})
	authzSvc := service.NewAuthzService(store, orgSvc)
	docProv := service.NewCloudDocumentProvider(store)
	bus := eventbus.New(nil, 0)
	flowSvc := service.NewFlowService(events, settings, docProv, store, authzSvc, nil, service.WithFlowEvents(bus))
	scopes := service.NewFlowScopeResolver(store, authzSvc)
	libSvc := service.NewLibraryService(store, bus, authzSvc, scopes)
	analysisSvc, err := service.NewAnalysisService(events, settings, nil, service.AnalysisOptions{})
	if err != nil {
		t.Fatalf("analysis service: %v", err)
	}
	exportSvc := service.NewExportService(events, flowSvc, analysisSvc)

	factory := ai.NewProviderFactory(func(scope, provider string) (string, error) {
		v, _ := secrets.Get(scope, provider)
		return v, nil
	}, nil, nil, nil)
	factory.SetOrgKeyLookup(orgKeys.OrgKeyLookup)
	// The Azure AI Foundry model registry (Part 9) as di.ProvideAzureModelService builds it in cloud mode — one registry per user and per organisation (the Postgres store's owners, kept in memory here), the backend for the caller's settings, organisation membership, corpus counts and user names — wired into the same three services.
	azure := service.NewAzureModelService(service.AzureModelServiceDeps{
		Registry:      storage.NewMemoryAzureRegistry(),
		Secrets:       secrets,
		KeyFamilies:   secrets,
		OrgKeys:       orgKeys,
		Factory:       factory,
		Settings:      settings,
		UserSettings:  store,
		Orgs:          store,
		Corpus:        store,
		Users:         store,
		PerUserOwners: true,
	})
	chatSvc := service.NewChatService(events, t.TempDir(), flowSvc, analysisSvc, settings, factory, store, config.ModeCloud, service.ChatOptions{Azure: azure})
	providerSvc := service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets, orgKeys, azure)
	sysSvc := service.NewSystemService(settings, secrets, events, store, config.ModeCloud, azure, nil)

	security := &SecurityConfig{
		JWTEnabled:     true,
		LocalUserID:    "local",
		LocalName:      "You",
		Token:          testToken,
		AuthMgr:        auth.NewManager(testToken, auth.NewTokenBlacklist()),
		Backend:        store,
		OrgSvc:         orgSvc,
		TrustedProxies: cfg.Server.TrustedProxies,
	}
	mail := mailer.NewService(config.EmailConfig{}, config.ModeLocal)
	dashboardSvc := service.NewDashboardService(store, analysisSvc, flowSvc, scopes)
	triageSvc := service.NewTriageService(store, store, store, bus)
	shareSvc := service.NewShareTokenService(store, store, bus)
	for _, s := range service.FlowCacheSubscribers(flowSvc, dashboardSvc, chatSvc) {
		eventbus.Subscribe(bus, eventbus.Sync, s.Name, s.Fn)
	}
	// Never started, as before boot: every row is a detached write to the backend.
	auditRec := testAuditRecorder(store)
	modules := []Module{
		NewSystemHandler(sysSvc, security, auditRec, nil, authzSvc, service.NewHealthService(store, nil)),
		NewFlowHandler(flowSvc, docProv, store, security, shareSvc, auditRec),
		NewLibraryHandler(libSvc, store, security, auditRec),
		NewChatHandler(chatSvc, flowSvc, security, auditRec),
		NewAnalysisHandler(analysisSvc, flowSvc, dashboardSvc, store, security, service.NewWebhookNotifier(nil), mail, "", service.NewPolicyService(store), service.NewGovernanceAlertService(store), triageSvc, service.NewCommentService(store, triageSvc, nil), shareSvc, auditRec),
		NewDashboardHandler(dashboardSvc, security),
		NewExportHandler(exportSvc, flowSvc, analysisSvc, security, auditRec),
		NewAuthHandler(service.NewSessionService(newMockTokenStore()), store, security, nil, service.NewIdentityService(store, nil), mail, service.NewAuthService(store, mail), service.NewAccountLifecycleService(store, nil, nil), service.NewAPITokenService(store), auditRec),
		NewAdminHandler(auditRec, security, NewMigrationRunner(nil), nil, authzSvc, service.NewUserAdminService(store)),
		NewProviderHandler(providerSvc, security, auditRec),
		NewOrgHandler(orgSvc, store, nil, security, mail, service.NewOrgRuleService(store, store, nil), providerSvc, authzSvc, service.NewChannelService(store), service.NewUserDirectory(store), auditRec),
		NewSharingHandler(store, flowSvc, service.NewCollaboratorService(store, bus), security, auditRec),
	}
	rt, err := NewRouter(RouterParams{Lifecycle: noopLifecycle{}, Security: security, Events: events, Modules: modules, Config: cfg, ShutdownCh: shutdown})
	if err != nil {
		t.Fatalf("router: %v", err)
	}
	rt.caps = testCaps(true, store)
	api := httptest.NewServer(rt)
	t.Cleanup(func() {
		close(shutdown) // ends the SSE handlers before the server waits on them
		api.Close()
	})
	return &azE2EEnv{rt: rt, api: api, fake: fake, backend: store, secrets: secrets, orgKeys: orgKeys, orgSvc: orgSvc}
}

// bearer issues an access token for userID.
func (e *azE2EEnv) bearer(t *testing.T, userID string, role auth.Role) string {
	t.Helper()
	pair, err := e.rt.security.AuthMgr.Issue(userID, userID+"@example.com", role)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	return "Bearer " + pair.AccessToken
}

// seedFlow stores a parsed flow owned by ownerID (in orgID when non-empty).
func (e *azE2EEnv) seedFlow(t *testing.T, id, ownerID, orgID string) {
	t.Helper()
	doc, err := parser.ParseText(azE2EFlowSource, id+".txt", int64(len(azE2EFlowSource)))
	if err != nil {
		t.Fatalf("parse seed flow: %v", err)
	}
	content, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal seed flow: %v", err)
	}
	if err := e.backend.SaveFlow(context.Background(), &storageif.FlowDocument{
		ID: id, Name: id, OwnerID: ownerID, OrganizationID: orgID, Content: content, Source: azE2EFlowSource,
	}); err != nil {
		t.Fatalf("seed flow %s: %v", id, err)
	}
}

// do sends one API request over HTTP and returns the status and body.
func (e *azE2EEnv) do(t *testing.T, method, path, bearer string, body any) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("encode body: %v", err)
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(context.Background(), method, e.api.URL+path, rd)
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if bearer != "" {
		req.Header.Set("Authorization", bearer)
	}
	resp, err := e.api.Client().Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s %s: %v", method, path, err)
	}
	return resp.StatusCode, out
}

// mustDo is do with a required status; the body is decoded into out when
// out is non-nil.
func (e *azE2EEnv) mustDo(t *testing.T, method, path, bearer string, body any, want int, out any) []byte {
	t.Helper()
	status, raw := e.do(t, method, path, bearer, body)
	if status != want {
		t.Fatalf("%s %s: status %d, want %d — body %s", method, path, status, want, raw)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("%s %s: decode %q: %v", method, path, raw, err)
		}
	}
	return raw
}

// azE2EUser is one signed-in user with a personal flow.
type azE2EUser struct {
	id     string
	bearer string
	flowID string
}

// newUser signs userID in (platform role member) and seeds their flow.
func (e *azE2EEnv) newUser(t *testing.T, userID string) azE2EUser {
	t.Helper()
	u := azE2EUser{id: userID, bearer: e.bearer(t, userID, auth.RoleMember), flowID: "flow-" + userID}
	e.seedFlow(t, u.flowID, userID, "")
	return u
}

// saveConfig stores the caller's Azure AI Foundry document through the API.
func (e *azE2EEnv) saveConfig(t *testing.T, u azE2EUser, doc map[string]any) {
	t.Helper()
	e.mustDo(t, http.MethodPost, "/api/providers/azure-foundry/config", u.bearer, doc, http.StatusOK, nil)
}

// azE2EAPIKeyDoc is an API-key document on https://<host>.
func azE2EAPIKeyDoc(host, key string) map[string]any {
	return map[string]any{
		"endpoint":            "https://" + host,
		"auth":                map[string]any{"mode": "api_key", "key": key},
		"chatDeployment":      "gpt-4o",
		"deployments":         []string{"model-router"},
		"claudeDeployments":   []string{"claude-sonnet-4-6"},
		"embeddingDeployment": "text-embedding-3-small",
	}
}

// --- client-visible chat events (GET /api/events) ---

// azE2EChatEvent is one "chat:event" SSE payload.
type azE2EChatEvent struct {
	StreamID string         `json:"streamId"`
	Type     string         `json:"type"`
	Data     map[string]any `json:"data"`
}

// azE2EChatOutcome is what a client saw for one stream.
type azE2EChatOutcome struct {
	StreamID string
	Text     string
	Done     map[string]any // the done payload (nil when the stream failed)
	Err      map[string]any // the error payload (nil when it finished)
	Events   []azE2EChatEvent
}

// has reports whether an event of type typ (and, when non-empty, with
// data.name == name) was delivered.
func (o azE2EChatOutcome) has(typ, name string) bool {
	for _, ev := range o.Events {
		if ev.Type == typ && (name == "" || ev.Data["name"] == name) {
			return true
		}
	}
	return false
}

// azE2ESSE is one live GET /api/events subscription.
type azE2ESSE struct {
	events chan azE2EChatEvent
	done   chan struct{}
}

// subscribe opens the caller's event stream (the subscription is registered
// by the time the response headers arrive).
func (e *azE2EEnv) subscribe(t *testing.T, bearer string) *azE2ESSE {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, e.api.URL+"/api/events", nil)
	if err != nil {
		cancel()
		t.Fatalf("build SSE request: %v", err)
	}
	req.Header.Set("Authorization", bearer)
	resp, err := e.api.Client().Do(req)
	if err != nil {
		cancel()
		t.Fatalf("GET /api/events: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		cancel()
		t.Fatalf("GET /api/events: status %d — %s", resp.StatusCode, b)
	}
	s := &azE2ESSE{events: make(chan azE2EChatEvent, 4096), done: make(chan struct{})}
	go func() {
		defer close(s.done)
		defer close(s.events)
		defer resp.Body.Close()
		sc := bufio.NewScanner(resp.Body)
		sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if !strings.HasPrefix(line, "data: ") {
				continue
			}
			var frame struct {
				Name string          `json:"name"`
				Data json.RawMessage `json:"data"`
			}
			if json.Unmarshal([]byte(line[len("data: "):]), &frame) != nil || frame.Name != "chat:event" {
				continue
			}
			var ev azE2EChatEvent
			if json.Unmarshal(frame.Data, &ev) != nil {
				continue
			}
			select {
			case s.events <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		cancel()
		<-s.done
	})
	return s
}

// await collects streamID's events until its done or error event.
func (s *azE2ESSE) await(t *testing.T, streamID string, timeout time.Duration) azE2EChatOutcome {
	t.Helper()
	out := azE2EChatOutcome{StreamID: streamID}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	for {
		select {
		case ev, ok := <-s.events:
			if !ok {
				t.Fatalf("event stream closed before stream %s finished (events %+v)", streamID, out.Events)
			}
			if ev.StreamID != streamID {
				continue
			}
			out.Events = append(out.Events, ev)
			switch ev.Type {
			case "chunk":
				c, _ := ev.Data["content"].(string)
				out.Text += c
			case "done":
				out.Done = ev.Data
				return out
			case "error":
				out.Err = ev.Data
				return out
			}
		case <-deadline.C:
			t.Fatalf("timed out after %s waiting for stream %s to finish (events %+v)", timeout, streamID, out.Events)
		}
	}
}

// azE2EChatReq is one chat turn as the web client sends it.
type azE2EChatReq struct {
	FlowID         string
	Model          string
	Message        string
	UseTools       bool
	ConversationID string
}

// chat subscribes, starts the stream through POST /api/chat/stream with a client stream id (the C-1 path: no /chat/begin round trip) and waits for its terminal event.
func (e *azE2EEnv) chat(t *testing.T, u azE2EUser, req azE2EChatReq) azE2EChatOutcome {
	t.Helper()
	sse := e.subscribe(t, u.bearer)
	return e.chatOn(t, sse, u, req)
}

func (e *azE2EEnv) chatOn(t *testing.T, sse *azE2ESSE, u azE2EUser, req azE2EChatReq) azE2EChatOutcome {
	t.Helper()
	streamID := uuid.NewString()
	if req.ConversationID == "" {
		req.ConversationID = uuid.NewString()
	}
	if req.FlowID == "" {
		req.FlowID = u.flowID
	}
	body := map[string]any{
		"flowId":         req.FlowID,
		"provider":       ai.AzureFoundryProviderID,
		"model":          req.Model,
		"userMessage":    req.Message,
		"clientStreamId": streamID,
		"conversationId": req.ConversationID,
		"useTools":       req.UseTools,
	}
	var got string
	e.mustDo(t, http.MethodPost, "/api/chat/stream", u.bearer, body, http.StatusOK, &got)
	if got != streamID {
		t.Fatalf("POST /api/chat/stream returned stream id %q, want the client's %q", got, streamID)
	}
	out := sse.await(t, streamID, 30*time.Second)
	if out.Done != nil && strings.TrimSpace(out.Text) != "" {
		// The server stores the answer from the stream's cleanup, AFTER the done event; wait until it is readable so no write is still in flight when the next step (or the test's temp-dir cleanup) runs.
		e.awaitStoredAnswer(t, u, req.FlowID, req.ConversationID, out.Text)
	}
	return out
}

// awaitStoredAnswer polls POST /api/chat/get until the thread holds an
// assistant message with text.
func (e *azE2EEnv) awaitStoredAnswer(t *testing.T, u azE2EUser, flowID, convID, text string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		e.mustDo(t, http.MethodPost, "/api/chat/get", u.bearer, map[string]any{"flowId": flowID, "provider": convID}, http.StatusOK, &body)
		msgs := body.Messages
		for _, m := range msgs {
			if m["role"] == "assistant" && m["content"] == text {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the answer %q was not stored within 5s (thread %v)", text, msgs)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// conversation reloads a stored thread (POST /api/chat/get), polling until it holds at least want messages: the server stores the answer after the done event, from the stream's cleanup.
func (e *azE2EEnv) conversation(t *testing.T, u azE2EUser, flowID, convID string, want int) []map[string]any {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var body struct {
			Messages []map[string]any `json:"messages"`
		}
		e.mustDo(t, http.MethodPost, "/api/chat/get", u.bearer, map[string]any{"flowId": flowID, "provider": convID}, http.StatusOK, &body)
		msgs := body.Messages
		if len(msgs) >= want || time.Now().After(deadline) {
			return msgs
		}
		time.Sleep(25 * time.Millisecond)
	}
}

// providerInfo fetches GET /api/providers/list (with ?orgId= when non-empty)
// and returns the azure-foundry entry.
func (e *azE2EEnv) providerInfo(t *testing.T, bearer, orgID string) map[string]any {
	t.Helper()
	path := "/api/providers/list"
	if orgID != "" {
		path += "?orgId=" + url.QueryEscape(orgID)
	}
	var list []map[string]any
	e.mustDo(t, http.MethodGet, path, bearer, nil, http.StatusOK, &list)
	for _, p := range list {
		if p["id"] == ai.AzureFoundryProviderID {
			return p
		}
	}
	t.Fatalf("GET %s: no %s entry in %v", path, ai.AzureFoundryProviderID, list)
	return nil
}

// azE2EModelIDs lists the ids of a provider entry's models.
func azE2EModelIDs(p map[string]any) []string {
	var ids []string
	models, _ := p["models"].([]any)
	for _, m := range models {
		if mm, ok := m.(map[string]any); ok {
			id, _ := mm["id"].(string)
			ids = append(ids, id)
		}
	}
	return ids
}

// azE2EModel returns one model entry of a provider entry by id.
func azE2EModel(p map[string]any, id string) map[string]any {
	models, _ := p["models"].([]any)
	for _, m := range models {
		if mm, ok := m.(map[string]any); ok && mm["id"] == id {
			return mm
		}
	}
	return nil
}

// azE2EModelByAlias returns the provider entry's model that answers for a pre-registry id — one of its aliases, which only the entries of the migrated resource "default" carry — or nil.
func azE2EModelByAlias(p map[string]any, alias string) map[string]any {
	models, _ := p["models"].([]any)
	for _, m := range models {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		aliases, _ := mm["aliases"].([]any)
		for _, a := range aliases {
			if a == alias {
				return mm
			}
		}
	}
	return nil
}

// modelIDFor is the "m/" id of the caller's listed model that answers for a
// pre-registry id (a deployment the legacy document named).
func (e *azE2EEnv) modelIDFor(t *testing.T, u azE2EUser, alias string) string {
	t.Helper()
	p := e.providerInfo(t, u.bearer, "")
	id, _ := azE2EModelByAlias(p, alias)["id"].(string)
	if !strings.HasPrefix(id, "m/") {
		t.Fatalf("no listed model answers for %q (models %v)", alias, azE2EModelIDs(p))
	}
	return id
}

// addModel registers a model on one of the caller's resources through the registry API (POST /api/providers/azure-foundry/models) and returns its view (id, modelId "m/<id>", kind, deployment, displayName, …).
func (e *azE2EEnv) addModel(t *testing.T, u azE2EUser, body map[string]any) map[string]any {
	t.Helper()
	var out struct {
		Model map[string]any `json:"model"`
	}
	e.mustDo(t, http.MethodPost, "/api/providers/azure-foundry/models", u.bearer, body, http.StatusCreated, &out)
	if id, _ := out.Model["modelId"].(string); !strings.HasPrefix(id, "m/") {
		t.Fatalf("POST /api/providers/azure-foundry/models answered %v", out.Model)
	}
	return out.Model
}

// chatRefused sends a chat turn the server must refuse before any stream starts, with 400 and the MODEL_NOT_AVAILABLE code (the client then puts the text back in the composer); it returns the error envelope.
func (e *azE2EEnv) chatRefused(t *testing.T, u azE2EUser, req azE2EChatReq) map[string]any {
	t.Helper()
	if req.FlowID == "" {
		req.FlowID = u.flowID
	}
	var body map[string]any
	e.mustDo(t, http.MethodPost, "/api/chat/stream", u.bearer, map[string]any{
		"flowId":         req.FlowID,
		"provider":       ai.AzureFoundryProviderID,
		"model":          req.Model,
		"userMessage":    req.Message,
		"clientStreamId": uuid.NewString(),
		"conversationId": uuid.NewString(),
	}, http.StatusBadRequest, &body)
	if body["code"] != service.AzureCodeModelNotAvailable {
		t.Errorf("refused chat = %v, want code %s", body, service.AzureCodeModelNotAvailable)
	}
	return body
}

// azE2EMessages returns a chat-completions body's messages.
func azE2EMessages(body map[string]any) []map[string]any {
	var out []map[string]any
	raw, _ := body["messages"].([]any)
	for _, m := range raw {
		if mm, ok := m.(map[string]any); ok {
			out = append(out, mm)
		}
	}
	return out
}

// azE2ENum reads a JSON number (float64) field, NaN-free: missing → -1.
func azE2ENum(m map[string]any, key string) float64 {
	if v, ok := m[key].(float64); ok {
		return v
	}
	return -1
}
