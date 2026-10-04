package ai

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// TestAzureClaude_CountTokens: the exact prompt-token count
// goes to the resource's Anthropic count_tokens route with the same
// credential as the Messages route (both key headers in API-key mode, a
// Bearer token in Entra mode), the deployment name as `model`, and none of
// the generation fields (max_tokens, stream) the endpoint does not take.
func TestAzureClaude_CountTokens(t *testing.T) {
	var gotPath string
	var gotHeaders http.Header
	var gotBody map[string]any
	calls := 0
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, body map[string]any) {
		calls++
		gotPath, gotHeaders, gotBody = r.URL.Path, r.Header.Clone(), body
		writeJSON(w, 200, map[string]any{"input_tokens": 1234})
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"my-claude"} })
	n, err := p.CountTokens(context.Background(), Request{
		Model: "claude:my-claude", SystemPrompt: "be brief", MaxTokens: 4096,
		Messages: []Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("CountTokens: %v", err)
	}
	if n != 1234 {
		t.Errorf("count = %d, want 1234", n)
	}
	if gotPath != "/anthropic/v1/messages/count_tokens" {
		t.Errorf("path = %q, want the count_tokens route", gotPath)
	}
	if gotHeaders.Get("api-key") != "secret-key" || gotHeaders.Get("x-api-key") != "secret-key" {
		t.Errorf("API-key mode must send both key headers, got %v", gotHeaders)
	}
	if gotHeaders.Get("anthropic-version") != claudeAPIVersion {
		t.Errorf("anthropic-version = %q", gotHeaders.Get("anthropic-version"))
	}
	if gotBody["model"] != "my-claude" {
		t.Errorf("model = %v, want the deployment name", gotBody["model"])
	}
	for _, absent := range []string{"max_tokens", "stream", "temperature", "cache_control"} {
		if _, has := gotBody[absent]; has {
			t.Errorf("count_tokens body must carry no %q: %v", absent, gotBody[absent])
		}
	}
	if sys, _ := gotBody["system"].([]any); len(sys) != 1 {
		t.Errorf("system = %v, want the structured system block", gotBody["system"])
	}

	// Entra mode: the same route under a Bearer token.
	entra := newAzureClaudeEntraProvider(t, srv)
	if _, err := entra.CountTokens(context.Background(), Request{Model: "claude:my-claude", Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatalf("CountTokens (Entra): %v", err)
	}
	if auth := gotHeaders.Get("Authorization"); !strings.HasPrefix(auth, "Bearer ") {
		t.Errorf("Entra mode must send a Bearer token, got %q", auth)
	}

	// A non-Claude deployment is refused locally, without a request.
	before := calls
	if _, err := p.CountTokens(context.Background(), Request{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Error("a non-Claude deployment must be refused, not counted")
	}
	if calls != before {
		t.Errorf("refusal must not hit the network, got %d more request(s)", calls-before)
	}
}

// TestAzureClaude_CountTokensUnavailableMemoised: an endpoint
// whose count_tokens ROUTE answers 404 (the gateway's own envelope, not
// Anthropic's) is memoised per endpoint (learnedRepairs), so later calls
// fail fast instead of paying the round trip every turn.
func TestAzureClaude_CountTokensUnavailableMemoised(t *testing.T) {
	calls := 0
	srv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		calls++
		writeJSON(w, 404, map[string]any{"error": map[string]any{"code": "404", "message": "Resource not found"}})
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"my-claude"} })
	req := Request{Model: "claude:my-claude", Messages: []Message{{Role: "user", Content: "hi"}}}
	if _, err := p.CountTokens(context.Background(), req); err == nil {
		t.Fatal("a 404 must surface an error")
	}
	if _, err := p.CountTokens(context.Background(), req); err == nil {
		t.Fatal("the memoised refusal must surface an error too")
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (the 404 is memoised per endpoint)", calls)
	}
	// An auth failure keeps its classification.
	authSrv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
	})
	authP := newAzureKeyProvider(t, authSrv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"my-claude"} })
	if _, err := authP.CountTokens(context.Background(), req); !errors.Is(err, ErrApiKeyInvalid) {
		t.Errorf("401 on count_tokens = %v, want the invalid-key classification", err)
	}
}

// Anthropic's own not_found_error is the route refusing THIS request (an
// unknown model or deployment): the memo is per endpoint, and one request's
// unknown model must not turn exact counting off for every user of the
// resource.
func TestAzureClaude_CountTokensModelNotFoundNotMemoised(t *testing.T) {
	calls := 0
	srv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		calls++
		writeJSON(w, 404, map[string]any{"type": "error", "error": map[string]any{
			"type": "not_found_error", "message": "model: nosuch"}})
	})
	p := newAzureKeyProvider(t, srv, nil)
	req := Request{Model: "claude:nosuch", Messages: []Message{{Role: "user", Content: "hi"}}}
	for range 2 {
		if _, err := p.CountTokens(context.Background(), req); err == nil {
			t.Fatal("an unknown model must surface an error")
		}
	}
	if calls != 2 {
		t.Errorf("calls = %d, want 2 (a model's not_found_error is not memoised)", calls)
	}
	if params := repairsFor(p.claudeProvider().endpoint() + "/count_tokens"); len(params) != 0 {
		t.Errorf("memo = %v, want none", params)
	}
}

// The audited wrapper bounds an exact count with countTokensTimeout — it
// runs on the turn's context ahead of the model call, and a count endpoint
// that never answers must fall back to the estimate in seconds, not hold
// the turn for the transport's minutes-long header timeout.
func TestAudited_CountTokensTimesOutOnHangingServer(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		select {
		case <-r.Context().Done():
		case <-time.After(countTokensTimeout + 30*time.Second):
			writeJSON(w, 200, map[string]any{"input_tokens": 1})
		}
	})
	raw := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"my-claude"} })
	ap := newAuditedProvider(NewTracedProvider(raw), nil, "u", azureFoundryProviderID)
	ap.adoptRawHints(raw)
	// A short bound proves the deadline is applied without waiting the
	// production countTokensTimeout (the default the zero value keeps).
	ap.countTimeout = 200 * time.Millisecond
	start := time.Now()
	_, err := ap.CountTokens(context.Background(), Request{Model: "claude:my-claude",
		Messages: []Message{{Role: "user", Content: "hi"}}})
	elapsed := time.Since(start)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("CountTokens = %v, want the count deadline", err)
	}
	if elapsed > ap.countTimeout+3*time.Second {
		t.Errorf("CountTokens took %v, want ≈ the count timeout (%v)", elapsed, ap.countTimeout)
	}
}

// countingStubProvider is a raw provider that implements TokenCounter.
type countingStubProvider struct {
	auditStub
	counted int
}

func (c *countingStubProvider) CountTokens(_ context.Context, _ Request) (int, error) {
	c.counted++
	return 4242, nil
}

// TestAudited_CountTokensForwarded: the factory's chain
// (breaker → retry → tracing → audited) hides the raw provider's optional
// methods, so the audited wrapper adopts the TokenCounter capability and a
// type assertion on the chain the chat service holds still matches — and
// reaches the raw provider.
func TestAudited_CountTokensForwarded(t *testing.T) {
	raw := &countingStubProvider{}
	cb := NewCircuitBreakerProvider(raw)
	rp := NewRetryingProvider(cb)
	ap := newAuditedProvider(NewTracedProvider(rp), nil, "u", "stub")
	ap.adoptRawHints(raw)

	tc, ok := any(ap).(TokenCounter)
	if !ok {
		t.Fatal("the audited chain must satisfy ai.TokenCounter")
	}
	n, err := tc.CountTokens(context.Background(), Request{Model: "claude-opus-5", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil || n != 4242 {
		t.Fatalf("CountTokens through the chain = %d, %v", n, err)
	}
	if raw.counted != 1 {
		t.Errorf("raw provider count calls = %d, want 1", raw.counted)
	}

	// A provider without the capability answers an error the caller falls
	// back from (to the estimate) — the assertion still matches.
	ap2 := newAuditedProvider(&auditStub{}, nil, "u", "stub")
	ap2.adoptRawHints(&auditStub{})
	tc2 := any(ap2).(TokenCounter)
	if _, err := tc2.CountTokens(context.Background(), Request{}); err == nil {
		t.Error("a provider that cannot count must answer an error, not 0/nil")
	}
}

// TestClaudeProvider_CountTokensURL: the count URL is the
// Messages endpoint with the trailing segment replaced — the plain provider
// hits api.anthropic.com/v1/messages/count_tokens, a Foundry resource its
// services.ai.azure.com host.
func TestClaudeProvider_CountTokensURL(t *testing.T) {
	plain := NewClaudeProvider("k")
	if got := plain.endpoint() + "/count_tokens"; got != "https://api.anthropic.com/v1/messages/count_tokens" {
		t.Errorf("plain provider count URL = %q", got)
	}
	foundry := newClaudeOnFoundry(AzureFoundryConfig{Endpoint: "https://my-resource.openai.azure.com"}, nil, nil, nil)
	if got := foundry.endpoint() + "/count_tokens"; got != "https://my-resource.services.ai.azure.com/anthropic/v1/messages/count_tokens" {
		t.Errorf("foundry count URL = %q", got)
	}
}

// CountTokens refuses an invalid stored document like Chat/Stream/Models
// do: the credential must never reach a host Validate refused, so neither
// mode makes a request.
func TestAzureClaude_CountTokensInvalidDocumentRefused(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		writeJSON(w, 200, map[string]any{"input_tokens": 1})
	})
	invalid := errors.New("endpoint host is not an Azure AI Foundry host")
	for name, cfg := range map[string]AzureFoundryConfig{
		"api_key": func() AzureFoundryConfig {
			c := azureKeyConfig(srv.URL)
			c.ClaudeDeployments = []string{"my-claude"}
			return c
		}(),
		"entra": {Endpoint: srv.URL, ClaudeDeployments: []string{"my-claude"},
			Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "s"}},
	} {
		t.Run(name, func(t *testing.T) {
			ResetAzureFoundryCaches()
			p := newAzureFoundryProvider(cfg, invalid, srv.Client())
			_, err := p.CountTokens(context.Background(), Request{Model: "claude:my-claude",
				Messages: []Message{{Role: "user", Content: "hi"}}})
			if !errors.Is(err, ErrProviderConfigInvalid) {
				t.Errorf("CountTokens = %v, want the config error", err)
			}
		})
	}
	if n := len(srv.requests()); n != 0 {
		t.Errorf("an invalid document must never be dialled, got %d request(s)", n)
	}
}

// A bare deployment name the cached project listing re-lists as a Claude
// deployment is counted on the count_tokens route (claudeRoute, as
// Chat and Stream route it), not refused as a non-Claude deployment.
func TestAzureClaude_CountTokensRelistedDeployment(t *testing.T) {
	srv := projectWithClaude(t)
	p := newAzureAgentProvider(t, srv)
	p.cfg.ChatDeployment = "foo"
	if _, err := p.Models(context.Background()); err != nil {
		t.Fatal(err)
	}
	n, err := p.CountTokens(context.Background(), Request{Model: "foo", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil || n != 77 {
		t.Fatalf("CountTokens(foo) = %d, %v, want 77 from count_tokens", n, err)
	}
	var sent azureSeenRequest
	for _, r := range srv.requests() {
		if r.Path == "/anthropic/v1/messages/count_tokens" {
			sent = r
		}
	}
	if sent.Body["model"] != "foo" {
		t.Errorf("count_tokens request = %+v, want model foo", sent)
	}
}
