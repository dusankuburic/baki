package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

const (
	lifecycleTenant = "11111111-2222-3333-4444-555555555555"
	lifecycleClient = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
)

// entraDoc is an Entra ID document on endpoint/project with an optional
// client secret.
func entraDoc(endpoint, project, secret string) ai.AzureFoundryConfig {
	return ai.AzureFoundryConfig{
		Endpoint:        endpoint,
		ProjectEndpoint: project,
		ChatDeployment:  "chat1",
		Auth:            ai.AzureFoundryAuth{Mode: ai.AzureAuthModeEntra, TenantID: lifecycleTenant, ClientID: lifecycleClient, ClientSecret: secret},
	}
}

// R-8: an omitted secret is carried only while the document sends it to the
// hosts it was entered for — for the user's own and the org's document.
func TestCarryAzureSecret_EndpointHostRule(t *testing.T) {
	const res = "https://my-res.services.ai.azure.com"
	const project = res + "/api/projects/p1"
	cases := []struct {
		name      string
		next      ai.AzureFoundryConfig
		wantCarry bool
	}{
		{"same endpoint and project", entraDoc(res, project, ""), true},
		{"project removed", entraDoc(res, "", ""), true},
		{"other project on the same host", entraDoc(res, res+"/api/projects/p2", ""), true},
		{"endpoint host changed", entraDoc("https://other-res.services.ai.azure.com", project, ""), false},
		{"project moved to another host", entraDoc(res, "https://other-res.services.ai.azure.com/api/projects/p1", ""), false},
		{"endpoint case only", entraDoc("https://MY-RES.services.ai.azure.com", project, ""), true},
	}
	prev := entraDoc(res, project, "old-secret")
	prev.Normalize()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			next := tc.next
			next.Normalize()
			err := carryAzureSecret(&next, prev, true)
			if tc.wantCarry {
				if err != nil || next.Auth.ClientSecret != "old-secret" {
					t.Errorf("carry = %v, secret %q; want the stored secret carried", err, next.Auth.ClientSecret)
				}
				return
			}
			var cfgErr *ai.AzureConfigError
			if !errors.As(err, &cfgErr) || cfgErr.Code != ai.AzureCfgErrMissingSecret || !strings.Contains(cfgErr.Message, "client secret") {
				t.Errorf("carry = %v, want MISSING_SECRET naming the client secret", err)
			}
			if next.Auth.ClientSecret != "" {
				t.Error("the stored secret was attached to a document for another host")
			}
		})
	}
	// Adding a project endpoint where there was none: allowed on the
	// endpoint's own host, refused elsewhere.
	noProject := entraDoc(res, "", "old-secret")
	noProject.Normalize()
	same := entraDoc(res, project, "")
	same.Normalize()
	if err := carryAzureSecret(&same, noProject, true); err != nil || same.Auth.ClientSecret != "old-secret" {
		t.Errorf("project added on the endpoint's host: %v", err)
	}
	other := entraDoc(res, "https://other-res.services.ai.azure.com/api/projects/p1", "")
	other.Normalize()
	if err := carryAzureSecret(&other, noProject, true); err == nil {
		t.Error("project added on another host must not carry the secret")
	}
}

// R-8 on the org route: another admin cannot point the lender's stored key at another
// host without re-entering it; a document error wins over the refusal (the host itself is
// not allowed). The Azure CLI sign-in mode is the lender's own desktop identity — an
// org-shared document runs on every member's machine, so saving one in that mode is
// refused with INVALID_AUTH in every deployment mode (the provider-level validation
// refuses it only on server deployments).
func TestSaveOrgAzureFoundryConfig_RefusesEntraCLI(t *testing.T) {
	orgKeys := testutil.NewFakeOrgSecretStore()
	svc := newOrgProviderService(t, testutil.NewFakeSecretStore(), orgKeys)
	cliDoc := ai.AzureFoundryConfig{
		Endpoint:       "https://" + azureTestHost,
		ChatDeployment: "chat1",
		Auth:           ai.AzureFoundryAuth{Mode: ai.AzureAuthModeEntraCLI},
	}
	_, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-a", "", cliDoc)
	if code := azureCfgErrCode(t, err); code != ai.AzureCfgErrInvalidAuth {
		t.Fatalf("entra_cli org save: code = %q (%v), want %q", code, err, ai.AzureCfgErrInvalidAuth)
	}
	if !strings.Contains(err.Error(), "cannot be shared with an organisation") {
		t.Errorf("message %q must say the mode cannot be shared with an organisation", err)
	}
	if raw, getErr := orgKeys.GetOrg("org-1", ai.AzureFoundryProviderID); getErr == nil && raw != "" {
		t.Error("a refused save stored an org document")
	}
}

func TestSaveOrgAzureFoundryConfig_EndpointChangeNeedsSecret(t *testing.T) {
	configureAzureForTests(t)
	orgKeys := testutil.NewFakeOrgSecretStore()
	svc := newOrgProviderService(t, testutil.NewFakeSecretStore(), orgKeys)
	at, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-a", "", ai.AzureFoundryConfig{
		Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "lender-key"}, ChatDeployment: "chat1"})
	if err != nil {
		t.Fatal(err)
	}
	moved := ai.AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: ai.AzureFoundryAuth{Mode: "api_key"}, ChatDeployment: "chat1"}
	_, err = svc.SaveOrgAzureFoundryConfig("org-1", "admin-b", at, moved)
	if code := azureCfgErrCode(t, err); code != ai.AzureCfgErrMissingSecret || !strings.Contains(err.Error(), "API key") {
		t.Errorf("endpoint change without the key: %v, want MISSING_SECRET", err)
	}
	evil := moved
	evil.Endpoint = "https://evil.example"
	_, err = svc.SaveOrgAzureFoundryConfig("org-1", "admin-b", at, evil)
	if code := azureCfgErrCode(t, err); code != ai.AzureCfgErrHostNotAllowed {
		t.Errorf("disallowed host without the key: code %q (%v), want HOST_NOT_ALLOWED first", code, err)
	}
	raw, _ := orgKeys.GetOrg("org-1", "azure-foundry")
	if stored, _ := ai.ParseAzureFoundryConfig(raw); stored.Endpoint != "https://"+azureTestHost || stored.Auth.Key != "lender-key" {
		t.Errorf("stored document changed under refused saves: %+v", stored)
	}
}

// Compare-and-swap on the org save: the token the GET returned is required
// once a document exists, a stale one is a conflict that writes nothing, and
// every successful save hands out the next token.
func TestSaveOrgAzureFoundryConfig_CompareAndSwap(t *testing.T) {
	configureAzureForTests(t)
	orgKeys := testutil.NewFakeOrgSecretStore()
	svc := newOrgProviderService(t, testutil.NewFakeSecretStore(), orgKeys)
	doc := func(chat string) ai.AzureFoundryConfig {
		return ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k"}, ChatDeployment: chat}
	}

	at1, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-a", "", doc("c1"))
	if err != nil || at1 == "" {
		t.Fatalf("first save: %q, %v", at1, err)
	}
	pub, err := svc.GetOrgAzureFoundryConfig("org-1")
	if err != nil || pub.UpdatedAt != at1 {
		t.Fatalf("GET updatedAt = %q (%v), want the save's token %q", pub.UpdatedAt, err, at1)
	}
	if _, err := time.Parse(time.RFC3339, pub.UpdatedAt); err != nil {
		t.Errorf("updatedAt %q is not RFC 3339: %v", pub.UpdatedAt, err)
	}

	if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-b", "", doc("c2")); !errors.Is(err, ErrOrgCredentialConflict) {
		t.Errorf("save without a token over an existing document: %v, want ErrOrgCredentialConflict", err)
	}
	at2, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-b", at1, doc("c2"))
	if err != nil || at2 == "" || at2 == at1 {
		t.Fatalf("save with the current token: %q, %v", at2, err)
	}
	if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-a", at1, doc("c3")); !errors.Is(err, ErrOrgCredentialConflict) {
		t.Errorf("save with a stale token: %v, want ErrOrgCredentialConflict", err)
	}
	raw, _ := orgKeys.GetOrg("org-1", "azure-foundry")
	if stored, _ := ai.ParseAzureFoundryConfig(raw); stored.ChatDeployment != "c2" {
		t.Errorf("a conflicting save wrote: %+v", stored)
	}
	if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-a", "yesterday", doc("c3")); azureCfgErrCode(t, err) != ai.AzureCfgErrInvalidConfig {
		t.Errorf("malformed token: %v, want INVALID_CONFIG", err)
	}
	// After a delete, only a token-less save recreates it.
	if err := svc.DeleteOrgAzureFoundryConfig("org-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-a", at2, doc("c4")); !errors.Is(err, ErrOrgCredentialConflict) {
		t.Errorf("save with a token after the document was deleted: %v, want ErrOrgCredentialConflict", err)
	}
	if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-a", "", doc("c4")); err != nil {
		t.Errorf("token-less save of a new document: %v", err)
	}
}

// unreadableOrgStore fails reads of the org document the way the database
// keystore does after a deployment key rotation.
type unreadableOrgStore struct {
	*testutil.FakeOrgSecretStore
	getErr error
}

func (s *unreadableOrgStore) GetOrg(orgID, provider string) (string, error) {
	if s.getErr != nil {
		return "", s.getErr
	}
	return s.FakeOrgSecretStore.GetOrg(orgID, provider)
}

// The org GET tells "no document" from "a document that cannot be read".
func TestGetOrgAzureFoundryConfig_Unreadable(t *testing.T) {
	svc := newProviderServiceWithFakeSecrets(t, testutil.NewFakeSecretStore())
	store := &unreadableOrgStore{FakeOrgSecretStore: testutil.NewFakeOrgSecretStore()}
	svc.orgKeys = store

	if pub, err := svc.GetOrgAzureFoundryConfig("org-1"); err != nil || pub.Configured {
		t.Fatalf("missing document: %+v, %v; want unconfigured", pub, err)
	}
	store.getErr = storage.ErrSecretDecryptFailed
	if _, err := svc.GetOrgAzureFoundryConfig("org-1"); !errors.Is(err, ErrOrgCredentialUnreadable) {
		t.Errorf("decrypt failure: %v, want ErrOrgCredentialUnreadable", err)
	}
	store.getErr = errors.New("keystore: get org: connection refused")
	if _, err := svc.GetOrgAzureFoundryConfig("org-1"); !errors.Is(err, ErrOrgCredentialUnreadable) {
		t.Errorf("database failure: %v, want ErrOrgCredentialUnreadable", err)
	}
	store.getErr = nil
	_ = store.SaveOrg("org-1", "azure-foundry", `{"endpoint":`, "admin")
	if _, err := svc.GetOrgAzureFoundryConfig("org-1"); !errors.Is(err, ErrOrgCredentialUnreadable) {
		t.Errorf("unparseable document: %v, want ErrOrgCredentialUnreadable", err)
	}
}

// R-5: the shared credential is disabled when the admin who saved it leaves
// the admin role — only theirs, and the org's cached listings go with it.
func TestDisableOrgAzureFoundryConfigSavedBy(t *testing.T) {
	configureAzureForTests(t)
	orgKeys := testutil.NewFakeOrgSecretStore()
	svc := newOrgProviderService(t, testutil.NewFakeSecretStore(), orgKeys)
	doc := ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k"}, ChatDeployment: "c1"}
	if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "lender", "", doc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveOrgAzureFoundryConfig("org-2", "other-admin", "", doc); err != nil {
		t.Fatal(err)
	}

	if disabled, err := svc.DisableOrgAzureFoundryConfigSavedBy("org-1", "someone-else"); err != nil || disabled {
		t.Errorf("another admin's departure: disabled=%v err=%v, want untouched", disabled, err)
	}
	if has, _ := orgKeys.HasOrg("org-1", "azure-foundry"); !has {
		t.Fatal("a non-lender's departure removed the credential")
	}
	svc.modelCache[ai.OrgScope("org-1")+"\x00glm"] = modelCacheEntry{}
	if disabled, err := svc.DisableOrgAzureFoundryConfigSavedBy("org-1", "lender"); err != nil || !disabled {
		t.Fatalf("lender's departure: disabled=%v err=%v", disabled, err)
	}
	if has, _ := orgKeys.HasOrg("org-1", "azure-foundry"); has {
		t.Error("the lender's credential is still stored")
	}
	if _, cached := svc.modelCache[ai.OrgScope("org-1")+"\x00glm"]; cached {
		t.Error("disabling must drop the org's cached listings")
	}
	if disabled, _ := svc.DisableOrgAzureFoundryConfigSavedBy("org-1", "lender"); disabled {
		t.Error("disabling twice reported a second removal")
	}
	if has, _ := orgKeys.HasOrg("org-2", "azure-foundry"); !has {
		t.Error("another org's credential was removed")
	}

	// An edit that CARRIED the stored secret does not make its editor
	// a lender — their departure keeps the credential, the lender's takes it.
	if _, err := svc.SaveOrgAzureFoundryConfig("org-carry", "lender-a", "", doc); err != nil {
		t.Fatal(err)
	}
	stamp, err := svc.GetOrgAzureFoundryConfig("org-carry")
	if err != nil || stamp.UpdatedAt == "" {
		t.Fatalf("stamp for the carrying edit: %+v, %v", stamp, err)
	}
	carried := doc
	carried.Auth.Key = "" // the editor renames a deployment without the key
	carried.ChatDeployment = "renamed"
	if _, err := svc.SaveOrgAzureFoundryConfig("org-carry", "editor-b", stamp.UpdatedAt, carried); err != nil {
		t.Fatalf("carrying edit: %v", err)
	}
	if meta, ok, _ := orgKeys.OrgKeyMeta("org-carry", "azure-foundry"); !ok || meta.UpdatedBy != "editor-b" || meta.SecretSetBy != "lender-a" {
		t.Fatalf("after the carrying edit: %+v (ok=%v), want editor-b as writer and lender-a as lender", meta, ok)
	}
	if disabled, err := svc.DisableOrgAzureFoundryConfigSavedBy("org-carry", "editor-b"); err != nil || disabled {
		t.Errorf("the carrying editor's departure: disabled=%v err=%v, want the lender's credential kept", disabled, err)
	}
	if has, _ := orgKeys.HasOrg("org-carry", "azure-foundry"); !has {
		t.Fatal("the editor's departure deleted the lender's credential")
	}
	if disabled, err := svc.DisableOrgAzureFoundryConfigSavedBy("org-carry", "lender-a"); err != nil || !disabled {
		t.Errorf("the lender's departure: disabled=%v err=%v, want disabled", disabled, err)
	}
	if orgs, err := svc.DisableOrgCredentialsSavedBy("editor-b"); err != nil || len(orgs) != 0 {
		t.Errorf("account erasure of a carrying editor disabled %v (%v), want none", orgs, err)
	}
	// Belt-and-braces: a row no save has stamped since v24 (empty
	// secret_set_by) still matches its last writer.
	_ = orgKeys.SaveOrg("org-unstamped", "azure-foundry", azureTestDoc, "editor-b")
	orgKeys.ClearSecretSetBy("org-unstamped", "azure-foundry")
	if disabled, err := svc.DisableOrgAzureFoundryConfigSavedBy("org-unstamped", "editor-b"); err != nil || !disabled {
		t.Errorf("unstamped row's writer: disabled=%v err=%v, want disabled", disabled, err)
	}
	// Account erasure: every org the user lent to.
	if _, err := svc.SaveOrgAzureFoundryConfig("org-3", "lender", "", doc); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveOrgAzureFoundryConfig("org-4", "lender", "", doc); err != nil {
		t.Fatal(err)
	}
	orgs, err := svc.DisableOrgCredentialsSavedBy("lender")
	if err != nil || strings.Join(orgs, ",") != "org-3,org-4" {
		t.Errorf("account erasure disabled %v (%v), want org-3 and org-4", orgs, err)
	}
	if has, _ := orgKeys.HasOrg("org-2", "azure-foundry"); !has {
		t.Error("account erasure removed a credential another admin saved")
	}

	noStore := newProviderServiceWithFakeSecrets(t, testutil.NewFakeSecretStore())
	if disabled, err := noStore.DisableOrgAzureFoundryConfigSavedBy("org-1", "lender"); disabled || err != nil {
		t.Errorf("no org store: disabled=%v err=%v", disabled, err)
	}
	if orgs, err := noStore.DisableOrgCredentialsSavedBy("lender"); orgs != nil || err != nil {
		t.Errorf("no org store: %v %v", orgs, err)
	}
}

// Org deletion and account erasure release the process state of a
// credential whose row cascades away: read before, released after.
func TestPrepareOrgDeletionAndAccountErasure_DropCaches(t *testing.T) {
	configureAzureForTests(t)
	secrets := testutil.NewFakeSecretStore()
	orgKeys := testutil.NewFakeOrgSecretStore()
	svc := newOrgProviderService(t, secrets, orgKeys)
	_ = orgKeys.SaveOrg("org-1", "azure-foundry", azureTestDoc, "admin")
	_ = secrets.Save("u1", "azure-foundry", azureTestDoc)

	release := svc.PrepareOrgDeletion("org-1")
	_ = orgKeys.DeleteOrg("org-1", "azure-foundry") // the cascade
	svc.modelCache[ai.OrgScope("org-1")+"\x00glm"] = modelCacheEntry{}
	release()
	if _, cached := svc.modelCache[ai.OrgScope("org-1")+"\x00glm"]; cached {
		t.Error("org deletion must drop the org's cached listings")
	}

	releaseOwn := svc.PrepareAccountErasure("u1")
	_ = secrets.Delete("u1", "azure-foundry")
	svc.modelCache["u1\x00glm"] = modelCacheEntry{}
	releaseOwn()
	if _, cached := svc.modelCache["u1\x00glm"]; cached {
		t.Error("account erasure must drop the user's cached listings")
	}
}

// R-4: the chat service's budget gate (AIBudget) is what the knowledge base consults.
func TestChatService_CheckBudget(t *testing.T) {
	backend := testutil.NewFakeBackend()
	backend.DailyUsage = 5
	svc := &ChatService{settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 5}}}, backend: backend}
	if err := svc.aiBudget().CheckBudget(context.Background(), "u1", "org-1"); !errors.Is(err, ErrAIBudgetExceeded) {
		t.Errorf("spent budget: %v, want ErrAIBudgetExceeded", err)
	}
	backend.DailyUsage = 1
	if err := svc.aiBudget().CheckBudget(context.Background(), "u1", "org-1"); err != nil {
		t.Errorf("budget left: %v", err)
	}
	backend.UsageErr = errors.New("db down")
	if err := svc.aiBudget().CheckBudget(context.Background(), "u1", "org-1"); !errors.Is(err, ErrAIBudgetUnavailable) {
		t.Errorf("unreadable spend: %v, want ErrAIBudgetUnavailable (fail closed)", err)
	}
}

// An unreadable stored org credential (a decrypt failure after a key
// rotation, or a document this build cannot parse) can be repaired from the
// UI — the token-less save with a fresh secret becomes a true compare-and-
// swap on the unreadable row's own stamp instead of the always-409
// INSERT ... DO NOTHING branch.
func TestSaveOrgAzureFoundryConfig_ReplacesUnreadable(t *testing.T) {
	doc := ai.AzureFoundryConfig{Endpoint: "https://" + azureTestHost, Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "fresh-key"}, ChatDeployment: "c1"}

	t.Run("decrypt failure repaired, repairing admin becomes the lender", func(t *testing.T) {
		configureAzureForTests(t)
		keys := testutil.NewFakeOrgSecretStore()
		store := &unreadableOrgStore{FakeOrgSecretStore: keys}
		svc := newOrgProviderService(t, testutil.NewFakeSecretStore(), keys)
		svc.orgKeys = store
		if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-a", "", doc); err != nil {
			t.Fatal(err)
		}
		store.getErr = storage.ErrSecretDecryptFailed

		// "Enter again": the GET cannot supply an updatedAt, the save supplies
		// a fresh secret — that combination now repairs the row.
		at, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-b", "", doc)
		if err != nil || at == "" {
			t.Fatalf("repair save: %q, %v", at, err)
		}
		store.getErr = nil
		pub, err := svc.GetOrgAzureFoundryConfig("org-1")
		if err != nil || !pub.Configured || pub.SecretSetByID != "admin-b" || pub.UpdatedByID != "admin-b" {
			t.Fatalf("after the repair: %+v, %v; want configured, admin-b as lender and writer", pub, err)
		}
		if pub.UpdatedAt != at {
			t.Errorf("GET updatedAt = %q, want the repair's token %q", pub.UpdatedAt, at)
		}

		// A secret-less token-less save over a still-unreadable row is the
		// ordinary MISSING_SECRET: there is nothing to carry.
		store.getErr = storage.ErrSecretDecryptFailed
		empty := doc
		empty.Auth.Key = ""
		if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-c", "", empty); azureCfgErrCode(t, err) != ai.AzureCfgErrMissingSecret {
			t.Errorf("secret-less save: %v, want MISSING_SECRET", err)
		}
	})

	t.Run("unparseable stored document", func(t *testing.T) {
		configureAzureForTests(t)
		keys := testutil.NewFakeOrgSecretStore()
		store := &unreadableOrgStore{FakeOrgSecretStore: keys}
		svc := newOrgProviderService(t, testutil.NewFakeSecretStore(), keys)
		svc.orgKeys = store
		_ = store.SaveOrg("org-1", "azure-foundry", `{"endpoint":`, "admin-a")

		at, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-b", "", doc)
		if err != nil || at == "" {
			t.Fatalf("repair of an unparseable row: %q, %v", at, err)
		}
		if pub, err := svc.GetOrgAzureFoundryConfig("org-1"); err != nil || !pub.Configured || pub.SecretSetByID != "admin-b" {
			t.Fatalf("after the repair: %+v, %v", pub, err)
		}
	})

	t.Run("a readable row still needs its token", func(t *testing.T) {
		configureAzureForTests(t)
		keys := testutil.NewFakeOrgSecretStore()
		store := &unreadableOrgStore{FakeOrgSecretStore: keys}
		svc := newOrgProviderService(t, testutil.NewFakeSecretStore(), keys)
		svc.orgKeys = store
		if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-a", "", doc); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.SaveOrgAzureFoundryConfig("org-1", "admin-b", "", doc); !errors.Is(err, ErrOrgCredentialConflict) {
			t.Errorf("token-less save over a readable row: %v, want ErrOrgCredentialConflict", err)
		}
	})
}
