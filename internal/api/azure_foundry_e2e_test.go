package api

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
)

// TestAzureFoundryE2E drives baki's Azure AI Foundry integration end to end, in-process, through the REAL HTTP API (JWT/cloud-mode router served over HTTP, client-visible chat events read from GET /api/events) against a FAKE Foundry resource that asserts the exact wire requests and answers with realistic Azure payloads; see azure_foundry_e2e_helpers_test.go for the harness and the redirect seam.
// Each subtest runs as its own user on its own resource host (<name>.unit-test.invalid), so circuit-breaker, served-model and listing state never leak between scenarios.
func TestAzureFoundryE2E(t *testing.T) {
	env := newAzureE2EEnv(t)

	t.Run("1_api_key_config_and_listing", func(t *testing.T) {
		host := "s1-res" + azE2EHostSuffix
		const key = "s1-secret-key-0123456789"
		u := env.newUser(t, "u-s1")

		var before map[string]any
		env.mustDo(t, http.MethodGet, "/api/providers/azure-foundry/config", u.bearer, nil, http.StatusOK, &before)
		if before["configured"] != false {
			t.Fatalf("unconfigured GET = %v", before)
		}

		env.saveConfig(t, u, azE2EAPIKeyDoc(host, key))

		var cfg map[string]any
		raw := env.mustDo(t, http.MethodGet, "/api/providers/azure-foundry/config", u.bearer, nil, http.StatusOK, &cfg)
		if bytes.Contains(raw, []byte(key)) {
			t.Errorf("GET config leaked the API key: %s", raw)
		}
		for _, secretField := range []string{"key", "auth", "clientSecret"} {
			if _, ok := cfg[secretField]; ok {
				t.Errorf("GET config carries %q: %s", secretField, raw)
			}
		}
		want := map[string]any{
			"configured": true, "hasSecret": true, "authMode": "api_key",
			"endpoint": "https://" + host, "chatDeployment": "gpt-4o",
			"embeddingDeployment": "text-embedding-3-small",
			"deployments":         []any{"model-router"},
			"claudeDeployments":   []any{"claude-sonnet-4-6"},
		}
		for k, v := range want {
			if !reflect.DeepEqual(cfg[k], v) {
				t.Errorf("GET config %s = %#v, want %#v (body %s)", k, cfg[k], v, raw)
			}
		}

		// The listing is the model registry's: the document's deployments became models of its resource "default" — "m/<id>" ids answering for their pre-registry ids (aliases, one release) — and the embedding deployment is not offered for chat.
		p := env.providerInfo(t, u.bearer, "")
		if p["configured"] != true || p["shared"] == true || p["suggestDiscovery"] == true {
			t.Errorf("listing configured/shared/suggestDiscovery = %v/%v/%v, want true/false/false", p["configured"], p["shared"], p["suggestDiscovery"])
		}
		ids := azE2EModelIDs(p)
		if len(ids) != 3 {
			t.Errorf("listed models = %v, want the three chat and Claude deployments (embedding hidden)", ids)
		}
		for _, id := range ids {
			if !strings.HasPrefix(id, "m/") {
				t.Errorf("listed model id %q is not a registry id", id)
			}
		}
		gpt := azE2EModelByAlias(p, "gpt-4o")
		router := azE2EModelByAlias(p, "model-router")
		cl := azE2EModelByAlias(p, "claude:claude-sonnet-4-6")
		if gpt == nil || router == nil || cl == nil || azE2EModelByAlias(p, "text-embedding-3-small") != nil {
			t.Fatalf("models = %v, want gpt-4o, model-router and claude:claude-sonnet-4-6 by alias, no embedding", p["models"])
		}
		// The server default (plan 9.2, rule 1): the single-document save stores its document as saved, so its chat deployment gpt-4o is the default — not claude-sonnet-4-6, which display order (purpose, then name) would put first.
		if p["defaultModel"] != gpt["id"] || gpt["isDefault"] != true || cl["isDefault"] == true {
			t.Errorf("defaultModel = %v (gpt-4o isDefault %v, claude isDefault %v), want gpt-4o's %v",
				p["defaultModel"], gpt["isDefault"], cl["isDefault"], gpt["id"])
		}
		if gpt["kind"] != "chat" || gpt["displayName"] != "gpt-4o" || gpt["purpose"] != "general" || gpt["status"] != "ready" ||
			gpt["connectionId"] != "default" || gpt["connectionName"] != "s1-res" || gpt["shared"] == true {
			t.Errorf("gpt-4o entry = %v", gpt)
		}
		if azE2ENum(gpt, "inputCostPerM") != 0 || azE2ENum(gpt, "outputCostPerM") != 0 || azE2ENum(gpt, "contextLimit") != 128000 {
			t.Errorf("gpt-4o entry = %v, want no price (Azure is unpriced by baki) and a 128000 window", gpt)
		}
		if router["kind"] != "chat" || azE2ENum(router, "inputCostPerM") != 0 {
			t.Errorf("model-router entry = %v, want an unpriced chat model", router)
		}
		if cl["kind"] != "claude" || azE2ENum(cl, "inputCostPerM") != 0 || azE2ENum(cl, "outputCostPerM") != 0 {
			t.Errorf("claude entry = %v, want an unpriced Claude model (Azure is unpriced by baki)", cl)
		}
		if got := env.fake.requests(host); len(got) != 0 {
			t.Errorf("the listing must never call the resource; got %d request(s): %s %s", len(got), got[0].Method, got[0].Path)
		}
	})

	t.Run("2_connection_test", func(t *testing.T) {
		host := "s2-res" + azE2EHostSuffix
		const key = "s2-secret-key"
		u := env.newUser(t, "u-s2")
		env.saveConfig(t, u, azE2EAPIKeyDoc(host, key))
		env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, rec azE2ERequest, _ int) {
			if rec.Method == http.MethodGet && rec.Path == "/openai/v1/models" {
				azE2EWriteJSON(w, http.StatusOK, `{"object":"list","data":[{"id":"gpt-4o-2024-11-20","object":"model"}]}`)
				return
			}
			azE2EWriteJSON(w, http.StatusOK, azE2ECompletionJSON("Hello! How can I help you today?", 8, 9))
		})

		// The Test button (/api/providers/test, deprecated with the model registry) tests resource "default": an authenticated probe of its v1 model route — no model call, nothing billed.
		var res map[string]any
		env.mustDo(t, http.MethodPost, "/api/providers/test", u.bearer, map[string]any{"provider": ai.AzureFoundryProviderID}, http.StatusOK, &res)
		if res["ok"] != true || res["error"] != nil {
			t.Fatalf("connection test = %v, want ok", res)
		}
		reqs := env.fake.requests(host)
		if len(reqs) != 1 {
			t.Fatalf("resource received %d requests, want exactly 1", len(reqs))
		}
		if probe := reqs[0]; probe.Method != http.MethodGet || probe.Path != "/openai/v1/models" || probe.RawQuery != "" ||
			probe.Header.Get("api-key") != key || len(probe.Body) != 0 {
			t.Errorf("resource test = %s %s?%s api-key %q body %q, want the key-authenticated GET /openai/v1/models probe",
				probe.Method, probe.Path, probe.RawQuery, probe.Header.Get("api-key"), probe.Body)
		}

		// A model test (the registry's POST /test with a saved model) sends
		// the deployment one tiny, non-streaming chat request.
		entryID := strings.TrimPrefix(env.modelIDFor(t, u, "gpt-4o"), "m/")
		res = nil
		env.mustDo(t, http.MethodPost, "/api/providers/azure-foundry/test", u.bearer, map[string]any{"model": map[string]any{"id": entryID}}, http.StatusOK, &res)
		if res["ok"] != true || res["error"] != nil {
			t.Fatalf("model test = %v, want ok", res)
		}
		reqs = env.fake.requests(host)
		if len(reqs) != 2 {
			t.Fatalf("resource received %d requests, want 2 (the resource probe and the model test)", len(reqs))
		}
		r := reqs[1]
		if r.Method != http.MethodPost || r.Path != "/openai/v1/chat/completions" {
			t.Errorf("request = %s %s, want POST /openai/v1/chat/completions", r.Method, r.Path)
		}
		if r.RawQuery != "" {
			t.Errorf("v1 route must carry no api-version query, got %q", r.RawQuery)
		}
		if r.Header.Get("api-key") != key {
			t.Errorf("api-key header = %q, want the configured key", r.Header.Get("api-key"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Content-Type = %q", r.Header.Get("Content-Type"))
		}
		body := r.JSON()
		if body == nil {
			t.Fatalf("request body is not JSON: %s", r.Body)
		}
		if body["model"] != "gpt-4o" {
			t.Errorf("model = %v, want the chat deployment gpt-4o", body["model"])
		}
		if azE2ENum(body, "max_completion_tokens") != 16 {
			t.Errorf("max_completion_tokens = %v, want 16 (body %s)", body["max_completion_tokens"], r.Body)
		}
		if _, ok := body["max_tokens"]; ok {
			t.Errorf("max_tokens must not be sent next to max_completion_tokens: %s", r.Body)
		}
		if body["stream"] == true {
			t.Errorf("the model test is a non-streaming call: %s", r.Body)
		}
		msgs := azE2EMessages(body)
		if len(msgs) == 0 || msgs[len(msgs)-1]["role"] != "user" || msgs[len(msgs)-1]["content"] != "ping" {
			t.Errorf("messages = %v, want the one-word user probe", msgs)
		}
	})

	t.Run("3_chat_stream_gpt4o", func(t *testing.T) {
		host := "s3-res" + azE2EHostSuffix
		const key = "s3-secret-key"
		const served = "gpt-4o-2024-11-20"
		u := env.newUser(t, "u-s3")
		env.saveConfig(t, u, azE2EAPIKeyDoc(host, key))
		env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, _ azE2ERequest, _ int) {
			azE2EWriteSSE(w,
				azE2EData(azE2EPromptFilterFrame()),
				azE2EData(azE2EChunk(served, map[string]any{"content": "", "refusal": nil, "role": "assistant"}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{"content": "Hello from "}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{"content": "Azure AI Foundry."}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{}, "stop")),
				azE2EData(azE2EUsageFrame(served, 123, 9)),
				azE2EData("[DONE]"),
			)
		})

		// The picker sends the model's registry id; the wire carries its
		// deployment.
		gptID := env.modelIDFor(t, u, "gpt-4o")
		conv := uuid.NewString()
		out := env.chat(t, u, azE2EChatReq{Model: gptID, Message: "Explain this flow", ConversationID: conv})
		if out.Err != nil {
			t.Fatalf("chat failed: %v", out.Err)
		}
		if out.Text != "Hello from Azure AI Foundry." {
			t.Errorf("streamed text = %q", out.Text)
		}
		d := out.Done
		if azE2ENum(d, "tokensIn") != 123 || azE2ENum(d, "tokensOut") != 9 {
			t.Errorf("done tokens = %v/%v, want 123/9 from the usage chunk", d["tokensIn"], d["tokensOut"])
		}
		if d["servedModel"] != served {
			t.Errorf("done servedModel = %v, want %s", d["servedModel"], served)
		}
		if azE2ENum(d, "inputCostPerM") != 0 || azE2ENum(d, "outputCostPerM") != 0 {
			t.Errorf("done rates = %v/%v, want zero (Azure is unpriced by baki)", d["inputCostPerM"], d["outputCostPerM"])
		}

		reqs := env.fake.requests(host)
		if len(reqs) != 1 {
			t.Fatalf("resource received %d requests, want 1", len(reqs))
		}
		r := reqs[0]
		body := r.JSON()
		switch {
		case r.Path != "/openai/v1/chat/completions" || r.RawQuery != "":
			t.Errorf("request = %s?%s", r.Path, r.RawQuery)
		case r.Header.Get("api-key") != key:
			t.Errorf("api-key = %q", r.Header.Get("api-key"))
		case r.Header.Get("Accept") != "text/event-stream":
			t.Errorf("Accept = %q", r.Header.Get("Accept"))
		case body["model"] != "gpt-4o" || body["stream"] != true:
			t.Errorf("model/stream = %v/%v", body["model"], body["stream"])
		}
		if so, _ := body["stream_options"].(map[string]any); so["include_usage"] != true {
			t.Errorf("stream_options = %v, want include_usage (usage arrives in the choices:[] chunk)", body["stream_options"])
		}
		if azE2ENum(body, "max_completion_tokens") <= 0 {
			t.Errorf("max_completion_tokens = %v", body["max_completion_tokens"])
		}
		if _, ok := body["max_tokens"]; ok {
			t.Errorf("max_tokens sent: %s", r.Body)
		}
		msgs := azE2EMessages(body)
		if len(msgs) < 2 || msgs[0]["role"] != "system" {
			t.Errorf("messages = %v, want the system prompt first", msgs)
		} else if last := msgs[len(msgs)-1]; last["role"] != "user" || !strings.Contains(last["content"].(string), "Explain this flow") {
			t.Errorf("last message = %v, want the user's question", last)
		}

		// The server persisted the turn (history is server-owned: the client sent no messages); reload it as a fresh client would.
		msgs = env.conversation(t, u, u.flowID, conv, 2)
		if len(msgs) != 2 {
			t.Fatalf("reloaded conversation = %v, want the question and the answer", msgs)
		}
		ans := msgs[1]
		if msgs[0]["role"] != "user" || msgs[0]["content"] != "Explain this flow" || ans["role"] != "assistant" || ans["content"] != "Hello from Azure AI Foundry." {
			t.Errorf("reloaded messages = %v", msgs)
		}
		// Stored with the id the client asked with — never the deployment it
		// resolved to — and the model's display name at send time.
		if ans["servedModel"] != served || azE2ENum(ans, "inputCostPerM") > 0 || azE2ENum(ans, "outputCostPerM") > 0 ||
			azE2ENum(ans, "tokensIn") != 123 || azE2ENum(ans, "tokensOut") != 9 || ans["model"] != gptID || ans["modelLabel"] != "gpt-4o" ||
			ans["provider"] != ai.AzureFoundryProviderID {
			t.Errorf("persisted answer = %v, want model %s labelled gpt-4o, servedModel %s, no rates, tokens 123/9", ans, gptID, served)
		}

		// A client that missed the live events gets the same from resume.
		var res map[string]any
		env.mustDo(t, http.MethodPost, "/api/chat/resume", u.bearer, map[string]any{"id": out.StreamID}, http.StatusOK, &res)
		if res["done"] != true || res["text"] != "Hello from Azure AI Foundry." || res["servedModel"] != served ||
			azE2ENum(res, "inputCostPerM") > 0 || azE2ENum(res, "outputCostPerM") > 0 {
			t.Errorf("resume = %v", res)
		}
	})

	t.Run("3b_multi_turn_history_and_model_router", func(t *testing.T) {
		host := "s3b-res" + azE2EHostSuffix
		const routed = "gpt-4.1-mini-2025-04-14"
		u := env.newUser(t, "u-s3b")
		env.saveConfig(t, u, azE2EAPIKeyDoc(host, "s3b-secret-key"))
		env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, _ azE2ERequest, n int) {
			served, text, in, out := "gpt-4o-2024-11-20", "First answer.", 100, 3
			if n > 1 {
				served, text, in, out = routed, "Routed answer.", 180, 4
			}
			azE2EWriteSSE(w,
				azE2EData(azE2EChunk(served, map[string]any{"role": "assistant", "content": ""}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{"content": text}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{}, "stop")),
				azE2EData(azE2EUsageFrame(served, in, out)),
				azE2EData("[DONE]"),
			)
		})

		conv := uuid.NewString()
		sse := env.subscribe(t, u.bearer)
		// A pre-registry id (a stale tab, an API client) still resolves through its alias for one release; the thread then switches to a registry id.
		first := env.chatOn(t, sse, u, azE2EChatReq{Model: "gpt-4o", Message: "First question", ConversationID: conv})
		if first.Err != nil || first.Text != "First answer." {
			t.Fatalf("first turn = err %v text %q", first.Err, first.Text)
		}
		// The server stores the answer from the stream's cleanup, after done.
		if msgs := env.conversation(t, u, u.flowID, conv, 2); len(msgs) != 2 {
			t.Fatalf("after the first turn the conversation holds %d messages, want 2", len(msgs))
		}

		routerID := env.modelIDFor(t, u, "model-router")
		second := env.chatOn(t, sse, u, azE2EChatReq{Model: routerID, Message: "Second question", ConversationID: conv})
		if second.Err != nil || second.Text != "Routed answer." {
			t.Fatalf("second turn = err %v text %q", second.Err, second.Text)
		}
		if d := second.Done; d["servedModel"] != routed || azE2ENum(d, "inputCostPerM") != 0 || azE2ENum(d, "outputCostPerM") != 0 {
			t.Errorf("router turn done = %v, want servedModel %s and no rates", d, routed)
		}

		reqs := env.fake.requests(host)
		if len(reqs) != 2 {
			t.Fatalf("resource received %d requests, want 2", len(reqs))
		}
		body := reqs[1].JSON()
		if body["model"] != "model-router" {
			t.Errorf("second request model = %v, want the model-router deployment", body["model"])
		}
		var convo []string
		for _, m := range azE2EMessages(body) {
			if m["role"] == "system" {
				continue
			}
			c, _ := m["content"].(string)
			convo = append(convo, m["role"].(string)+": "+c)
		}
		if len(convo) != 3 || convo[0] != "user: First question" || convo[1] != "assistant: First answer." ||
			!strings.HasPrefix(convo[2], "user: ") || !strings.Contains(convo[2], "Second question") {
			t.Errorf("second request history = %q, want the stored first turn replayed before the new question", convo)
		}

		msgs := env.conversation(t, u, u.flowID, conv, 4)
		if len(msgs) != 4 {
			t.Fatalf("reloaded conversation holds %d messages, want 4", len(msgs))
		}
		if last := msgs[3]; last["servedModel"] != routed || last["model"] != routerID || last["modelLabel"] != "model-router" ||
			azE2ENum(last, "inputCostPerM") > 0 || azE2ENum(last, "outputCostPerM") > 0 {
			t.Errorf("persisted router answer = %v", last)
		}
		// The raw id is stored as it was sent.
		if prev := msgs[1]; prev["servedModel"] != "gpt-4o-2024-11-20" || prev["model"] != "gpt-4o" || azE2ENum(prev, "inputCostPerM") > 0 {
			t.Errorf("persisted first answer = %v (a later turn must not rewrite it)", prev)
		}
	})

	t.Run("4_chat_claude_on_foundry", func(t *testing.T) {
		host := "s4-res" + azE2EHostSuffix
		const key = "s4-secret-key"
		u := env.newUser(t, "u-s4")
		env.saveConfig(t, u, azE2EAPIKeyDoc(host, key))
		env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, _ azE2ERequest, _ int) {
			azE2EWriteSSE(w,
				azE2EEvent("message_start", map[string]any{"type": "message_start", "message": map[string]any{
					"id": "msg_01E2E", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "content": []any{},
					"stop_reason": nil, "stop_sequence": nil,
					"usage": map[string]any{"input_tokens": 150, "cache_creation_input_tokens": 0, "cache_read_input_tokens": 0, "output_tokens": 1},
				}}),
				azE2EEvent("ping", map[string]any{"type": "ping"}),
				azE2EEvent("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": "", "signature": ""}}),
				azE2EEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "thinking_delta", "thinking": "SECRET-REASONING the user wants a greeting."}}),
				azE2EEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "signature_delta", "signature": "EqQBCgIYAhIM"}}),
				azE2EEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
				azE2EEvent("content_block_start", map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "text", "text": ""}}),
				azE2EEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "Hi from "}}),
				azE2EEvent("ping", map[string]any{"type": "ping"}),
				azE2EEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1, "delta": map[string]any{"type": "text_delta", "text": "Claude on Foundry."}}),
				azE2EEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1}),
				azE2EEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 12}}),
				azE2EEvent("message_stop", map[string]any{"type": "message_stop"}),
			)
		})

		out := env.chat(t, u, azE2EChatReq{Model: env.modelIDFor(t, u, "claude:claude-sonnet-4-6"), Message: "Say hi"})
		if out.Err != nil {
			t.Fatalf("chat failed: %v", out.Err)
		}
		if out.Text != "Hi from Claude on Foundry." {
			t.Errorf("streamed text = %q (thinking must not leak into the answer)", out.Text)
		}
		d := out.Done
		if d["servedModel"] != "claude-sonnet-4-6" {
			t.Errorf("done servedModel = %v, want claude-sonnet-4-6", d["servedModel"])
		}
		if azE2ENum(d, "tokensIn") != 150 || azE2ENum(d, "tokensOut") != 12 {
			t.Errorf("done tokens = %v/%v, want 150/12", d["tokensIn"], d["tokensOut"])
		}
		if azE2ENum(d, "inputCostPerM") != 0 || azE2ENum(d, "outputCostPerM") != 0 {
			t.Errorf("done rates = %v/%v, want zero (Azure is unpriced by baki)", d["inputCostPerM"], d["outputCostPerM"])
		}

		reqs := env.fake.requests(host)
		if len(reqs) != 1 {
			t.Fatalf("resource received %d requests, want 1", len(reqs))
		}
		r := reqs[0]
		if r.Method != http.MethodPost || r.Path != "/anthropic/v1/messages" || r.RawQuery != "" {
			t.Errorf("request = %s %s?%s, want POST /anthropic/v1/messages", r.Method, r.Path, r.RawQuery)
		}
		if r.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("anthropic-version = %q", r.Header.Get("anthropic-version"))
		}
		if r.Header.Get("api-key") != key || r.Header.Get("x-api-key") != key {
			t.Errorf("api-key/x-api-key = %q/%q, want the configured key on both", r.Header.Get("api-key"), r.Header.Get("x-api-key"))
		}
		body := r.JSON()
		if body["model"] != "claude-sonnet-4-6" {
			t.Errorf("model = %v, want the deployment name without the claude: prefix", body["model"])
		}
		if azE2ENum(body, "max_tokens") <= 0 {
			t.Errorf("max_tokens = %v", body["max_tokens"])
		}
		if body["stream"] != true {
			t.Errorf("stream = %v", body["stream"])
		}
		if !bytes.Contains(r.Body, []byte("Say hi")) {
			t.Errorf("the user's message is not in the request: %s", r.Body)
		}
	})

	// F-47: a near-window Claude prompt is re-checked by the provider's own tokenizer (Anthropic count_tokens on the Foundry resource). The route is optional: whatever it answers, the chat turn still succeeds.
	claudeReply := func(w http.ResponseWriter, text string) {
		azE2EWriteSSE(w,
			azE2EEvent("message_start", map[string]any{"type": "message_start", "message": map[string]any{
				"id": "msg_01CNT", "type": "message", "role": "assistant", "model": "claude-haiku-4-5", "content": []any{},
				"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 170000, "output_tokens": 1},
			}}),
			azE2EEvent("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}),
			azE2EEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": text}}),
			azE2EEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
			azE2EEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 4}}),
			azE2EEvent("message_stop", map[string]any{"type": "message_stop"}),
		)
	}
	// Claude Haiku 4.5's 200k window; "a " is one cl100k token per two bytes, so ~300 KB of prompt estimates at ~150k tokens — with the completion reserve above 70% of the window (and under it), the band where the estimate's ~30% Claude undercount could hide an overflow and the provider's tokenizer is asked.
	// The bytes alone decide nothing (review #4 B4 gates the count on the estimate): a prompt whose BYTES merely crossed 80% of the window (168 KB of "lorem " is ~28k tokens) pays no round trip.
	nearWindow := strings.Repeat("a ", 150000)
	claudeCountDoc := func(host, key string) map[string]any {
		doc := azE2EAPIKeyDoc(host, key)
		doc["claudeDeployments"] = []string{"claude-haiku-4-5"}
		return doc
	}
	countCalls := func(host string) (counts, messages []azE2ERequest) {
		for _, r := range env.fake.requests(host) {
			switch r.Path {
			case "/anthropic/v1/messages/count_tokens":
				counts = append(counts, r)
			case "/anthropic/v1/messages":
				messages = append(messages, r)
			}
		}
		return counts, messages
	}

	t.Run("4b_claude_count_tokens_route_served", func(t *testing.T) {
		host := "s4b-res" + azE2EHostSuffix
		const key = "s4b-secret-key"
		u := env.newUser(t, "u-s4b")
		env.saveConfig(t, u, claudeCountDoc(host, key))
		env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, rec azE2ERequest, _ int) {
			if rec.Path == "/anthropic/v1/messages/count_tokens" {
				// Exact count: the prompt fits the window, so nothing is trimmed.
				azE2EWriteJSON(w, http.StatusOK, `{"input_tokens": 150000}`)
				return
			}
			claudeReply(w, "Counted and answered.")
		})

		out := env.chat(t, u, azE2EChatReq{Model: "claude:claude-haiku-4-5", Message: nearWindow})
		if out.Err != nil || out.Text != "Counted and answered." {
			t.Fatalf("chat = err %v text %q, want the answer", out.Err, out.Text)
		}
		counts, messages := countCalls(host)
		if len(counts) != 1 || len(messages) != 1 {
			t.Fatalf("count_tokens/messages requests = %d/%d, want 1/1", len(counts), len(messages))
		}
		c := counts[0]
		if c.Method != http.MethodPost || c.RawQuery != "" {
			t.Errorf("count request = %s ?%s, want a POST without a query", c.Method, c.RawQuery)
		}
		if c.Header.Get("api-key") != key || c.Header.Get("anthropic-version") != "2023-06-01" {
			t.Errorf("count request auth/version headers = %q/%q", c.Header.Get("api-key"), c.Header.Get("anthropic-version"))
		}
		body := c.JSON()
		if body["model"] != "claude-haiku-4-5" {
			t.Errorf("count model = %v, want the deployment without the claude: prefix", body["model"])
		}
		for _, absent := range []string{"max_tokens", "stream", "temperature", "top_p"} {
			if _, ok := body[absent]; ok {
				t.Errorf("count_tokens body carries %q: %s", absent, c.Body)
			}
		}
		if !bytes.Contains(c.Body, []byte("a a a a")) {
			t.Error("the count request does not carry the prompt")
		}
		// The count came before the turn it guarded.
		if !counts[0].At.Before(messages[0].At) && !counts[0].At.Equal(messages[0].At) {
			t.Errorf("count_tokens at %v after messages at %v", counts[0].At, messages[0].At)
		}
	})

	t.Run("4c_claude_count_tokens_404_falls_back_without_repeats", func(t *testing.T) {
		host := "s4c-res" + azE2EHostSuffix
		u := env.newUser(t, "u-s4c")
		env.saveConfig(t, u, claudeCountDoc(host, "s4c-secret-key"))
		env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, rec azE2ERequest, _ int) {
			if rec.Path == "/anthropic/v1/messages/count_tokens" {
				// The gateway's route-level 404 (no count_tokens route on this resource); Anthropic's own not_found_error envelope names one request's unknown model instead and is not memoised (A6).
				azE2EWriteJSON(w, http.StatusNotFound, `{"error":{"code":"404","message":"Resource not found"}}`)
				return
			}
			claudeReply(w, "Answered without a count.")
		})

		sse := env.subscribe(t, u.bearer)
		first := env.chatOn(t, sse, u, azE2EChatReq{Model: "claude:claude-haiku-4-5", Message: nearWindow})
		if first.Err != nil || first.Text != "Answered without a count." {
			t.Fatalf("first turn = err %v text %q, want the estimate's verdict and an answer", first.Err, first.Text)
		}
		if counts, messages := countCalls(host); len(counts) != 1 || len(messages) != 1 {
			t.Fatalf("after turn 1: count_tokens/messages = %d/%d, want 1/1", len(counts), len(messages))
		}

		// A second near-window turn on the same endpoint: the 404 is
		// memoised, so the turn runs without paying the rejection again.
		second := env.chatOn(t, sse, u, azE2EChatReq{Model: "claude:claude-haiku-4-5", Message: nearWindow})
		if second.Err != nil || second.Text != "Answered without a count." {
			t.Fatalf("second turn = err %v text %q", second.Err, second.Text)
		}
		counts, messages := countCalls(host)
		if len(counts) != 1 {
			t.Errorf("count_tokens requests = %d after two turns, want 1 (the 404 is memoised per endpoint)", len(counts))
		}
		if len(messages) != 2 {
			t.Errorf("messages requests = %d, want 2", len(messages))
		}
	})

	t.Run("5_content_filter", func(t *testing.T) {
		host := "s5-res" + azE2EHostSuffix
		u := env.newUser(t, "u-s5")
		env.saveConfig(t, u, azE2EAPIKeyDoc(host, "s5-secret-key"))
		env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, rec azE2ERequest, _ int) {
			if rec.Path == "/anthropic/v1/messages" {
				azE2EWriteSSE(w,
					azE2EEvent("message_start", map[string]any{"type": "message_start", "message": map[string]any{
						"id": "msg_01REF", "type": "message", "role": "assistant", "model": "claude-sonnet-4-6", "content": []any{},
						"stop_reason": nil, "stop_sequence": nil, "usage": map[string]any{"input_tokens": 30, "output_tokens": 1},
					}}),
					azE2EEvent("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}}),
					azE2EEvent("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "I can"}}),
					azE2EEvent("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0}),
					azE2EEvent("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "refusal", "stop_sequence": nil}, "usage": map[string]any{"output_tokens": 2}}),
					azE2EEvent("message_stop", map[string]any{"type": "message_stop"}),
				)
				return
			}
			if bytes.Contains(rec.Body, []byte("[filter-prompt]")) {
				azE2EWriteJSON(w, http.StatusBadRequest, azE2EContentFilterJSON)
				return
			}
			const served = "gpt-4o-2024-11-20"
			azE2EWriteSSE(w,
				azE2EData(azE2EPromptFilterFrame()),
				azE2EData(azE2EChunk(served, map[string]any{"content": "", "role": "assistant"}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{"content": "Here is how to"}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{}, "content_filter")),
				azE2EData(azE2EUsageFrame(served, 40, 5)),
				azE2EData("[DONE]"),
			)
		})
		sse := env.subscribe(t, u.bearer)

		t.Run("prompt_rejected_400", func(t *testing.T) {
			before := len(env.fake.requests(host))
			out := env.chatOn(t, sse, u, azE2EChatReq{Model: "gpt-4o", Message: "[filter-prompt] something harmful"})
			if out.Err == nil || out.Err["code"] != "PROVIDER_CONTENT_FILTERED" {
				t.Fatalf("outcome = err %v done %v, want error code PROVIDER_CONTENT_FILTERED", out.Err, out.Done)
			}
			if n := len(env.fake.requests(host)) - before; n != 1 {
				t.Errorf("a content-filter 400 was sent %d times, want 1 (it is not retryable)", n)
			}
		})

		t.Run("completion_cut_mid_stream", func(t *testing.T) {
			out := env.chatOn(t, sse, u, azE2EChatReq{Model: "gpt-4o", Message: "[filter-completion] tell me"})
			if out.Err == nil || out.Err["code"] != "PROVIDER_CONTENT_FILTERED" {
				t.Fatalf("outcome = err %v done %v, want error code PROVIDER_CONTENT_FILTERED", out.Err, out.Done)
			}
		})

		t.Run("claude_refusal", func(t *testing.T) {
			out := env.chatOn(t, sse, u, azE2EChatReq{Model: "claude:claude-sonnet-4-6", Message: "refuse this"})
			if out.Err == nil || out.Err["code"] != "PROVIDER_CONTENT_FILTERED" {
				t.Fatalf("outcome = err %v done %v, want a Claude refusal reported as PROVIDER_CONTENT_FILTERED", out.Err, out.Done)
			}
		})
	})

	t.Run("6_rate_limited_429", func(t *testing.T) {
		const hint = 150 * time.Millisecond
		rateLimited := func(w http.ResponseWriter) {
			azE2EWriteJSON(w, http.StatusTooManyRequests, azE2ERateLimitJSON,
				"retry-after-ms", "150", "retry-after", "5",
				"x-ratelimit-remaining-requests", "0", "x-ratelimit-remaining-tokens", "0")
		}

		t.Run("retried_then_succeeds", func(t *testing.T) {
			host := "s6a-res" + azE2EHostSuffix
			u := env.newUser(t, "u-s6a")
			env.saveConfig(t, u, azE2EAPIKeyDoc(host, "s6a-secret-key"))
			env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, _ azE2ERequest, n int) {
				if n <= 2 {
					rateLimited(w)
					return
				}
				const served = "gpt-4o-2024-11-20"
				azE2EWriteSSE(w,
					azE2EData(azE2EChunk(served, map[string]any{"content": "", "role": "assistant"}, nil)),
					azE2EData(azE2EChunk(served, map[string]any{"content": "Worth the wait."}, nil)),
					azE2EData(azE2EChunk(served, map[string]any{}, "stop")),
					azE2EData(azE2EUsageFrame(served, 30, 4)),
					azE2EData("[DONE]"),
				)
			})
			out := env.chat(t, u, azE2EChatReq{Model: "gpt-4o", Message: "hello"})
			if out.Err != nil || out.Text != "Worth the wait." {
				t.Fatalf("outcome = err %v text %q, want the answer after two 429s", out.Err, out.Text)
			}
			reqs := env.fake.requests(host)
			if len(reqs) != 3 {
				t.Fatalf("resource received %d requests, want 3 (two 429s + the answer)", len(reqs))
			}
			for i := 1; i < len(reqs); i++ {
				if gap := reqs[i].At.Sub(reqs[i-1].At); gap < hint-10*time.Millisecond {
					t.Errorf("retry %d came %s after the 429, want at least the retry-after-ms hint %s", i, gap, hint)
				}
			}
			// The first backoff is at most 500 ms of jitter or the 150 ms hint: a wait of the coarse retry-after (5 s) means the ms hint was ignored. Asserting against the 5 s coarse value keeps the guard while leaving headroom for -race CI scheduling stalls.
			if gap := reqs[1].At.Sub(reqs[0].At); gap >= 5*time.Second {
				t.Errorf("first retry waited %s: the retry-after-ms hint must win over the coarse retry-after (5 s)", gap)
			}
		})

		t.Run("exhausted_reports_rate_limited", func(t *testing.T) {
			host := "s6b-res" + azE2EHostSuffix
			u := env.newUser(t, "u-s6b")
			env.saveConfig(t, u, azE2EAPIKeyDoc(host, "s6b-secret-key"))
			env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, _ azE2ERequest, _ int) { rateLimited(w) })
			out := env.chat(t, u, azE2EChatReq{Model: "gpt-4o", Message: "hello"})
			if out.Err == nil || out.Err["code"] != "PROVIDER_RATE_LIMITED" {
				t.Fatalf("outcome = err %v done %v, want PROVIDER_RATE_LIMITED", out.Err, out.Done)
			}
			if n := len(env.fake.requests(host)); n != 3 {
				t.Errorf("resource received %d requests, want 3 (the retry budget)", n)
			}
		})
	})

	t.Run("7_tool_loop_list_findings", func(t *testing.T) {
		host := "s7-res" + azE2EHostSuffix
		const callID = "call_e2e_list_1"
		const served = "gpt-4o-2024-11-20"
		u := env.newUser(t, "u-s7")
		env.saveConfig(t, u, azE2EAPIKeyDoc(host, "s7-secret-key"))
		env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, _ azE2ERequest, n int) {
			if n == 1 {
				idx := 0
				azE2EWriteSSE(w,
					azE2EData(azE2EPromptFilterFrame()),
					azE2EData(azE2EChunk(served, map[string]any{"role": "assistant", "content": nil, "refusal": nil, "tool_calls": []any{
						map[string]any{"index": idx, "id": callID, "type": "function", "function": map[string]any{"name": "list_findings", "arguments": ""}},
					}}, nil)),
					azE2EData(azE2EChunk(served, map[string]any{"tool_calls": []any{
						map[string]any{"index": idx, "function": map[string]any{"arguments": "{}"}},
					}}, nil)),
					azE2EData(azE2EChunk(served, map[string]any{}, "tool_calls")),
					azE2EData(azE2EUsageFrame(served, 200, 15)),
					azE2EData("[DONE]"),
				)
				return
			}
			azE2EWriteSSE(w,
				azE2EData(azE2EChunk(served, map[string]any{"role": "assistant", "content": ""}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{"content": "I checked the findings."}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{}, "stop")),
				azE2EData(azE2EUsageFrame(served, 260, 6)),
				azE2EData("[DONE]"),
			)
		})

		out := env.chat(t, u, azE2EChatReq{Model: env.modelIDFor(t, u, "gpt-4o"), Message: "What is wrong with this flow?", UseTools: true})
		if out.Err != nil {
			t.Fatalf("chat failed: %v", out.Err)
		}
		if out.Text != "I checked the findings." {
			t.Errorf("final text = %q", out.Text)
		}
		if !out.has("tool", "list_findings") || !out.has("tool_result", "list_findings") {
			t.Errorf("events = %+v, want tool + tool_result for list_findings", out.Events)
		}
		if out.Done["servedModel"] != served {
			t.Errorf("done servedModel = %v", out.Done["servedModel"])
		}

		reqs := env.fake.requests(host)
		if len(reqs) != 2 {
			t.Fatalf("resource received %d requests, want 2 (tool call, final answer)", len(reqs))
		}
		first := reqs[0].JSON()
		if !azE2EOffersTool(first, "list_findings") {
			t.Errorf("first request does not offer list_findings: %s", reqs[0].Body)
		}
		second := azE2EMessages(reqs[1].JSON())
		var sawCall, sawResult bool
		for _, m := range second {
			if m["role"] == "assistant" {
				calls, _ := m["tool_calls"].([]any)
				for _, c := range calls {
					cm, _ := c.(map[string]any)
					fn, _ := cm["function"].(map[string]any)
					if cm["id"] == callID && fn["name"] == "list_findings" {
						sawCall = true
					}
				}
			}
			if m["role"] == "tool" && m["tool_call_id"] == callID {
				if c, _ := m["content"].(string); strings.TrimSpace(c) != "" {
					sawResult = true
				}
			}
		}
		if !sawCall || !sawResult {
			t.Errorf("second request messages = %v, want the assistant tool call %s replayed and a non-empty tool result with the same tool_call_id", second, callID)
		}
	})

	t.Run("8_entra_project_agent", func(t *testing.T) {
		host := "s8-res" + azE2EHostSuffix
		const (
			tenantID     = "11111111-2222-3333-4444-555555555555"
			clientID     = "66666666-7777-8888-9999-000000000000"
			clientSecret = "s8-client-secret"
			project      = "/api/projects/proj1"
			served       = "gpt-4o-2024-11-20"
		)
		u := env.newUser(t, "u-s8")
		env.saveConfig(t, u, map[string]any{
			"endpoint":        "https://" + host,
			"auth":            map[string]any{"mode": "entra", "tenantId": tenantID, "clientId": clientID, "clientSecret": clientSecret},
			"projectEndpoint": "https://" + host + project,
		})
		env.fake.handle(host, func(w http.ResponseWriter, r *http.Request, rec azE2ERequest, _ int) {
			switch {
			case r.Method == http.MethodGet && rec.Path == project+"/deployments":
				azE2EWriteJSON(w, http.StatusOK, azE2EProjectDeploymentsJSON)
			case r.Method == http.MethodGet && rec.Path == project+"/agents":
				azE2EWriteJSON(w, http.StatusOK, azE2EProjectAgentsJSON)
			case r.Method == http.MethodGet && rec.Path == project+"/agents/helper":
				azE2EWriteJSON(w, http.StatusOK, azE2EProjectAgentJSON)
			case r.Method == http.MethodGet && rec.Path == "/openai/v1/models":
				azE2EWriteJSON(w, http.StatusOK, `{"object":"list","data":[]}`)
			case r.Method == http.MethodPost && rec.Path == project+"/openai/v1/responses":
				azE2EWriteSSE(w, azE2EResponsesStream(served, "Hello from ", "the helper agent.", 42, 7)...)
			case r.Method == http.MethodPost && rec.Path == "/openai/v1/chat/completions":
				azE2EWriteJSON(w, http.StatusOK, azE2ECompletionJSON("Hi!", 8, 2))
			default:
				azE2EWriteJSON(w, http.StatusNotFound, `{"error":{"code":"NotFound","message":"no such route"}}`)
			}
		})

		// The picker offers only the models the user added: an Entra ID + project resource with none yet lists nothing, suggests discovering its project, and costs no request.
		signInsBefore := len(env.fake.requests(azE2EAADHost))
		p := env.providerInfo(t, u.bearer, "")
		if p["configured"] != true || len(azE2EModelIDs(p)) != 0 || p["defaultModel"] != "" || p["suggestDiscovery"] != true {
			t.Errorf("listing = configured %v models %v default %v suggestDiscovery %v, want a configured resource with no models that suggests discovery",
				p["configured"], azE2EModelIDs(p), p["defaultModel"], p["suggestDiscovery"])
		}
		if n := len(env.fake.requests(host)) + len(env.fake.requests(azE2EAADHost)) - signInsBefore; n != 0 {
			t.Errorf("the listing made %d request(s)", n)
		}

		// The user adds the project's agent and its gpt-4o deployment.
		agent := env.addModel(t, u, map[string]any{"connectionId": "default", "kind": "agent", "deployment": "helper", "displayName": "Flow reviewer", "purpose": "review"})
		gpt := env.addModel(t, u, map[string]any{"connectionId": "default", "kind": "chat", "deployment": "gpt-4o"})
		agentID, _ := agent["modelId"].(string)
		gptID, _ := gpt["modelId"].(string)
		p = env.providerInfo(t, u.bearer, "")
		if ids := azE2EModelIDs(p); len(ids) != 2 || azE2EModel(p, agentID) == nil || azE2EModel(p, gptID) == nil {
			t.Fatalf("listing = %v, want the agent and gpt-4o", ids)
		}
		if a := azE2EModel(p, agentID); a["kind"] != "agent" || a["displayName"] != "Flow reviewer" || a["purpose"] != "review" || a["status"] != "ready" {
			t.Errorf("agent entry = %v", a)
		}
		// The server default: the first chat model, before any agent.
		if p["defaultModel"] != gptID {
			t.Errorf("defaultModel = %v, want gpt-4o's %s", p["defaultModel"], gptID)
		}

		// The listing never waits on Azure; it refreshes the project's own listing in the background: an Entra ID sign-in (client credentials, the Foundry scope), then the deployments and the agents on the Entra bearer.
		var sawDeployments, sawAgents bool
		deadline := time.Now().Add(5 * time.Second)
		for (!sawDeployments || !sawAgents) && time.Now().Before(deadline) {
			time.Sleep(10 * time.Millisecond)
			for _, r := range env.fake.requests(host) {
				sawDeployments = sawDeployments || (r.Method == http.MethodGet && r.Path == project+"/deployments")
				sawAgents = sawAgents || (r.Method == http.MethodGet && r.Path == project+"/agents")
			}
		}
		if !sawDeployments || !sawAgents {
			t.Errorf("background project listing calls: deployments %v agents %v", sawDeployments, sawAgents)
		}
		var tokenReq *azE2ERequest
		for _, r := range env.fake.requests(azE2EAADHost) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/oauth2/v2.0/token") {
				r := r
				tokenReq = &r
			}
		}
		if tokenReq == nil {
			t.Fatalf("no token request reached Entra ID (requests: %+v)", env.fake.requests(azE2EAADHost))
		}
		form, _ := url.ParseQuery(string(tokenReq.Body))
		if tokenReq.Path != "/"+tenantID+"/oauth2/v2.0/token" || form.Get("grant_type") != "client_credentials" ||
			form.Get("client_id") != clientID || form.Get("client_secret") != clientSecret ||
			!strings.Contains(form.Get("scope"), "https://ai.azure.com/.default") {
			t.Errorf("token request = %s %s", tokenReq.Path, tokenReq.Body)
		}
		for _, r := range env.fake.requests(host) {
			if r.Method != http.MethodGet || (r.Path != project+"/deployments" && r.Path != project+"/agents") {
				continue
			}
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer "+azE2EEntraToken) || r.Header.Get("api-key") != "" {
				t.Errorf("%s: Authorization %q api-key %q, want only the Entra bearer", r.Path, r.Header.Get("Authorization"), r.Header.Get("api-key"))
			}
			want := "api-version=v1"
			if r.Path == project+"/agents" {
				want = "api-version=v1&limit=100" // the List Agents page size
			}
			if r.RawQuery != want {
				t.Errorf("%s query = %q, want %q", r.Path, r.RawQuery, want)
			}
		}

		// The Test button tests resource "default" without a model call: a sign-in, the v1 model route and the project, on the Entra bearer (never an api-key).
		before := len(env.fake.requests(host))
		var res map[string]any
		env.mustDo(t, http.MethodPost, "/api/providers/test", u.bearer, map[string]any{"provider": ai.AzureFoundryProviderID}, http.StatusOK, &res)
		if res["ok"] != true {
			t.Errorf("Entra connection test = %v", res)
		}
		var probe *azE2ERequest
		for _, r := range env.fake.requests(host)[before:] {
			if r.Method == http.MethodPost {
				t.Errorf("the resource test called %s: a resource test never calls a model", r.Path)
			}
			if r.Method == http.MethodGet && r.Path == "/openai/v1/models" {
				r := r
				probe = &r
			}
		}
		if probe == nil {
			t.Errorf("the resource test did not probe the v1 model route")
		} else if !strings.HasPrefix(probe.Header.Get("Authorization"), "Bearer "+azE2EEntraToken) || probe.Header.Get("api-key") != "" || probe.RawQuery != "" {
			t.Errorf("resource probe = query %q Authorization %q api-key %q", probe.RawQuery, probe.Header.Get("Authorization"), probe.Header.Get("api-key"))
		}

		// Testing the agent only looks it up: it is never run.
		before = len(env.fake.requests(host))
		res = nil
		env.mustDo(t, http.MethodPost, "/api/providers/azure-foundry/test", u.bearer,
			map[string]any{"model": map[string]any{"id": agent["id"]}}, http.StatusOK, &res)
		if res["ok"] != true {
			t.Errorf("agent test = %v", res)
		}
		var lookedUp bool
		for _, r := range env.fake.requests(host)[before:] {
			if r.Method == http.MethodPost {
				t.Errorf("the agent test sent POST %s: an agent test never runs the agent", r.Path)
			}
			lookedUp = lookedUp || (r.Method == http.MethodGet && r.Path == project+"/agents/helper")
		}
		if !lookedUp {
			t.Error("the agent test did not look the agent up")
		}

		conv := uuid.NewString()
		out := env.chat(t, u, azE2EChatReq{Model: agentID, Message: "Review my flow", ConversationID: conv})
		if out.Err != nil {
			t.Fatalf("agent chat failed: %v", out.Err)
		}
		if out.Text != "Hello from the helper agent." {
			t.Errorf("streamed text = %q", out.Text)
		}
		d := out.Done
		if d["servedModel"] != served || azE2ENum(d, "tokensIn") != 42 || azE2ENum(d, "tokensOut") != 7 {
			t.Errorf("done = %v, want servedModel %s and tokens 42/7", d, served)
		}
		if azE2ENum(d, "inputCostPerM") != 0 || azE2ENum(d, "outputCostPerM") != 0 {
			t.Errorf("done rates = %v/%v, want zero (Azure is unpriced by baki)", d["inputCostPerM"], d["outputCostPerM"])
		}

		if msgs := env.conversation(t, u, u.flowID, conv, 2); len(msgs) != 2 || msgs[1]["model"] != agentID ||
			msgs[1]["modelLabel"] != "Flow reviewer" || msgs[1]["servedModel"] != served || azE2ENum(msgs[1], "inputCostPerM") > 0 {
			t.Errorf("persisted agent turn = %v, want model %s labelled Flow reviewer, served by %s with no rates", msgs, agentID, served)
		}

		var resp *azE2ERequest
		for _, r := range env.fake.requests(host) {
			if r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/responses") {
				r := r
				resp = &r
			}
		}
		if resp == nil {
			t.Fatal("no Responses request reached the project")
		}
		if resp.Path != project+"/openai/v1/responses" || resp.RawQuery != "" {
			t.Errorf("responses request = %s?%s", resp.Path, resp.RawQuery)
		}
		if !strings.HasPrefix(resp.Header.Get("Authorization"), "Bearer "+azE2EEntraToken) {
			t.Errorf("responses Authorization = %q", resp.Header.Get("Authorization"))
		}
		body := resp.JSON()
		if ref, _ := body["agent_reference"].(map[string]any); ref["type"] != "agent_reference" || ref["name"] != "helper" {
			t.Errorf("agent_reference = %v", body["agent_reference"])
		}
		if st, ok := body["store"]; !ok || st != false {
			t.Errorf("store = %v (present %v), want an explicit false", st, ok)
		}
		if body["stream"] != true {
			t.Errorf("stream = %v", body["stream"])
		}
		for _, forbidden := range []string{"agent", "tools", "temperature"} {
			if _, ok := body[forbidden]; ok {
				t.Errorf("responses body carries %q: %s", forbidden, resp.Body)
			}
		}
		if !bytes.Contains(resp.Body, []byte("Review my flow")) {
			t.Errorf("the user's message is not in the input: %s", resp.Body)
		}
	})

	t.Run("8b_agent_function_call_loop", func(t *testing.T) {
		host := "s8b-res" + azE2EHostSuffix
		const (
			project = "/api/projects/proj1"
			callID  = "call_agent_e2e_1"
			served  = "gpt-4o-2024-11-20"
		)
		u := env.newUser(t, "u-s8b")
		env.saveConfig(t, u, map[string]any{
			"endpoint":        "https://" + host,
			"auth":            map[string]any{"mode": "entra", "tenantId": "11111111-2222-3333-4444-555555555555", "clientId": "66666666-7777-8888-9999-000000000000", "clientSecret": "s8b-client-secret"},
			"projectEndpoint": "https://" + host + project,
		})
		env.fake.handle(host, func(w http.ResponseWriter, r *http.Request, rec azE2ERequest, _ int) {
			switch {
			case r.Method == http.MethodGet && rec.Path == project+"/deployments":
				azE2EWriteJSON(w, http.StatusOK, azE2EProjectDeploymentsJSON)
			case r.Method == http.MethodGet && rec.Path == project+"/agents":
				azE2EWriteJSON(w, http.StatusOK, azE2EProjectAgentsJSON)
			case r.Method == http.MethodPost && rec.Path == project+"/openai/v1/responses":
				if !bytes.Contains(rec.Body, []byte("function_call_output")) {
					azE2EWriteSSE(w, azE2EResponsesFunctionCall(served, callID, "list_findings", "{}", 60, 12)...)
					return
				}
				azE2EWriteSSE(w, azE2EResponsesStream(served, "The flow has ", "two findings.", 90, 6)...)
			default:
				azE2EWriteJSON(w, http.StatusNotFound, `{"error":{"code":"NotFound","message":"no such route"}}`)
			}
		})

		// The agent is a model of the resource; its pre-registry id still
		// resolves to it (an alias, one release).
		env.addModel(t, u, map[string]any{"connectionId": "default", "kind": "agent", "deployment": "helper"})
		out := env.chat(t, u, azE2EChatReq{Model: "agent:helper", Message: "Any issues?"})
		if out.Err != nil {
			t.Fatalf("agent chat failed: %v", out.Err)
		}
		if out.Text != "The flow has two findings." {
			t.Errorf("final text = %q", out.Text)
		}
		if !out.has("tool", "list_findings") || !out.has("tool_result", "list_findings") {
			t.Errorf("events = %+v, want tool + tool_result for the agent's list_findings call", out.Events)
		}
		if out.Done["servedModel"] != served {
			t.Errorf("done servedModel = %v", out.Done["servedModel"])
		}

		var posts []azE2ERequest
		for _, r := range env.fake.requests(host) {
			if r.Method == http.MethodPost {
				posts = append(posts, r)
			}
		}
		if len(posts) != 2 {
			t.Fatalf("project received %d Responses requests, want 2 (function call, final answer)", len(posts))
		}
		second := posts[1].JSON()
		if st, ok := second["store"]; !ok || st != false {
			t.Errorf("follow-up store = %v (present %v), want false", st, ok)
		}
		if ref, _ := second["agent_reference"].(map[string]any); ref["name"] != "helper" {
			t.Errorf("follow-up agent_reference = %v", second["agent_reference"])
		}
		var sawCall, sawOutput bool
		input, _ := second["input"].([]any)
		for _, it := range input {
			im, _ := it.(map[string]any)
			switch im["type"] {
			case "function_call":
				sawCall = sawCall || (im["call_id"] == callID && im["name"] == "list_findings")
			case "function_call_output":
				if o, _ := im["output"].(string); im["call_id"] == callID && strings.TrimSpace(o) != "" {
					sawOutput = true
				}
			}
		}
		if !sawCall || !sawOutput {
			t.Errorf("follow-up input = %s, want the function_call %s replayed and its function_call_output", posts[1].Body, callID)
		}
	})

	t.Run("8c_entra_token_rejected_then_refreshed", func(t *testing.T) {
		host := "s8c-res" + azE2EHostSuffix
		const secret = "s8c-client-secret"
		u := env.newUser(t, "u-s8c")
		env.saveConfig(t, u, map[string]any{
			"endpoint":       "https://" + host,
			"auth":           map[string]any{"mode": "entra", "tenantId": "11111111-2222-3333-4444-555555555555", "clientId": "66666666-7777-8888-9999-000000000000", "clientSecret": secret},
			"chatDeployment": "gpt-4o",
		})
		env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, _ azE2ERequest, n int) {
			if n == 1 {
				azE2EWriteJSON(w, http.StatusUnauthorized, `{"error":{"code":"401","message":"Unauthorized. Access token is missing, invalid, audience is incorrect (https://cognitiveservices.azure.com), or have expired."}}`)
				return
			}
			const served = "gpt-4o-2024-11-20"
			azE2EWriteSSE(w,
				azE2EData(azE2EChunk(served, map[string]any{"role": "assistant", "content": ""}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{"content": "Signed in again."}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{}, "stop")),
				azE2EData(azE2EUsageFrame(served, 20, 4)),
				azE2EData("[DONE]"),
			)
		})

		out := env.chat(t, u, azE2EChatReq{Model: "gpt-4o", Message: "hello"})
		if out.Err != nil || out.Text != "Signed in again." {
			t.Fatalf("outcome = err %v text %q, want the answer after one token refresh", out.Err, out.Text)
		}
		reqs := env.fake.requests(host)
		if len(reqs) != 2 {
			t.Fatalf("resource received %d requests, want 2 (rejected token, fresh token)", len(reqs))
		}
		a, b := reqs[0].Header.Get("Authorization"), reqs[1].Header.Get("Authorization")
		if !strings.HasPrefix(a, "Bearer "+azE2EEntraToken) || !strings.HasPrefix(b, "Bearer "+azE2EEntraToken) || a == b {
			t.Errorf("Authorization rejected/retried = %q / %q, want two different Entra tokens", a, b)
		}
		signIns := 0
		for _, r := range env.fake.requests(azE2EAADHost) {
			if form, _ := url.ParseQuery(string(r.Body)); r.Method == http.MethodPost && form.Get("client_secret") == secret {
				signIns++
			}
		}
		if signIns != 2 {
			t.Errorf("Entra ID sign-ins for this principal = %d, want 2 (initial + after the 401)", signIns)
		}
	})

	t.Run("9_org_shared_credential", func(t *testing.T) {
		host := "s9-res" + azE2EHostSuffix
		const orgKey = "s9-org-shared-key"
		const served = "gpt-4o-2024-11-20"
		ctx := context.Background()
		admin, member, viewer := "u-s9-admin", "u-s9-member", "u-s9-viewer"
		org, err := env.orgSvc.Create(ctx, "Acme", admin)
		if err != nil {
			t.Fatalf("create org: %v", err)
		}
		if err := env.orgSvc.AddMember(ctx, org.ID, member, auth.RoleMember); err != nil {
			t.Fatal(err)
		}
		if err := env.orgSvc.AddMember(ctx, org.ID, viewer, auth.RoleViewer); err != nil {
			t.Fatal(err)
		}
		const orgFlow = "flow-s9-org"
		env.seedFlow(t, orgFlow, admin, org.ID)
		adminU := azE2EUser{id: admin, bearer: env.bearer(t, admin, auth.RoleMember), flowID: orgFlow}
		memberU := azE2EUser{id: member, bearer: env.bearer(t, member, auth.RoleMember), flowID: orgFlow}
		viewerU := azE2EUser{id: viewer, bearer: env.bearer(t, viewer, auth.RoleViewer), flowID: orgFlow}

		env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, rec azE2ERequest, _ int) {
			if rec.JSON()["stream"] != true {
				azE2EWriteJSON(w, http.StatusOK, azE2ECompletionJSON("Hi!", 8, 2))
				return
			}
			azE2EWriteSSE(w,
				azE2EData(azE2EChunk(served, map[string]any{"role": "assistant", "content": ""}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{"content": "Shared answer."}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{}, "stop")),
				azE2EData(azE2EUsageFrame(served, 50, 3)),
				azE2EData("[DONE]"),
			)
		})

		cfgPath := "/api/orgs/" + org.ID + "/providers/azure-foundry/config"
		var before map[string]any
		env.mustDo(t, http.MethodGet, cfgPath, adminU.bearer, nil, http.StatusOK, &before)
		if before["configured"] != false {
			t.Fatalf("org config before save = %v", before)
		}
		// A member may not write the org's credential.
		env.mustDo(t, http.MethodPost, cfgPath, memberU.bearer, map[string]any{
			"endpoint": "https://" + host, "auth": map[string]any{"mode": "api_key", "key": "member-key"}, "chatDeployment": "gpt-4o",
		}, http.StatusForbidden, nil)

		var saved map[string]any
		env.mustDo(t, http.MethodPost, cfgPath, adminU.bearer, map[string]any{
			"endpoint": "https://" + host, "auth": map[string]any{"mode": "api_key", "key": orgKey}, "chatDeployment": "gpt-4o",
		}, http.StatusOK, &saved)
		if saved["status"] != "ok" || saved["updatedAt"] == "" || saved["updatedAt"] == nil {
			t.Errorf("org save = %v, want ok + an updatedAt token", saved)
		}
		var memberView map[string]any
		raw := env.mustDo(t, http.MethodGet, cfgPath, memberU.bearer, nil, http.StatusOK, &memberView)
		if memberView["configured"] != true || memberView["hasSecret"] != true || bytes.Contains(raw, []byte(orgKey)) {
			t.Errorf("member's view of the org config = %s", raw)
		}

		var test map[string]any
		env.mustDo(t, http.MethodPost, "/api/orgs/"+org.ID+"/providers/azure-foundry/test", adminU.bearer, nil, http.StatusOK, &test)
		if test["ok"] != true {
			t.Errorf("org connection test = %v", test)
		}

		// A member without a resource of their own is offered the org's models, flagged shared: the saved configuration became the org's resource "default", its chat deployment the org's default for members (and, for a member without a "default" of their own, the answer to the pre-registry id gpt-4o).
		p := env.providerInfo(t, memberU.bearer, org.ID)
		sharedModel := azE2EModelByAlias(p, "gpt-4o")
		sharedID, _ := sharedModel["id"].(string)
		if p["configured"] != true || p["shared"] != true || len(azE2EModelIDs(p)) != 1 || sharedModel == nil || p["defaultModel"] != sharedID {
			t.Fatalf("member listing with orgId = configured %v shared %v models %v default %v", p["configured"], p["shared"], azE2EModelIDs(p), p["defaultModel"])
		}
		if sharedModel["shared"] != true || sharedModel["isOrgDefault"] != true || sharedModel["orgId"] != org.ID || sharedModel["orgName"] != "Acme" {
			t.Errorf("shared model = %v", sharedModel)
		}
		if p := env.providerInfo(t, memberU.bearer, ""); p["configured"] != false || len(azE2EModelIDs(p)) != 0 {
			t.Errorf("member listing without orgId = configured %v models %v, want nothing (no resource of their own)", p["configured"], azE2EModelIDs(p))
		}

		before9 := len(env.fake.requests(host))
		conv := uuid.NewString()
		out := env.chat(t, memberU, azE2EChatReq{Model: sharedID, Message: "Shared question", ConversationID: conv})
		if out.Err != nil || out.Text != "Shared answer." {
			t.Fatalf("member chat on the org flow = err %v text %q", out.Err, out.Text)
		}
		if out.Done["servedModel"] != served {
			t.Errorf("done servedModel = %v", out.Done["servedModel"])
		}
		reqs := env.fake.requests(host)
		if len(reqs) != before9+1 {
			t.Fatalf("resource received %d new requests for the member's turn, want 1", len(reqs)-before9)
		}
		if got := reqs[len(reqs)-1].Header.Get("api-key"); got != orgKey {
			t.Errorf("member's turn api-key = %q, want the org's shared key", got)
		}
		if msgs := env.conversation(t, memberU, orgFlow, conv, 2); len(msgs) != 2 || msgs[1]["model"] != sharedID || msgs[1]["modelLabel"] != "gpt-4o" {
			t.Errorf("persisted shared turn = %v, want model %s labelled gpt-4o", msgs, sharedID)
		}
		// The pre-registry id still reaches the org's model through its alias.
		if out := env.chat(t, memberU, azE2EChatReq{Model: "gpt-4o", Message: "Shared question again"}); out.Err != nil || out.Text != "Shared answer." {
			t.Errorf("member chat by the pre-registry id = err %v text %q", out.Err, out.Text)
		}
		if reqs := env.fake.requests(host); reqs[len(reqs)-1].Header.Get("api-key") != orgKey {
			t.Errorf("the pre-registry id's turn ran on api-key %q", reqs[len(reqs)-1].Header.Get("api-key"))
		}

		// A viewer is not offered the organisation's models and cannot run
		// on them: the send is refused before any stream starts.
		if p := env.providerInfo(t, viewerU.bearer, org.ID); p["configured"] != false || p["shared"] == true || len(azE2EModelIDs(p)) != 0 {
			t.Errorf("viewer listing with orgId = configured %v shared %v models %v, want nothing", p["configured"], p["shared"], azE2EModelIDs(p))
		}
		before9 = len(env.fake.requests(host))
		env.chatRefused(t, viewerU, azE2EChatReq{Model: sharedID, Message: "Viewer question"})
		env.chatRefused(t, viewerU, azE2EChatReq{Model: "gpt-4o", Message: "Viewer question"})
		if n := len(env.fake.requests(host)) - before9; n != 0 {
			t.Errorf("the viewer's turns reached the resource %d time(s)", n)
		}
	})

	t.Run("10_model_not_available_and_preview", func(t *testing.T) {
		host := "s10-res" + azE2EHostSuffix
		u := env.newUser(t, "u-s10")
		env.saveConfig(t, u, map[string]any{
			"endpoint":       "https://" + host,
			"auth":           map[string]any{"mode": "api_key", "key": "s10-secret-key"},
			"chatDeployment": "gpt-4o",
		})
		env.fake.handle(host, func(w http.ResponseWriter, _ *http.Request, _ azE2ERequest, _ int) {
			const served = "gpt-4o-2024-11-20"
			azE2EWriteSSE(w,
				azE2EData(azE2EChunk(served, map[string]any{"role": "assistant", "content": "Default answer."}, nil)),
				azE2EData(azE2EChunk(served, map[string]any{}, "stop")),
				azE2EData(azE2EUsageFrame(served, 10, 2)),
				azE2EData("[DONE]"),
			)
		})
		distilled := env.addModel(t, u, map[string]any{"connectionId": "default", "kind": "chat", "deployment": "distilled",
			"displayName": "Distilled", "purpose": "documentation", "contextWindow": 20000, "maxOutputTokens": 1024})
		distilledID, _ := distilled["modelId"].(string)
		gptID := env.modelIDFor(t, u, "gpt-4o")

		// An unknown or removed model is refused at create time — 400 MODEL_NOT_AVAILABLE, the text goes back to the composer — and by the preview alike; neither reaches the resource.
		env.chatRefused(t, u, azE2EChatReq{Model: "m/zzzzzzzzzz", Message: "q"})
		var refused map[string]any
		env.mustDo(t, http.MethodPost, "/api/chat/preview-context", u.bearer, map[string]any{
			"flowId": u.flowID, "provider": ai.AzureFoundryProviderID, "model": "m/zzzzzzzzzz", "userMessage": "q",
		}, http.StatusBadRequest, &refused)
		if refused["code"] != service.AzureCodeModelNotAvailable {
			t.Errorf("preview of an unknown model = %v, want code %s", refused, service.AzureCodeModelNotAvailable)
		}

		// The preview's window is the model's own: the distilled model's
		// 20000-token override, gpt-4o's catalogue 128000.
		for id, want := range map[string]float64{distilledID: 20000, gptID: 128000} {
			var preview map[string]any
			env.mustDo(t, http.MethodPost, "/api/chat/preview-context", u.bearer, map[string]any{
				"flowId": u.flowID, "provider": ai.AzureFoundryProviderID, "model": id, "userMessage": "q",
			}, http.StatusOK, &preview)
			if azE2ENum(preview, "contextLimit") != want {
				t.Errorf("preview of %s = contextLimit %v, want %v", id, preview["contextLimit"], want)
			}
		}
		if n := len(env.fake.requests(host)); n != 0 {
			t.Errorf("refusals and previews made %d request(s) to the resource", n)
		}

		// model "" is the server default — the first chat model in display order, gpt-4o (general before documentation) — stored with its registry id.
		conv := uuid.NewString()
		if out := env.chat(t, u, azE2EChatReq{Message: "Which model?", ConversationID: conv}); out.Err != nil || out.Text != "Default answer." {
			t.Fatalf("default model turn = err %v text %q", out.Err, out.Text)
		}
		if reqs := env.fake.requests(host); len(reqs) != 1 || reqs[0].JSON()["model"] != "gpt-4o" {
			t.Errorf("default model request = %v", reqs)
		}
		if msgs := env.conversation(t, u, u.flowID, conv, 2); len(msgs) != 2 || msgs[1]["model"] != gptID || msgs[1]["modelLabel"] != "gpt-4o" {
			t.Errorf("persisted default turn = %v, want model %s", msgs, gptID)
		}

		// A distilled model's output cap bounds the request.
		if out := env.chat(t, u, azE2EChatReq{Model: distilledID, Message: "Short answer please"}); out.Err != nil {
			t.Fatalf("distilled turn = err %v", out.Err)
		}
		if reqs := env.fake.requests(host); len(reqs) != 2 || reqs[1].JSON()["model"] != "distilled" || azE2ENum(reqs[1].JSON(), "max_completion_tokens") != 1024 {
			t.Errorf("distilled request = model %v max_completion_tokens %v, want distilled capped at 1024",
				reqs[len(reqs)-1].JSON()["model"], reqs[len(reqs)-1].JSON()["max_completion_tokens"])
		}

		// Removed after the list loaded (a stale tab): refused, nothing sent.
		env.mustDo(t, http.MethodDelete, "/api/providers/azure-foundry/models/"+distilled["id"].(string), u.bearer, nil, http.StatusOK, nil)
		env.chatRefused(t, u, azE2EChatReq{Model: distilledID, Message: "Still there?"})
		if n := len(env.fake.requests(host)); n != 2 {
			t.Errorf("the refused send reached the resource (%d requests)", n)
		}
	})

	t.Run("11_save_keeps_a_clamped_model_label", func(t *testing.T) {
		u := env.newUser(t, "u-s11")
		conv := uuid.NewString()
		now := time.Now().UTC().Format(time.RFC3339)
		env.mustDo(t, http.MethodPost, "/api/chat/save", u.bearer, map[string]any{
			"flowId": u.flowID, "provider": conv,
			"messages": []map[string]any{
				{"id": uuid.NewString(), "role": "user", "content": "q", "timestamp": now},
				{"id": uuid.NewString(), "role": "assistant", "content": "a", "timestamp": now,
					"provider": ai.AzureFoundryProviderID, "model": "m/abcdefghij", "modelLabel": "Docs writer"},
				{"id": uuid.NewString(), "role": "assistant", "content": "b", "timestamp": now,
					"provider": ai.AzureFoundryProviderID, "model": "m/abcdefghij",
					"modelLabel": "  Docs\u202e\twriter\u200b \n" + strings.Repeat("x", 80)},
			},
		}, http.StatusOK, nil)
		msgs := env.conversation(t, u, u.flowID, conv, 3)
		if len(msgs) != 3 {
			t.Fatalf("saved conversation = %v", msgs)
		}
		if msgs[1]["modelLabel"] != "Docs writer" || msgs[1]["model"] != "m/abcdefghij" {
			t.Errorf("round-tripped label = %v, want Docs writer", msgs[1])
		}
		want := "Docs writer " + strings.Repeat("x", 52) // 64 characters
		if msgs[2]["modelLabel"] != want {
			t.Errorf("clamped label = %q, want %q", msgs[2]["modelLabel"], want)
		}
	})

}

// --- realistic Azure payloads ---

// azE2EChunk is one chat.completion.chunk as Azure OpenAI / Foundry streams it.
func azE2EChunk(model string, delta map[string]any, finish any) map[string]any {
	return map[string]any{
		"id": "chatcmpl-E2E0001", "object": "chat.completion.chunk", "created": 1759000000,
		"model": model, "system_fingerprint": "fp_ee1d74bde0",
		"choices": []any{map[string]any{
			"index": 0, "delta": delta, "finish_reason": finish, "logprobs": nil,
			"content_filter_results": map[string]any{},
		}},
	}
}

// azE2EPromptFilterFrame is the first frame Azure streams: prompt filter
// results, no model, no choices.
func azE2EPromptFilterFrame() map[string]any {
	safe := map[string]any{"filtered": false, "severity": "safe"}
	return map[string]any{
		"choices": []any{}, "created": 0, "id": "", "model": "", "object": "",
		"prompt_filter_results": []any{map[string]any{"prompt_index": 0, "content_filter_results": map[string]any{
			"hate": safe, "self_harm": safe, "sexual": safe, "violence": safe,
			"jailbreak": map[string]any{"filtered": false, "detected": false},
		}}},
	}
}

// azE2EUsageFrame is the include_usage frame: choices [] and the usage.
func azE2EUsageFrame(model string, in, out int) map[string]any {
	return map[string]any{
		"id": "chatcmpl-E2E0001", "object": "chat.completion.chunk", "created": 1759000000,
		"model": model, "system_fingerprint": "fp_ee1d74bde0", "choices": []any{},
		"usage": map[string]any{
			"prompt_tokens": in, "completion_tokens": out, "total_tokens": in + out,
			"prompt_tokens_details":     map[string]any{"audio_tokens": 0, "cached_tokens": 0},
			"completion_tokens_details": map[string]any{"accepted_prediction_tokens": 0, "audio_tokens": 0, "reasoning_tokens": 0, "rejected_prediction_tokens": 0},
		},
	}
}

// azE2ECompletionJSON is a non-streaming chat.completion.
func azE2ECompletionJSON(text string, in, out int) string {
	return `{"choices":[{"content_filter_results":{"hate":{"filtered":false,"severity":"safe"},"self_harm":{"filtered":false,"severity":"safe"},"sexual":{"filtered":false,"severity":"safe"},"violence":{"filtered":false,"severity":"safe"}},` +
		`"finish_reason":"stop","index":0,"logprobs":null,"message":{"annotations":[],"content":"` + text + `","refusal":null,"role":"assistant"}}],` +
		`"created":1759000000,"id":"chatcmpl-E2E0002","model":"gpt-4o-2024-11-20","object":"chat.completion",` +
		`"prompt_filter_results":[{"prompt_filter_results":{},"prompt_index":0,"content_filter_results":{}}],"system_fingerprint":"fp_ee1d74bde0",` +
		`"usage":{"completion_tokens":` + strconv.Itoa(out) + `,"completion_tokens_details":{"accepted_prediction_tokens":0,"audio_tokens":0,"reasoning_tokens":0,"rejected_prediction_tokens":0},` +
		`"prompt_tokens":` + strconv.Itoa(in) + `,"prompt_tokens_details":{"audio_tokens":0,"cached_tokens":0},"total_tokens":` + strconv.Itoa(in+out) + `}}`
}

// azE2EContentFilterJSON is Azure's 400 for a prompt the filter rejected.
const azE2EContentFilterJSON = `{"error":{"message":"The response was filtered due to the prompt triggering Azure OpenAI's content management policy. Please modify your prompt and retry. To learn more about our content filtering policies please read our documentation: https://go.microsoft.com/fwlink/?linkid=2198766","type":null,"param":"prompt","code":"content_filter","status":400,"innererror":{"code":"ResponsibleAIPolicyViolation","content_filter_result":{"hate":{"filtered":false,"severity":"safe"},"jailbreak":{"filtered":true,"detected":true},"self_harm":{"filtered":false,"severity":"safe"},"sexual":{"filtered":false,"severity":"safe"},"violence":{"filtered":false,"severity":"safe"}}}}}`

// azE2ERateLimitJSON is Azure's 429 body.
const azE2ERateLimitJSON = `{"error":{"code":"429","message":"Requests to the ChatCompletions_Create Operation under Azure OpenAI API version 2025-04-01-preview have exceeded token rate limit of your current AIServices S0 pricing tier. Please retry after 1 second. Please contact Azure support service if you would like to further increase the default rate limit."}}`

// azE2EProjectDeploymentsJSON is GET {project}/deployments?api-version=v1.
const azE2EProjectDeploymentsJSON = `{"value":[` +
	`{"type":"ModelDeployment","name":"gpt-4o","modelName":"gpt-4o","modelVersion":"2024-11-20","modelPublisher":"OpenAI","capabilities":{"chat_completion":"true"},"sku":{"name":"GlobalStandard","capacity":50},"connectionName":null},` +
	`{"type":"ModelDeployment","name":"text-embedding-3-small","modelName":"text-embedding-3-small","modelVersion":"1","modelPublisher":"OpenAI","capabilities":{"embeddings":"true"},"sku":{"name":"Standard","capacity":120},"connectionName":null}` +
	`]}`

// azE2EProjectAgentJSON is GET {project}/agents/helper?api-version=v1: one
// prompt agent, as the list below carries it.
const azE2EProjectAgentJSON = `{"object":"agent","id":"helper","name":"helper","versions":{"latest":{"metadata":{},"object":"agent.version","id":"helper:1","name":"helper","version":"1","description":"","created_at":1759000000,"definition":{"kind":"prompt","model":"gpt-4o","instructions":"You review Power Automate Desktop flows."}}}}`

// azE2EProjectAgentsJSON is GET {project}/agents?api-version=v1&limit=100.
const azE2EProjectAgentsJSON = `{"object":"list","data":[` + azE2EProjectAgentJSON + `],"first_id":"helper","last_id":"helper","has_more":false}`

// azE2EResponsesStream is a Responses API stream for an agent run: created,
// a message item streamed in two deltas, and response.completed with usage.
func azE2EResponsesStream(model, part1, part2 string, in, out int) []string {
	respObj := func(status string, output []any, usage any) map[string]any {
		return azE2EResponseObj(model, status, output, usage)
	}
	msg := func(status, text string) map[string]any {
		content := []any{}
		if text != "" {
			content = append(content, map[string]any{"type": "output_text", "annotations": []any{}, "logprobs": []any{}, "text": text})
		}
		return map[string]any{"id": "msg_e2e", "type": "message", "status": status, "role": "assistant", "content": content}
	}
	full := part1 + part2
	return []string{
		azE2EEvent("response.created", map[string]any{"type": "response.created", "sequence_number": 0, "response": respObj("in_progress", []any{}, nil)}),
		azE2EEvent("response.in_progress", map[string]any{"type": "response.in_progress", "sequence_number": 1, "response": respObj("in_progress", []any{}, nil)}),
		azE2EEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "sequence_number": 2, "output_index": 0, "item": msg("in_progress", "")}),
		azE2EEvent("response.content_part.added", map[string]any{"type": "response.content_part.added", "sequence_number": 3, "item_id": "msg_e2e", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "annotations": []any{}, "text": ""}}),
		azE2EEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "sequence_number": 4, "item_id": "msg_e2e", "output_index": 0, "content_index": 0, "delta": part1, "logprobs": []any{}}),
		azE2EEvent("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "sequence_number": 5, "item_id": "msg_e2e", "output_index": 0, "content_index": 0, "delta": part2, "logprobs": []any{}}),
		azE2EEvent("response.output_text.done", map[string]any{"type": "response.output_text.done", "sequence_number": 6, "item_id": "msg_e2e", "output_index": 0, "content_index": 0, "text": full, "logprobs": []any{}}),
		azE2EEvent("response.content_part.done", map[string]any{"type": "response.content_part.done", "sequence_number": 7, "item_id": "msg_e2e", "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "annotations": []any{}, "text": full}}),
		azE2EEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "sequence_number": 8, "output_index": 0, "item": msg("completed", full)}),
		azE2EEvent("response.completed", map[string]any{"type": "response.completed", "sequence_number": 9, "response": respObj("completed", []any{msg("completed", full)}, azE2EResponsesUsage(in, out))}),
	}
}

// azE2EResponseObj is the response object Responses events carry.
func azE2EResponseObj(model, status string, output []any, usage any) map[string]any {
	return map[string]any{
		"id": "resp_e2e", "object": "response", "created_at": 1759000000, "status": status,
		"error": nil, "incomplete_details": nil, "model": model, "output": output, "usage": usage,
		"agent_reference": map[string]any{"type": "agent_reference", "name": "helper", "version": "1"},
	}
}

// azE2EResponsesUsage is a Responses usage object.
func azE2EResponsesUsage(in, out int) map[string]any {
	return map[string]any{
		"input_tokens": in, "input_tokens_details": map[string]any{"cached_tokens": 0},
		"output_tokens": out, "output_tokens_details": map[string]any{"reasoning_tokens": 0}, "total_tokens": in + out,
	}
}

// azE2EResponsesFunctionCall is an agent run that ends by calling one of
// baki's function tools (the agent declares it; baki answers it).
func azE2EResponsesFunctionCall(model, callID, name, args string, in, out int) []string {
	item := func(status, a string) map[string]any {
		return map[string]any{"id": "fc_e2e", "type": "function_call", "status": status, "call_id": callID, "name": name, "arguments": a}
	}
	return []string{
		azE2EEvent("response.created", map[string]any{"type": "response.created", "sequence_number": 0, "response": azE2EResponseObj(model, "in_progress", []any{}, nil)}),
		azE2EEvent("response.output_item.added", map[string]any{"type": "response.output_item.added", "sequence_number": 1, "output_index": 0, "item": item("in_progress", "")}),
		azE2EEvent("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "sequence_number": 2, "item_id": "fc_e2e", "output_index": 0, "delta": args}),
		azE2EEvent("response.function_call_arguments.done", map[string]any{"type": "response.function_call_arguments.done", "sequence_number": 3, "item_id": "fc_e2e", "output_index": 0, "arguments": args}),
		azE2EEvent("response.output_item.done", map[string]any{"type": "response.output_item.done", "sequence_number": 4, "output_index": 0, "item": item("completed", args)}),
		azE2EEvent("response.completed", map[string]any{"type": "response.completed", "sequence_number": 5, "response": azE2EResponseObj(model, "completed", []any{item("completed", args)}, azE2EResponsesUsage(in, out))}),
	}
}

// azE2EOffersTool reports whether a chat-completions body offers the named
// function tool.
func azE2EOffersTool(body map[string]any, name string) bool {
	tools, _ := body["tools"].([]any)
	for _, tl := range tools {
		tm, _ := tl.(map[string]any)
		fn, _ := tm["function"].(map[string]any)
		if tm["type"] == "function" && fn["name"] == name {
			return true
		}
	}
	return false
}
