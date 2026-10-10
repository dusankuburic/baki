package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/events"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/platform/eventbus"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// orgProvidersFixture is an organisation with an admin and a plain member,
// a provider service holding organisation credentials with the Azure AI
// Foundry model registry wired as DI wires it (per-user and organisation
// registries), and one org handler per caller (local-mode security: the
// caller is LocalUserID).
type orgProvidersFixture struct {
	orgID    string
	orgSvc   *collaboration.OrgService
	secrets  *testutil.FakeSecretStore
	orgKeys  *testutil.FakeOrgSecretStore
	svc      *service.ProviderService
	backend  storageif.StorageBackend
	factory  *ai.ProviderFactory
	registry *testutil.FakeAzureRegistry
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

	bus := eventbus.New(nil, 0)
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore(), collaboration.WithEventBus(bus))
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
	f := &orgProvidersFixture{orgID: org.ID, orgSvc: orgSvc, secrets: secrets, orgKeys: orgKeys,
		backend: &namedUsersBackend{FakeBackend: testutil.NewFakeBackend()}, factory: factory, registry: testutil.NewFakeAzureRegistry()}
	if withOrgStore {
		factory.SetOrgKeyLookup(orgKeys.OrgKeyLookup)
		f.useOrgStore(orgKeys)
	} else {
		f.useOrgStore(nil)
	}
	// As APIModule subscribes it, on the provider service and backend the fixture holds at delivery.
	eventbus.Subscribe(bus, eventbus.Async, "lent-credential-revocation", func(ctx context.Context, e events.OrgMemberChanged) {
		revokeLentCredentials(ctx, testAuditRecorder(f.backend), f.svc, e)
	}, eventbus.InlineWhenSaturated())
	return f
}

// useOrgStore builds the provider service and its model registry (on the
// fixture's registry rows) with the organisation credential store — nil for
// none — as DI wires one store into both.
func (f *orgProvidersFixture) useOrgStore(store service.OrgKeyStore) {
	azure, _ := newTestAzureModels(testAzureModelsDeps{
		Registry: f.registry, Secrets: f.secrets, OrgKeys: store, Factory: f.factory,
		Orgs: f.orgSvc, Users: f.backend.(service.AzureUserDirectory), PerUser: true,
	})
	f.svc = service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), f.factory, f.secrets, store, azure)
}

// orgModels lists the organisation's registry models (the service directly,
// as its admin).
func (f *orgProvidersFixture) orgModels(t *testing.T) []service.AzureModelView {
	t.Helper()
	out, err := f.svc.AzureModels().ListModels(bgCtx(), service.AzureScope{Scope: "u-admin", OrgID: f.orgID})
	if err != nil {
		t.Fatalf("list the org's models: %v", err)
	}
	return out.Models
}

// orgModelDeployments is orgModels as "kind:deployment" strings.
func (f *orgProvidersFixture) orgModelDeployments(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, m := range f.orgModels(t) {
		out = append(out, m.Kind+":"+m.Deployment)
	}
	sort.Strings(out)
	return out
}

// router serves the org credential routes as caller.
func (f *orgProvidersFixture) router(caller string) http.Handler {
	security := &SecurityConfig{LocalUserID: caller, OrgSvc: f.orgSvc}
	h := NewOrgHandler(f.orgSvc, f.backend, nil, security, mailer.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(f.backend, f.backend, nil), f.svc, service.NewAuthzService(f.backend, f.orgSvc), service.NewChannelService(f.backend), service.NewUserDirectory(f.backend), testAuditRecorder(f.backend), nil)
	return mountedAs(h, caller)
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
		// canOrg answers a member action on an org the caller is not in with 403 (it
		// does not exist, so nobody is); for OrgAdmin it answers 404.
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
		auditCh := captureAuditEvents(t, 1)
		rr := f.do(t, "u-admin", http.MethodPost, path, map[string]any{
			"endpoint":       "https://unit-test.invalid",
			"auth":           map[string]any{"mode": " API_KEY ", "key": "org-secret-key"},
			"chatDeployment": "org-chat",
		})
		checkStatus(t, rr, http.StatusOK)
		select {
		case ev := <-auditCh:
			if ev.Action != audit.ActionKeysSave || ev.ResourceType != "org" || ev.ResourceID != f.orgID ||
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
		// The deprecated save maps its deployment fields onto the
		// organisation's resource "default": the chat deployment is a model
		// there — the default for members — and the document is stored as
		// saved, so a rolled-back binary still finds its chat deployment.
		if !strings.Contains(raw, `"chatDeployment":"org-chat"`) {
			t.Errorf("stored document = %q, want it as saved (chatDeployment org-chat)", raw)
		}
		models := f.orgModels(t)
		if len(models) != 1 || models[0].ConnectionID != "default" || models[0].Kind != "chat" ||
			models[0].Deployment != "org-chat" || !models[0].IsDefault || !models[0].Shared {
			t.Errorf("org models after the save = %+v, want org-chat on resource default, the default for members", models)
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
		// A rejected save changes nothing: neither the stored credential nor
		// the resource's models.
		raw, _ := f.orgKeys.GetOrg(f.orgID, "azure-foundry")
		if !strings.Contains(raw, "org-secret-key") {
			t.Error("a rejected save replaced the stored document")
		}
		if got := f.orgModelDeployments(t); !reflect.DeepEqual(got, []string{"chat:org-chat"}) {
			t.Errorf("org models after the rejected saves = %v, want [chat:org-chat]", got)
		}
	})

	t.Run("admin deletes with an audit row", func(t *testing.T) {
		auditCh := captureAuditEvents(t, 1)
		rr := f.do(t, "u-admin", http.MethodDelete, path, nil)
		checkStatus(t, rr, http.StatusOK)
		if got := azureDecode(t, rr); got["status"] != "ok" {
			t.Errorf("delete body = %v", got)
		}
		select {
		case ev := <-auditCh:
			if ev.Action != audit.ActionKeysSave || ev.ResourceID != f.orgID || ev.Meta["op"] != "delete" {
				t.Errorf("audit event = %+v (meta %v)", ev, ev.Meta)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("no audit event was enqueued")
		}
		if has, _ := f.orgKeys.HasOrg(f.orgID, "azure-foundry"); has {
			t.Error("delete left the row")
		}
		// The deprecated delete removes the organisation's resource
		// "default" together with its models.
		if got := f.orgModelDeployments(t); len(got) != 0 {
			t.Errorf("org models after the delete = %v, want none", got)
		}
		conns, err := f.svc.AzureModels().ListConnections(bgCtx(), service.AzureScope{Scope: "u-admin", OrgID: f.orgID})
		if err != nil || len(conns.Connections) != 0 {
			t.Errorf("org resources after the delete = %+v, %v; want none", conns.Connections, err)
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
	h := NewOrgHandler(f.orgSvc, nil, nil, &SecurityConfig{LocalUserID: "u-admin"}, mailer.NewService(config.EmailConfig{}, capability.NewSet(capability.Desktop)), service.NewOrgRuleService(nil, nil, nil), nil, service.NewAuthzService(nil, f.orgSvc), service.NewChannelService(nil), service.NewUserDirectory(nil), nil, nil)
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
	// The saved chat deployment is the org's model on its resource "default"
	// and its default for members: the member's server default, as its
	// registry id.
	orgChat := ""
	for _, m := range f.orgModels(t) {
		if m.Deployment == "org-chat" {
			orgChat = m.ModelID
		}
	}
	if !strings.HasPrefix(orgChat, "m/") {
		t.Fatalf("org-chat registry id = %q, want m/<id>", orgChat)
	}
	if az := list("u-member"); az["configured"] != true || az["shared"] != true || az["defaultModel"] != orgChat {
		t.Errorf("member: %v, want configured and shared, defaultModel %s", az, orgChat)
	}
	if az := list("u-stranger"); az["configured"] != false || az["shared"] != nil {
		t.Errorf("non-member: %v, want the parameter ignored", az)
	}
}
