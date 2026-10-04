package ai

// The local models provider (Foundry Local, Ollama, LM Studio) —
// loopback-only validation, the egress-policy gate, listing, chat, the
// key-less request, the context window and the shared client.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/egress"
)

func saveLocalModelsPolicy(t *testing.T, policy egress.Policy) {
	t.Helper()
	localModelsRuntime.mu.Lock()
	prev := localModelsRuntime.egress
	localModelsRuntime.egress = policy
	localModelsRuntime.mu.Unlock()
	t.Cleanup(func() {
		localModelsRuntime.mu.Lock()
		localModelsRuntime.egress = prev
		localModelsRuntime.mu.Unlock()
	})
}

func TestLocalConfig_Validation(t *testing.T) {
	saveLocalModelsPolicy(t, egress.AllowLoopback)
	cases := []struct {
		raw  string
		want string // "" for valid; "PARSE-" prefixes a decode failure
	}{
		{`{"baseUrl":"http://127.0.0.1:11434","contextWindow":32768}`, ""},
		{`{"baseUrl":"http://[::1]:1234"}`, ""},
		{`{"baseUrl":"https://127.0.0.1:5272"}`, ""},                  // https loopback allowed
		{`{"baseUrl":"http://localhost:11434"}`, ""},                  // the literal name is pinned to 127.0.0.1
		{`{"baseUrl":"http://localhost.evil.com:11434"}`, "loopback"}, // any other NAME is refused
		{`{"baseUrl":"http://10.0.0.5:11434"}`, "loopback"},
		{`{"baseUrl":"https://api.openai.com"}`, "loopback"},
		{`{"baseUrl":"http://127.0.0.1:11434/ollama"}`, "without a path"},
		{`{"baseUrl":"ftp://127.0.0.1:11434"}`, "http:// or https://"},
		{`{"baseUrl":"http://user:pass@127.0.0.1:11434"}`, "credentials"},
		{`{`, "PARSE:not valid JSON"},
		{`{"baseUrl":"http://127.0.0.1:11434","unknown":1}`, "PARSE:not valid JSON"},
	}
	for _, tc := range cases {
		cfg, err := ParseLocalConfig(tc.raw)
		if strings.HasPrefix(tc.want, "PARSE:") {
			if err == nil || !strings.Contains(err.Error(), tc.want[6:]) {
				t.Errorf("%s: parse err = %v, want it to mention %q", tc.raw, err, tc.want[6:])
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: parse: %v", tc.raw, err)
			continue
		}
		err = cfg.Validate()
		if tc.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error %v", tc.raw, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want it to mention %q", tc.raw, err, tc.want)
		}
	}
	// An empty document is a valid UNCONFIGURED config; Validate names the
	// missing base URL.
	cfg, err := ParseLocalConfig("")
	if err != nil || cfg.Configured() {
		t.Fatalf("empty document: cfg = %+v, err = %v", cfg, err)
	}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "base URL is required") {
		t.Errorf("unconfigured Validate err = %v", err)
	}
}

func TestLocalProvider_UnavailableUnderBlockInternal(t *testing.T) {
	saveLocalModelsPolicy(t, egress.BlockInternal)
	p := NewLocalProvider(`{"baseUrl":"http://127.0.0.1:11434"}`)
	if p.configErr == nil || !strings.Contains(p.configErr.Error(), "desktop app only") {
		t.Fatalf("configErr = %v, want the egress refusal", p.configErr)
	}
	if _, err := p.Chat(context.Background(), Request{Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Error("Chat must refuse under the cloud egress policy")
	}

	saveLocalModelsPolicy(t, egress.AllowLoopback)
	q := NewLocalProvider(`{"baseUrl":"http://127.0.0.1:11434"}`)
	if q.configErr != nil {
		t.Fatalf("configErr = %v under AllowLoopback", q.configErr)
	}
}

// The runtime's zero value is the cloud posture: an entrypoint that never
// calls ConfigureLocalModels leaves the provider disabled (fails closed).
func TestLocalProvider_UnconfiguredRuntimeIsDisabled(t *testing.T) {
	var zero egress.Policy
	saveLocalModelsPolicy(t, zero)
	if zero != egress.BlockInternal || LocalModelsAvailable() {
		t.Fatalf("zero policy = %v, available = %v: want BlockInternal and unavailable", zero, LocalModelsAvailable())
	}
	if err := CheckLocalModelsAvailable(); err == nil {
		t.Error("CheckLocalModelsAvailable must refuse under the zero policy")
	}
	if p := NewLocalProvider(`{"baseUrl":"http://127.0.0.1:11434"}`); p.configErr == nil {
		t.Error("the provider must be unusable under the zero policy")
	}
}

func TestLocalProvider_ListingChatAndNoAuthHeader(t *testing.T) {
	saveLocalModelsPolicy(t, egress.AllowLoopback)
	var authHeaders []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/v1/models"):
			writeJSON(w, 200, map[string]any{"data": []map[string]any{
				{"id": "qwen2.5:7b"}, {"id": "nomic-embed-text:latest"}, {"id": "phi-4-mini"},
				{"id": "text-embedding-3-small"}, {"id": "Qwen3-Embedding-0.6B"}}})
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/v1/chat/completions"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["model"] != "qwen2.5:7b" {
				t.Errorf("chat model = %v, want the requested id", body["model"])
			}
			if _, has := body["max_tokens"]; !has {
				t.Errorf("local servers read the legacy max_tokens: %v", body)
			}
			writeJSON(w, 200, map[string]any{"model": "qwen2.5:7b", "choices": []map[string]any{
				{"finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "local answer"}}},
				"usage": map[string]any{"prompt_tokens": 4, "completion_tokens": 2}})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)

	p := NewLocalProvider(`{"baseUrl":"` + srv.URL + `"}`)
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatalf("Models: %v", err)
	}
	// Embedding models share the listing but cannot chat: hidden, whatever
	// the case of "embed" in the id.
	if len(models) != 2 || models[0].ID != "qwen2.5:7b" || models[1].ID != "phi-4-mini" {
		t.Errorf("models = %+v, want the two chat models only", models)
	}
	// Free pricing: the budget meter stays at $0.
	if p.PricePerMillionTokens() != (Pricing{}) {
		t.Error("local models must price at zero")
	}
	// The document names no model: the listing's first chat model is the
	// default (the service's chooseDefaultModel).
	if p.DefaultModel() != "" {
		t.Errorf("DefaultModel = %q, want empty", p.DefaultModel())
	}

	resp, err := p.Chat(context.Background(), Request{Model: "qwen2.5:7b", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "local answer" || resp.TokensIn != 4 || resp.TokensOut != 2 {
		t.Errorf("resp = %+v", resp)
	}
	for _, h := range authHeaders {
		if h != "" {
			t.Errorf("no credential is stored, yet Authorization %q was sent", h)
		}
	}
	// Not embedding-capable: no embedding model is configured.
	if _, err := p.Embed(context.Background(), []string{"x"}); err == nil || !strings.Contains(err.Error(), "not supported") {
		t.Errorf("Embed err = %v, want not supported", err)
	}
}

// The context window history is trimmed against: 8192 by default (Ollama and
// LM Studio run 2k–8k unless told otherwise), the configured value when set —
// on the provider and on every listed model, never the old fixed 128k.
func TestLocalProvider_ContextWindow(t *testing.T) {
	saveLocalModelsPolicy(t, egress.AllowLoopback)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"data": []map[string]any{{"id": "llama3.2:3b"}, {"id": "phi-4-mini"}}})
	}))
	t.Cleanup(srv.Close)

	for doc, want := range map[string]int{
		`{"baseUrl":"` + srv.URL + `"}`:                           LocalDefaultContextWindow,
		`{"baseUrl":"` + srv.URL + `","contextWindow":32768}`:     32768,
		`{"baseUrl":"` + srv.URL + `","contextWindow":1024}`:      1024,
		`{"baseUrl":"` + srv.URL + `","contextWindow":512}`:       LocalDefaultContextWindow, // out of range: default, not bricked
		`{"baseUrl":"` + srv.URL + `","contextWindow":-5}`:        LocalDefaultContextWindow,
		`{"baseUrl":"` + srv.URL + `","contextWindow":999999999}`: LocalDefaultContextWindow,
	} {
		p := NewLocalProvider(doc)
		if p.configErr != nil {
			t.Errorf("%s: configErr = %v", doc, p.configErr)
			continue
		}
		if got := p.ContextLimit(); got != want {
			t.Errorf("%s: ContextLimit = %d, want %d", doc, got, want)
		}
		models, err := p.Models(context.Background())
		if err != nil || len(models) != 2 {
			t.Fatalf("%s: Models = %+v, %v", doc, models, err)
		}
		for _, m := range models {
			if m.ContextLimit != want {
				t.Errorf("%s: model %s ContextLimit = %d, want %d", doc, m.ID, m.ContextLimit, want)
			}
		}
	}
	if LocalDefaultContextWindow != 8192 {
		t.Errorf("LocalDefaultContextWindow = %d, want 8192", LocalDefaultContextWindow)
	}
}

// One client for every provider instance (the factory builds a provider per
// request), still refusing redirects and with the raised header timeout.
func TestLocalModelsClient_BuiltOnce(t *testing.T) {
	saveLocalModelsPolicy(t, egress.AllowLoopback)
	a := NewLocalProvider(`{"baseUrl":"http://127.0.0.1:11434"}`)
	b := NewLocalProvider(`{"baseUrl":"http://[::1]:1234"}`)
	if a.client == nil || a.client != b.client {
		t.Fatalf("clients = %p / %p, want one shared client", a.client, b.client)
	}
	if a.client.CheckRedirect == nil || a.client.CheckRedirect(httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1/", nil), nil) == nil {
		t.Error("the local client must refuse redirects")
	}
	tr, ok := a.client.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport = %T, want *http.Transport", a.client.Transport)
	}
	if tr.ResponseHeaderTimeout != azureResponseHeaderTimeout {
		t.Errorf("header timeout = %v, want the raised %v", tr.ResponseHeaderTimeout, azureResponseHeaderTimeout)
	}
	if tr == sharedTransportBase() {
		t.Error("the local transport must be a clone, not the shared one")
	}
}
