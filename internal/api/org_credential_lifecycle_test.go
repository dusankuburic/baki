package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/config"
	mailer "pad-analyzer/internal/mail"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// captureAudit routes audit events to a fresh buffered channel for the test.
func captureAudit(t *testing.T) chan *storageif.AuditEvent {
	t.Helper()
	prevCh := auditCh
	ch := make(chan *storageif.AuditEvent, 32)
	auditCh = ch
	auditClosed.Store(false)
	t.Cleanup(func() {
		auditCh = prevCh
		auditClosed.Store(false)
	})
	return ch
}

// awaitAudit returns the first captured event matching match, failing the
// test when none arrives in time.
func awaitAudit(t *testing.T, ch chan *storageif.AuditEvent, match func(*storageif.AuditEvent) bool) *storageif.AuditEvent {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-ch:
			if match(ev) {
				return ev
			}
		case <-deadline:
			t.Fatal("expected audit event was not enqueued")
			return nil
		}
	}
}

// noAudit fails when an event matching match was captured.
func noAudit(t *testing.T, ch chan *storageif.AuditEvent, match func(*storageif.AuditEvent) bool) {
	t.Helper()
	for {
		select {
		case ev := <-ch:
			if match(ev) {
				t.Errorf("unexpected audit event %+v (meta %v)", ev, ev.Meta)
			}
		default:
			return
		}
	}
}

func isDisableAudit(orgID, reason string) func(*storageif.AuditEvent) bool {
	return func(ev *storageif.AuditEvent) bool {
		return ev.Action == AuditActionKeysSave && ev.ResourceType == "org" && ev.ResourceID == orgID &&
			ev.Meta["op"] == "disable" && ev.Meta["provider"] == ai.AzureFoundryProviderID && ev.Meta["reason"] == reason
	}
}

// orgRouter serves the membership, org and knowledge routes the lifecycle
// hooks ride on, as caller.
func (f *orgProvidersFixture) orgRouter(caller string, knowledge *rag.KnowledgeService) http.Handler {
	security := &SecurityConfig{LocalUserID: caller, OrgSvc: f.orgSvc}
	h := NewOrgHandler(f.orgSvc, f.backend, knowledge, security, mailer.NewService(config.EmailConfig{}, config.ModeLocal), nil, f.svc)
	r := chi.NewRouter()
	r.Route("/api/orgs/{id}", func(r chi.Router) {
		r.Delete("/", h.handleOrgDelete)
		r.Delete("/members/{userId}", h.handleOrgMemberRemove)
		r.Put("/members/{userId}/role", h.handleOrgMemberRoleUpdate)
		r.Post("/knowledge/upload", h.handleKnowledgeUpload)
		r.Post("/knowledge/reindex", h.handleKnowledgeReindex)
	})
	return r
}

func (f *orgProvidersFixture) doOrg(t *testing.T, caller, method, path string, body any, knowledge *rag.KnowledgeService) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	f.orgRouter(caller, knowledge).ServeHTTP(rr, req)
	return rr
}

// saveOrgDoc stores the org's shared document, saved by admin.
func (f *orgProvidersFixture) saveOrgDoc(t *testing.T, admin string) {
	t.Helper()
	if _, err := f.svc.SaveOrgAzureFoundryConfig(f.orgID, admin, "", ai.AzureFoundryConfig{
		Endpoint: "https://unit-test.invalid", Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "lender-key"}, ChatDeployment: "org-chat"}); err != nil {
		t.Fatal(err)
	}
}

func (f *orgProvidersFixture) hasOrgDoc() bool {
	has, _ := f.orgKeys.HasOrg(f.orgID, ai.AzureFoundryProviderID)
	return has
}

// R-5: the shared credential goes when the admin who saved it stops being an
// admin — demoted or removed — and the row is audited; another admin's
// departure or a promotion leaves it.
func TestOrgLenderLosesAdmin_DisablesSharedCredential(t *testing.T) {
	t.Run("demoted", func(t *testing.T) {
		f := newOrgProvidersFixture(t, true)
		if err := f.orgSvc.AddMember(bgCtx(), f.orgID, "u-admin2", auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		f.saveOrgDoc(t, "u-admin2")
		ch := captureAudit(t)
		// Promoting someone else, or re-affirming an admin, disables nothing.
		checkStatus(t, f.doOrg(t, "u-admin", http.MethodPut, "/api/orgs/"+f.orgID+"/members/u-member/role", map[string]string{"role": "admin"}, nil), http.StatusOK)
		checkStatus(t, f.doOrg(t, "u-admin", http.MethodPut, "/api/orgs/"+f.orgID+"/members/u-admin2/role", map[string]string{"role": "admin"}, nil), http.StatusOK)
		if !f.hasOrgDoc() {
			t.Fatal("the credential was disabled without a demotion")
		}
		checkStatus(t, f.doOrg(t, "u-admin", http.MethodPut, "/api/orgs/"+f.orgID+"/members/u-admin2/role", map[string]string{"role": "member"}, nil), http.StatusOK)
		if f.hasOrgDoc() {
			t.Error("the demoted lender's credential is still live")
		}
		awaitAudit(t, ch, isDisableAudit(f.orgID, lenderReasonDemoted))
	})

	t.Run("removed", func(t *testing.T) {
		f := newOrgProvidersFixture(t, true)
		if err := f.orgSvc.AddMember(bgCtx(), f.orgID, "u-admin2", auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		f.saveOrgDoc(t, "u-admin2")
		ch := captureAudit(t)
		checkStatus(t, f.doOrg(t, "u-admin", http.MethodDelete, "/api/orgs/"+f.orgID+"/members/u-member", nil, nil), http.StatusOK)
		if !f.hasOrgDoc() {
			t.Fatal("removing a non-lender disabled the credential")
		}
		noAudit(t, ch, isDisableAudit(f.orgID, lenderReasonRemoved))
		// Leaving on one's own counts too.
		checkStatus(t, f.doOrg(t, "u-admin2", http.MethodDelete, "/api/orgs/"+f.orgID+"/members/u-admin2", nil, nil), http.StatusOK)
		if f.hasOrgDoc() {
			t.Error("the removed lender's credential is still live")
		}
		awaitAudit(t, ch, isDisableAudit(f.orgID, lenderReasonRemoved))
	})

	t.Run("another admin saved it last", func(t *testing.T) {
		f := newOrgProvidersFixture(t, true)
		if err := f.orgSvc.AddMember(bgCtx(), f.orgID, "u-admin2", auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
		f.saveOrgDoc(t, "u-admin")
		checkStatus(t, f.doOrg(t, "u-admin", http.MethodPut, "/api/orgs/"+f.orgID+"/members/u-admin2/role", map[string]string{"role": "viewer"}, nil), http.StatusOK)
		if !f.hasOrgDoc() {
			t.Error("demoting an admin who did not save the credential disabled it")
		}
	})
}

// Org deletion runs through the credential release (read before the
// cascade; the service test covers what it drops) and still deletes.
func TestOrgDelete_ReleasesSharedCredential(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	f.saveOrgDoc(t, "u-admin")
	checkStatus(t, f.doOrg(t, "u-member", http.MethodDelete, "/api/orgs/"+f.orgID, nil, nil), http.StatusForbidden)
	checkStatus(t, f.doOrg(t, "u-admin", http.MethodDelete, "/api/orgs/"+f.orgID, nil, nil), http.StatusOK)
	if _, err := f.orgSvc.Get(bgCtx(), f.orgID); err == nil {
		t.Fatal("org not deleted")
	}
}

// Account erasure disables every org credential the user lent and audits it.
func TestAccountDelete_DisablesLentOrgCredentials(t *testing.T) {
	rt, _ := newAccountTestRouter(t, "u1", "alice@example.com")
	bearer := jwtBearer(t, rt, "u1", "alice@example.com")

	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "", nil }, nil, nil, nil)
	svc := service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets)
	svc.SetOrgKeyStore(orgKeys)
	doc := `{"endpoint":"https://unit-test.invalid","auth":{"mode":"api_key","key":"k"},"chatDeployment":"c"}`
	_ = orgKeys.SaveOrg("org-lent", ai.AzureFoundryProviderID, doc, "u1")
	_ = orgKeys.SaveOrg("org-other", ai.AzureFoundryProviderID, doc, "u2")
	_ = secrets.Save("u1", ai.AzureFoundryProviderID, doc)
	rt.handlers.Auth.SetProviderService(svc)
	ch := captureAudit(t)

	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/account", bearer, map[string]string{"confirmEmail": "alice@example.com"})
	checkStatus(t, rr, http.StatusOK)
	if has, _ := orgKeys.HasOrg("org-lent", ai.AzureFoundryProviderID); has {
		t.Error("the erased user's lent org credential is still live")
	}
	if has, _ := orgKeys.HasOrg("org-other", ai.AzureFoundryProviderID); !has {
		t.Error("another admin's org credential was removed")
	}
	awaitAudit(t, ch, isDisableAudit("org-lent", lenderReasonAccountDeleted))
}

// The org config GET reports an unreadable document instead of "none".
func TestOrgAzureFoundryConfigGet_Unreadable(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	f.svc.SetOrgKeyStore(&failingOrgStore{FakeOrgSecretStore: f.orgKeys, err: storage.ErrSecretDecryptFailed})
	rr := f.do(t, "u-member", http.MethodGet, f.configPath(), nil)
	checkStatus(t, rr, http.StatusInternalServerError)
	if got := azureDecode(t, rr); got["code"] != "ORG_CREDENTIAL_UNREADABLE" {
		t.Errorf("code = %v, want ORG_CREDENTIAL_UNREADABLE (body %s)", got["code"], rr.Body.String())
	}
}

type failingOrgStore struct {
	*testutil.FakeOrgSecretStore
	err error
}

func (s *failingOrgStore) GetOrg(string, string) (string, error) { return "", s.err }

// Compare-and-swap over HTTP: the save returns the next token, a missing or
// stale token over an existing document is 409 CONFLICT.
func TestOrgAzureFoundryConfigSave_CompareAndSwap(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	path := f.configPath()
	body := func(chat, updatedAt string) map[string]any {
		b := map[string]any{"endpoint": "https://unit-test.invalid", "auth": map[string]any{"mode": "api_key", "key": "k"}, "chatDeployment": chat}
		if updatedAt != "" {
			b["updatedAt"] = updatedAt
		}
		return b
	}
	rr := f.do(t, "u-admin", http.MethodPost, path, body("c1", ""))
	checkStatus(t, rr, http.StatusOK)
	first := azureDecode(t, rr)
	token, _ := first["updatedAt"].(string)
	if first["status"] != "ok" || token == "" {
		t.Fatalf("first save = %v, want status ok and an updatedAt", first)
	}
	got := azureDecode(t, f.do(t, "u-member", http.MethodGet, path, nil))
	if got["updatedAt"] != token {
		t.Errorf("GET updatedAt = %v, want the save's token %q", got["updatedAt"], token)
	}

	rr = f.do(t, "u-admin", http.MethodPost, path, body("c2", ""))
	checkStatus(t, rr, http.StatusConflict)
	if got := azureDecode(t, rr); got["code"] != "CONFLICT" {
		t.Errorf("token-less save over a document: code %v", got["code"])
	}
	rr = f.do(t, "u-admin", http.MethodPost, path, body("c2", token))
	checkStatus(t, rr, http.StatusOK)
	rr = f.do(t, "u-admin", http.MethodPost, path, body("c3", token))
	checkStatus(t, rr, http.StatusConflict)
	if got := azureDecode(t, f.do(t, "u-member", http.MethodGet, path, nil)); got["chatDeployment"] != "c2" {
		t.Errorf("a stale save wrote: %v", got)
	}
}

// R-6: the org connection test is audited (providers.test), and it shares
// the per-user test's chat bucket and RLS exemption.
func TestOrgAzureFoundryTest_AuditedChatBucketRLSExempt(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	ch := captureAudit(t)
	rr := f.do(t, "u-admin", http.MethodPost, "/api/orgs/"+f.orgID+"/providers/azure-foundry/test", nil)
	checkStatus(t, rr, http.StatusOK)
	ev := awaitAudit(t, ch, func(ev *storageif.AuditEvent) bool { return ev.Action == AuditActionProvidersTest })
	if ev.ResourceType != "org" || ev.ResourceID != f.orgID || ev.Meta["provider"] != ai.AzureFoundryProviderID || ev.Meta["ok"] != "false" {
		t.Errorf("audit event = %+v (meta %v)", ev, ev.Meta)
	}

	for _, p := range []string{"/api/orgs/o1/providers/azure-foundry/test", "/api/v1/orgs/o1/providers/azure-foundry/test", "/api/orgs/o1/../o1/providers/azure-foundry/test"} {
		if g := rateLimitGroup(http.MethodPost, p); g != rlGroupChat {
			t.Errorf("rateLimitGroup(POST %s) = %q, want chat", p, g)
		}
	}
	if !rlsExemptPath("/api/orgs/o1/providers/azure-foundry/test") {
		t.Error("the org connection test must be exempt from the request-long RLS transaction")
	}
	for _, p := range []string{"/api/orgs/o1/providers/azure-foundry/config", "/api/orgs//providers/azure-foundry/test", "/api/orgs/o1/x/providers/azure-foundry/test"} {
		if isOrgProviderTestPath(p) {
			t.Errorf("isOrgProviderTestPath(%q) = true", p)
		}
	}
	if rlsExemptPath("/api/orgs/o1/providers/azure-foundry/config") {
		t.Error("the org config routes keep their RLS transaction")
	}
}

// R-1 on the listing: a viewer (and a guest) is not offered the shared
// credential they could not chat on.
func TestListProviders_OrgIdOnlyForAdminsAndMembers(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	f.saveOrgDoc(t, "u-admin")
	if err := f.orgSvc.AddMember(bgCtx(), f.orgID, "u-viewer", auth.RoleViewer); err != nil {
		t.Fatal(err)
	}
	if err := f.orgSvc.AddMember(bgCtx(), f.orgID, "u-guest", auth.RoleGuest); err != nil {
		t.Fatal(err)
	}
	for caller, wantShared := range map[string]bool{"u-admin": true, "u-member": true, "u-viewer": false, "u-guest": false} {
		h := NewProviderHandler(f.svc, &SecurityConfig{LocalUserID: caller, OrgSvc: f.orgSvc}, nil)
		rr := httptest.NewRecorder()
		h.handleListProviders(rr, httptest.NewRequest(http.MethodGet, "/api/providers/list?orgId="+f.orgID, nil))
		checkStatus(t, rr, http.StatusOK)
		var providers []map[string]any
		decodeJSON(t, rr, &providers)
		for _, p := range providers {
			if p["id"] == "azure-foundry" && (p["shared"] == true) != wantShared {
				t.Errorf("%s: azure-foundry = %v, want shared=%v", caller, p, wantShared)
			}
		}
	}
}

// budgetRefusal is a rag.BudgetChecker refusing with the chat service's
// budget sentinel.
type budgetRefusal struct{ err error }

func (b budgetRefusal) CheckBudget(context.Context, string, string) error { return b.err }

// R-4: knowledge upload and re-index over the daily budget are 429
// BUDGET_EXCEEDED; an unreadable spend is 503 BUDGET_UNAVAILABLE.
func TestKnowledge_BudgetRendered(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	ks := rag.NewKnowledgeService(testutil.NewFakeBackend(), nil, nil)
	ks.SetBudgetChecker(budgetRefusal{err: fmt.Errorf("refused: %w", service.ErrAIBudgetExceeded)})
	rr := f.doOrg(t, "u-admin", http.MethodPost, "/api/orgs/"+f.orgID+"/knowledge/upload", map[string]string{"filename": "a.md", "content": "text"}, ks)
	checkStatus(t, rr, http.StatusTooManyRequests)
	if got := azureDecode(t, rr); got["code"] != "BUDGET_EXCEEDED" {
		t.Errorf("upload code = %v (body %s)", got["code"], rr.Body.String())
	}
	rr = f.doOrg(t, "u-admin", http.MethodPost, "/api/orgs/"+f.orgID+"/knowledge/reindex", nil, ks)
	checkStatus(t, rr, http.StatusTooManyRequests)

	ks.SetBudgetChecker(budgetRefusal{err: fmt.Errorf("refused: %w", service.ErrAIBudgetUnavailable)})
	rr = f.doOrg(t, "u-admin", http.MethodPost, "/api/orgs/"+f.orgID+"/knowledge/upload", map[string]string{"filename": "a.md", "content": "text"}, ks)
	checkStatus(t, rr, http.StatusServiceUnavailable)
	if got := azureDecode(t, rr); got["code"] != "BUDGET_UNAVAILABLE" {
		t.Errorf("unavailable code = %v", got["code"])
	}
}
