package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Claude deployments on Azure AI Foundry ("claude:<deployment>" ids): the
// Anthropic Messages route, the credential per auth mode, streaming, tools,
// auth/429 classification, listing, discovery, config and pricing.

func claudeMessageJSON(text, model string) map[string]any {
	return map[string]any{
		"id":          "msg_1",
		"type":        "message",
		"role":        "assistant",
		"model":       model,
		"content":     []map[string]any{{"type": "text", "text": text}},
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 12, "output_tokens": 5},
	}
}

const claudeStreamFixture = "event: message_start\n" +
	"data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude-sonnet-4-6\",\"usage\":{\"input_tokens\":12,\"output_tokens\":1}}}\n\n" +
	"event: content_block_start\n" +
	"data: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hel\"}}\n\n" +
	"event: content_block_delta\n" +
	"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"lo\"}}\n\n" +
	"event: content_block_stop\n" +
	"data: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\n" +
	"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\n" +
	"event: message_stop\n" +
	"data: {\"type\":\"message_stop\"}\n\n"

// newAzureClaudeEntraProvider builds an Entra ID provider (no project) whose
// tokens come from fakeCred (tok-1, tok-2, …).
func newAzureClaudeEntraProvider(t *testing.T, srv *azureTestServer) *AzureFoundryProvider {
	t.Helper()
	ResetAzureFoundryCaches()
	cfg := AzureFoundryConfig{Endpoint: srv.URL, ClaudeDeployments: []string{"my-claude"},
		Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "s"}}
	p := newAzureFoundryProvider(cfg, nil, srv.Client())
	p.tokens = newEntraTokenSource(&fakeCred{})
	p.tokenFn = p.entraToken
	return p
}

// failOnOpenAIRoute makes a handler refuse the chat-completions route: a
// Claude deployment must never be called through /openai/v1.
func failOnOpenAIRoute(t *testing.T, r *http.Request) bool {
	t.Helper()
	if strings.HasPrefix(r.URL.Path, "/openai/") {
		t.Errorf("a claude: model must not hit the OpenAI route, got %s", r.URL.Path)
		return true
	}
	return false
}

// TestAzureClaude_TopLevelCacheControlAndThinking: a claude:<name>
// deployment's Messages request carries the top-level
// automatic-caching marker (the growing history and tool_results behind the
// cached system block become cache reads on later tool-loop iterations) and
// asks for adaptive thinking even with tools attached.
func TestAzureClaude_TopLevelCacheControlAndThinking(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		if failOnOpenAIRoute(t, r) {
			w.WriteHeader(404)
			return
		}
		writeJSON(w, 200, claudeMessageJSON("hello", "claude-opus-4-8"))
	})
	// A portal-named deployment: the sampling policy reads the model from the
	// name itself (a custom name stays on the temperature path until its
	// served model is memoised).
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"claude-opus-4-8"} })
	_, err := p.Chat(context.Background(), Request{Model: "claude:claude-opus-4-8", MaxTokens: 256, SystemPrompt: "be brief",
		Messages: []Message{{Role: "user", Content: "hi"}},
		Tools:    []ToolDefinition{{Name: "search_flow", Description: "x", InputSchema: json.RawMessage(`{"type":"object"}`)}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	body := srv.requests()[0].Body
	if top, _ := body["cache_control"].(map[string]any); top["type"] != "ephemeral" {
		t.Errorf("top-level cache_control = %v, want ephemeral automatic caching", body["cache_control"])
	}
	if think, _ := body["thinking"].(map[string]any); think["type"] != "adaptive" {
		t.Errorf("thinking = %v, want adaptive with tools attached", body["thinking"])
	}
	if _, ok := body["temperature"]; ok {
		t.Error("temperature must be omitted on the thinking path")
	}
}

// TestAzureClaude_MythosRequiresEntra: a Mythos deployment in API-key mode
// fails with the Entra-only explanation instead of the misleading "invalid
// API key" (the key is valid for every other deployment). The error carries
// BOTH sentinels so the auth-failure rules hold; Fable is not Entra-gated;
// Entra mode is untouched; a permission_error keeps Azure's explanation.
func TestAzureClaude_MythosRequiresEntra(t *testing.T) {
	// A Mythos deployment (portal-named) in API-key mode: any 401 on the
	// invalid-key path is remapped.
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	})
	mythos := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"claude-mythos-5-1"} })
	_, err := mythos.Chat(context.Background(), Request{Model: "claude:claude-mythos-5-1", Messages: []Message{{Role: "user", Content: "hi"}}})
	if !errors.Is(err, ErrClaudeEntraOnly) || !errors.Is(err, ErrApiKeyInvalid) {
		t.Fatalf("mythos 401 = %v, want the ErrClaudeEntraOnly + ErrApiKeyInvalid pair", err)
	}
	if !strings.Contains(err.Error(), "Entra ID") {
		t.Errorf("error text must name Entra ID: %v", err)
	}
	// The stream path remaps too.
	err = mythos.Stream(context.Background(), Request{Model: "claude:claude-mythos-5-1", Messages: []Message{{Role: "user", Content: "hi"}}}, func(Chunk) {})
	if !errors.Is(err, ErrClaudeEntraOnly) {
		t.Fatalf("mythos 401 on stream = %v, want ErrClaudeEntraOnly", err)
	}

	// A custom deployment name recognised through its served model.
	custom := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"research"} })
	recordServedModel("scope\x00"+srv.URL, "azure-foundry", "claude:research", "claude-mythos-preview")
	_, err = custom.Chat(context.Background(), Request{Model: "claude:research", Messages: []Message{{Role: "user", Content: "hi"}}})
	if !errors.Is(err, ErrClaudeEntraOnly) {
		t.Fatalf("custom mythos-serving deployment = %v, want ErrClaudeEntraOnly", err)
	}

	// Fable is not Entra-gated: the bare invalid-key classification stays.
	fableSrv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)
	})
	fable := newAzureKeyProvider(t, fableSrv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"claude-fable-5-1"} })
	_, err = fable.Chat(context.Background(), Request{Model: "claude:claude-fable-5-1", Messages: []Message{{Role: "user", Content: "hi"}}})
	if !errors.Is(err, ErrApiKeyInvalid) || errors.Is(err, ErrClaudeEntraOnly) {
		t.Fatalf("fable 401 = %v, want the bare invalid-key classification", err)
	}

	// Entra mode is untouched: a rejected token stays ErrEntraTokenRejected.
	entraSrv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		w.WriteHeader(401)
		_, _ = io.WriteString(w, `{"error":{"code":"Unauthorized","message":"token rejected"}}`)
	})
	entra := newAzureClaudeEntraProvider(t, entraSrv)
	_, err = entra.Chat(context.Background(), Request{Model: "claude:my-claude", Messages: []Message{{Role: "user", Content: "hi"}}})
	if !errors.Is(err, ErrEntraTokenRejected) || errors.Is(err, ErrClaudeEntraOnly) {
		t.Fatalf("entra 401 = %v, want ErrEntraTokenRejected without the Entra-only remap", err)
	}

	// An Anthropic permission_error keeps the permission-denied path.
	permSrv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, _ map[string]any) {
		w.WriteHeader(403)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"permission_error","message":"your key may not use this deployment"}}`)
	})
	perm := newAzureKeyProvider(t, permSrv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"claude-mythos-5-1"} })
	_, err = perm.Chat(context.Background(), Request{Model: "claude:claude-mythos-5-1", Messages: []Message{{Role: "user", Content: "hi"}}})
	if !errors.Is(err, ErrProviderPermissionDenied) || errors.Is(err, ErrClaudeEntraOnly) {
		t.Fatalf("permission_error on mythos = %v, want the permission-denied path", err)
	}
}

func TestAzureClaude_ChatOnTheAnthropicRouteWithAPIKey(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		if failOnOpenAIRoute(t, r) {
			w.WriteHeader(404)
			return
		}
		writeJSON(w, 200, claudeMessageJSON("hello", "claude-sonnet-4-6"))
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"my-claude"} })

	resp, err := p.Chat(context.Background(), Request{Model: "claude:my-claude", Temperature: 0.3, MaxTokens: 256,
		SystemPrompt: "be brief", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "hello" || resp.ServedModel != "claude-sonnet-4-6" || resp.TokensIn != 12 || resp.TokensOut != 5 || resp.FinishReason != "end_turn" {
		t.Errorf("response = %+v", resp)
	}
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests", len(reqs))
	}
	got := reqs[0]
	if got.Path != "/anthropic/v1/messages" {
		t.Errorf("path = %q", got.Path)
	}
	if got.Headers.Get("x-api-key") != "secret-key" || got.Headers.Get("anthropic-version") != claudeAPIVersion {
		t.Errorf("headers = %v, want x-api-key and anthropic-version", got.Headers)
	}
	if got.Headers.Get("api-key") != "secret-key" || got.Headers.Get("Authorization") != "" {
		t.Errorf("API-key mode must send the key as api-key and x-api-key (no bearer) on the Anthropic route, got %v", got.Headers)
	}
	if got.Body["model"] != "my-claude" {
		t.Errorf("model = %v, want the deployment name without the claude: prefix", got.Body["model"])
	}
	if got.Body["max_tokens"] != float64(256) || got.Body["temperature"] != 0.3 {
		t.Errorf("body = %v, want Anthropic max_tokens and temperature", got.Body)
	}
	if _, has := got.Body["max_completion_tokens"]; has {
		t.Error("max_completion_tokens is the OpenAI-route parameter")
	}
	if sys, ok := got.Body["system"].([]any); !ok || len(sys) != 1 {
		t.Errorf("system = %v, want one cached text block", got.Body["system"])
	}
}

func TestAzureClaude_StreamWithEntraBearer(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		if failOnOpenAIRoute(t, r) {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, claudeStreamFixture)
	})
	p := newAzureClaudeEntraProvider(t, srv)

	var text string
	var done Chunk
	var doneCount int
	err := p.Stream(context.Background(), Request{Model: "claude:my-claude", Messages: []Message{{Role: "user", Content: "hi"}}}, func(c Chunk) {
		if c.Done {
			done = c
			doneCount++
		}
		text += c.Text
	})
	if err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if text != "Hello" || doneCount != 1 {
		t.Errorf("text = %q, done chunks = %d", text, doneCount)
	}
	if done.TokensIn != 12 || done.TokensOut != 5 || done.ServedModel != "claude-sonnet-4-6" || done.FinishReason != "end_turn" {
		t.Errorf("done = %+v, want usage and the served model from message_start", done)
	}
	reqs := srv.requests()
	if len(reqs) != 1 {
		t.Fatalf("got %d requests", len(reqs))
	}
	got := reqs[0]
	if got.Path != "/anthropic/v1/messages" || got.Body["model"] != "my-claude" || got.Body["stream"] != true {
		t.Errorf("request = %+v", got)
	}
	if got.Headers.Get("Authorization") != "Bearer tok-1" || got.Headers.Get("anthropic-version") != claudeAPIVersion {
		t.Errorf("headers = %v, want the Entra bearer and anthropic-version", got.Headers)
	}
	if got.Headers.Get("x-api-key") != "" || got.Headers.Get("api-key") != "" {
		t.Errorf("Entra mode must not send a key header, got %v", got.Headers)
	}
}

// A tool round trip: the deployment asks for a tool (tool_use), the loop
// answers with the result, and the next request carries the assistant's
// tool_use block and the user's tool_result block in Anthropic form.
func TestAzureClaude_ToolRoundTrip(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
		if failOnOpenAIRoute(t, r) {
			w.WriteHeader(404)
			return
		}
		if n == 1 {
			writeJSON(w, 200, map[string]any{
				"id": "msg_1", "model": "claude-sonnet-4-6", "stop_reason": "tool_use",
				"content": []map[string]any{
					{"type": "text", "text": "Looking…"},
					{"type": "tool_use", "id": "toolu_1", "name": "search_flow", "input": map[string]any{"q": "x"}},
				},
				"usage": map[string]any{"input_tokens": 20, "output_tokens": 9},
			})
			return
		}
		writeJSON(w, 200, claudeMessageJSON("found it", "claude-sonnet-4-6"))
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"my-claude"} })
	tools := []ToolDefinition{{Name: "search_flow", Description: "search", InputSchema: json.RawMessage(`{"type":"object"}`)}}

	first, err := p.Chat(context.Background(), Request{Model: "claude:my-claude", Tools: tools,
		Messages: []Message{{Role: "user", Content: "find x"}}})
	if err != nil {
		t.Fatalf("first Chat: %v", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].ID != "toolu_1" || first.ToolCalls[0].Name != "search_flow" ||
		string(first.ToolCalls[0].Input) != `{"q":"x"}` || first.FinishReason != "tool_use" {
		t.Fatalf("tool call = %+v (finish %q)", first.ToolCalls, first.FinishReason)
	}

	second, err := p.Chat(context.Background(), Request{Model: "claude:my-claude", Tools: tools, Messages: []Message{
		{Role: "user", Content: "find x"},
		{Role: "assistant", Content: first.Content, ToolCalls: first.ToolCalls},
		{Role: "tool", ToolCallID: "toolu_1", Content: `{"hits":1}`},
	}})
	if err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if second.Content != "found it" {
		t.Errorf("second content = %q", second.Content)
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("got %d requests", len(reqs))
	}
	if tl, ok := reqs[0].Body["tools"].([]any); !ok || len(tl) != 1 {
		t.Errorf("first request tools = %v", reqs[0].Body["tools"])
	}
	msgs, _ := reqs[1].Body["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("second request messages = %v", reqs[1].Body["messages"])
	}
	assistant, _ := msgs[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	var sawToolUse bool
	for _, b := range blocks {
		if bm, _ := b.(map[string]any); bm["type"] == "tool_use" && bm["id"] == "toolu_1" && bm["name"] == "search_flow" {
			sawToolUse = true
		}
	}
	if assistant["role"] != "assistant" || !sawToolUse {
		t.Errorf("assistant turn = %v, want a tool_use block", assistant)
	}
	result, _ := msgs[2].(map[string]any)
	rblocks, _ := result["content"].([]any)
	rb, _ := rblocks[0].(map[string]any)
	if result["role"] != "user" || len(rblocks) != 1 || rb["type"] != "tool_result" || rb["tool_use_id"] != "toolu_1" || rb["content"] != `{"hits":1}` {
		t.Errorf("tool result turn = %v", result)
	}
}

// Entra ID mode: a 401 on the Anthropic route is a rejected access token —
// ErrEntraTokenRejected and ErrApiKeyInvalid with Azure's explanation — and
// gets exactly one replay with a fresh token.
func TestAzureClaude_EntraTokenRejectedRetriesOnce(t *testing.T) {
	const msg = "Unauthorized. Access token is missing, invalid, audience is incorrect (https://cognitiveservices.azure.com), or have expired."
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		if r.Header.Get("Authorization") != "Bearer tok-2" {
			writeJSON(w, 401, map[string]any{"type": "error", "error": map[string]any{"type": "authentication_error", "message": msg}})
			return
		}
		writeJSON(w, 200, claudeMessageJSON("ok", "claude-sonnet-4-6"))
	})
	p := newAzureClaudeEntraProvider(t, srv)
	req := Request{Model: "claude:my-claude", Messages: []Message{{Role: "user", Content: "x"}}}

	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	reqs := srv.requests()
	if len(reqs) != 2 || reqs[0].Headers.Get("Authorization") != "Bearer tok-1" || reqs[1].Headers.Get("Authorization") != "Bearer tok-2" {
		t.Errorf("requests = %+v, want tok-1 then a fresh tok-2", reqs)
	}

	// A rejection that survives the fresh token is final, for chat and
	// stream alike, and keeps Azure's one-line explanation.
	for _, envelope := range []map[string]any{
		{"type": "error", "error": map[string]any{"type": "authentication_error", "message": msg}},
		{"statusCode": 401, "message": msg},
	} {
		srv2 := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
			writeJSON(w, 401, envelope)
		})
		p2 := newAzureClaudeEntraProvider(t, srv2)
		for what, err := range map[string]error{
			"stream": p2.Stream(context.Background(), req, func(Chunk) {}),
		} {
			if !errors.Is(err, ErrEntraTokenRejected) || !errors.Is(err, ErrApiKeyInvalid) {
				t.Errorf("%s: want ErrEntraTokenRejected and ErrApiKeyInvalid, got %v", what, err)
			}
			if err == nil || !strings.Contains(err.Error(), "audience is incorrect") || strings.ContainsAny(err.Error(), "\r\n") {
				t.Errorf("%s: want Azure's explanation on one line, got %v", what, err)
			}
		}
		if n := len(srv2.requests()); n != 2 {
			t.Errorf("stream: %d requests, want exactly one fresh-token replay", n)
		}
		p3 := newAzureClaudeEntraProvider(t, srv2)
		if _, err := p3.Chat(context.Background(), req); !errors.Is(err, ErrEntraTokenRejected) {
			t.Errorf("chat: want ErrEntraTokenRejected, got %v", err)
		}
	}
}

func TestAzureClaude_APIKey401IsInvalidKey(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		writeJSON(w, 401, map[string]any{"type": "error", "error": map[string]any{"type": "authentication_error", "message": "invalid x-api-key"}})
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"my-claude"} })
	req := Request{Model: "claude:my-claude", Messages: []Message{{Role: "user", Content: "x"}}}
	_, err := p.Chat(context.Background(), req)
	if !errors.Is(err, ErrApiKeyInvalid) || errors.Is(err, ErrEntraTokenRejected) {
		t.Errorf("chat: want the bare ErrApiKeyInvalid, got %v", err)
	}
	if err := p.Stream(context.Background(), req, func(Chunk) {}); !errors.Is(err, ErrApiKeyInvalid) || errors.Is(err, ErrEntraTokenRejected) {
		t.Errorf("stream: want the bare ErrApiKeyInvalid, got %v", err)
	}
	if n := len(srv.requests()); n != 2 {
		t.Errorf("API-key mode has no token to refresh: %d requests, want 2 (one per call)", n)
	}
}

func TestAzureClaude_RateLimitCarriesRetryAfter(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		w.Header().Set("Retry-After", "7")
		writeJSON(w, 429, map[string]any{"type": "error", "error": map[string]any{"type": "rate_limit_error", "message": "slow down"}})
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"my-claude"} })
	req := Request{Model: "claude:my-claude", Messages: []Message{{Role: "user", Content: "x"}}}
	for what, err := range map[string]error{
		"chat":   func() error { _, err := p.Chat(context.Background(), req); return err }(),
		"stream": p.Stream(context.Background(), req, func(Chunk) {}),
	} {
		var rl *RateLimitError
		if !errors.As(err, &rl) || rl.RetryAfter != 7*time.Second || !errors.Is(err, ErrRateLimited) {
			t.Errorf("%s: want RateLimitError with 7s, got %v", what, err)
		}
	}
}

// The listing offers each Claude deployment as "claude:<name>", sized at
// the Claude window; a deployment named after a catalogued Claude model
// (optionally with a trailing version date) is sized and capped from the
// Claude catalogue, any other name starts with the defaults. Azure is
// unpriced, so every entry carries a zero Pricing.
func TestAzureClaude_ModelsListClaudeDeployments(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		t.Errorf("an API-key listing needs no network, got %s", r.URL.Path)
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) {
		c.ChatDeployment = "gpt-4o"
		c.EmbeddingDeployment = "emb"
		c.ClaudeDeployments = []string{"claude-sonnet-4-6", "my-claude", "claude-opus-4-8-20260901"}
	})
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	byID := map[string]ModelInfo{}
	for _, m := range models {
		ids = append(ids, m.ID)
		byID[m.ID] = m
	}
	if got := strings.Join(ids, ","); got != "gpt-4o,claude:claude-sonnet-4-6,claude:my-claude,claude:claude-opus-4-8-20260901,emb" {
		t.Errorf("listing = %s", got)
	}
	if m := byID["claude:claude-sonnet-4-6"]; m.DisplayName != "claude-sonnet-4-6 · Claude" || m.ContextLimit != 1000000 ||
		m.MaxOutputTokens != 128000 || m.Embedding {
		t.Errorf("catalogued Claude deployment = %+v", m)
	}
	if m := byID["claude:claude-opus-4-8-20260901"]; m.MaxOutputTokens != 128000 {
		t.Errorf("date-suffixed Claude deployment = %+v, want Opus 4.8's cap", m)
	}
	if m := byID["claude:my-claude"]; m.DisplayName != "my-claude · Claude" || m.ContextLimit != 200000 || m.MaxOutputTokens != claudeOnFoundryDefaultOutput || m.ServedModel != "" {
		t.Errorf("custom-named Claude deployment = %+v, want the defaults (output cap %d)", m, claudeOnFoundryDefaultOutput)
	}
	// The unlisted-deployment default output cap must be the smallest cap
	// of any Claude model Foundry serves (64K), so the reasoning floor of a
	// first turn on an unknown Opus deployment is not clipped.
	if claudeOnFoundryDefaultOutput != 64000 {
		t.Errorf("claudeOnFoundryDefaultOutput = %d, want 64000", claudeOnFoundryDefaultOutput)
	}
	if def := p.DefaultModel(); def != "gpt-4o" {
		t.Errorf("DefaultModel = %q, want the chat deployment first", def)
	}
	onlyClaude := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) { c.ClaudeDeployments = []string{"my-claude"} })
	if def := onlyClaude.DefaultModel(); def != "claude:my-claude" {
		t.Errorf("DefaultModel with only Claude deployments = %q", def)
	}
}

// Entra ID + project: a project deployment whose modelPublisher is Anthropic
// is a Claude deployment — listed under "claude:<name>" with the model it
// serves; a document entry naming it as a chat-completions deployment is
// re-listed under that id (it cannot be served on /openai/v1); a document
// Claude entry matched by the project is enriched in place.
func TestAzureClaude_ProjectListingDiscoversAnthropicDeployments(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			writeJSON(w, 200, map[string]any{"value": []map[string]any{
				{"name": "chat1", "type": "ModelDeployment", "modelName": "gpt-4o", "modelPublisher": "OpenAI"},
				{"name": "sonnet", "type": "ModelDeployment", "modelName": "claude-sonnet-4-6", "modelPublisher": "Anthropic", "capabilities": map[string]any{"chat_completion": "true"}},
				{"name": "my-claude", "type": "ModelDeployment", "modelName": "claude-haiku-4-5-20260101", "modelPublisher": "anthropic"},
				{"name": "opus", "type": "ModelDeployment", "modelName": "claude-opus-4-8", "modelPublisher": "Anthropic"},
			}})
		case "/api/projects/p1/agents":
			writeJSON(w, 200, map[string]any{"data": []map[string]any{}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	p := newAzureAgentProvider(t, srv)
	p.cfg.ChatDeployment = "chat1"
	p.cfg.Deployments = []string{"sonnet"}
	p.cfg.ClaudeDeployments = []string{"my-claude"}
	models, err := p.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	byID := map[string]ModelInfo{}
	for _, m := range models {
		ids = append(ids, m.ID)
		byID[m.ID] = m
	}
	if got := strings.Join(ids, ","); got != "chat1,claude:sonnet,claude:my-claude,claude:opus" {
		t.Errorf("listing = %s", got)
	}
	if m := byID["claude:sonnet"]; m.ServedModel != "claude-sonnet-4-6" || m.ContextLimit != 1000000 ||
		m.MaxOutputTokens != 128000 || m.DisplayName != "sonnet · Claude" || m.Embedding {
		t.Errorf("re-listed Anthropic deployment = %+v", m)
	}
	if m := byID["claude:my-claude"]; m.ServedModel != "claude-haiku-4-5-20260101" || m.ContextLimit != 200000 {
		t.Errorf("document Claude entry enriched by the project = %+v", m)
	}
	if m := byID["claude:opus"]; m.ServedModel != "claude-opus-4-8" || m.MaxOutputTokens != 128000 {
		t.Errorf("project-only Anthropic deployment = %+v", m)
	}
	if m := byID["chat1"]; m.ServedModel != "gpt-4o" || IsClaudeModel(m.ID) {
		t.Errorf("OpenAI-published deployment = %+v", m)
	}
}

// The claudeDeployments document field: normalised like the other lists,
// validated and bounded, present in the public view and the stored
// document, counted as a chat deployment by the save-time rule, part of the
// listing cache key but not of the credential fingerprint.
func TestAzureClaude_ConfigClaudeDeployments(t *testing.T) {
	cfg, err := ParseAzureFoundryConfig(`{"endpoint":"https://my-res.services.ai.azure.com","auth":{"mode":"api_key","key":"k"},` +
		`"chatDeployment":"chat","embeddingDeployment":"emb","deployments":["gpt-4o"],` +
		`"claudeDeployments":[" claude-sonnet-4-6 ","","CLAUDE-SONNET-4-6","chat","EMB","GPT-4o","my-claude"]}`)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cfg.ClaudeDeployments, ","); got != "claude-sonnet-4-6,my-claude" {
		t.Errorf("normalised claude deployments = %q", got)
	}
	if got := strings.Join(cfg.chatDeployments(), ","); got != "chat,gpt-4o" {
		t.Errorf("chat deployments must not include the Claude ones: %q", got)
	}
	if pub := cfg.Public(); strings.Join(pub.ClaudeDeployments, ",") != "claude-sonnet-4-6,my-claude" {
		t.Errorf("public view = %+v", pub)
	}
	if raw, err := cfg.Encode(); err != nil || !strings.Contains(raw, `"claudeDeployments":["claude-sonnet-4-6","my-claude"]`) {
		t.Errorf("encoded = %s, %v", raw, err)
	}
	if raw, _ := (AzureFoundryConfig{Endpoint: "https://a.openai.azure.com"}).Encode(); strings.Contains(raw, "claudeDeployments") {
		t.Errorf("an empty list must be omitted: %s", raw)
	}
	if err := cfg.Validate(azureHostPolicy{}); err != nil {
		t.Errorf("Validate: %v", err)
	}

	policy := azureHostPolicy{}
	base := AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeAPIKey, Key: "k"}}
	bad := base
	bad.ClaudeDeployments = []string{"ok", "claude:no"}
	if code := azureCfgCode(t, bad.Validate(policy)); code != AzureCfgErrInvalidDeploy {
		t.Errorf("bad Claude deployment name: code %q", code)
	}
	many := base
	for i := 0; i <= azureMaxClaudeDeployments; i++ {
		many.ClaudeDeployments = append(many.ClaudeDeployments, fmt.Sprintf("claude-%02d", i))
	}
	if code := azureCfgCode(t, many.Validate(policy)); code != AzureCfgErrInvalidDeploy {
		t.Errorf("%d Claude deployments: code %q, want INVALID_DEPLOYMENT", len(many.ClaudeDeployments), code)
	}
	atCap := base
	atCap.ClaudeDeployments = many.ClaudeDeployments[:azureMaxClaudeDeployments]
	if err := atCap.Validate(policy); err != nil {
		t.Errorf("%d Claude deployments must validate: %v", azureMaxClaudeDeployments, err)
	}

	onlyClaude := base
	onlyClaude.ClaudeDeployments = []string{"my-claude"}
	if err := ValidateAzureFoundryConfig(onlyClaude); err != nil {
		t.Errorf("a Claude deployment satisfies the chat-deployment rule: %v", err)
	}
	if code := azureCfgCode(t, ValidateAzureFoundryConfig(base)); code != AzureCfgErrMissingDeploy {
		t.Errorf("nothing named: code %q", code)
	}
	if base.Fingerprint() != onlyClaude.Fingerprint() {
		t.Error("Claude deployments are not part of the credential fingerprint")
	}
	if azureModelsCacheKey(base) == azureModelsCacheKey(onlyClaude) {
		t.Error("the listing cache key must change with the Claude deployments")
	}
	withDeployment := base
	withDeployment.Deployments = []string{"my-claude"}
	if azureModelsCacheKey(withDeployment) == azureModelsCacheKey(onlyClaude) {
		t.Error("a name listed as a Claude deployment must key the cache differently from the same name as a chat deployment")
	}
}

// Through the audited wrapper a Claude deployment is unpriced: reservation,
// usage pricing and the recorded cost are all zero (tokens still recorded),
// while the served-model memo enriches the listing (served model, window and
// output cap) after the first turn.
func TestAzureClaude_AuditedUnpricedAndMemo(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		if failOnOpenAIRoute(t, r) {
			w.WriteHeader(404)
			return
		}
		writeJSON(w, 200, claudeMessageJSON("hi", "claude-sonnet-4-6-20260301"))
	})
	p := newAzureKeyProvider(t, srv, func(c *AzureFoundryConfig) {
		c.ClaudeDeployments = []string{"my-claude", "claude-opus-4-8"}
	})
	ap := newAuditedProvider(p, nil, "user-1", azureFoundryProviderID)
	ap.adoptRawHints(p)
	ctx := context.Background()

	for _, model := range []string{"claude:my-claude", "claude:claude-opus-4-8"} {
		for _, served := range []string{"", "claude-sonnet-4-6-20260301"} {
			if m := ap.usageMetric(model, served, "", usage{In: 1_000_000}); m.PromptTokens != 1_000_000 {
				t.Errorf("usage metric for %s served %q = %d tokens, want them recorded", model, served, m.PromptTokens)
			}
		}
	}
	// A router that routed to Claude keeps the catalogue's window and cap.
	if entry, ok := catalogEntryFor(azureFoundryProviderID, "claude-haiku-4-5"); !ok || entry.ContextLimit == 0 {
		t.Errorf("catalogEntryFor(azure-foundry, claude-haiku-4-5) = %+v %v, want a window", entry, ok)
	}
	if _, ok := catalogEntryFor(azureFoundryProviderID, "claude:my-claude"); ok {
		t.Error("a custom Claude deployment name must not be resolved by guesswork")
	}

	// First turn: the response names the model; the next wrapper's listing
	// carries it (served model, output cap), with the window from the model.
	if _, err := ap.Chat(ctx, Request{Model: "claude:my-claude", Messages: []Message{{Role: "user", Content: "x"}}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	next := newAuditedProvider(newAzureFoundryProvider(p.cfg, nil, srv.Client()), nil, "user-1", azureFoundryProviderID)
	next.adoptRawHints(next.inner.(*AzureFoundryProvider))
	models, err := next.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, m := range models {
		if m.ID != "claude:my-claude" {
			continue
		}
		found = true
		if m.ServedModel != "claude-sonnet-4-6-20260301" || m.MaxOutputTokens != 128000 || m.ContextLimit != 1000000 {
			t.Errorf("memo-enriched Claude entry = %+v", m)
		}
	}
	if !found {
		t.Error("claude:my-claude missing from the listing")
	}
}

// The plain Claude provider is unchanged on the wire and now reports the
// model that answered, for chat and stream.
func TestClaudeProvider_ReportsServedModel(t *testing.T) {
	var path, key string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, key = r.URL.Path, r.Header.Get("x-api-key")
		if r.Header.Get("Accept") == "text/event-stream" {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, claudeStreamFixture)
			return
		}
		writeJSON(w, 200, claudeMessageJSON("ok", "claude-sonnet-4-6"))
	}))
	defer server.Close()
	p := NewClaudeProvider("test-key")
	p.client = server.Client()
	orig := claudeAPIURL
	defer func() { claudeAPIURL = orig }()
	claudeAPIURL = server.URL + "/v1/messages"

	resp, err := p.Chat(context.Background(), Request{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil {
		t.Fatal(err)
	}
	if resp.ServedModel != "claude-sonnet-4-6" || path != "/v1/messages" || key != "test-key" {
		t.Errorf("chat: served %q path %q key %q", resp.ServedModel, path, key)
	}
	var done Chunk
	if err := p.Stream(context.Background(), Request{Model: "claude-sonnet-4-6", Messages: []Message{{Role: "user", Content: "x"}}}, func(c Chunk) {
		if c.Done {
			done = c
		}
	}); err != nil {
		t.Fatal(err)
	}
	if done.ServedModel != "claude-sonnet-4-6" || done.TokensIn != 12 || done.TokensOut != 5 {
		t.Errorf("stream done = %+v", done)
	}
}
