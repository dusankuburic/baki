package ai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// F2 (Foundry Responses / agents route) — stateless replay of the run's own
// output items, agent listing filters, workflow agents, run blockers, trail
// pairing, call keying, stream error types and the agent's served model.

// replayFixture is one agent run's output, as recorded: reasoning with and
// without encrypted content, a hosted web search, a Foundry-only Bing step
// (and its output) after a reasoning item, the answer and a function call.
const replayFixture = `[` +
	`{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"ENC1","agent_reference":{"type":"agent_reference","name":"Helper"},"response_id":"resp_1"},` +
	`{"id":"ws_1","type":"web_search_call","status":"completed","action":{"type":"search","query":"q"}},` +
	`{"id":"rs_2","type":"reasoning","summary":[],"encrypted_content":"ENC2"},` +
	`{"type":"bing_grounding_call","call_id":"b1","arguments":"{}","status":"completed"},` +
	`{"type":"bing_grounding_call_output","call_id":"b1","output":"x","status":"completed"},` +
	`{"id":"rs_3","type":"reasoning","summary":[]},` +
	`{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"looking","annotations":[]}]},` +
	`{"id":"fc_1","type":"function_call","call_id":"call_1","name":"search_flow","arguments":"{\"q\":\"x\"}","status":"completed"}` +
	`]`

// inputItems renders a request input as generic maps.
func inputItems(t *testing.T, input []any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("unmarshal input: %v", err)
	}
	return out
}

func itemTypes(items []map[string]any) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i], _ = it["type"].(string)
	}
	return out
}

func replayHistory(from string, calls ...ToolCall) []Message {
	return []Message{
		{Role: "user", Content: "find"},
		{Role: "assistant", Content: "looking", ToolCalls: calls, ProviderItems: json.RawMessage(replayFixture), ProviderItemsFrom: from},
		{Role: "tool", ToolCallID: "call_1", Content: "result"},
	}
}

var flowCall = ToolCall{ID: "call_1", Name: "search_flow", Input: json.RawMessage(`{"q":"x"}`)}

// Item 1: an assistant turn carrying this provider's items is replayed
// verbatim and in order; Foundry-only items (and the reasoning item bound to
// them), reasoning without encrypted content and output-only annotations are
// dropped; the legacy rebuild is kept for the fallback.
func TestResponsesInput_ReplaysProviderItems(t *testing.T) {
	input, legacy := responsesInput("SYS", replayHistory(azureFoundryProviderID, flowCall))
	items := inputItems(t, input)
	want := []string{"message", "message", "reasoning", "web_search_call", "message", "function_call", "function_call_output"}
	if got := itemTypes(items); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("input types = %v, want %v", got, want)
	}
	rs := items[2]
	if rs["id"] != "rs_1" || rs["encrypted_content"] != "ENC1" {
		t.Errorf("reasoning item = %v", rs)
	}
	if _, ok := rs["agent_reference"]; ok {
		t.Errorf("output-only agent_reference replayed: %v", rs)
	}
	if _, ok := rs["response_id"]; ok {
		t.Errorf("output-only response_id replayed: %v", rs)
	}
	if fc := items[5]; fc["id"] != "fc_1" || fc["call_id"] != "call_1" || fc["name"] != "search_flow" {
		t.Errorf("function_call must be replayed with its item id: %v", fc)
	}
	if msg := items[4]; msg["id"] != "msg_1" || msg["status"] != "completed" {
		t.Errorf("message item = %v", msg)
	}
	if legacy == nil {
		t.Fatal("a replayed request must keep the legacy rebuild for the fallback")
	}
	legacyItems := inputItems(t, legacy)
	if got := itemTypes(legacyItems); strings.Join(got, ",") != "message,message,message,function_call,function_call_output" {
		t.Errorf("legacy types = %v", got)
	}
}

// Item 1: items another provider wrote, or items that no longer describe the
// turn (a tool call without its function_call item), are not replayed.
func TestResponsesInput_RebuildsWhenItemsDoNotApply(t *testing.T) {
	for name, msgs := range map[string][]Message{
		"other provider":   replayHistory("claude", flowCall),
		"uncovered call":   replayHistory(azureFoundryProviderID, flowCall, ToolCall{ID: "call_2", Name: "get_block"}),
		"undecodable":      {{Role: "assistant", Content: "x", ProviderItems: json.RawMessage(`{`), ProviderItemsFrom: azureFoundryProviderID}},
		"nothing to reuse": {{Role: "assistant", Content: "x", ProviderItems: json.RawMessage(`[{"type":"openapi_call","call_id":"c"}]`), ProviderItemsFrom: azureFoundryProviderID}},
	} {
		t.Run(name, func(t *testing.T) {
			input, legacy := responsesInput("", msgs)
			if legacy != nil {
				t.Errorf("nothing replayed, yet a legacy fallback was built")
			}
			for _, it := range inputItems(t, input) {
				if it["type"] == "reasoning" || it["id"] != nil {
					t.Errorf("provider item leaked into the rebuild: %v", it)
				}
			}
		})
	}
}

// Item 1: the stream records the run's replayable items, in output order, on
// the Done chunk — function calls with their item id.
func TestParseResponsesSSE_RecordsProviderItems(t *testing.T) {
	reasoning := `{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"ENC"}`
	events := itemPair(0, reasoning)
	events = append(events, itemPair(1, `{"id":"ws_1","type":"web_search_call","status":"completed"}`)...)
	events = append(events, itemPair(2, `{"type":"bing_grounding_call","call_id":"b1","arguments":"{}","status":"completed"}`)...)
	events = append(events, itemPair(3, answerItem)...)
	events = append(events, itemPair(4, flowCallItem)...)
	events = append(events, `{"type":"response.completed","response":{"status":"completed","model":"gpt-5","usage":{"input_tokens":3,"output_tokens":2}}}`)
	s := summarize(parseChunks(t, sseEvents(events...)))
	if s.done == nil {
		t.Fatalf("no Done chunk: %+v", s.errs)
	}
	var items []map[string]any
	if err := json.Unmarshal(s.done.ProviderItems, &items); err != nil {
		t.Fatalf("ProviderItems = %s: %v", s.done.ProviderItems, err)
	}
	if got := strings.Join(itemTypes(items), ","); got != "reasoning,web_search_call,message,function_call" {
		t.Errorf("recorded types = %s", got)
	}
	if items[3]["id"] != "fc_1" || items[0]["encrypted_content"] != "ENC" {
		t.Errorf("recorded items = %v", items)
	}
}

// Item 1: the non-streaming turn asks for encrypted reasoning and records the
// items on the Response; the next request replays them.
func TestAzureFoundry_AgentChatRecordsAndReplaysProviderItems(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 200, map[string]any{
			"id": "resp_1", "status": "completed", "model": "gpt-5",
			"output": json.RawMessage(`[{"id":"rs_1","type":"reasoning","summary":[],"encrypted_content":"ENC"},` + flowCallItem + `]`),
		})
	})
	p := newAzureAgentProvider(t, srv)
	req := Request{Model: "agent:Helper", Messages: []Message{{Role: "user", Content: "hi"}}}
	resp, err := p.Chat(context.Background(), req)
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	first := srv.requests()[0].Body
	if inc, _ := first["include"].([]any); len(inc) != 1 || inc[0] != "reasoning.encrypted_content" {
		t.Errorf("include = %v", first["include"])
	}
	if len(resp.ProviderItems) == 0 || len(resp.ToolCalls) != 1 {
		t.Fatalf("response = %+v", resp)
	}
	req.Messages = append(req.Messages,
		Message{Role: "assistant", ToolCalls: resp.ToolCalls, ProviderItems: resp.ProviderItems, ProviderItemsFrom: azureFoundryProviderID},
		Message{Role: "tool", ToolCallID: "call_1", Content: "found"})
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	input, _ := srv.requests()[1].Body["input"].([]any)
	var types []string
	for _, it := range input {
		m, _ := it.(map[string]any)
		types = append(types, m["type"].(string))
	}
	if strings.Join(types, ",") != "message,reasoning,function_call,function_call_output" {
		t.Errorf("replayed input types = %v", types)
	}
}

// Item 1: a 400 blaming a replayed item costs one retry with the legacy
// rebuild (no items, no include), remembered per agent.
func TestAzureFoundry_AgentReplayRejectedFallsBackOnce(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		raw, _ := json.Marshal(body["input"])
		if strings.Contains(string(raw), `"reasoning"`) {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"type": "invalid_request_error", "param": nil,
				"code": "invalid_encrypted_content", "message": "The encrypted content for item rs_1 could not be verified."}})
			return
		}
		writeJSON(w, 200, map[string]any{"id": "resp_2", "status": "completed", "model": "gpt-5", "output": json.RawMessage(`[` + answerItem + `]`)})
	})
	p := newAzureAgentProvider(t, srv)
	req := Request{Model: "agent:Helper", Messages: replayHistory(azureFoundryProviderID, flowCall)}
	resp, err := p.Chat(context.Background(), req)
	if err != nil || resp.Content != "Answer" {
		t.Fatalf("Chat = %+v, %v", resp, err)
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d, want the rejected replay and one legacy retry", len(reqs))
	}
	retry := reqs[1].Body
	if _, ok := retry["include"]; ok {
		t.Errorf("the legacy retry must not carry include: %v", retry)
	}
	raw, _ := json.Marshal(retry["input"])
	if strings.Contains(string(raw), `"fc_1"`) || strings.Contains(string(raw), "ENC") {
		t.Errorf("the legacy retry replayed provider items: %s", raw)
	}
	if retry["store"] != false {
		t.Errorf("store:false must survive the fallback: %v", retry)
	}
	// Remembered: the next turn goes out as the legacy rebuild at once.
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("second Chat: %v", err)
	}
	if n := len(srv.requests()); n != 3 {
		t.Errorf("second turn made %d requests, want 1", n-2)
	}
	// Another agent starts from the replay.
	other := req
	other.Model = "agent:Other"
	if _, err := p.Chat(context.Background(), other); err != nil {
		t.Fatalf("other agent: %v", err)
	}
	if reqs := srv.requests(); len(reqs) != 5 || reqs[3].Body["include"] == nil {
		t.Errorf("another agent must try the replay first: %d requests", len(reqs))
	}
}

// Item 1: a structured rejection of `include` alone is a parameter repair —
// the items still go out.
func TestAzureFoundry_AgentIncludeRejectionKeepsReplay(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if _, ok := body["include"]; ok {
			writeJSON(w, 400, map[string]any{"error": map[string]any{"code": "unsupported_parameter", "param": "include",
				"message": "Unsupported parameter: 'include' is not supported with this model."}})
			return
		}
		writeJSON(w, 200, map[string]any{"id": "resp_3", "status": "completed", "model": "gpt-5", "output": json.RawMessage(`[` + answerItem + `]`)})
	})
	p := newAzureAgentProvider(t, srv)
	if _, err := p.Chat(context.Background(), Request{Model: "agent:Helper", Messages: replayHistory(azureFoundryProviderID, flowCall)}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("requests = %d", len(reqs))
	}
	raw, _ := json.Marshal(reqs[1].Body["input"])
	if !strings.Contains(string(raw), `"fc_1"`) || !strings.Contains(string(raw), "ENC1") {
		t.Errorf("the include repair must keep the replayed items: %s", raw)
	}
}

// Item 2: a deployment that comes from a project connection (AI gateway) is
// not callable on /openai/v1 and is not listed.
func TestAzureFoundry_ProjectDeploymentsSkipConnectionDeployments(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
		writeJSON(w, 200, map[string]any{"value": []map[string]any{
			{"name": "gpt-4o", "modelName": "gpt-4o", "type": "ModelDeployment"},
			{"name": "gw-gpt", "modelName": "gpt-4.1", "connectionName": "my-apim-gateway", "type": "ModelDeployment"},
		}})
	})
	p := newAzureAgentProvider(t, srv)
	deps, err := p.fetchProjectDeployments(context.Background())
	if err != nil {
		t.Fatalf("fetchProjectDeployments: %v", err)
	}
	if len(deps) != 1 || deps[0].name != "gpt-4o" {
		t.Errorf("deployments = %+v, want only the resource deployment", deps)
	}
}

func agentsListingServer(t *testing.T, agents []map[string]any, onResponses func(r *http.Request)) *azureTestServer {
	t.Helper()
	return newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
		switch r.URL.Path {
		case "/api/projects/p1/agents":
			writeJSON(w, 200, map[string]any{"data": agents, "has_more": false})
		case "/api/projects/p1/openai/v1/responses":
			onResponses(r)
			writeJSON(w, 200, map[string]any{"id": "r", "status": "completed", "model": "gpt-4.1", "output": json.RawMessage(`[` + answerItem + `]`)})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
}

func latest(kind, status string) map[string]any {
	l := map[string]any{"definition": map[string]any{"kind": kind}}
	if status != "" {
		l["status"] = status
	}
	return map[string]any{"latest": l}
}

// Items 3 and 4: workflow agents stay listed, marked, and are invoked with
// their preview opt-in header; disabled agents and versions that are not
// active are hidden.
func TestAzureFoundry_AgentListingWorkflowAndState(t *testing.T) {
	agents := []map[string]any{
		{"name": "Helper", "state": "enabled", "versions": latest("prompt", "active")},
		{"name": "flowy", "state": "enabled", "versions": latest("workflow", "")},
		{"name": "legacy", "definition": map[string]any{"kind": "prompt"}},
		{"name": "off", "state": "disabled", "versions": latest("prompt", "active")},
		{"name": "fresh", "state": "enabled", "versions": latest("prompt", "creating")},
		{"name": "broken", "state": "enabled", "versions": latest("prompt", "failed")},
	}
	headers := map[string]string{}
	srv := agentsListingServer(t, agents, func(r *http.Request) {})
	p := newAzureAgentProvider(t, srv)
	listed, err := p.fetchAgents(context.Background())
	if err != nil {
		t.Fatalf("fetchAgents: %v", err)
	}
	display := map[string]string{}
	for _, m := range p.agentModels(listed, nil) {
		display[m.ID] = m.DisplayName
	}
	if len(display) != 3 || display["agent:flowy"] != "Agent · flowy (workflow)" || display["agent:Helper"] != "Agent · Helper" || display["agent:legacy"] == "" {
		t.Errorf("listed agents = %v", display)
	}
	for _, name := range []string{"flowy", "Helper"} {
		if _, err := p.Chat(context.Background(), Request{Model: "agent:" + name, Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
			t.Fatalf("Chat %s: %v", name, err)
		}
	}
	for _, rq := range srv.requests() {
		if strings.HasSuffix(rq.Path, "/responses") {
			ref, _ := rq.Body["agent_reference"].(map[string]any)
			name, _ := ref["name"].(string)
			headers[name] = rq.Headers.Get("Foundry-Features")
		}
	}
	if headers["flowy"] != "WorkflowAgents=V1Preview" || headers["Helper"] != "" {
		t.Errorf("Foundry-Features headers = %v", headers)
	}
}

func streamOf(events ...string) string { return sseEvents(events...) }

const completedEvent = `{"type":"response.completed","response":{"status":"completed","model":"gpt-4.1","usage":{"input_tokens":7,"output_tokens":2}}}`

// Item 5: items the client must execute, or an OAuth consent, end the turn
// with an explanation (usage attached) instead of an empty answer.
func TestParseResponsesSSE_RunBlockers(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []string
		want  []string
	}{
		{"custom tool", []string{`{"id":"ct_1","type":"custom_tool_call","call_id":"c1","name":"grep","input":"x"}`}, []string{"custom_tool_call (grep)", "remove that tool"}},
		{"shell", []string{`{"id":"sh_1","type":"shell_call","call_id":"c2","action":{}}`}, []string{"shell_call"}},
		{"local shell", []string{`{"id":"ls_1","type":"local_shell_call","call_id":"c3","action":{}}`}, []string{"local_shell_call"}},
		{"apply patch", []string{`{"id":"ap_1","type":"apply_patch_call","call_id":"c4","operation":{}}`}, []string{"apply_patch_call"}},
		{"computer without output", []string{`{"id":"cu_1","type":"computer_call","call_id":"c5","action":{"type":"screenshot"}}`}, []string{"computer_call"}},
		{"client tool search", []string{`{"id":"ts_1","type":"tool_search_call","call_id":"c6","execution":"client","arguments":{}}`}, []string{"tool_search_call"}},
		{"oauth consent", []string{`{"id":"oc_1","type":"oauth_consent_request","server_label":"github","consent_link":"https://consent.example.test/ok?x=1"}`}, []string{`"github"`, "https://consent.example.test/ok?x=1", "consent"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var events []string
			for i, it := range tc.items {
				events = append(events, itemPair(i, it)...)
			}
			events = append(events, completedEvent)
			s := summarize(parseChunks(t, streamOf(events...)))
			if s.done != nil || len(s.errs) != 1 {
				t.Fatalf("done=%+v errs=%+v", s.done, s.errs)
			}
			e := s.errs[0]
			for _, w := range tc.want {
				if !strings.Contains(e.Error.Error(), w) {
					t.Errorf("error %q does not mention %s", e.Error, w)
				}
			}
			if e.TokensIn != 7 || e.TokensOut != 2 {
				t.Errorf("usage must ride on the error chunk: %+v", e)
			}
		})
	}
	// A computer call the service executed (its output is in the run) and a
	// server-side tool search are hosted steps, not blockers.
	events := itemPair(0, `{"id":"cu_1","type":"computer_call","call_id":"c5","action":{"type":"screenshot"},"status":"completed"}`)
	events = append(events, itemPair(1, `{"id":"cuo_1","type":"computer_call_output","call_id":"c5","output":{"type":"computer_screenshot","image_url":"data:"}}`)...)
	events = append(events, itemPair(2, `{"id":"ts_1","type":"tool_search_call","call_id":"c6","execution":"server","arguments":{},"status":"completed"}`)...)
	events = append(events, completedEvent)
	s := summarize(parseChunks(t, streamOf(events...)))
	if s.done == nil || len(s.errs) != 0 {
		t.Fatalf("hosted computer use / tool search must finish normally: errs=%+v", s.errs)
	}
}

// Item 5: the non-streaming turn reports the same blockers.
func TestAzureFoundry_AgentChatOAuthConsent(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 200, map[string]any{"id": "r", "status": "completed", "model": "gpt-4.1",
			"output": json.RawMessage(`[{"id":"oc","type":"oauth_consent_request","server_label":"jira","consent_link":"https://login.example.test/c"}]`),
			"usage":  map[string]any{"input_tokens": 4, "output_tokens": 1}})
	})
	p := newAzureAgentProvider(t, srv)
	_, err := p.Chat(context.Background(), Request{Model: "agent:Helper", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "jira") || !strings.Contains(err.Error(), "https://login.example.test/c") {
		t.Fatalf("Chat error = %v", err)
	}
	if in, out, _, ok := usageOf(err); !ok || in != 4 || out != 1 {
		t.Errorf("usage must ride on the error: %d/%d/%v", in, out, ok)
	}
}

// Item 5: trail labels for Foundry's run steps.
func TestHostedToolLabel_F2Types(t *testing.T) {
	for _, tc := range []struct {
		item responsesOutputItem
		want string
	}{
		{responsesOutputItem{Type: "workflow_action", Kind: "InvokeAzureAgent"}, "Agent · workflow step: InvokeAzureAgent"},
		{responsesOutputItem{Type: "memory_command_preview_call"}, "Agent · memory update"},
		{responsesOutputItem{Type: "structured_outputs"}, "Agent · structured output"},
		{responsesOutputItem{Type: "program"}, "Agent · program"},
		{responsesOutputItem{Type: "tool_search_call"}, "Agent · tool search"},
	} {
		if got := hostedToolLabel(tc.item); got != tc.want {
			t.Errorf("%s: label = %q, want %q", tc.item.Type, got, tc.want)
		}
	}
	if !hostedToolOK(responsesOutputItem{Type: "workflow_action", Status: "completed"}) || hostedToolOK(responsesOutputItem{Type: "workflow_action", Status: "cancelled"}) {
		t.Error("a cancelled workflow action is not OK")
	}
}

// Item 6: a Foundry tool's *_call_output completes its call's trail entry
// (summary and OK from the output); code interpreter logs and image results
// are read.
func TestParseResponsesSSE_PairsCallOutputs(t *testing.T) {
	events := itemPair(0, `{"type":"bing_grounding_call","call_id":"b1","arguments":"{}","status":"completed"}`)
	events = append(events, itemPair(1, `{"type":"bing_grounding_call_output","call_id":"b1","output":"Found 3 pages","status":"completed"}`)...)
	events = append(events, itemPair(2, `{"type":"openapi_call","call_id":"o1","name":"tickets","arguments":"{}","status":"completed"}`)...)
	events = append(events, itemPair(3, `{"type":"openapi_call_output","call_id":"o1","output":"boom","status":"failed"}`)...)
	events = append(events, itemPair(4, `{"id":"ci_1","type":"code_interpreter_call","status":"completed","code":"print(2)","outputs":[{"type":"logs","logs":"2\n"}]}`)...)
	events = append(events, itemPair(5, `{"id":"ig_1","type":"image_generation_call","status":"completed","result":"iVBORw0KGgo="}`)...)
	events = append(events, itemPair(6, `{"id":"ig_2","type":"image_generation_call","status":"completed","result":null}`)...)
	events = append(events, itemPair(7, `{"type":"azure_function_call","call_id":"f1","name":"fn","arguments":"{}","status":"completed"}`)...)
	events = append(events, completedEvent)
	s := summarize(parseChunks(t, streamOf(events...)))
	done := map[string]ToolEvent{}
	for _, e := range s.events {
		if e.Done {
			done[e.Label] = e
		}
	}
	if e := done["Agent · Bing search"]; !e.OK || e.Summary != "Found 3 pages" {
		t.Errorf("bing step = %+v", e)
	}
	if e := done["Agent · OpenAPI tool: tickets"]; e.OK || e.Summary != "boom" {
		t.Errorf("failed openapi output must mark the step failed: %+v", e)
	}
	if e := done["Agent · code interpreter"]; !e.OK || e.Summary != "2" {
		t.Errorf("code interpreter step = %+v", e)
	}
	var images []ToolEvent
	for _, e := range s.events {
		if e.Done && e.Name == "image_generation_call" {
			images = append(images, e)
		}
	}
	if len(images) != 2 || !images[0].OK || images[0].Summary != "image generated" || images[1].OK {
		t.Errorf("image steps = %+v", images)
	}
	// A call whose output never came still completes at the terminal event.
	if e, ok := done["Agent · Azure Function: fn"]; !ok || !e.OK {
		t.Errorf("unpaired call must complete at the end: %+v (%v)", e, ok)
	}
	if n := len(s.events); n != 12 {
		t.Errorf("trail events = %d, want a start and one completion per step", n)
	}
}

// Item 7: pending calls are keyed by item id with a call_id alias, so the
// argument deltas (which name the item id) accumulate even when the done
// event carries no arguments.
func TestParseResponsesSSE_ArgumentDeltasMatchByItemID(t *testing.T) {
	body := streamOf(
		itemEvent("response.output_item.added", 0, `{"id":"fc_1","type":"function_call","call_id":"call_9","name":"search_flow","arguments":""}`),
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"q\":"}`,
		`{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"\"x\"}"}`,
		itemEvent("response.output_item.done", 0, `{"id":"fc_1","type":"function_call","call_id":"call_9","name":"search_flow","status":"completed"}`),
		completedEvent,
	)
	s := summarize(parseChunks(t, body))
	if s.done == nil || len(s.done.ToolCalls) != 1 {
		t.Fatalf("done = %+v", s.done)
	}
	if tc := s.done.ToolCalls[0]; tc.ID != "call_9" || string(tc.Input) != `{"q":"x"}` {
		t.Errorf("tool call = %+v", tc)
	}
}

// Item 8: the stream `error` event's forbidden / user_error types.
func TestParseResponsesSSE_ErrorEventTypes(t *testing.T) {
	errOf := func(typ, msg string) error {
		s := summarize(parseChunks(t, streamOf(`{"type":"error","error":{"type":"`+typ+`","code":null,"message":"`+msg+`"}}`)))
		if len(s.errs) != 1 {
			t.Fatalf("errs = %+v", s.errs)
		}
		return s.errs[0].Error
	}
	if err := errOf("forbidden", "Access denied by network rules"); !errors.Is(err, ErrProviderPermissionDenied) || !strings.Contains(err.Error(), "network rules") {
		t.Errorf("forbidden = %v", err)
	}
	err := errOf("user_error", "Invalid tool definition")
	if isRetryable(err) || errors.Is(err, ErrApiKeyInvalid) || !strings.Contains(err.Error(), "bad request") || !strings.Contains(err.Error(), "Invalid tool definition") {
		t.Errorf("user_error = %v", err)
	}
	if err := errOf("user_error", "This model's maximum context length is 8192 tokens"); !errors.Is(err, ErrContextLimit) {
		t.Errorf("user_error context length = %v", err)
	}
	if err := errOf("too_many_requests", "busy"); !errors.Is(err, ErrRateLimited) {
		t.Errorf("too_many_requests = %v", err)
	}
}

// Item 9: response.model is the agent's deployment; the cached project
// listing resolves the model behind it for pricing and the memo.
func TestAzureFoundry_AgentServedModelResolvedThroughDeployments(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(streamOf(
				`{"type":"response.created","response":{"status":"in_progress","model":"prod-chat"}}`,
				`{"type":"response.output_text.delta","item_id":"msg_1","delta":"Hi"}`,
				`{"type":"response.completed","response":{"status":"completed","model":"prod-chat","usage":{"input_tokens":1,"output_tokens":1}}}`)))
			return
		}
		writeJSON(w, 200, map[string]any{"id": "r", "status": "completed", "model": "prod-chat", "output": json.RawMessage(`[` + answerItem + `]`)})
	})
	p := newAzureAgentProvider(t, srv)
	storeAzureModels(azureModelsCacheKey(p.cfg), []ModelInfo{{ID: "prod-chat", ServedModel: "gpt-4.1"}, {ID: "agent:Helper"}}, time.Minute)
	req := Request{Model: "agent:Helper", Messages: []Message{{Role: "user", Content: "hi"}}}
	resp, err := p.Chat(context.Background(), req)
	if err != nil || resp.ServedModel != "gpt-4.1" {
		t.Fatalf("Chat served model = %q (%v)", resp.ServedModel, err)
	}
	var served []string
	if err := p.Stream(context.Background(), req, func(c Chunk) {
		if c.ServedModel != "" {
			served = append(served, c.ServedModel)
		}
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	for _, s := range served {
		if s != "gpt-4.1" {
			t.Errorf("stream chunk served model = %q, want gpt-4.1", s)
		}
	}
	if len(served) == 0 {
		t.Error("no chunk carried the served model")
	}
	// Without a listing entry the reported model passes through.
	ResetAzureFoundryCaches()
	if resp, _ := p.Chat(context.Background(), req); resp == nil || resp.ServedModel != "prod-chat" {
		t.Errorf("uncached served model = %+v", resp)
	}
}
