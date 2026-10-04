package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// nextAudit returns the next captured audit event (see captureAudit).
func nextAudit(t *testing.T, ch chan *storageif.AuditEvent) *storageif.AuditEvent {
	t.Helper()
	select {
	case ev := <-ch:
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no audit event was enqueued")
		return nil
	}
}

// A save body is exactly one JSON object: trailing data — a second object
// the server silently ignored — is refused on both routes with
// INVALID_CONFIG, and nothing is stored.
func TestAzureFoundryConfigSave_RejectsTrailingData(t *testing.T) {
	const first = `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k1"},"chatDeployment":"c1"}`
	for name, body := range map[string]string{
		"second object": first + `{"endpoint":"https://other.openai.azure.com"}`,
		"garbage":       first + ` x`,
		"array close":   first + `]`,
	} {
		t.Run("user route/"+name, func(t *testing.T) {
			h, secrets := newAzureProviderHandler(t)
			req := httptest.NewRequest(http.MethodPost, "/api/providers/azure-foundry/config", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rr := httptest.NewRecorder()
			h.handleSaveAzureFoundryConfig(rr, req)
			checkStatus(t, rr, http.StatusBadRequest)
			if got := azureDecode(t, rr); got["code"] != ai.AzureCfgErrInvalidConfig {
				t.Errorf("code = %v, want %s", got["code"], ai.AzureCfgErrInvalidConfig)
			}
			if has, _ := secrets.Has("", ai.AzureFoundryProviderID); has {
				t.Error("a refused body stored a document")
			}
		})
	}
	t.Run("user route/trailing whitespace is fine", func(t *testing.T) {
		h, _ := newAzureProviderHandler(t)
		req := httptest.NewRequest(http.MethodPost, "/api/providers/azure-foundry/config", strings.NewReader(first+"\r\n  \n"))
		rr := httptest.NewRecorder()
		h.handleSaveAzureFoundryConfig(rr, req)
		checkStatus(t, rr, http.StatusOK)
	})
	t.Run("org route", func(t *testing.T) {
		f := newOrgProvidersFixture(t, true)
		rr := f.do(t, "u-admin", http.MethodPost, f.configPath(),
			`{"endpoint":"https://unit-test.invalid","auth":{"mode":"api_key","key":"k"},"chatDeployment":"c"}{"updatedAt":""}`)
		checkStatus(t, rr, http.StatusBadRequest)
		if got := azureDecode(t, rr); got["code"] != ai.AzureCfgErrInvalidConfig {
			t.Errorf("code = %v, want %s", got["code"], ai.AzureCfgErrInvalidConfig)
		}
		if has, _ := f.orgKeys.HasOrg(f.orgID, ai.AzureFoundryProviderID); has {
			t.Error("a refused body stored a document")
		}
	})
}

// The audit row of a configuration save says where the credential now goes
// and whose it is — endpoint and project hosts, auth mode, tenant and
// client IDs — and whether the stored secret was carried over; never the
// secret itself.
func TestAzureFoundryConfigSave_AuditMeta(t *testing.T) {
	ch := captureAudit(t)
	h, _ := newAzureProviderHandlerWithBackend(t, testutil.NewFakeBackend())
	entra := map[string]any{"mode": "entra", "tenantId": "Contoso.onmicrosoft.com",
		"clientId": "22222222-2222-2222-2222-222222222222", "clientSecret": "top-secret-value"}
	doc := map[string]any{
		"endpoint":        "https://My-Res.services.ai.azure.com",
		"auth":            entra,
		"projectEndpoint": "https://my-res.services.ai.azure.com/api/projects/p1",
	}
	checkStatus(t, azureConfigRequest(t, h, http.MethodPost, doc), http.StatusOK)
	ev := nextAudit(t, ch)
	want := map[string]string{
		"op": "save", "authMode": "entra", "endpointHost": "my-res.services.ai.azure.com",
		"projectHost": "my-res.services.ai.azure.com", "tenantId": "contoso.onmicrosoft.com",
		"clientId": "22222222-2222-2222-2222-222222222222", "secretCarried": "false",
	}
	for k, v := range want {
		if ev.Meta[k] != v {
			t.Errorf("meta[%s] = %q, want %q (meta %v)", k, ev.Meta[k], v, ev.Meta)
		}
	}

	// The same principal without its secret: carried over.
	delete(entra, "clientSecret")
	checkStatus(t, azureConfigRequest(t, h, http.MethodPost, doc), http.StatusOK)
	if ev := nextAudit(t, ch); ev.Meta["secretCarried"] != "true" {
		t.Errorf("secretCarried = %q, want true (meta %v)", ev.Meta["secretCarried"], ev.Meta)
	}

	// API-key mode names no principal.
	checkStatus(t, azureConfigRequest(t, h, http.MethodPost, map[string]any{
		"endpoint": "https://my-res.openai.azure.com", "auth": map[string]any{"mode": "api_key", "key": "top-secret-value"}, "chatDeployment": "c"}), http.StatusOK)
	ev = nextAudit(t, ch)
	if _, has := ev.Meta["tenantId"]; has || ev.Meta["endpointHost"] != "my-res.openai.azure.com" || ev.Meta["projectHost"] != "" {
		t.Errorf("api-key meta = %v", ev.Meta)
	}
	for _, e := range []*storageif.AuditEvent{ev} {
		for k, v := range e.Meta {
			if strings.Contains(v, "top-secret-value") {
				t.Errorf("audit meta %s leaks the secret", k)
			}
		}
	}
}

// lenderNamesBackend resolves both admins' display names.
type lenderNamesBackend struct {
	*testutil.FakeBackend
}

func (b *lenderNamesBackend) LoadUserByID(_ context.Context, id string) (*storageif.User, error) {
	switch id {
	case "u-admin":
		return &storageif.User{ID: id, DisplayName: "Ada Admin"}, nil
	case "u-admin2":
		return &storageif.User{ID: id, DisplayName: "Bob Editor"}, nil
	}
	return nil, storageif.ErrNotFound
}

// The org GET's "shared by" names the credential's lender — the admin who
// supplied the secret — not another admin who later edited a deployment
// name; the edit's audit row says the secret was carried; and the lender's
// demotion disables the credential even though the editor saved last.
func TestOrgAzureFoundryConfig_LabelNamesLender(t *testing.T) {
	f := newOrgProvidersFixture(t, true)
	f.backend = &lenderNamesBackend{FakeBackend: testutil.NewFakeBackend()}
	if err := f.orgSvc.AddMember(bgCtx(), f.orgID, "u-admin2", auth.RoleAdmin); err != nil {
		t.Fatal(err)
	}
	ch := captureAudit(t)
	path := f.configPath()

	checkStatus(t, f.do(t, "u-admin", http.MethodPost, path, orgAzureBody), http.StatusOK)
	_ = nextAudit(t, ch)
	got := azureDecode(t, f.do(t, "u-member", http.MethodGet, path, nil))
	token, _ := got["updatedAt"].(string)

	edit := map[string]any{
		"endpoint":       "https://unit-test.invalid",
		"auth":           map[string]any{"mode": "api_key"},
		"chatDeployment": "edited-chat",
		"updatedAt":      token,
	}
	checkStatus(t, f.do(t, "u-admin2", http.MethodPost, path, edit), http.StatusOK)
	if ev := nextAudit(t, ch); ev.Meta["secretCarried"] != "true" || ev.Meta["provider"] != ai.AzureFoundryProviderID || ev.Meta["endpointHost"] != "unit-test.invalid" {
		t.Errorf("edit audit meta = %v, want secretCarried true", ev.Meta)
	}
	got = azureDecode(t, f.do(t, "u-member", http.MethodGet, path, nil))
	if got["chatDeployment"] != "edited-chat" || got["updatedBy"] != "Ada Admin" {
		t.Errorf("after the edit: chat=%v updatedBy=%v, want the edit and the lender Ada Admin", got["chatDeployment"], got["updatedBy"])
	}

	// The lender is demoted: their secret goes, although Bob saved last.
	disableLentCredential(httptest.NewRequest(http.MethodPost, "/", nil), f.backend, nil, f.svc, f.orgID, "u-admin", lenderReasonDemoted)
	if has, _ := f.orgKeys.HasOrg(f.orgID, ai.AzureFoundryProviderID); has {
		t.Error("the demoted lender's credential is still stored")
	}
}
