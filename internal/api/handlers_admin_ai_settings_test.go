package api

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/secret"
	"pad-analyzer/internal/service"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/pgtest"
)

const deploymentAISettingsPath = "/api/admin/settings/ai"

// newDeploymentSettingsRouter is a cloud router whose SystemService reads the deployment document through a
// BackendSettingsProvider over backend, as di wires it (newTestRouter keeps an in-memory settings store): what a
// PUT answers depends on what that provider's re-read can see while the request's transaction is open.
func newDeploymentSettingsRouter(t *testing.T, backend storageif.StorageBackend) *Router {
	t.Helper()
	cfg := config.Default()
	cfg.Auth.Enabled = true
	cfg.Auth.Secret = secret.New(testToken)
	routePolicyFromEnv(cfg)
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	security := &SecurityConfig{
		JWTEnabled:  true,
		LocalUserID: "local",
		LocalName:   "You",
		Token:       secret.New(testToken),
		AuthMgr:     auth.NewManager(testToken, auth.NewTokenBlacklist()),
		Backend:     backend,
		RequestTx:   testRequestTx(true, backend),
		Idempotency: testIdempotency(true, backend),
		OrgSvc:      orgSvc,
	}
	settings := service.NewBackendSettingsProvider(context.Background(), backend)
	sysSvc := service.NewSystemService(settings, nil, &mockNotifier{}, backend, config.ModeCloud, nil, nil)
	authzSvc := service.NewAuthzService(backend, orgSvc)
	modules := []Module{NewSystemHandler(sysSvc, security, testAuditRecorder(backend), nil, authzSvc, service.NewHealthService(backend, testBlob(backend)))}
	rt, err := NewRouter(RouterParams{Lifecycle: noopLifecycle{}, Security: security, Events: NewEventManager(make(chan struct{})), Modules: modules, Config: cfg, ShutdownCh: make(chan struct{})})
	if err != nil {
		t.Fatalf("NewRouter: %v", err)
	}
	rt.caps = testCaps(true, backend)
	return rt
}

// adminAndMember seeds an admin and a member and returns their sessions.
func adminAndMember(t *testing.T, rt *Router) (admin, member string) {
	t.Helper()
	seedUserWithRole(t, rt, "ai-admin", "ai-admin@example.com", auth.RoleAdmin)
	seedUserWithRole(t, rt, "ai-member", "ai-member@example.com", auth.RoleMember)
	return sessionBearer(t, rt, "ai-admin", auth.RoleAdmin), sessionBearer(t, rt, "ai-member", auth.RoleMember)
}

func getDeploymentAISettings(t *testing.T, rt *Router, bearer string) service.DeploymentAISettings {
	t.Helper()
	rr := doRequestWithAuth(t, rt, http.MethodGet, deploymentAISettingsPath, bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var got service.DeploymentAISettings
	decodeJSON(t, rr, &got)
	return got
}

// TestDeploymentAISettings_Routes pins the admin AI settings contract: a member is refused, a refused value
// answers 400 naming the field, a write answers with the values it wrote (which the next read serves) and records
// an audit row that says what the deployment's values became. On the fake backend, through both settings stores.
func TestDeploymentAISettings_Routes(t *testing.T) {
	for _, tc := range []struct {
		name string
		rt   func(t *testing.T) *Router
	}{
		{"backend settings provider", func(t *testing.T) *Router { return newDeploymentSettingsRouter(t, testutil.NewFakeBackend()) }},
		{"in-memory settings store", func(t *testing.T) *Router { return newTestRouter(testutil.NewFakeBackend(), true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rt := tc.rt(t)
			admin, member := adminAndMember(t, rt)
			in := service.DeploymentAISettings{EmbeddingProvider: "gemini", EmbeddingModel: "text-embedding-004"}

			checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, deploymentAISettingsPath, member, nil), http.StatusForbidden)
			checkStatus(t, doRequestWithAuth(t, rt, http.MethodPut, deploymentAISettingsPath, member, in), http.StatusForbidden)

			before := getDeploymentAISettings(t, rt, admin)
			for _, bad := range []struct {
				body service.DeploymentAISettings
				want string
			}{
				{service.DeploymentAISettings{EmbeddingProvider: "claude"}, "ai.embeddingProvider"},
				{service.DeploymentAISettings{EmbeddingProvider: "azure-foundry"}, "PAD_EMBEDDING_PROVIDER=azure-foundry"},
				{service.DeploymentAISettings{EmbeddingModel: strings.Repeat("m", 201)}, "ai.embeddingModel"},
			} {
				rr := doRequestWithAuth(t, rt, http.MethodPut, deploymentAISettingsPath, admin, bad.body)
				checkStatus(t, rr, http.StatusBadRequest)
				if env := errorEnvelope(t, rr); env.Code != "BAD_REQUEST" || !strings.Contains(env.Message, bad.want) {
					t.Errorf("PUT %+v = %s %q, want BAD_REQUEST naming %s", bad.body, env.Code, env.Message, bad.want)
				}
			}
			if got := getDeploymentAISettings(t, rt, admin); got != before {
				t.Errorf("a refused write changed the settings from %+v to %+v", before, got)
			}

			ch := captureAuditEvents(t, 1)
			rr := doRequestWithAuth(t, rt, http.MethodPut, deploymentAISettingsPath, admin, in)
			checkStatus(t, rr, http.StatusOK)
			var got service.DeploymentAISettings
			decodeJSON(t, rr, &got)
			if got != in {
				t.Errorf("PUT answered %+v, want the written %+v", got, in)
			}
			if got := getDeploymentAISettings(t, rt, admin); got != in {
				t.Errorf("GET after the write = %+v, want %+v", got, in)
			}
			ev := nextAuditEvent(t, ch)
			if ev.ResourceType != "settings" || ev.ResourceID != "deployment" || ev.UserID != "ai-admin" ||
				ev.Meta["embeddingProvider"] != "gemini" || ev.Meta["embeddingModel"] != "text-embedding-004" {
				t.Errorf("audit row = %+v, want settings/deployment by ai-admin with the written values in meta", ev)
			}
		})
	}
}

// TestDeploymentAISettings_PutAnswersWithTheWrite_Postgres: the write joins the request's RLS transaction, and
// the provider's cache refresh reads on a pool connection, outside it, where the row is still the old one until
// the response commits; a PUT answered from that re-read carried the pre-write values. Skips without
// DATABASE_URL.
func TestDeploymentAISettings_PutAnswersWithTheWrite_Postgres(t *testing.T) {
	pg, err := storagedb.New(context.Background(), storagedb.DefaultConfig(pgtest.NewDatabase(t, "")))
	if err != nil {
		t.Fatalf("open the test database: %v", err)
	}
	t.Cleanup(func() { pg.Close() })
	rt := newDeploymentSettingsRouter(t, pg)
	admin, _ := adminAndMember(t, rt)

	before := getDeploymentAISettings(t, rt, admin)
	in := service.DeploymentAISettings{EmbeddingProvider: "gemini", EmbeddingModel: before.EmbeddingModel + "-next"}
	rr := doRequestWithAuth(t, rt, http.MethodPut, deploymentAISettingsPath, admin, in)
	checkStatus(t, rr, http.StatusOK)
	var got service.DeploymentAISettings
	decodeJSON(t, rr, &got)
	if got != in {
		t.Errorf("PUT answered %+v, want the written %+v", got, in)
	}
	if got := getDeploymentAISettings(t, rt, admin); got != in {
		t.Errorf("GET after the commit = %+v, want %+v", got, in)
	}
}
