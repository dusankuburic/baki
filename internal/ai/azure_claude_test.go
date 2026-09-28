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
	if got.Headers.Get("api-key") != "" || got.Headers.Get("Authorization") != "" {
		t.Errorf("API-key mode must send only x-api-key on the Anthropic route, got %v", got.Headers)
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
// (optionally with a trailing version date) is priced and capped from the
// Claude catalogue, any other name starts unpriced.
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
	if m := byID["claude:claude-sonnet-4-6"]; m.DisplayName != "claude-sonnet-4-6 · Claude" || m.ContextLimit != 200000 ||
		m.MaxOutputTokens != 64000 || m.Pricing != (Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0}) || m.Embedding {
		t.Errorf("catalogued Claude deployment = %+v", m)
	}
	if m := byID["claude:claude-opus-4-8-20260901"]; m.Pricing != (Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}) || m.MaxOutputTokens != 128000 {
		t.Errorf("date-suffixed Claude deployment = %+v, want Opus 4.8 pricing and cap", m)
	}
	if m := byID["claude:my-claude"]; m.DisplayName != "my-claude · Claude" || m.ContextLimit != 200000 || m.MaxOutputTokens != 8192 || m.Pricing != (Pricing{}) || m.ServedModel != "" {
		t.Errorf("custom-named Claude deployment = %+v, want the defaults and no price", m)
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
	if m := byID["claude:sonnet"]; m.ServedModel != "claude-sonnet-4-6" || m.Pricing.InputCostPerM != 3.0 || m.ContextLimit != 200000 ||
		m.MaxOutputTokens != 64000 || m.DisplayName != "sonnet · Claude" || m.Embedding {
		t.Errorf("re-listed Anthropic deployment = %+v", m)
	}
	if m := byID["claude:my-claude"]; m.ServedModel != "claude-haiku-4-5-20260101" || m.Pricing.InputCostPerM != 1.0 || m.ContextLimit != 200000 {
		t.Errorf("document Claude entry enriched by the project = %+v", m)
	}
	if m := byID["claude:opus"]; m.ServedModel != "claude-opus-4-8" || m.Pricing.OutputCostPerM != 25.0 || m.MaxOutputTokens != 128000 {
		t.Errorf("project-only Anthropic deployment = %+v", m)
	}
	if m := byID["chat1"]; m.ServedModel != "gpt-4o" || IsClaudeModel(m.ID) {
		t.Errorf("OpenAI-published deployment = %+v", m)
	}
}

// The claudeDeployments document field: normalised like the other lists
// (trimmed, blanks and case-insensitive duplicates dropped, names already
// listed as chat or embedding deployments dropped), validated and bounded,
// in the public view and the stored document, counted as a chat deployment
// by the save-time rule, and part of the listing cache key but not of the
// credential fingerprint.
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

// Pricing through the audited wrapper: a Claude deployment is reserved and
// billed at the Claude ceiling until its model is known, priced by the Claude
// catalogue once the response names the model, and the served-model memo
// enriches the listing after the first turn (window kept at 200k).
func TestAzureClaude_AuditedPricingAndMemo(t *testing.T) {
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

	want := AliasClaudeCeiling(azureFoundryProviderID)
	if want != (Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}) {
		t.Fatalf("AliasClaudeCeiling = %+v, want the most expensive Claude rate", want)
	}
	if AliasClaudeCeiling("openai") != (Pricing{}) {
		t.Error("the Claude ceiling is for alias providers only")
	}
	if got := ap.ReservationPricing(ctx, "claude:my-claude"); got != want {
		t.Errorf("reservation for an unknown Claude deployment = %+v, want the Claude ceiling", got)
	}
	if got := ap.ReservationPricing(ctx, "claude:claude-opus-4-8"); got != (Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}) {
		t.Errorf("reservation for a catalogued Claude deployment = %+v", got)
	}
	if got, ok := ap.UsagePricing(ctx, "claude:my-claude", ""); !ok || got != want {
		t.Errorf("usage with no served model = %+v %v, want the Claude ceiling", got, ok)
	}
	if got, ok := ap.UsagePricing(ctx, "claude:my-claude", "claude-sonnet-4-6-20260301"); !ok || got != (Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0}) {
		t.Errorf("usage priced by the served Claude model = %+v %v", got, ok)
	}
	if m := ap.usageMetric(ctx, "claude:my-claude", "claude-sonnet-4-6-20260301", "", 1_000_000, 0); m.EstimatedCost != 3.0 {
		t.Errorf("usage metric cost = %v, want Sonnet's input rate", m.EstimatedCost)
	}
	if m := ap.usageMetric(ctx, "claude:my-claude", "", "", 1_000_000, 0); m.EstimatedCost != 5.0 {
		t.Errorf("unknown-model usage metric cost = %v, want the Claude ceiling", m.EstimatedCost)
	}
	// A model router that routed to Claude is priced by the Claude catalogue too.
	if entry, ok := catalogEntryFor(azureFoundryProviderID, "claude-haiku-4-5"); !ok || entry.Pricing.InputCostPerM != 1.0 {
		t.Errorf("catalogEntryFor(azure-foundry, claude-haiku-4-5) = %+v %v", entry, ok)
	}
	if _, ok := catalogEntryFor(azureFoundryProviderID, "claude:my-claude"); ok {
		t.Error("a custom Claude deployment name must not be priced by guesswork")
	}

	// First turn: the response names the model; the next wrapper's listing
	// carries it (price, output cap), with the window kept at 200k.
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
		if m.ServedModel != "claude-sonnet-4-6-20260301" || m.Pricing.InputCostPerM != 3.0 || m.MaxOutputTokens != 64000 || m.ContextLimit != 200000 {
			t.Errorf("memo-enriched Claude entry = %+v", m)
		}
	}
	if !found {
		t.Error("claude:my-claude missing from the listing")
	}
	if got := next.ReservationPricing(ctx, "claude:my-claude"); got != (Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0}) {
		t.Errorf("reservation after the memo = %+v, want the listing price", got)
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
