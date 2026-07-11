package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"

	"go.uber.org/fx"
)

type mockNotifier = service.NilNotifier

// memIdentityStore is an in-memory IdentityStore for SSO handler tests (the
// filesystem backend used by the test router doesn't persist identity links).
type memIdentityStore struct {
	links map[string]*storageif.IdentityLink // key: provider + "\x00" + subject
}

func newMemIdentityStore() *memIdentityStore {
	return &memIdentityStore{links: map[string]*storageif.IdentityLink{}}
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
func newTestRouterSSO(backend storageif.StorageBackend, jwtEnabled bool, ssoClient SSOClient, identityStore IdentityStore) *Router {
	cfg := config.Default()
	cfg.Auth.Enabled = jwtEnabled
	cfg.Auth.Secret = testToken

	notifier := &mockNotifier{}
	settings, _ := storage.NewSettingsStore()
	sysSvc := service.NewSystemService(settings, storage.CurrentSecretStore(), notifier, backend, cfg.Mode)
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
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	authzSvc := service.NewAuthzService(backend, orgSvc)

	flowSvc := service.NewFlowService(notifier, settings, docProv, backend, authzSvc, nil)
	libSvc := service.NewLibraryService(backend, flowSvc, authzSvc)
	analysisSvc := service.NewAnalysisService(notifier, settings, nil)
	exportSvc := service.NewExportService(notifier, flowSvc, analysisSvc)

	demo := ai.NewDemoLimiter("")
	factory := ai.NewProviderFactory(func(s, p string) (string, error) { return "test", nil }, nil, nil, nil)
	chatSvc := service.NewChatService(notifier, "", flowSvc, analysisSvc, settings, factory, demo, backend, config.ModeLocal)

	ghAuth := ai.NewGitHubAuth()
	cpAuth := ai.NewCopilotAuth()
	providerSvc := service.NewProviderService(ghAuth, cpAuth, factory, storage.CurrentSecretStore())

	security := &SecurityConfig{
		JWTEnabled:     jwtEnabled,
		LocalUserID:    "local",
		LocalName:      "You",
		Token:          testToken,
		AuthMgr:        authMgr,
		Backend:        backend,
		OrgSvc:         orgSvc,
		TrustedProxies: cfg.Server.TrustedProxies,
	}

	eventManager := NewEventManager(make(chan struct{}))

	dashboardSvc := service.NewDashboardService(backend, analysisSvc, flowSvc)
	handlers := Handlers{
		Sys:       NewSystemHandler(sysSvc, security, backend),
		Flow:      NewFlowHandler(flowSvc, docProv, backend, security),
		Library:   NewLibraryHandler(libSvc, backend, security),
		Chat:      NewChatHandler(chatSvc, flowSvc, security),
		Analysis:  NewAnalysisHandler(analysisSvc, flowSvc, dashboardSvc, backend, security),
		Dashboard: NewDashboardHandler(dashboardSvc, security),
		Export:    NewExportHandler(exportSvc, flowSvc, analysisSvc, security),
		Auth:      NewAuthHandler(nil, backend, security, ssoClient, identityStore, mailer.NewService(config.EmailConfig{}, config.ModeLocal)),
		Admin:     NewAdminHandler(backend, security, NewMigrationRunner(nil), nil),
		Provider:  NewProviderHandler(providerSvc, security),
		Org:       NewOrgHandler(orgSvc, backend, nil, security, mailer.NewService(config.EmailConfig{}, config.ModeLocal)),
		Sharing:   NewSharingHandler(backend, flowSvc, security),
	}

	return NewRouter(noopLifecycle{}, security, eventManager, handlers, cfg, make(chan struct{}), nil, nil)
}

func newJWTTestRouter(t *testing.T) *Router {
	t.Helper()
	fs, _ := filesystem.NewLocalStorageBackend(t.TempDir())
	return newTestRouter(fs, true)
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
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("create local storage: %v", err)
	}
	rt := newTestRouter(fs, true)
	seed := func(id, ownerID string) {
		doc := &storageif.FlowDocument{
			ID:      id,
			Name:    "test",
			OwnerID: ownerID,
		}
		if err := fs.SaveFlow(context.Background(), doc); err != nil {
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

func newBadBodyRequest(t *testing.T, rt *Router, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString("not-json"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}
