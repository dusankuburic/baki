package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/egress"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// orgProvidersFixture is an organisation with an admin and a plain member,
// a provider service holding organisation credentials, and one org handler
// per caller (local-mode security: the caller is LocalUserID).
type orgProvidersFixture struct {
	orgID   string
	orgSvc  *collaboration.OrgService
	secrets *testutil.FakeSecretStore
	orgKeys *testutil.FakeOrgSecretStore
	svc     *service.ProviderService
	backend storageif.StorageBackend
}

// namedUsersBackend resolves the admin's display name for the "shared by"
// label; every other user is unknown.
type namedUsersBackend struct {
	*testutil.FakeBackend
}

func (b *namedUsersBackend) LoadUserByID(_ context.Context, id string) (*storageif.User, error) {
	if id == "u-admin" {
		return &storageif.User{ID: id, Email: "admin@example.com", DisplayName: "Ada Admin"}, nil
	}
	return nil, storageif.ErrNotFound
}

func newOrgProvidersFixture(t *testing.T, withOrgStore bool) *orgProvidersFixture {
	t.Helper()
	ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{Egress: egress.AllowLoopback, ExtraAllowedHosts: []string{"unit-test.invalid"}})
	ai.ResetAzureFoundryCaches()
	t.Cleanup(func() { ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{}) })

	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(bgCtx(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(bgCtx(), org.ID, "u-member", auth.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()
	factory := ai.NewProviderFactory(func(scope, provider string) (string, error) {
		v, _ := secrets.Get(scope, provider)
		return v, nil
	}, nil, nil, nil)
	svc := service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets)
	if withOrgStore {
		factory.SetOrgKeyLookup(orgKeys.OrgKeyLookup)
		svc.SetOrgKeyStore(orgKeys)
	}
	return &orgProvidersFixture{orgID: org.ID, orgSvc: orgSvc, secrets: secrets, orgKeys: orgKeys, svc: svc, backend: &namedUsersBackend{FakeBackend: testutil.NewFakeBackend()}}
}

// router serves the org credential routes as caller.
func (f *orgProvidersFixture) router(caller string) http.Handler {
	security := &SecurityConfig{LocalUserID: caller, OrgSvc: f.orgSvc}
	h := NewOrgHandler(f.orgSvc, f.backend, nil, security, mailer.NewService(config.EmailConfig{}, config.ModeLocal), nil, f.svc)
	r := chi.NewRouter()
	r.Route("/api/orgs/{id}/providers/azure-foundry", func(r chi.Router) {
		r.Get("/config", h.handleOrgAzureFoundryConfigGet)
		r.Post("/config", h.handleOrgAzureFoundryConfigSave)
		r.Delete("/config", h.handleOrgAzureFoundryConfigDelete)
		r.Post("/test", h.handleOrgAzureFoundryTest)
	})
	return r
}

func (f *orgProvidersFixture) do(t *testing.T, caller, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if s, ok := body.(string); ok {
			buf.WriteString(s)
		} else if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.router(caller).ServeHTTP(rr, req)
	return rr
}

func (f *orgProvidersFixture) configPath() string {
	return "/api/orgs/" + f.orgID + "/providers/azure-foundry/config"
}

var orgAzureBody = map[string]any{
	"endpoint":       "https://unit-test.invalid",
	"auth":           map[string]any{"mode": "api_key", "key": "org-secret-key"},
	"chatDeployment": "org-chat",
}

// G7 authz: admins write, members read (never the secret), non-members get
// nothing, and every write is audited without the secret.
func TestOrgAzureFoundryConfig_Authz(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	path := f.configPath()

	t.Run("member cannot save or delete", func(t *testing.T) {
		checkStatus(t, f.do(t, "u-member", http.MethodPost, path, orgAzureBody), http.StatusForbidden)
		checkStatus(t, f.do(t, "u-member", http.MethodDelete, path, nil), http.StatusForbidden)
		checkStatus(t, f.do(t, "u-member", http.MethodPost, "/api/orgs/"+f.orgID+"/providers/azure-foundry/test", nil), http.StatusForbidden)
		if has, _ := f.orgKeys.HasOrg(f.orgID, "azure-foundry"); has {
			t.Error("a member's save must not store anything")
		}
	})

	t.Run("non-member sees nothing", func(t *testing.T) {
		checkStatus(t, f.do(t, "u-stranger", http.MethodGet, path, nil), http.StatusForbidden)
		checkStatus(t, f.do(t, "u-stranger", http.MethodPost, path, orgAzureBody), http.StatusForbidden)
		checkStatus(t, f.do(t, "u-stranger", http.MethodDelete, path, nil), http.StatusForbidden)
	})

	t.Run("unknown org", func(t *testing.T) {
		// requireMember answers 403 for an org the caller is not in (it does
		// not exist, so nobody is); requireAdmin reports it as 404.
		checkStatus(t, f.do(t, "u-admin", http.MethodGet, "/api/orgs/nope/providers/azure-foundry/config", nil), http.StatusForbidden)
		checkStatus(t, f.do(t, "u-admin", http.MethodPost, "/api/orgs/nope/providers/azure-foundry/config", orgAzureBody), http.StatusNotFound)
	})

	t.Run("member reads unconfigured", func(t *testing.T) {
		rr := f.do(t, "u-member", http.MethodGet, path, nil)
		checkStatus(t, rr, http.StatusOK)
		got := azureDecode(t, rr)
		if got["configured"] != false || got["shared"] != false {
			t.Errorf("empty config = %v", got)
		}
	})

	t.Run("admin saves with an audit row", func(t *testing.T) {
		prevCh := auditCh
		auditCh = make(chan *storageif.AuditEvent, 1)
		auditClosed.Store(false)
		t.Cleanup(func() {
			auditCh = prevCh
			auditClosed.Store(false)
		})
		rr := f.do(t, "u-admin", http.MethodPost, path, map[string]any{
			"endpoint":       "https://unit-test.invalid",
			"auth":           map[string]any{"mode": " API_KEY ", "key": "org-secret-key"},
			"chatDeployment": "org-chat",
		})
		checkStatus(t, rr, http.StatusOK)
		select {
		case ev := <-auditCh:
			if ev.Action != AuditActionKeysSave || ev.ResourceType != "org" || ev.ResourceID != f.orgID ||
				ev.Meta["op"] != "save" || ev.Meta["provider"] != ai.AzureFoundryProviderID || ev.Meta["authMode"] != "api_key" {
				t.Errorf("audit event = %+v (meta %v)", ev, ev.Meta)
			}
			for k, v := range ev.Meta {
				if strings.Contains(v, "org-secret-key") {
					t.Errorf("audit meta %s leaks the secret", k)
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no audit event was enqueued")
		}
		raw, err := f.orgKeys.GetOrg(f.orgID, "azure-foundry")
		if err != nil || !strings.Contains(raw, "org-secret-key") {
			t.Errorf("stored document = %q, %v", raw, err)
		}
	})

	t.Run("member reads the public form, never the secret", func(t *testing.T) {
		rr := f.do(t, "u-member", http.MethodGet, path, nil)
		checkStatus(t, rr, http.StatusOK)
		body := rr.Body.String()
		if strings.Contains(body, "org-secret-key") {
			t.Fatalf("response leaks the secret: %s", body)
		}
		got := azureDecode(t, rr)
		if got["configured"] != true || got["shared"] != true || got["hasSecret"] != true ||
			got["endpoint"] != "https://unit-test.invalid" || got["chatDeployment"] != "org-chat" || got["authMode"] != "api_key" {
			t.Errorf("public config = %v", got)
		}
		if got["updatedBy"] != "Ada Admin" {
			t.Errorf("updatedBy = %v, want the admin's display name", got["updatedBy"])
		}
		if at, _ := got["updatedAt"].(string); at == "" {
			t.Error("updatedAt missing")
		} else if _, err := time.Parse(time.RFC3339, at); err != nil {
			t.Errorf("updatedAt %q is not RFC 3339: %v", at, err)
		}
	})

	t.Run("validation codes match the per-user route", func(t *testing.T) {
		cases := []struct {
			name string
			body any
			code string
		}{
			{"unknown field", `{"endpoint":"https://unit-test.invalid","auth":{"mode":"api_key","key":"k"},"chatDeploymnt":"c"}`, ai.AzureCfgErrInvalidConfig},
			{"host not allowed", map[string]any{"endpoint": "https://evil.example", "auth": map[string]any{"mode": "api_key", "key": "k"}, "chatDeployment": "c"}, ai.AzureCfgErrHostNotAllowed},
			{"missing deployment", map[string]any{"endpoint": "https://unit-test.invalid", "auth": map[string]any{"mode": "api_key", "key": "k"}}, ai.AzureCfgErrMissingDeploy},
			{"project host", map[string]any{"endpoint": "https://unit-test.invalid", "auth": map[string]any{"mode": "entra", "tenantId": "11111111-1111-1111-1111-111111111111",
				"clientId": "22222222-2222-2222-2222-222222222222", "clientSecret": "s"}, "projectEndpoint": "https://apim.contoso.com/api/projects/p"}, ai.AzureCfgErrInvalidProject},
		}
		for _, tc := range cases {
			rr := f.do(t, "u-admin", http.MethodPost, path, tc.body)
			checkStatus(t, rr, http.StatusBadRequest)
			if got := azureDecode(t, rr); got["code"] != tc.code {
				t.Errorf("%s: code = %v, want %s (body %s)", tc.name, got["code"], tc.code, rr.Body.String())
			}
		}
		raw, _ := f.orgKeys.GetOrg(f.orgID, "azure-foundry")
		if !strings.Contains(raw, "org-chat") {
			t.Error("a rejected save replaced the stored document")
		}
	})

	t.Run("admin deletes with an audit row", func(t *testing.T) {
		prevCh := auditCh
		auditCh = make(chan *storageif.AuditEvent, 1)
		auditClosed.Store(false)
		t.Cleanup(func() {
			auditCh = prevCh
			auditClosed.Store(false)
		})
		rr := f.do(t, "u-admin", http.MethodDelete, path, nil)
		checkStatus(t, rr, http.StatusOK)
		if got := azureDecode(t, rr); got["status"] != "ok" {
			t.Errorf("delete body = %v", got)
		}
		select {
		case ev := <-auditCh:
			if ev.Action != AuditActionKeysSave || ev.ResourceID != f.orgID || ev.Meta["op"] != "delete" {
				t.Errorf("audit event = %+v (meta %v)", ev, ev.Meta)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no audit event was enqueued")
		}
		if has, _ := f.orgKeys.HasOrg(f.orgID, "azure-foundry"); has {
			t.Error("delete left the row")
		}
	})
}

// The admin's connection test runs on the org's credential only.
func TestOrgAzureFoundryTest_OrgCredentialOnly(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	_ = f.secrets.Save("", "azure-foundry", `{"endpoint":"https://unit-test.invalid","auth":{"mode":"api_key","key":"own"},"chatDeployment":"own-chat"}`)
	rr := f.do(t, "u-admin", http.MethodPost, "/api/orgs/"+f.orgID+"/providers/azure-foundry/test", nil)
	checkStatus(t, rr, http.StatusOK)
	got := azureDecode(t, rr)
	if got["ok"] != false || got["error"] == "" {
		t.Errorf("without an org credential the test must fail, got %v", got)
	}
}

// Without an org keystore (desktop, local mode) every route is a clean 404
// ORG_KEYSTORE_UNAVAILABLE — for admins too, before any membership check.
func TestOrgAzureFoundryConfig_NoOrgKeystore(t *testing.T) {
	f := newOrgProvidersFixture(t, false)
	path := f.configPath()
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, path}, {http.MethodPost, path}, {http.MethodDelete, path},
		{http.MethodPost, "/api/orgs/" + f.orgID + "/providers/azure-foundry/test"},
	} {
		rr := f.do(t, "u-admin", tc.method, tc.path, orgAzureBody)
		checkStatus(t, rr, http.StatusNotFound)
		if got := azureDecode(t, rr); got["code"] != "ORG_KEYSTORE_UNAVAILABLE" {
			t.Errorf("%s %s: code = %v (body %s)", tc.method, tc.path, got["code"], rr.Body.String())
		}
	}
	// A nil provider service (older test fixtures) degrades the same way.
	h := NewOrgHandler(f.orgSvc, nil, nil, &SecurityConfig{LocalUserID: "u-admin"}, mailer.NewService(config.EmailConfig{}, config.ModeLocal), nil, nil)
	rr := httptest.NewRecorder()
	h.handleOrgAzureFoundryConfigGet(rr, httptest.NewRequest(http.MethodGet, path, nil))
	checkStatus(t, rr, http.StatusNotFound)
}

// GET /api/providers/list?orgId= includes the org's shared credential for a
// member and is silently ignored for a non-member.
func TestListProviders_OrgIdOnlyForMembers(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	if _, err := f.svc.SaveOrgAzureFoundryConfig(f.orgID, "u-admin", "", ai.AzureFoundryConfig{
		Endpoint: "https://unit-test.invalid", Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k"}, ChatDeployment: "org-chat"}); err != nil {
		t.Fatal(err)
	}
	list := func(caller string) map[string]any {
		t.Helper()
		h := NewProviderHandler(f.svc, &SecurityConfig{LocalUserID: caller, OrgSvc: f.orgSvc}, nil)
		rr := httptest.NewRecorder()
		h.handleListProviders(rr, httptest.NewRequest(http.MethodGet, "/api/providers/list?orgId="+f.orgID, nil))
		checkStatus(t, rr, http.StatusOK)
		var providers []map[string]any
		decodeJSON(t, rr, &providers)
		for _, p := range providers {
			if p["id"] == "azure-foundry" {
				return p
			}
		}
		t.Fatal("azure-foundry missing from the listing")
		return nil
	}
	if az := list("u-member"); az["configured"] != true || az["shared"] != true || az["defaultModel"] != "org-chat" {
		t.Errorf("member: %v, want configured and shared", az)
	}
	if az := list("u-stranger"); az["configured"] != false || az["shared"] != nil {
		t.Errorf("non-member: %v, want the parameter ignored", az)
	}
}
