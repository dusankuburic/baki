package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
)

type mockNotifier struct{}
func (m *mockNotifier) Emit(name string, data any) {}

func newTestRouter(backend storageif.StorageBackend, jwtEnabled bool) *Router {
	cfg := config.Default()
	cfg.Auth.Enabled = jwtEnabled
	cfg.Auth.Secret = testToken

	notifier := &mockNotifier{}
	settings, _ := storage.NewSettingsStore()
	sysSvc := service.NewSystemService(settings, notifier)
	docProv := service.DocumentProvider(service.NewLocalDocumentProvider())
	if jwtEnabled {
		docProv = service.NewCloudDocumentProvider(backend)
	}
	
	authMgr := auth.NewManager(testToken)
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	
	flowSvc := service.NewFlowService(notifier, settings, docProv, backend, orgSvc)
	libSvc := service.NewLibraryService(backend, flowSvc)
	analysisSvc := service.NewAnalysisService(notifier, settings)
	exportSvc := service.NewExportService(context.Background(), notifier, flowSvc, analysisSvc)
	
	demo := ai.NewDemoLimiter("")
	factory := ai.NewProviderFactory(func(s, p string) (string, error) { return "test", nil }, nil)
	chatSvc := service.NewChatService(notifier, "", flowSvc, analysisSvc, settings, factory, demo)
	
	ghAuth := ai.NewGitHubAuth()
	cpAuth := ai.NewCopilotAuth()
	providerSvc := service.NewProviderService(ghAuth, cpAuth, factory)
	
	security := &SecurityConfig{
		JWTEnabled:  jwtEnabled,
		LocalUserID: "local",
		LocalName:   "You",
		Token:       testToken,
		AuthMgr:     authMgr,
		Backend:     backend,
		OrgSvc:      orgSvc,
	}
	
	eventManager := NewEventManager(make(chan struct{}))
	
	handlers := Handlers{
		Sys:      NewSystemHandler(sysSvc, security),
		Flow:     NewFlowHandler(flowSvc, docProv, backend, security),
		Library:  NewLibraryHandler(libSvc, security),
		Chat:     NewChatHandler(chatSvc, flowSvc, security),
		Analysis: NewAnalysisHandler(analysisSvc, flowSvc, security),
		Export:   NewExportHandler(exportSvc, flowSvc, security),
		Auth:     NewAuthHandler(nil, backend, security),
		Admin:    NewAdminHandler(backend, security),
		Provider: NewProviderHandler(providerSvc, security),
		Org:      NewOrgHandler(orgSvc, backend, security),
		Sharing:  NewSharingHandler(backend, flowSvc, security),
	}

	return NewRouter(security, eventManager, handlers, cfg, make(chan struct{}))
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

var badBody = bytes.NewBufferString("not-json")

func newBadBodyRequest(t *testing.T, rt *Router, method, path string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString("not-json"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	return rr
}
