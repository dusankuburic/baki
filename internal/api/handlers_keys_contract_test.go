package api

// Step 5 of the AI roadmap: what /api/keys/* and the credential routes answer.
//   - R-config-3: a key is trimmed, never empty, bounded and free of control characters (400 KEY_INVALID); the
//     body is bounded and names no field the provider's credential does not declare.
//   - R-provider-2: an unreachable secret store answers 503 SECRET_STORAGE_UNAVAILABLE on every route that writes
//     or removes a credential, as the Azure routes already did.
//   - R-provider-18: a stored credential the keystore cannot decrypt (the encryption key changed) is "unreadable",
//     never "configured" or "not connected".

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/testutil"
)

// keyStoreFault is a KeyStore whose writes and removals fail with err (when set) and whose reads of the
// unreadable entries fail as a rotated encryption key makes them fail.
type keyStoreFault struct {
	*testutil.FakeSecretStore
	err        error
	unreadable map[string]bool
}

func (s keyStoreFault) Save(scope, provider, key string) error {
	if s.err != nil {
		return s.err
	}
	return s.FakeSecretStore.Save(scope, provider, key)
}

func (s keyStoreFault) Delete(scope, provider string) error {
	if s.err != nil {
		return s.err
	}
	return s.FakeSecretStore.Delete(scope, provider)
}

func (s keyStoreFault) Get(scope, provider string) (string, error) {
	if s.unreadable[provider] {
		return "", storage.ErrSecretDecryptFailed
	}
	v, err := s.FakeSecretStore.Get(scope, provider)
	if err == nil && v == "" {
		if has, _ := s.FakeSecretStore.Has(scope, provider); !has {
			return "", storage.ErrSecretNotFound
		}
	}
	return v, err
}

func (s keyStoreFault) Has(scope, provider string) (bool, error) {
	if s.unreadable[provider] {
		return true, nil
	}
	return s.FakeSecretStore.Has(scope, provider)
}

func keysHandler(secrets service.KeyStore) *SystemHandler {
	sys := service.NewSystemService(storage.NewInMemorySettingsStore(), secrets, nil, nil, "local", nil, nil)
	return NewSystemHandler(sys, &SecurityConfig{}, nil, nil, nil, nil)
}

func callHandler(handle http.HandlerFunc, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handle(rr, req)
	return rr
}

func keysErrorCode(t *testing.T, rr *httptest.ResponseRecorder) string {
	t.Helper()
	var body struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("error body %q: %v", rr.Body.String(), err)
	}
	return body.Code
}

// R-config-3, done when: "" and "   " answer 400 KEY_INVALID and nothing is stored; a key with a control
// character is refused alike; a key is stored trimmed.
func TestSaveApiKey_RefusesAnUnusableKey(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	h := keysHandler(secrets)
	for name, key := range map[string]string{
		"empty":              "",
		"blank":              "   ",
		"a newline inside":   "sk-abc\ndef",
		"a NUL":              "sk-abc\x00",
		"an escape":          "\x1b[31msk-abc",
		"over the length":    strings.Repeat("k", service.MaxAPIKeyBytes+1),
		"a tab then blanks":  "\t  \t",
		"a zero-width space": "sk-abc\u200b",
		"a byte-order mark":  "\ufeffsk-abc",
	} {
		t.Run(name, func(t *testing.T) {
			body, _ := json.Marshal(map[string]string{"provider": "openai", "key": key})
			rr := callHandler(h.handleSaveApiKey, http.MethodPost, "/api/keys/save", string(body))
			checkStatus(t, rr, http.StatusBadRequest)
			if code := keysErrorCode(t, rr); code != service.ProviderCodeKeyInvalid {
				t.Errorf("code = %q, want %s", code, service.ProviderCodeKeyInvalid)
			}
			if has, _ := secrets.Has("", "openai"); has {
				t.Error("the refused key was stored")
			}
		})
	}

	rr := callHandler(h.handleSaveApiKey, http.MethodPost, "/api/keys/save", `{"provider":"openai","key":"  sk-good  "}`)
	checkStatus(t, rr, http.StatusOK)
	if got, _ := secrets.Get("", "openai"); got != "sk-good" {
		t.Errorf("stored key = %q, want it trimmed", got)
	}
}

// R-config-3, done when: a 1 MiB key is refused and nothing is stored (the body cap answers before the key check);
// a body naming a field no credential of the provider declares is refused.
func TestSaveApiKey_BoundsTheBody(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	h := keysHandler(secrets)

	rr := callHandler(h.handleSaveApiKey, http.MethodPost, "/api/keys/save",
		`{"provider":"openai","key":"`+strings.Repeat("k", 1<<20)+`"}`)
	if rr.Code != http.StatusBadRequest || !strings.Contains(rr.Body.String(), "request body too large") {
		t.Errorf("1 MiB key: %d %s, want 400 from the body cap (not the key check)", rr.Code, rr.Body.String())
	}
	if has, _ := secrets.Has("", "openai"); has {
		t.Error("the 1 MiB key was stored")
	}

	rr = callHandler(h.handleSaveApiKey, http.MethodPost, "/api/keys/save", `{"provider":"openai","key":"sk-1","endpoint":"https://evil.example"}`)
	checkStatus(t, rr, http.StatusBadRequest)
	if has, _ := secrets.Has("", "openai"); has {
		t.Error("a body with an undeclared field stored its key")
	}

	// A credential's declared field names ride beside the key (no registered spec declares a non-secret field
	// today, so the allow-list is checked on a synthetic schema); any other name is refused, case and all.
	declared := []ai.CredentialField{{Name: "pat", Type: ai.FieldSecret}, {Name: "region", Type: ai.FieldSelect}}
	for body, wantOK := range map[string]bool{
		`{"provider":"x","key":"k"}`:                true,
		`{"provider":"x","key":"k","region":"eu"}`:  true,
		`{"provider":"x","key":"k","pat":"p"}`:      true,
		`{"provider":"x","key":"k","Region":"eu"}`:  false,
		`{"provider":"x","key":"k","endpoint":"e"}`: false,
		`{"provider":"x","Key":"k","region":"eu"}`:  false,
	} {
		var fields map[string]json.RawMessage
		_ = json.Unmarshal([]byte(body), &fields)
		if name, ok := undeclaredKeyField(declared, fields); ok != wantOK {
			t.Errorf("%s: ok=%v (field %q), want %v", body, ok, name, wantOK)
		}
	}
}

// R-provider-2, done when: with the secret store unreachable, saving and removing a key answer 503
// SECRET_STORAGE_UNAVAILABLE, not an opaque 500 (the Azure create route's code, pinned in
// handlers_azure_registry_test.go), and so does saving the local models configuration.
func TestKeys_SecretStorageUnavailableIs503(t *testing.T) {
	secrets := keyStoreFault{FakeSecretStore: testutil.NewFakeSecretStore(), err: storage.ErrSecretStorageUnavailable}
	h := keysHandler(secrets)
	for _, tc := range []struct {
		path   string
		handle http.HandlerFunc
		body   string
	}{
		{"/api/keys/save", h.handleSaveApiKey, `{"provider":"openai","key":"sk-1"}`},
		{"/api/keys/delete", h.handleDeleteApiKey, `{"provider":"openai"}`},
	} {
		rr := callHandler(tc.handle, http.MethodPost, tc.path, tc.body)
		checkStatus(t, rr, http.StatusServiceUnavailable)
		if code := keysErrorCode(t, rr); code != service.ProviderCodeSecretStorageUnavailable {
			t.Errorf("%s: code = %q, want %s", tc.path, code, service.ProviderCodeSecretStorageUnavailable)
		}
	}

	setLocalModelsPolicy(t, egress.AllowLoopback)
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "", nil }, nil, nil, nil)
	ph := NewProviderHandler(service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets, nil, nil), &SecurityConfig{}, nil)
	rr := callHandler(ph.handleSaveLocalConfig, http.MethodPost, "/api/providers/local/config", `{"baseUrl":"http://127.0.0.1:11434"}`)
	checkStatus(t, rr, http.StatusServiceUnavailable)
	if code := keysErrorCode(t, rr); code != service.ProviderCodeSecretStorageUnavailable {
		t.Errorf("local config: code = %q, want %s", code, service.ProviderCodeSecretStorageUnavailable)
	}
}

// R-provider-18, done when: /api/keys/has reports has:true, unreadable:true for a key the keystore cannot decrypt;
// a readable key and an absent one carry no unreadable flag.
func TestHasApiKey_ReportsAnUnreadableKey(t *testing.T) {
	secrets := keyStoreFault{FakeSecretStore: testutil.NewFakeSecretStore(), unreadable: map[string]bool{"openai": true}}
	_ = secrets.FakeSecretStore.Save("", "gemini", "AIza-1")
	h := keysHandler(secrets)
	for provider, want := range map[string]map[string]any{
		"openai": {"has": true, "unreadable": true},
		"gemini": {"has": true},
		"xai":    {"has": false},
	} {
		rr := callHandler(h.handleHasApiKey, http.MethodPost, "/api/keys/has", `{"provider":"`+provider+`"}`)
		checkStatus(t, rr, http.StatusOK)
		var got map[string]any
		decodeJSON(t, rr, &got)
		if len(got) != len(want) || got["has"] != want["has"] || got["unreadable"] != want["unreadable"] {
			t.Errorf("%s: /api/keys/has = %v, want %v", provider, got, want)
		}
	}
}

// R-provider-18, done when: the catalogue reports credentialStatus "unreadable" for a provider whose key cannot be
// decrypted and does not call it configured; "configured" and "missing" for the others.
func TestProviderCatalog_ReportsAnUnreadableKey(t *testing.T) {
	secrets := keyStoreFault{FakeSecretStore: testutil.NewFakeSecretStore(), unreadable: map[string]bool{"openai": true}}
	_ = secrets.FakeSecretStore.Save("", "gemini", "AIza-1")
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "", nil }, nil, nil, nil)
	ph := NewProviderHandler(service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets, nil, nil), &SecurityConfig{}, nil)

	out, err := ph.providerCatalog(context.Background(), providerCatalogRequest{})
	if err != nil {
		t.Fatalf("catalogue: %v", err)
	}
	raw, _ := json.Marshal(out)
	var body struct {
		Providers []struct {
			ID               string `json:"id"`
			Configured       bool   `json:"configured"`
			CredentialStatus string `json:"credentialStatus"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		configured bool
		status     string
	}{
		"openai": {false, "unreadable"},
		"gemini": {true, "configured"},
		"xai":    {false, "missing"},
	}
	for _, p := range body.Providers {
		w, ok := want[p.ID]
		if !ok {
			continue
		}
		delete(want, p.ID)
		if p.Configured != w.configured || p.CredentialStatus != w.status {
			t.Errorf("%s: configured=%v credentialStatus=%q, want configured=%v %q", p.ID, p.Configured, p.CredentialStatus, w.configured, w.status)
		}
	}
	for id := range want {
		t.Errorf("%s missing from the catalogue", id)
	}
}

// R-provider-18, done when: the device-flow user route answers a coded error for a token the keystore cannot
// decrypt, never null ("not connected" while the catalogue said configured).
func TestDeviceFlowUser_UnreadableTokenIsCoded(t *testing.T) {
	secrets := keyStoreFault{FakeSecretStore: testutil.NewFakeSecretStore(), unreadable: map[string]bool{
		"github-models-token": true, "copilot-oauth-token": true,
	}}
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "", nil }, nil, nil, nil)
	ph := NewProviderHandler(service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets, nil, nil), &SecurityConfig{}, nil)
	for path, handle := range map[string]http.HandlerFunc{
		"/api/providers/github/user":  ph.handleGetGitHubUser,
		"/api/providers/copilot/user": ph.handleGetCopilotUser,
	} {
		rr := callHandler(handle, http.MethodGet, path, "")
		if rr.Code < 400 {
			t.Errorf("%s: status %d body %s, want a coded error", path, rr.Code, rr.Body.String())
			continue
		}
		if code := keysErrorCode(t, rr); code != service.ProviderCodeKeyUnreadable {
			t.Errorf("%s: code = %q, want %s", path, code, service.ProviderCodeKeyUnreadable)
		}
	}
}

// keyStoreFault itself: a missing entry reads as storage.ErrSecretNotFound, as both real keystores answer.
func TestKeyStoreFault_MissingIsNotFound(t *testing.T) {
	s := keyStoreFault{FakeSecretStore: testutil.NewFakeSecretStore()}
	if _, err := s.Get("", "openai"); !errors.Is(err, storage.ErrSecretNotFound) {
		t.Errorf("Get on a missing entry = %v, want ErrSecretNotFound", err)
	}
}

// R-config-2, done when: a 2 MiB user settings body is refused like the org route's, and a 64 KiB
// systemPromptSuffix answers 400 with copy naming the field.
func TestUpdateSettings_BoundsTheBodyAndTheAISection(t *testing.T) {
	h := keysHandler(testutil.NewFakeSecretStore())

	big := `{"ai":{"systemPromptSuffix":"` + strings.Repeat("x", 2<<20) + `"}}`
	rr := callHandler(h.handleUpdateSettings, http.MethodPost, "/api/system/settings", big)
	if rr.Code != http.StatusBadRequest && rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("2 MiB body: status %d, want 400 or 413", rr.Code)
	}
	if strings.Contains(rr.Body.String(), "ai.systemPromptSuffix") {
		t.Error("the 2 MiB body reached the field check: the route read it whole")
	}

	rr = callHandler(h.handleUpdateSettings, http.MethodPost, "/api/system/settings",
		`{"ai":{"activeProvider":"claude","systemPromptSuffix":"`+strings.Repeat("x", 64<<10)+`"}}`)
	checkStatus(t, rr, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "ai.systemPromptSuffix") {
		t.Errorf("64 KiB suffix: body %s, want it to name ai.systemPromptSuffix", rr.Body.String())
	}
}

// Review: the org route writes the deployment's document on the desktop, which every turn reads its custom
// instructions from; it refuses an oversized suffix with 400 like the member route.
func TestUpdateOrgSettings_RefusesAnOversizedSuffix(t *testing.T) {
	h := keysHandler(testutil.NewFakeSecretStore())
	req := httptest.NewRequest(http.MethodPost, "/api/system/settings/org/org-1",
		strings.NewReader(`{"ai":{"systemPromptSuffix":"`+strings.Repeat("x", 64<<10)+`"}}`))
	rctx := chi.NewRouteContext()
	rctx.URLParams.Add("id", "org-1")
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
	rr := httptest.NewRecorder()
	h.handleUpdateOrgSettings(rr, req)
	checkStatus(t, rr, http.StatusBadRequest)
	if !strings.Contains(rr.Body.String(), "ai.systemPromptSuffix") {
		t.Errorf("body %s, want it to name ai.systemPromptSuffix", rr.Body.String())
	}
}
