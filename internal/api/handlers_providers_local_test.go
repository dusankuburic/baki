package api

// The local models provider (Foundry Local / Ollama / LM Studio) as a
// product feature — configuration routes, keys/save rejection, the cloud
// policy gate (on reads too), the context window and the provider listing.

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/testutil"
)

// setLocalModelsPolicy installs the runtime egress policy for the test and
// restores the cloud default (BlockInternal) afterwards.
func setLocalModelsPolicy(t *testing.T, p egress.Policy) {
	t.Helper()
	ai.ConfigureLocalModels(ai.LocalModelsOptions{Egress: p})
	t.Cleanup(func() { ai.ConfigureLocalModels(ai.LocalModelsOptions{Egress: egress.BlockInternal}) })
}

type localHarness struct {
	h       *ProviderHandler
	svc     *service.ProviderService
	sys     *service.SystemService
	secrets *testutil.FakeSecretStore
}

func newLocalHarness(t *testing.T) *localHarness {
	t.Helper()
	secrets := testutil.NewFakeSecretStore()
	factory := ai.NewProviderFactory(func(scope, provider string) (string, error) {
		v, _ := secrets.Get(scope, provider)
		return v, nil
	}, nil, nil, nil)
	svc := service.NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets, nil, nil)
	sys := service.NewSystemService(storage.NewInMemorySettingsStore(), secrets, nil, nil, "local", nil)
	sys.OnApiKeyChanged(svc.InvalidateModels)
	return &localHarness{h: NewProviderHandler(svc, &SecurityConfig{}, nil), svc: svc, sys: sys, secrets: secrets}
}

func (l *localHarness) post(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/providers/local/config", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	l.h.handleSaveLocalConfig(rr, req)
	return rr
}

func (l *localHarness) getRaw(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	l.h.handleGetLocalConfig(rr, httptest.NewRequest(http.MethodGet, "/api/providers/local/config", nil))
	return rr
}

func (l *localHarness) get(t *testing.T) map[string]any {
	t.Helper()
	rr := l.getRaw(t)
	checkStatus(t, rr, http.StatusOK)
	return azureDecode(t, rr)
}

func (l *localHarness) localEntry(t *testing.T) map[string]any {
	t.Helper()
	rr := httptest.NewRecorder()
	l.h.handleListProviders(rr, httptest.NewRequest(http.MethodGet, "/api/providers/list", nil))
	checkStatus(t, rr, http.StatusOK)
	var providers []map[string]any
	decodeJSON(t, rr, &providers)
	for _, p := range providers {
		if p["id"] == ai.LocalProviderID {
			return p
		}
	}
	return nil
}

// The save route's validation ladder: cloud policy first, then body shape,
// then the loopback-only URL rules; nothing is stored on a refusal.
func TestLocalConfigSave_ValidationLadder(t *testing.T) {
	t.Run("cloud policy refuses even a valid URL", func(t *testing.T) {
		setLocalModelsPolicy(t, egress.BlockInternal)
		l := newLocalHarness(t)
		// Even a malformed body or a bad window: the cloud refusal comes first.
		for _, body := range []string{`{"baseUrl":"http://127.0.0.1:11434/v1"}`, `{`, `{"baseUrl":"http://127.0.0.1:11434","contextWindow":1}`} {
			rr := l.post(t, body)
			checkStatus(t, rr, http.StatusBadRequest)
			if got := azureDecode(t, rr); got["code"] != ai.LocalCfgErrUnavailable {
				t.Errorf("%s: code = %v, want LOCAL_MODELS_UNAVAILABLE", body, got["code"])
			}
		}
		if has, _ := l.secrets.Has("", ai.LocalProviderID); has {
			t.Error("a refused save stored a document")
		}
	})

	t.Run("bad context windows", func(t *testing.T) {
		setLocalModelsPolicy(t, egress.AllowLoopback)
		l := newLocalHarness(t)
		for name, window := range map[string]string{
			"below the floor":   `1023`,
			"above the ceiling": `2000001`,
			"negative":          `-8192`,
			"fractional":        `8192.5`,
			"string":            `"8192"`,
			"overflow":          `1e30`,
		} {
			t.Run(name, func(t *testing.T) {
				rr := l.post(t, `{"baseUrl":"http://127.0.0.1:11434","contextWindow":`+window+`}`)
				checkStatus(t, rr, http.StatusBadRequest)
				if got := azureDecode(t, rr); got["code"] != ai.LocalCfgErrInvalidContextWindow {
					t.Errorf("code = %v (%v), want INVALID_CONTEXT_WINDOW", got["code"], got["message"])
				}
				if has, _ := l.secrets.Has("", ai.LocalProviderID); has {
					t.Error("a refused save stored a document")
				}
			})
		}
	})

	t.Run("bad URLs", func(t *testing.T) {
		setLocalModelsPolicy(t, egress.AllowLoopback)
		l := newLocalHarness(t)
		for name, body := range map[string]string{
			"empty":          `{"baseUrl":""}`,
			"malformed":      `{`,
			"unknown field":  `{"baseUrl":"http://127.0.0.1:11434","model":"x"}`,
			"trailing data":  `{"baseUrl":"http://127.0.0.1:11434"}{"baseUrl":"http://10.0.0.1"}`,
			"remote host":    `{"baseUrl":"https://api.openai.com/v1"}`,
			"private ip":     `{"baseUrl":"http://10.0.0.5:11434"}`,
			"metadata ip":    `{"baseUrl":"http://169.254.169.254"}`,
			"lookalike host": `{"baseUrl":"http://localhost.evil.com:11434"}`,
			"ftp":            `{"baseUrl":"ftp://127.0.0.1:21"}`,
			"credentials":    `{"baseUrl":"http://u:p@127.0.0.1:11434"}`,
			"path":           `{"baseUrl":"http://127.0.0.1:11434/ollama"}`,
			"query":          `{"baseUrl":"http://127.0.0.1:11434?x=1"}`,
		} {
			t.Run(name, func(t *testing.T) {
				rr := l.post(t, body)
				checkStatus(t, rr, http.StatusBadRequest)
				if got := azureDecode(t, rr); got["code"] != ai.LocalCfgErrInvalidBaseURL {
					t.Errorf("code = %v, want INVALID_BASE_URL", got["code"])
				}
				if has, _ := l.secrets.Has("", ai.LocalProviderID); has {
					t.Error("a refused save stored a document")
				}
			})
		}
	})
}

func TestLocalConfigRoutes_SaveGetAndDelete(t *testing.T) {
	setLocalModelsPolicy(t, egress.AllowLoopback)
	l := newLocalHarness(t)

	if got := l.get(t); got["configured"] != false || got["baseUrl"] != "" {
		t.Fatalf("unconfigured GET = %v", got)
	}
	// The documented example carries the /v1 root; it is stored without it.
	rr := l.post(t, `{"baseUrl":"http://127.0.0.1:11434/v1"}`)
	checkStatus(t, rr, http.StatusOK)
	if got := azureDecode(t, rr); got["status"] != "ok" {
		t.Errorf("save response = %v", got)
	}
	if got := l.get(t); got["configured"] != true || got["baseUrl"] != "http://127.0.0.1:11434" {
		t.Errorf("GET after save = %v", got)
	}
	raw, _ := l.secrets.Get("", ai.LocalProviderID)
	if cfg, err := ai.ParseLocalConfig(raw); err != nil || cfg.BaseURL != "http://127.0.0.1:11434" {
		t.Errorf("stored document = %q (%v)", raw, err)
	}
	if got := l.get(t); got["contextWindow"] != nil {
		t.Errorf("an unset window must be omitted: %v", got)
	}
	// IPv6 loopback and a replacement save, now with a context window.
	checkStatus(t, l.post(t, `{"baseUrl":"http://[::1]:1234/","contextWindow":32768}`), http.StatusOK)
	if got := l.get(t); got["baseUrl"] != "http://[::1]:1234" || got["contextWindow"] != float64(32768) {
		t.Errorf("GET after replace = %v", got)
	}
	raw, _ = l.secrets.Get("", ai.LocalProviderID)
	if cfg, err := ai.ParseLocalConfig(raw); err != nil || cfg.ContextWindow != 32768 {
		t.Errorf("stored document = %q (%v)", raw, err)
	}
	// Saving without a window goes back to the default.
	checkStatus(t, l.post(t, `{"baseUrl":"http://[::1]:1234","contextWindow":0}`), http.StatusOK)
	if got := l.get(t); got["contextWindow"] != nil {
		t.Errorf("GET after clearing the window = %v", got)
	}

	// Removal goes through the existing keys/delete service path.
	if err := l.sys.DeleteApiKey("", ai.LocalProviderID); err != nil {
		t.Fatal(err)
	}
	if got := l.get(t); got["configured"] != false {
		t.Errorf("GET after delete = %v", got)
	}
}

// Under the cloud policy the read is refused like a save (400
// LOCAL_MODELS_UNAVAILABLE, the same message), stored document or not — the
// settings UI drops the section on it instead of offering an unusable form.
func TestLocalConfigGet_RefusedUnderCloudPolicy(t *testing.T) {
	setLocalModelsPolicy(t, egress.AllowLoopback)
	l := newLocalHarness(t)
	checkStatus(t, l.post(t, `{"baseUrl":"http://127.0.0.1:11434"}`), http.StatusOK)

	setLocalModelsPolicy(t, egress.BlockInternal)
	rr := l.getRaw(t)
	checkStatus(t, rr, http.StatusBadRequest)
	got := azureDecode(t, rr)
	if got["code"] != ai.LocalCfgErrUnavailable {
		t.Errorf("code = %v, want LOCAL_MODELS_UNAVAILABLE", got["code"])
	}
	save := azureDecode(t, l.post(t, `{"baseUrl":"http://127.0.0.1:11434"}`))
	if got["message"] == "" || got["message"] != save["message"] {
		t.Errorf("GET message %q, save message %q: want the same refusal", got["message"], save["message"])
	}
	if _, has := got["baseUrl"]; has {
		t.Errorf("the refusal leaked the stored configuration: %v", got)
	}
}

// The save's audit row names the server host and the configured window
// (a token count), nothing else.
func TestLocalConfigSave_AuditMeta(t *testing.T) {
	setLocalModelsPolicy(t, egress.AllowLoopback)
	auditCh := captureAuditEvents(t, 2)
	l := newLocalHarness(t)
	l.h.audit = testAuditRecorder(testutil.NewFakeBackend())
	for body, want := range map[string]map[string]string{
		`{"baseUrl":"http://127.0.0.1:11434/v1","contextWindow":16384}`: {"op": "save", "baseUrlHost": "127.0.0.1:11434", "contextWindow": "16384"},
		`{"baseUrl":"http://127.0.0.1:11434"}`:                          {"op": "save", "baseUrlHost": "127.0.0.1:11434"},
	} {
		checkStatus(t, l.post(t, body), http.StatusOK)
		select {
		case ev := <-auditCh:
			if ev.Action != audit.ActionKeysSave || ev.ResourceID != ai.LocalProviderID || len(ev.Meta) != len(want) {
				t.Errorf("%s: audit event = %+v (meta %v)", body, ev, ev.Meta)
			}
			for k, v := range want {
				if ev.Meta[k] != v {
					t.Errorf("%s: meta[%s] = %q, want %q", body, k, ev.Meta[k], v)
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s: no audit event was enqueued", body)
		}
	}
}

// keys/save is refused for `local` (a raw key would bypass the loopback
// validator); has/delete accept the provider id.
func TestSaveApiKey_RejectsLocal(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/keys/save", map[string]any{"provider": "local", "key": `{"baseUrl":"http://10.0.0.1"}`})
	checkStatus(t, rr, http.StatusBadRequest)
	got := azureDecode(t, rr)
	if got["code"] != "USE_PROVIDER_CONFIG" {
		t.Errorf("response = %v", got)
	}
	if msg, _ := got["message"].(string); !strings.Contains(msg, "/api/providers/local/config") {
		t.Errorf("message %q does not point at the config route", msg)
	}
	if !validProviders[ai.LocalProviderID] {
		t.Error("local must be a valid provider id for keys/has and keys/delete")
	}
}

// The routes sit behind the router's authentication: no token is a 401, a
// signed-in member reaches the handler (which answers the cloud policy on
// both the read and the save).
func TestLocalConfigRoutes_AuthLadder(t *testing.T) {
	setLocalModelsPolicy(t, egress.BlockInternal)
	rt := newTestRouter(nil, true)
	body := map[string]any{"baseUrl": "http://127.0.0.1:11434"}

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/providers/local/config", "", nil), http.StatusUnauthorized)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/providers/local/config", "", body), http.StatusUnauthorized)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/providers/local/config", "Bearer not-a-token", body), http.StatusUnauthorized)

	pair, err := rt.security.AuthMgr.Issue("u-member", "m@example.com", auth.RoleMember)
	if err != nil {
		t.Fatal(err)
	}
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/providers/local/config", "Bearer "+pair.AccessToken, body)
	checkStatus(t, rr, http.StatusBadRequest)
	if got := azureDecode(t, rr); got["code"] != ai.LocalCfgErrUnavailable {
		t.Errorf("POST code = %v, want LOCAL_MODELS_UNAVAILABLE", got["code"])
	}
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/providers/local/config", "Bearer "+pair.AccessToken, nil)
	checkStatus(t, rr, http.StatusBadRequest)
	if got := azureDecode(t, rr); got["code"] != ai.LocalCfgErrUnavailable {
		t.Errorf("GET code = %v, want LOCAL_MODELS_UNAVAILABLE", got["code"])
	}
}

// The provider appears in GET /api/providers/list only when configured (and
// only where local models are allowed), with the server's model ids, tool
// support and zero pricing; a config change or removal drops the cached
// listing.
func TestListProviders_LocalOnlyWhenConfigured(t *testing.T) {
	setLocalModelsPolicy(t, egress.AllowLoopback)
	var models atomic.Value
	models.Store([]string{"qwen2.5:7b", "nomic-embed-text", "phi-4-mini"})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v1/models") {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		data := []map[string]any{}
		for _, id := range models.Load().([]string) {
			data = append(data, map[string]any{"id": id})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)

	l := newLocalHarness(t)
	if p := l.localEntry(t); p != nil {
		t.Fatalf("unconfigured local provider is listed: %v", p)
	}

	checkStatus(t, l.post(t, `{"baseUrl":"`+srv.URL+`/v1"}`), http.StatusOK)
	p := l.localEntry(t)
	if p == nil {
		t.Fatal("configured local provider is not listed")
	}
	if p["name"] != "Local model (OpenAI-compatible)" || p["configured"] != true || p["authType"] != "api_key" {
		t.Errorf("entry = %v", p)
	}
	// The embedding model is not offered.
	ms, _ := p["models"].([]any)
	if len(ms) != 2 {
		t.Fatalf("models = %v", p["models"])
	}
	first := ms[0].(map[string]any)
	if first["id"] != "qwen2.5:7b" || first["supportsTools"] != true || first["inputCostPerM"] != float64(0) {
		t.Errorf("first model = %v", first)
	}
	if p["defaultModel"] != "qwen2.5:7b" {
		t.Errorf("defaultModel = %v, want the first served model", p["defaultModel"])
	}
	// No window configured: 8192, on the entry and the provider.
	if first["contextLimit"] != float64(ai.LocalDefaultContextWindow) || p["contextLimit"] != float64(ai.LocalDefaultContextWindow) {
		t.Errorf("context limits = %v / %v, want the 8192 default", first["contextLimit"], p["contextLimit"])
	}

	// Saving again invalidates the cached listing; the new window applies.
	models.Store([]string{"llama3.2:3b"})
	checkStatus(t, l.post(t, `{"baseUrl":"`+srv.URL+`","contextWindow":32768}`), http.StatusOK)
	p = l.localEntry(t)
	ms, _ = p["models"].([]any)
	if len(ms) != 1 || ms[0].(map[string]any)["id"] != "llama3.2:3b" {
		t.Errorf("listing after re-save = %v", ms)
	} else if ms[0].(map[string]any)["contextLimit"] != float64(32768) || p["contextLimit"] != float64(32768) {
		t.Errorf("context limits after re-save = %v / %v, want 32768", ms[0].(map[string]any)["contextLimit"], p["contextLimit"])
	}

	// Removal drops the provider from the list again.
	if err := l.sys.DeleteApiKey("", ai.LocalProviderID); err != nil {
		t.Fatal(err)
	}
	if p := l.localEntry(t); p != nil {
		t.Errorf("removed provider is still listed: %v", p)
	}

	// A stored document under the cloud policy stays hidden.
	checkStatus(t, l.post(t, `{"baseUrl":"`+srv.URL+`"}`), http.StatusOK)
	setLocalModelsPolicy(t, egress.BlockInternal)
	if p := l.localEntry(t); p != nil {
		t.Errorf("local provider listed under the cloud policy: %v", p)
	}
}

// The connection test reaches the configured server through the existing
// test route's service path, with no model named in the document.
func TestLocalConnectionTest(t *testing.T) {
	setLocalModelsPolicy(t, egress.AllowLoopback)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/v1/models"):
			_, _ = w.Write([]byte(`{"data":[{"id":"phi-4-mini"}]}`))
		case strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] != "phi-4-mini" {
				t.Errorf("test model = %v, want the first served model", body["model"])
			}
			_, _ = w.Write([]byte(`{"model":"phi-4-mini","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	l := newLocalHarness(t)
	checkStatus(t, l.post(t, `{"baseUrl":"`+srv.URL+`"}`), http.StatusOK)

	var buf bytes.Buffer
	_ = json.NewEncoder(&buf).Encode(map[string]string{"provider": ai.LocalProviderID})
	req := httptest.NewRequest(http.MethodPost, "/api/providers/test", &buf)
	rr := httptest.NewRecorder()
	l.h.handleTestProviderConnection(rr, req)
	checkStatus(t, rr, http.StatusOK)
	if got := azureDecode(t, rr); got["ok"] != true {
		t.Errorf("connection test = %v", got)
	}
}
