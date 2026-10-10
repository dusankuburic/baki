package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/events"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/platform/reqtx"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"

	"go.uber.org/fx"
)

type mockNotifier = service.NilNotifier

// memIdentityStore is an in-memory identity-link store for SSO handler tests.
type memIdentityStore struct {
	links map[string]*storageif.IdentityLink // key: provider + "\x00" + subject
}

func newMemIdentityStore() *memIdentityStore {
	return &memIdentityStore{links: map[string]*storageif.IdentityLink{}}
}

func (m *memIdentityStore) ListIdentityLinks(_ context.Context, userID string) ([]*storageif.IdentityLink, error) {
	out := []*storageif.IdentityLink{}
	for _, l := range m.links {
		if l.UserID == userID {
			cp := *l
			out = append(out, &cp)
		}
	}
	return out, nil
}

func (m *memIdentityStore) DeleteIdentityLink(_ context.Context, userID, provider string) error {
	for key, l := range m.links {
		if l.UserID == userID && l.Provider == provider {
			delete(m.links, key)
		}
	}
	return nil
}

func (m *memIdentityStore) SaveIdentityLink(_ context.Context, link *storageif.IdentityLink) error {
	key := link.Provider + "\x00" + link.Subject
	if _, exists := m.links[key]; exists {
		return fmt.Errorf("identity link already exists")
	}
	cp := *link
	m.links[key] = &cp
	return nil
}

func (m *memIdentityStore) LoadIdentityLink(_ context.Context, provider, subject string) (*storageif.IdentityLink, error) {
	if link, ok := m.links[provider+"\x00"+subject]; ok {
		cp := *link
		return &cp, nil
	}
	return nil, storageif.ErrNotFound
}

// orgServiceDirectory is the Azure AI Foundry registry's view of an
// OrgService's organisations (service.AzureOrgDirectory): the test routers
// keep organisations in a collaboration store, where production keeps them
// in the backend the registry reads.
type orgServiceDirectory struct{ orgs *collaboration.OrgService }

func (d orgServiceDirectory) ListOrgsForUser(ctx context.Context, userID string) ([]*storageif.Organisation, error) {
	return d.orgs.ListForUser(ctx, userID)
}

func (d orgServiceDirectory) LoadOrg(ctx context.Context, id string) (*storageif.Organisation, error) {
	return d.orgs.Get(ctx, id)
}

// testAzureModelsDeps is what newTestAzureModels builds the registry service
// from. Leave OrgKeys a nil INTERFACE (not a typed nil pointer) for a
// deployment without organisation registries.
type testAzureModelsDeps struct {
	// Registry is the store (nil: a fresh fake); pass the same one to a
	// rebuilt service to keep the registry's rows.
	Registry *testutil.FakeAzureRegistry
	Secrets  service.KeyStore
	OrgKeys  service.OrgKeyStore
	Factory  *ai.ProviderFactory
	// Orgs is where the organisations live (nil: no "Shared with you").
	Orgs  *collaboration.OrgService
	Users service.AzureUserDirectory
	// Corpus counts the knowledge bases' chunks (nil: every corpus empty).
	Corpus service.AzureCorpusCounter
	// PerUser: one registry per user and per organisation (the database
	// store's shape, cloud); false is the desktop's single personal registry.
	PerUser bool
	// KnowledgeEmbedding: the knowledge base embeds with Azure AI Foundry.
	KnowledgeEmbedding bool
}

// newTestAzureModels builds the Azure AI Foundry model registry service the
// way di.ProvideAzureModelService does, on a fake registry store; the caller
// passes it to NewProviderService (and NewSystemService), as DI does.
func newTestAzureModels(d testAzureModelsDeps) (*service.AzureModelService, *testutil.FakeAzureRegistry) {
	reg := d.Registry
	if reg == nil {
		reg = testutil.NewFakeAzureRegistry()
	}
	deps := service.AzureModelServiceDeps{
		Registry:                  reg,
		Secrets:                   d.Secrets,
		OrgKeys:                   d.OrgKeys,
		Factory:                   d.Factory,
		Users:                     d.Users,
		Corpus:                    d.Corpus,
		PerUserOwners:             d.PerUser,
		KnowledgeEmbeddingEnabled: d.KnowledgeEmbedding,
	}
	if d.Orgs != nil {
		deps.Orgs = orgServiceDirectory{orgs: d.Orgs}
	}
	// As provideKeyStore wires the database keystore: a keystore that lists its key families is the lister too.
	deps.KeyFamilies, _ = d.Secrets.(storageif.SecretFamilyLister)
	return service.NewAzureModelService(deps), reg
}

// noopLifecycle is a stand-in fx.Lifecycle for tests that construct a Router
// directly; it discards registered hooks (the WS hub's OnStop close is a no-op
// with a nil backplane anyway).
type noopLifecycle struct{}

func (noopLifecycle) Append(fx.Hook) {}

func newTestRouter(backend storageif.StorageBackend, jwtEnabled bool) *Router {
	return newTestRouterSSO(backend, jwtEnabled, nil, nil)
}

// newTestRouterSSO is newTestRouter with SSO collaborators injected into the
// auth handler (nil/nil = SSO disabled, the common case).
//
// tweak lets a caller adjust the config the router is built from (e.g. to turn
// on PAD_PPROF_ENABLED). Variadic so the existing four-arg call sites are
// unaffected.
func newTestRouterSSO(backend storageif.StorageBackend, jwtEnabled bool, ssoClient SSOClient, identityStore storageif.IdentityLinkStore, tweak ...func(*config.Config)) *Router {
	cfg := config.Default()
	cfg.Auth.Enabled = jwtEnabled
	cfg.Auth.Secret = testToken
	routePolicyFromEnv(cfg)
	for _, fn := range tweak {
		fn(cfg)
	}

	notifier := &mockNotifier{}
	// In memory: the settings file is the developer's own desktop settings, and the keyring store runs on
	// go-keyring's in-memory mock (TestMain), never the OS keychain.
	settings := storage.NewInMemorySettingsStore()
	secrets := storage.NewKeyringSecretStore()
	sysSvc := service.NewSystemService(settings, secrets, notifier, backend, cfg.Mode, nil, nil)
	docProv := service.DocumentProvider(service.NewLocalDocumentProvider())
	if jwtEnabled {
		docProv = service.NewCloudDocumentProvider(backend)
	}

	// Cloud mode always runs with a blacklist (main.go creates one whenever
	// auth is enabled); single-use SSO tickets and logout revocation depend
	// on it, so the test router mirrors that.
	var blacklist auth.BlacklistStore
	if jwtEnabled {
		blacklist = auth.NewTokenBlacklist()
	}
	authMgr := auth.NewManager(testToken, blacklist)
	// The event bus as di builds it, its async deliveries inline (after the request's commit).
	bus := eventbus.New(nil, 0)
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore(), collaboration.WithEventBus(bus))
	authzSvc := service.NewAuthzService(backend, orgSvc)

	// The flow caches subscribe to the same bus below; one analyzer serves the analysis service and is invalidated
	// by the flow service, as di wires them.
	cachedAnalyzer := service.NewDefaultAnalyzer()
	flowSvc := service.NewFlowService(notifier, settings, docProv, backend, authzSvc, nil, service.WithFlowEvents(bus), service.WithAnalyzer(cachedAnalyzer))
	scopes := service.NewFlowScopeResolver(backend, authzSvc)
	libSvc := service.NewLibraryService(backend, bus, authzSvc, scopes)
	analysisSvc, err := service.NewAnalysisService(notifier, settings, nil, service.AnalysisOptions{Analyzer: cachedAnalyzer})
	if err != nil {
		// NewAnalysisService keeps an error return for the DI signature but
		// never fails; this helper isn't t-aware, so panic is acceptable.
		panic("NewAnalysisService: " + err.Error())
	}
	exportSvc := service.NewExportService(notifier, flowSvc, analysisSvc)

	factory := ai.NewProviderFactory(func(s, p string) (string, error) { return "test", nil }, nil, nil, nil)
	// configDir: a real temp dir (not "") so the ChatService conversation store
	// never writes into the source tree (internal/api/conversations pollution).
	chatDir, dirErr := os.MkdirTemp("", "baki-test-conversations")
	if dirErr != nil {
		panic("test conversation dir: " + dirErr.Error())
	}
	chatSvc := service.NewChatService(notifier, chatDir, flowSvc, analysisSvc, settings, factory, backend, config.ModeLocal, service.ChatOptions{})

	ghAuth := ai.NewGitHubAuth()
	cpAuth := ai.NewCopilotAuth()
	providerSvc := service.NewProviderService(ghAuth, cpAuth, factory, secrets, nil, nil)

	security := &SecurityConfig{
		JWTEnabled:     jwtEnabled,
		LocalUserID:    "local",
		LocalName:      "You",
		Token:          testToken,
		AuthMgr:        authMgr,
		Backend:        backend,
		RequestTx:      testRequestTx(jwtEnabled, backend),
		Idempotency:    testIdempotency(jwtEnabled, backend),
		OrgSvc:         orgSvc,
		TrustedProxies: cfg.Server.TrustedProxies,
	}

	eventManager := NewEventManager(make(chan struct{}))

	dashboardSvc := service.NewDashboardService(backend, analysisSvc, flowSvc, scopes)
	authSvc := service.NewAuthService(backend, mailer.NewService(config.EmailConfig{}, config.ModeLocal))
	triageSvc := service.NewTriageService(backend, backend, backend, bus)
	shareSvc := service.NewShareTokenService(backend, backend, bus)
	for _, s := range service.FlowCacheSubscribers(flowSvc, dashboardSvc, chatSvc) {
		eventbus.Subscribe(bus, eventbus.Sync, s.Name, s.Fn)
	}
	// Never started, as before boot: every row is a detached write to the backend.
	auditRec := testAuditRecorder(backend)
	orgHandler := NewOrgHandler(orgSvc, backend, nil, security, mailer.NewService(config.EmailConfig{}, config.ModeLocal), service.NewOrgRuleService(backend, backend, bus), providerSvc, authzSvc, service.NewChannelService(backend), service.NewUserDirectory(backend), auditRec)
	// As APIModule subscribes it, on the provider service the org handler holds at delivery (tests swap it).
	eventbus.Subscribe(bus, eventbus.Async, "lent-credential-revocation", func(ctx context.Context, e events.OrgMemberChanged) {
		revokeLentCredentials(ctx, auditRec, orgHandler.providers, e)
	}, eventbus.InlineWhenSaturated())
	analysisHandler := NewAnalysisHandler(analysisSvc, flowSvc, dashboardSvc, backend, security, service.NewWebhookNotifier(nil), mailer.NewService(config.EmailConfig{}, config.ModeLocal), "", service.NewPolicyService(backend), service.NewGovernanceAlertService(backend), triageSvc, service.NewCommentService(backend, triageSvc, bus), shareSvc, auditRec, exportSvc)
	// The subscriptions internal/di.SubscribeRealtime makes for the handlers.
	eventbus.Subscribe(bus, eventbus.Async, "finding-comment-fanout", analysisHandler.OnFindingCommented, eventbus.InlineWhenSaturated())
	eventbus.Subscribe(bus, eventbus.Async, "finding-assignment-fanout", analysisHandler.OnFindingAssigned, eventbus.InlineWhenSaturated())
	modules := []Module{
		NewSystemHandler(sysSvc, security, auditRec, nil, authzSvc, service.NewHealthService(backend, testBlob(backend))),
		NewFlowHandler(flowSvc, docProv, backend, security, shareSvc, auditRec),
		NewLibraryHandler(libSvc, backend, security, auditRec),
		NewChatHandler(chatSvc, flowSvc, security, auditRec),
		analysisHandler,
		NewDashboardHandler(dashboardSvc, security),
		NewExportHandler(exportSvc, flowSvc, analysisSvc, security, auditRec),
		// Cloud/JWT mode always runs with a refresh-token store in production
		// (main.go wires one whenever auth is enabled); mirror that here so the
		// refresh handler's replay/rotation path is exercised, and a nil store
		// (which the handler now rejects as a misconfiguration) isn't relied on.
		NewAuthHandler(service.NewSessionService(newMockTokenStore()), backend, security, ssoClient, service.NewIdentityService(backend, identityStore), mailer.NewService(config.EmailConfig{}, config.ModeLocal), authSvc, service.NewAccountLifecycleService(backend, nil, nil), service.NewAPITokenService(backend), auditRec),
		NewAdminHandler(auditRec, security, NewMigrationRunner(nil), nil, authzSvc, service.NewUserAdminService(backend)),
		NewProviderHandler(providerSvc, security, auditRec),
		orgHandler,
		NewSharingHandler(backend, flowSvc, service.NewCollaboratorService(backend, bus), security, auditRec),
	}

	rt, err := NewRouter(RouterParams{Lifecycle: noopLifecycle{}, Security: security, Events: eventManager, Modules: modules, Config: cfg, ShutdownCh: make(chan struct{})})
	if err != nil {
		panic("NewRouter: " + err.Error())
	}
	rt.caps = testCaps(jwtEnabled, backend)
	testRouterBuses.Store(rt, bus)
	return rt
}

// testRequestTx and testIdempotency are what di takes from the database, taken from what the test wired instead:
// a backend that begins RLS transactions (Postgres, or a test double that counts them) opens each request's
// transaction, and one that keeps Idempotency-Key responses (Postgres, the fake) stores them; both only with
// accounts, as row-level security needs a caller.
func testRequestTx(jwtEnabled bool, backend storageif.StorageBackend) reqtx.Opener {
	if b, ok := backend.(storagedb.RLSBeginner); ok && jwtEnabled {
		return storagedb.RequestTx(b)
	}
	return nil
}

// testBlob is the blob probe di takes from the database (Postgres probes its blob container).
func testBlob(backend storageif.StorageBackend) service.BlobHealthChecker {
	b, _ := backend.(service.BlobHealthChecker)
	return b
}

func testIdempotency(jwtEnabled bool, backend storageif.StorageBackend) storageif.IdempotencyStore {
	if s, ok := backend.(storageif.IdempotencyStore); ok && jwtEnabled {
		return s
	}
	return nil
}

// testCaps is what a test router's deployment offers, from what the test wired rather than from config: the
// server-side store whenever a backend is wired (the fake backend stands in for Postgres here), accounts
// with JWT auth, row-level security with both.
func testCaps(jwtEnabled bool, backend storageif.StorageBackend) capability.Set {
	profile, caps := capability.Desktop, []capability.Capability(nil)
	if backend != nil {
		profile, caps = capability.LocalDB, append(caps, capability.ServerStore)
	}
	if jwtEnabled {
		profile, caps = capability.Cloud, append(caps, capability.Accounts)
		if backend != nil {
			caps = append(caps, capability.RowLevelSecurity)
		}
	}
	return capability.NewSet(profile, caps...)
}

// moduleOf returns rt's module of type M (a handler such as *AnalysisHandler), so a test can reach into the
// handler the router serves.
func moduleOf[M Module](t testing.TB, rt *Router) M {
	t.Helper()
	for _, m := range rt.modules {
		if h, ok := m.(M); ok {
			return h
		}
	}
	var none M
	t.Fatalf("router has no %T module", none)
	return none
}

func newJWTTestRouter(t *testing.T) *Router {
	t.Helper()
	return newTestRouter(testutil.NewFakeBackend(), true)
}

func jwtBearer(t *testing.T, rt *Router, userID, email string) string {
	t.Helper()
	pair, err := rt.security.AuthMgr.Issue(userID, email, auth.RoleAdmin)
	if err != nil {
		t.Fatalf("issue jwt: %v", err)
	}
	return "Bearer " + pair.AccessToken
}

func doRequest(t *testing.T, rt *Router, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	return doRequestWithAuth(t, rt, method, path, "Bearer "+testToken, body)
}

func doRequestWithAuth(t *testing.T, rt *Router, method, path, authHeader string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var b bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&b).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &b)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}

func decodeJSON(t *testing.T, rr *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.NewDecoder(rr.Body).Decode(v); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

func newLibraryTestRouter(t *testing.T) (*Router, func(id, ownerID string)) {
	t.Helper()
	backend := testutil.NewFakeBackend()
	rt := newTestRouter(backend, true)
	seed := func(id, ownerID string) {
		doc := &storageif.FlowDocument{
			ID:      id,
			Name:    "test",
			OwnerID: ownerID,
		}
		if err := backend.SaveFlow(context.Background(), doc); err != nil {
			t.Fatalf("seed flow %s: %v", id, err)
		}
	}
	return rt, seed
}

func checkStatus(t *testing.T, rr *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rr.Code != want {
		t.Errorf("expected status %d, got %d — body: %s", want, rr.Code, rr.Body.String())
	}
}

// newRecorder sends req through rt and returns the recorded response.
// Unlike doRequest / doRequestWithAuth it accepts a fully formed *http.Request
// so callers can set custom RemoteAddr, headers, etc.
func newRecorder(t *testing.T, rt *Router, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}

// seedOrg creates an org owned by ownerID (who becomes its sole admin) and
// returns the org ID.
func seedOrg(t *testing.T, rt *Router, name, ownerID string) string {
	t.Helper()
	org, err := rt.security.OrgSvc.Create(context.Background(), name, ownerID)
	if err != nil {
		t.Fatalf("seed org %s: %v", name, err)
	}
	return org.ID
}

// mirrorOrg copies the org from the router's in-memory org store into the backend, which holds both on Postgres:
// rows keyed by the org (its settings) need it there.
func mirrorOrg(t *testing.T, rt *Router, orgID string) {
	t.Helper()
	ctx := context.Background()
	org, err := rt.security.OrgSvc.Get(ctx, orgID)
	if err != nil {
		t.Fatalf("load org %s: %v", orgID, err)
	}
	if err := rt.security.Backend.SaveOrg(ctx, org); err != nil {
		t.Fatalf("mirror org %s: %v", orgID, err)
	}
}

// addOrgMember adds userID to the org with the given role.
func addOrgMember(t *testing.T, rt *Router, orgID, userID string, role auth.Role) {
	t.Helper()
	if err := rt.security.OrgSvc.AddMember(context.Background(), orgID, userID, role); err != nil {
		t.Fatalf("add org member %s: %v", userID, err)
	}
}

// seedOrgFlow inserts a flow that belongs to an org directly via the backend.
func seedOrgFlow(t *testing.T, rt *Router, id, ownerID, orgID string) {
	t.Helper()
	doc := &storageif.FlowDocument{
		ID:             id,
		Name:           "test",
		OwnerID:        ownerID,
		OrganizationID: orgID,
	}
	if err := rt.security.Backend.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("seed org flow %s: %v", id, err)
	}
}

// seedUserWithRole inserts a user with the given role directly via the backend.
func seedUserWithRole(t *testing.T, rt *Router, id, email string, role auth.Role) {
	t.Helper()
	u := &storageif.User{
		ID:       id,
		Email:    email,
		Password: "hash",
		Role:     role,
	}
	if err := rt.security.Backend.SaveUser(context.Background(), u); err != nil {
		t.Fatalf("seed user %s: %v", id, err)
	}
}

// ensureUser seeds a member user row for id unless one exists: a collaborator grant needs its user, as the
// foreign key does on Postgres.
func ensureUser(t *testing.T, rt *Router, id string) {
	t.Helper()
	if _, err := rt.security.Backend.LoadUserByID(context.Background(), id); errors.Is(err, storageif.ErrNotFound) {
		seedUserWithRole(t, rt, id, id+"@example.com", auth.RoleMember)
	}
}

func newBadBodyRequest(t *testing.T, rt *Router, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString("not-json"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}

// testRouterBuses maps each test router to the event bus its services publish on, for tests that subscribe.
var testRouterBuses sync.Map

// busOf returns the event bus rt's services publish on.
func busOf(t *testing.T, rt *Router) *eventbus.Bus {
	t.Helper()
	b, ok := testRouterBuses.Load(rt)
	if !ok {
		t.Fatal("not a test router built by newTestRouterSSO")
	}
	return b.(*eventbus.Bus)
}
