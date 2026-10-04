package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		rr := f.doOrg(t, "u-admin", http.MethodPut, "/api/orgs/"+f.orgID+"/members/u-admin2/role", map[string]string{"role": "member"}, nil)
		checkStatus(t, rr, http.StatusOK)
		if !decodeMutationFlag(t, rr) {
			t.Error("the lender's demotion did not report the shared credential disabled")
		}
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
		rr := f.doOrg(t, "u-admin2", http.MethodDelete, "/api/orgs/"+f.orgID+"/members/u-admin2", nil, nil)
		checkStatus(t, rr, http.StatusOK)
		if !decodeMutationFlag(t, rr) {
			t.Error("the removed lender's departure did not report the shared credential disabled")
		}
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

// Account erasure disables every org credential the user lent — each key of
// the Azure family, in every organisation — and audits each key.
func TestAccountDelete_DisablesLentOrgCredentials(t *testing.T) {
	rt, _ := newAccountTestRouter(t, "u1", "alice@example.com")
	bearer := jwtBearer(t, rt, "u1", "alice@example.com")

	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "", nil }, nil, nil, nil)
	svc := service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets)
	svc.SetOrgKeyStore(orgKeys)
	azure, _ := newTestAzureModels(testAzureModelsDeps{Secrets: secrets, OrgKeys: orgKeys, Factory: factory, PerUser: true})
	svc.SetAzureModels(azure)
	doc := `{"endpoint":"https://unit-test.invalid","auth":{"mode":"api_key","key":"k"},"chatDeployment":"c"}`
	resource := `{"endpoint":"https://unit-test.invalid","auth":{"mode":"api_key","key":"k2"}}`
	const secondKey = ai.AzureFoundryProviderID + "/abcdefghij"
	_ = orgKeys.SaveOrg("org-lent", ai.AzureFoundryProviderID, doc, "u1")
	_ = orgKeys.SaveOrg("org-lent", secondKey, resource, "u1")
	_ = orgKeys.SaveOrg("org-other", ai.AzureFoundryProviderID, doc, "u2")
	_ = orgKeys.SaveOrg("org-other", secondKey, resource, "u1")
	_ = secrets.Save("u1", ai.AzureFoundryProviderID, doc)
	rt.handlers.Auth.SetProviderService(svc)
	ch := captureAudit(t)

	rr := doRequestWithAuth(t, rt, http.MethodDelete, "/api/auth/account", bearer, map[string]string{"confirmEmail": "alice@example.com"})
	checkStatus(t, rr, http.StatusOK)
	for _, k := range []struct{ org, key string }{{"org-lent", ai.AzureFoundryProviderID}, {"org-lent", secondKey}, {"org-other", secondKey}} {
		if has, _ := orgKeys.HasOrg(k.org, k.key); has {
			t.Errorf("the erased user's lent credential %s of %s is still live", k.key, k.org)
		}
	}
	if has, _ := orgKeys.HasOrg("org-other", ai.AzureFoundryProviderID); !has {
		t.Error("another admin's org credential was removed")
	}
	// One keys.save {op: disable} row per disabled key, naming it.
	want := map[string]bool{"org-lent " + ai.AzureFoundryProviderID: true, "org-lent " + secondKey: true, "org-other " + secondKey: true}
	for len(want) > 0 {
		ev := awaitAudit(t, ch, func(ev *storageif.AuditEvent) bool {
			return ev.Action == AuditActionKeysSave && ev.Meta["op"] == "disable" && ev.Meta["reason"] == lenderReasonAccountDeleted
		})
		k := ev.ResourceID + " " + ev.Meta["provider"]
		if !want[k] {
			t.Fatalf("unexpected disable audit row %s (meta %v)", k, ev.Meta)
		}
		if ev.Meta["user"] != "u1" {
			t.Errorf("disable row %s: user = %q, want u1", k, ev.Meta["user"])
		}
		delete(want, k)
	}
	noAudit(t, ch, func(ev *storageif.AuditEvent) bool { return ev.Meta["op"] == "disable" })
}

// A demoted lender's every shared resource credential goes —
// one audit row per key, naming the key and its resource — the reply flags
// it, and another admin's resource stays.
func TestOrgLenderLosesAdmin_DisablesEachLentKey(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	if err := f.orgSvc.AddMember(bgCtx(), f.orgID, "u-admin2", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	const lentKey, otherKey = ai.AzureFoundryProviderID + "/lent000001", ai.AzureFoundryProviderID + "/other00001"
	resource := `{"endpoint":"https://unit-test.invalid","auth":{"mode":"api_key","key":"k"}}`
	f.saveOrgDoc(t, "u-admin2")
	_ = f.orgKeys.SaveOrg(f.orgID, lentKey, resource, "u-admin2")
	_ = f.orgKeys.SaveOrg(f.orgID, otherKey, resource, "u-admin")
	ch := captureAudit(t)

	rr := f.doOrg(t, "u-admin", http.MethodPut, "/api/orgs/"+f.orgID+"/members/u-admin2/role", map[string]string{"role": "member"}, nil)
	checkStatus(t, rr, http.StatusOK)
	if !decodeMutationFlag(t, rr) {
		t.Error("the lender's demotion did not report the shared credentials disabled")
	}
	for _, k := range []string{ai.AzureFoundryProviderID, lentKey} {
		if has, _ := f.orgKeys.HasOrg(f.orgID, k); has {
			t.Errorf("the demoted lender's credential %s is still live", k)
		}
	}
	if has, _ := f.orgKeys.HasOrg(f.orgID, otherKey); !has {
		t.Error("another admin's resource credential was disabled")
	}
	seen := map[string]map[string]string{}
	for len(seen) < 2 {
		ev := awaitAudit(t, ch, func(ev *storageif.AuditEvent) bool {
			return ev.Action == AuditActionKeysSave && ev.Meta["op"] == "disable"
		})
		if ev.ResourceType != "org" || ev.ResourceID != f.orgID || ev.Meta["reason"] != lenderReasonDemoted || ev.Meta["user"] != "u-admin2" {
			t.Errorf("disable row = %+v (meta %v)", ev, ev.Meta)
		}
		seen[ev.Meta["provider"]] = ev.Meta
	}
	if m := seen[ai.AzureFoundryProviderID]; m == nil || m["connectionId"] != "default" {
		t.Errorf("default key row = %v, want connectionId default", m)
	}
	if m := seen[lentKey]; m == nil || m["connectionId"] != "lent000001" {
		t.Errorf("resource key row = %v, want connectionId lent000001", m)
	}
	noAudit(t, ch, func(ev *storageif.AuditEvent) bool { return ev.Meta["op"] == "disable" })
}

// The org config GET reports an unreadable document instead of "none".
func TestOrgAzureFoundryConfigGet_Unreadable(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	f.useOrgStore(&failingOrgStore{FakeOrgSecretStore: f.orgKeys, err: storage.ErrSecretDecryptFailed})
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
		if g := rateLimitGroup(http.MethodPost, &url.URL{Path: p}); g != rlGroupChat {
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
// BUDGET_EXCEEDED; an unreadable spend is 503 BUDGET_UNAVAILABLE. The run
// embeds with a priced provider: the embedder is resolved before the budget
// is consulted (an Azure AI Foundry embedder is never gated).
func TestKnowledge_BudgetRendered(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	ks := rag.NewKnowledgeService(testutil.NewFakeBackend(), nil, nil, rag.WithEmbedder(&testutil.FakeProvider{}))
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

// decodeMutationFlag reads a membership mutation's reply and reports whether
// it said the org's shared credential was disabled.
func decodeMutationFlag(t *testing.T, rr *httptest.ResponseRecorder) bool {
	t.Helper()
	var out struct {
		Status                   string `json:"status"`
		SharedCredentialDisabled bool   `json:"sharedCredentialDisabled"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode %s: %v", rr.Body.String(), err)
	}
	if out.Status != "ok" {
		t.Fatalf("mutation status = %q, want ok", out.Status)
	}
	return out.SharedCredentialDisabled
}

// After the stored credential became unreadable, the admin's
// "enter it again" save — no updatedAt, the GET could not supply one — is
// accepted (the server CASes on the unreadable row's own stamp) and the next
// GET shows the repaired document.
func TestOrgAzureFoundryConfigSave_ReplacesUnreadable(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	f.saveOrgDoc(t, "u-admin")
	f.useOrgStore(&failingOrgStore{FakeOrgSecretStore: f.orgKeys, err: storage.ErrSecretDecryptFailed})

	rr := f.do(t, "u-admin", http.MethodPost, f.configPath(), orgAzureBody)
	checkStatus(t, rr, http.StatusOK)
	first := azureDecode(t, rr)
	if first["status"] != "ok" || first["updatedAt"] == "" {
		t.Fatalf("repair save = %v, want status ok and an updatedAt", first)
	}

	// The repaired row reads configured through the ordinary store.
	f.useOrgStore(f.orgKeys)
	got := azureDecode(t, f.do(t, "u-member", http.MethodGet, f.configPath(), nil))
	if got["configured"] != true || got["chatDeployment"] != "org-chat" {
		t.Errorf("GET after the repair = %v, want the repaired document", got)
	}
}

// A carrying edit does not make its editor a lender — demoting
// the editor keeps the lender's credential (and the reply says nothing was
// disabled), demoting the lender disables it (the reply flags it, the audit
// row names them).
func TestOrgLenderLosesAdmin_CarryingEditorKeepsCredential(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	for _, admin := range []string{"u-lender", "u-editor"} {
		if err := f.orgSvc.AddMember(bgCtx(), f.orgID, admin, auth.RoleAdmin); err != nil {
			t.Fatal(err)
		}
	}
	f.saveOrgDoc(t, "u-lender")
	// u-editor renames the chat deployment without re-entering the secret (a
	// carrying edit: updated_by moves, the lender stays).
	cfg, err := f.svc.GetOrgAzureFoundryConfig(f.orgID)
	if err != nil || cfg.UpdatedAt == "" {
		t.Fatalf("stamp: %+v, %v", cfg, err)
	}
	if _, err := f.svc.SaveOrgAzureFoundryConfig(f.orgID, "u-editor", cfg.UpdatedAt, ai.AzureFoundryConfig{
		Endpoint: "https://unit-test.invalid", Auth: ai.AzureFoundryAuth{Mode: "api_key"}, ChatDeployment: "renamed"}); err != nil {
		t.Fatal(err)
	}
	ch := captureAudit(t)

	rr := f.doOrg(t, "u-admin", http.MethodPut, "/api/orgs/"+f.orgID+"/members/u-editor/role", map[string]string{"role": "member"}, nil)
	checkStatus(t, rr, http.StatusOK)
	if decodeMutationFlag(t, rr) {
		t.Error("demoting a carrying editor reported the shared credential disabled")
	}
	if !f.hasOrgDoc() {
		t.Fatal("demoting the editor deleted the lender's credential")
	}
	noAudit(t, ch, isDisableAudit(f.orgID, lenderReasonDemoted))

	rr = f.doOrg(t, "u-admin", http.MethodPut, "/api/orgs/"+f.orgID+"/members/u-lender/role", map[string]string{"role": "member"}, nil)
	checkStatus(t, rr, http.StatusOK)
	if !decodeMutationFlag(t, rr) {
		t.Error("demoting the lender did not report the shared credential disabled")
	}
	if f.hasOrgDoc() {
		t.Error("the lender's credential is still live after their demotion")
	}
	ev := awaitAudit(t, ch, isDisableAudit(f.orgID, lenderReasonDemoted))
	if ev.Meta["user"] != "u-lender" {
		t.Errorf("disable audit meta = %v, want the departed lender's id under \"user\"", ev.Meta)
	}
}
