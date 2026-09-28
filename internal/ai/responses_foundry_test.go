package ai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// sseEvents renders Responses streaming events (JSON objects with a "type")
// as an SSE body.
func sseEvents(events ...string) string {
	var b strings.Builder
	for _, e := range events {
		var head struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal([]byte(e), &head)
		fmt.Fprintf(&b, "event: %s\ndata: %s\n\n", head.Type, e)
	}
	return b.String()
}

func itemEvent(typ string, index int, item string) string {
	return fmt.Sprintf(`{"type":%q,"output_index":%d,"item":%s}`, typ, index, item)
}

// itemPair is the output_item.added + output_item.done pair for one item.
func itemPair(index int, item string) []string {
	return []string{itemEvent("response.output_item.added", index, item), itemEvent("response.output_item.done", index, item)}
}

func parseChunks(t *testing.T, body string) []Chunk {
	t.Helper()
	var got []Chunk
	if err := parseResponsesSSE(strings.NewReader(body), func(c Chunk) { got = append(got, c) }); err != nil {
		t.Fatalf("parseResponsesSSE: %v", err)
	}
	return got
}

type chunkSummary struct {
	events      []ToolEvent
	done        *Chunk
	errs        []Chunk
	serverSteps int
	text        string
}

func summarize(chunks []Chunk) chunkSummary {
	var s chunkSummary
	for i := range chunks {
		c := chunks[i]
		switch {
		case c.Error != nil:
			s.errs = append(s.errs, c)
		case c.ToolEvent != nil:
			s.events = append(s.events, *c.ToolEvent)
		case c.Done:
			s.done = &chunks[i]
		case c.ServerStep:
			s.serverSteps++
		default:
			s.text += c.Text
		}
	}
	return s
}

// Foundry tool items as the v1 spec shapes them: MCP calls report
// "error": null on success and an object on failure; Foundry tools'
// *_call_output items carry object or array outputs.
const (
	mcpOKItem      = `{"id":"mcp_1","type":"mcp_call","server_label":"dice","name":"roll","arguments":"{}","output":"4","error":null,"status":"completed"}`
	mcpErrItem     = `{"id":"mcp_2","type":"mcp_call","server_label":"dice","name":"roll","arguments":"{}","output":null,"error":{"type":"mcp_protocol_error","code":32600,"message":"Session terminated"},"status":"failed"}`
	mcpListErrItem = `{"id":"mcpl_1","type":"mcp_list_tools","server_label":"dice","tools":[],"error":{"type":"http_error","code":500,"message":"upstream down"}}`
	searchOutItem  = `{"type":"azure_ai_search_call_output","call_id":"c1","output":{"summary":"Retrieved 2 documents.","documents":[{"id":"d1"}]}}`
	openapiOutItem = `{"type":"openapi_call_output","call_id":"c2","output":[{"status":200},{"body":"ok"}]}`
	answerItem     = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Answer"}]}`
	flowCallItem   = `{"id":"fc_1","type":"function_call","call_id":"call_1","name":"search_flow","arguments":"{\"q\":\"x\"}","status":"completed"}`
)

var foundryOutputItems = []string{mcpOKItem, mcpErrItem, mcpListErrItem, searchOutItem, openapiOutItem, answerItem, flowCallItem}

// TestParseResponsesSSE_ObjectValuedToolItems: object / array outputs and
// object errors must neither drop the items' own events nor sink the terminal
// event (whose response.output repeats every item).
func TestParseResponsesSSE_ObjectValuedToolItems(t *testing.T) {
	var events []string
	for i, item := range foundryOutputItems {
		if item == answerItem {
			events = append(events, `{"type":"response.output_text.delta","item_id":"msg_1","delta":"Answer"}`)
			continue
		}
		events = append(events, itemPair(i, item)...)
	}
	events = append(events, `{"type":"response.completed","response":{"id":"resp_1","status":"completed","model":"gpt-4.1-2025-04-14",`+
		`"usage":{"input_tokens":21,"output_tokens":8},"output":[`+strings.Join(foundryOutputItems, ",")+`]}}`)

	s := summarize(parseChunks(t, sseEvents(events...)))
	if len(s.errs) > 0 {
		t.Fatalf("unexpected error chunks: %+v", s.errs)
	}
	if s.done == nil {
		t.Fatal("no Done chunk: the terminal event was dropped")
	}
	if s.done.TokensIn != 21 || s.done.TokensOut != 8 || s.done.ServedModel != "gpt-4.1-2025-04-14" {
		t.Errorf("done = %+v", s.done)
	}
	if len(s.done.ToolCalls) != 1 || s.done.ToolCalls[0].ID != "call_1" || string(s.done.ToolCalls[0].Input) != `{"q":"x"}` {
		t.Errorf("tool calls = %+v", s.done.ToolCalls)
	}
	if s.text != "Answer" {
		t.Errorf("text = %q", s.text)
	}
	var finished []ToolEvent
	for _, ev := range s.events {
		if ev.Done {
			finished = append(finished, ev)
		}
	}
	if len(finished) != 3 {
		t.Fatalf("finished hosted steps = %+v", finished)
	}
	if ev := finished[0]; ev.Label != "Agent · MCP tool: roll" || !ev.OK || ev.Summary != "4" {
		t.Errorf("successful MCP call (error: null) = %+v", ev)
	}
	if ev := finished[1]; ev.OK || ev.Summary != "Session terminated" {
		t.Errorf("failed MCP call (object error) = %+v", ev)
	}
	if ev := finished[2]; ev.Name != "mcp_list_tools" || ev.OK || ev.Summary != "upstream down" {
		t.Errorf("failed MCP listing (object error) = %+v", ev)
	}
	// The *_call_output items are server-side but have no trail label.
	if s.serverSteps == 0 {
		t.Error("unlabelled *_call_output items emitted no ServerStep marker")
	}
}

// TestAzureFoundry_AgentChatToleratesObjectOutputs is the non-streaming
// counterpart (the connection test path).
func TestAzureFoundry_AgentChatToleratesObjectOutputs(t *testing.T) {
	output := "[" + strings.Join(foundryOutputItems, ",") + `,{"type":"future_item","content":"not an array"}]`
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 200, map[string]any{
			"id": "resp_3", "status": "completed", "model": "gpt-4.1-2025-04-14",
			"output": json.RawMessage(output),
			"usage":  map[string]any{"input_tokens": 5, "output_tokens": 3},
		})
	})
	p := newAzureAgentProvider(t, srv)
	resp, err := p.Chat(context.Background(), Request{Model: "agent:Helper", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "Answer" || len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "call_1" || resp.TokensIn != 5 || resp.ServedModel != "gpt-4.1-2025-04-14" {
		t.Errorf("response = %+v", resp)
	}
}

const (
	githubToolsItem   = `{"id":"mcpl_1","type":"mcp_list_tools","server_label":"github","tools":[{"name":"create_issue","input_schema":{}}]}`
	githubApprovalReq = `{"id":"mcpr_1","type":"mcp_approval_request","server_label":"github","name":"create_issue","arguments":"{\"title\":\"x\"}"}`
)

// TestParseResponsesSSE_MCPApprovalRequestExplains: a run that stops for MCP
// tool approval (Foundry's default) must end with an explanation, not with a
// successful empty answer.
func TestParseResponsesSSE_MCPApprovalRequestExplains(t *testing.T) {
	events := itemPair(0, githubToolsItem)
	events = append(events, itemPair(1, githubApprovalReq)...)
	events = append(events, itemPair(2, flowCallItem)...)
	events = append(events, `{"type":"response.completed","response":{"status":"completed","model":"gpt-4.1","usage":{"input_tokens":11,"output_tokens":3}}}`)

	s := summarize(parseChunks(t, sseEvents(events...)))
	if s.done != nil {
		t.Fatalf("a run waiting for MCP approval finished as a normal turn: %+v", s.done)
	}
	if len(s.errs) != 1 {
		t.Fatalf("error chunks = %+v", s.errs)
	}
	e := s.errs[0]
	msg := e.Error.Error()
	for _, want := range []string{"github/create_issue", "require_approval", `"never"`} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q does not mention %s", msg, want)
		}
	}
	if errors.Is(e.Error, ErrProviderConfigInvalid) || errors.Is(e.Error, ErrProviderDown) || errors.Is(e.Error, ErrRateLimited) {
		t.Errorf("the explanation must be a plain error the chat service passes through: %v", e.Error)
	}
	if e.TokensIn != 11 || e.TokensOut != 3 || e.ServedModel != "gpt-4.1" {
		t.Errorf("usage must ride on the error chunk: %+v", e)
	}
	if s.serverSteps == 0 {
		t.Error("the approval request emitted no ServerStep marker")
	}
}

func TestAzureFoundry_AgentChatMCPApprovalRequestExplains(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 200, map[string]any{
			"id": "resp_4", "status": "completed", "model": "gpt-4.1",
			"output": json.RawMessage("[" + githubToolsItem + "," + githubApprovalReq + "]"),
		})
	})
	p := newAzureAgentProvider(t, srv)
	_, err := p.Chat(context.Background(), Request{Model: "agent:Helper", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err == nil || !strings.Contains(err.Error(), "require_approval") || !strings.Contains(err.Error(), "github/create_issue") {
		t.Fatalf("Chat error = %v", err)
	}
}

// TestParseResponsesSSE_UnlabelledServerStep: a server-side item without a
// trail label still marks the stream as delivered (Chunk.ServerStep).
func TestParseResponsesSSE_UnlabelledServerStep(t *testing.T) {
	events := itemPair(0, `{"type":"future_tool_call","call_id":"c9","status":"completed"}`)
	events = append(events, `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"slow down"}}}`)
	s := summarize(parseChunks(t, sseEvents(events...)))
	if len(s.events) != 0 {
		t.Errorf("unlabelled item produced trail events: %+v", s.events)
	}
	if s.serverSteps == 0 {
		t.Error("no ServerStep marker for the unlabelled server-side item")
	}
	if len(s.errs) != 1 || !errors.Is(s.errs[0].Error, ErrRateLimited) {
		t.Errorf("error chunks = %+v", s.errs)
	}
}

// TestAzureFoundry_AgentStreamNotReplayedAfterServerStep: the retry wrapper
// must not re-POST an agent run whose server-side step already executed,
// whether or not the step has a trail label.
func TestAzureFoundry_AgentStreamNotReplayedAfterServerStep(t *testing.T) {
	for _, tc := range []struct{ name, item string }{
		{"labelled step", `{"type":"openapi_call","call_id":"c1","name":"tickets","arguments":"{}","status":"completed"}`},
		{"unlabelled step", `{"type":"future_tool_call","call_id":"c2","status":"completed"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events := itemPair(0, tc.item)
			events = append(events, `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"slow down"}}}`)
			body := sseEvents(events...)
			srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(body))
			})
			rp := NewRetryingProviderWithConfig(newAzureAgentProvider(t, srv), 3, time.Millisecond)
			var errs []error
			err := rp.Stream(context.Background(), Request{Model: "agent:Helper", Messages: []Message{{Role: "user", Content: "open a ticket"}}}, func(c Chunk) {
				if c.Error != nil {
					errs = append(errs, c.Error)
				}
			})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if got := len(srv.requests()); got != 1 {
				t.Errorf("agent run POSTed %d times; a server-side step must block the replay", got)
			}
			if len(errs) != 1 || !errors.Is(errs[0], ErrRateLimited) {
				t.Errorf("forwarded errors = %v", errs)
			}
		})
	}
}

// TestParseResponsesSSE_LargeImageGenerationLine: a generated image travels
// base64-encoded in output_item.done and again in response.completed; a line
// over the former 4 MB cap must not fail the turn.
func TestParseResponsesSSE_LargeImageGenerationLine(t *testing.T) {
	item := `{"id":"ig_1","type":"image_generation_call","status":"completed","result":"` + strings.Repeat("A", 5<<20) + `"}`
	body := sseEvents(
		itemEvent("response.output_item.added", 0, `{"id":"ig_1","type":"image_generation_call","status":"in_progress"}`),
		itemEvent("response.output_item.done", 0, item),
		`{"type":"response.completed","response":{"status":"completed","model":"gpt-4.1","usage":{"input_tokens":9,"output_tokens":4},"output":[`+item+`]}}`,
	)
	s := summarize(parseChunks(t, body))
	if s.done == nil || s.done.TokensIn != 9 || s.done.TokensOut != 4 || s.done.ServedModel != "gpt-4.1" {
		t.Fatalf("done = %+v", s.done)
	}
	if len(s.events) != 2 || !s.events[1].Done || s.events[1].Name != "image_generation_call" || !s.events[1].OK {
		t.Errorf("image generation trail events = %+v", s.events)
	}
}

// TestParseResponsesSSE_FoundryToolLabels: Foundry's own tool items (call_id,
// no id) reach the trail; their *_call_output items do not.
func TestParseResponsesSSE_FoundryToolLabels(t *testing.T) {
	search := `{"type":"azure_ai_search_call","call_id":"c1","status":"completed"}`
	a2a := `{"type":"a2a_preview_call","call_id":"c2","name":"Helper","status":"completed"}`
	events := []string{
		itemEvent("response.output_item.added", 0, search),
		itemEvent("response.output_item.added", 1, a2a),
		itemEvent("response.output_item.done", 0, search),
	}
	events = append(events, itemPair(2, searchOutItem)...)
	events = append(events, itemEvent("response.output_item.done", 1, a2a),
		`{"type":"response.completed","response":{"status":"completed"}}`)

	s := summarize(parseChunks(t, sseEvents(events...)))
	want := []struct {
		label string
		done  bool
	}{
		{"Agent · Azure AI Search", false},
		{"Agent · connected agent: Helper", false},
		{"Agent · Azure AI Search", true},
		{"Agent · connected agent: Helper", true},
	}
	if len(s.events) != len(want) {
		t.Fatalf("trail events = %+v", s.events)
	}
	for i, w := range want {
		if s.events[i].Label != w.label || s.events[i].Done != w.done || (w.done && !s.events[i].OK) {
			t.Errorf("event %d = %+v, want %+v", i, s.events[i], w)
		}
	}
	if s.done == nil {
		t.Error("no Done chunk")
	}
}

// TestParseResponsesSSE_UndecodableItemEmitsKeepalive: an undecodable
// output_item event is exactly the unknown-shape silent event the keepalive
// exists for — it used to `continue` straight past the end-of-loop keepalive,
// so a silent phase made only of such events fed the idle watchdog nothing.
func TestParseResponsesSSE_UndecodableItemEmitsKeepalive(t *testing.T) {
	body := sseEvents(
		`{"type":"response.output_item.added","output_index":0,"item":42}`,
		`{"type":"response.output_item.done","output_index":0,"item":false}`,
		`{"type":"response.output_text.delta","item_id":"msg_1","delta":"late answer"}`,
		`{"type":"response.completed","response":{"status":"completed","model":"gpt-4.1"}}`,
	)
	var chunks []Chunk
	if err := parseResponsesSSE(strings.NewReader(body), func(c Chunk) { chunks = append(chunks, c) }); err != nil {
		t.Fatalf("parseResponsesSSE: %v", err)
	}
	if len(chunks) != 4 {
		t.Fatalf("chunks = %+v, want a keepalive per undecodable item, the text and Done", chunks)
	}
	for i := 0; i < 2; i++ {
		if !isKeepalive(chunks[i]) {
			t.Errorf("undecodable item %d must emit one keepalive, got %+v", i, chunks[i])
		}
	}
	if chunks[2].Text != "late answer" || !chunks[3].Done {
		t.Errorf("later chunks = %+v", chunks[2:])
	}
}

// TestParseResponsesSSE_AsymmetricCallKeyingSingleCall: when the added event
// carries only call_id but the done event carries id AND call_id, the two
// events must still pair (both canonicalise to call_id); keying the done
// event id-first used to create a second pending call and double-emit it.
func TestParseResponsesSSE_AsymmetricCallKeyingSingleCall(t *testing.T) {
	added := `{"type":"function_call","call_id":"call_x","name":"search_flow","arguments":""}`
	done := `{"id":"fc_9","type":"function_call","call_id":"call_x","name":"search_flow","arguments":"{\"q\":\"z\"}","status":"completed"}`
	events := []string{
		itemEvent("response.output_item.added", 0, added),
		itemEvent("response.output_item.done", 0, done),
		`{"type":"response.completed","response":{"status":"completed"}}`,
	}
	s := summarize(parseChunks(t, sseEvents(events...)))
	if s.done == nil {
		t.Fatal("no Done chunk")
	}
	if got := s.done.ToolCalls; len(got) != 1 || got[0].ID != "call_x" || string(got[0].Input) != `{"q":"z"}` {
		t.Errorf("tool calls = %+v, want exactly the one call keyed by call_id", got)
	}
}

// TestParseResponsesSSE_ContentFilterCarriesUsage: the tokens a filtered run
// consumed ride on the error chunk (as with MCP approvals) so they are still
// billed instead of escaping the budget.
func TestParseResponsesSSE_ContentFilterCarriesUsage(t *testing.T) {
	body := sseEvents(`{"type":"response.incomplete","response":{"status":"incomplete","model":"gpt-4.1","usage":{"input_tokens":31,"output_tokens":4},"incomplete_details":{"reason":"content_filter"}}}`)
	var got []Chunk
	if err := parseResponsesSSE(strings.NewReader(body), func(c Chunk) { got = append(got, c) }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !errors.Is(got[0].Error, ErrContentFiltered) ||
		got[0].TokensIn != 31 || got[0].TokensOut != 4 || got[0].ServedModel != "gpt-4.1" {
		t.Errorf("content_filter chunk = %+v", got)
	}
}

// D-33: a response.failed event that carries usage and the model bills the
// tokens the failed run spent by Azure's numbers, not a tokenizer estimate.
func TestParseResponsesSSE_FailedCarriesUsage(t *testing.T) {
	body := sseEvents(`{"type":"response.failed","response":{"status":"failed","model":"gpt-4.1","usage":{"input_tokens":31,"output_tokens":4},"error":{"code":"server_error","message":"boom"}}}`)
	got := parseChunks(t, body)
	if len(got) != 1 || !errors.Is(got[0].Error, ErrProviderDown) ||
		got[0].TokensIn != 31 || got[0].TokensOut != 4 || got[0].ServedModel != "gpt-4.1" {
		t.Errorf("response.failed chunk = %+v", got)
	}
	// Without an error object the run still fails, usage or not.
	bare := parseChunks(t, sseEvents(`{"type":"response.failed","response":{"status":"failed"}}`))
	if len(bare) != 1 || bare[0].Error == nil || bare[0].TokensIn != 0 {
		t.Errorf("bare response.failed chunk = %+v", bare)
	}
}

// D-35: answer text delivered whole in a message's done event, and refusal
// content (a refusal is the model's answer, not silence), reach the user
// exactly once; text that streamed as deltas is not repeated.
func TestParseResponsesSSE_WholeMessageAndRefusal(t *testing.T) {
	completed := `{"type":"response.completed","response":{"status":"completed","model":"gpt-4.1"}}`
	t.Run("message delivered whole", func(t *testing.T) {
		s := summarize(parseChunks(t, sseEvents(itemEvent("response.output_item.done", 0, answerItem), completed)))
		if s.text != "Answer" || s.done == nil {
			t.Errorf("text = %q done = %v", s.text, s.done)
		}
	})
	t.Run("streamed text is not repeated", func(t *testing.T) {
		s := summarize(parseChunks(t, sseEvents(
			`{"type":"response.output_text.delta","item_id":"msg_1","delta":"Ans"}`,
			`{"type":"response.output_text.delta","item_id":"msg_1","delta":"wer"}`,
			itemEvent("response.output_item.done", 0, answerItem), completed)))
		if s.text != "Answer" {
			t.Errorf("text = %q, want the answer exactly once", s.text)
		}
	})
	t.Run("id-less deltas never repeat", func(t *testing.T) {
		s := summarize(parseChunks(t, sseEvents(
			`{"type":"response.output_text.delta","delta":"Answer"}`,
			itemEvent("response.output_item.done", 0, answerItem), completed)))
		if s.text != "Answer" {
			t.Errorf("text = %q", s.text)
		}
	})
	t.Run("a second message that never streamed is added", func(t *testing.T) {
		second := `{"id":"msg_2","type":"message","role":"assistant","content":[{"type":"output_text","text":" More"}]}`
		s := summarize(parseChunks(t, sseEvents(
			`{"type":"response.output_text.delta","item_id":"msg_1","delta":"Answer"}`,
			itemEvent("response.output_item.done", 0, answerItem),
			itemEvent("response.output_item.done", 1, second), completed)))
		if s.text != "Answer More" {
			t.Errorf("text = %q", s.text)
		}
	})
	t.Run("refusal delta", func(t *testing.T) {
		s := summarize(parseChunks(t, sseEvents(`{"type":"response.refusal.delta","item_id":"msg_1","delta":"no"}`, completed)))
		if s.text != "no" {
			t.Errorf("text = %q", s.text)
		}
	})
	t.Run("refusal content in the done event", func(t *testing.T) {
		refusal := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"refusal","refusal":"I can't help with that"}]}`
		s := summarize(parseChunks(t, sseEvents(itemEvent("response.output_item.done", 0, refusal), completed)))
		if s.text != "I can't help with that" {
			t.Errorf("text = %q", s.text)
		}
	})
}

// D-35 (non-streaming): the connection test's agent turn reads refusal
// content as the answer.
func TestAzureFoundry_AgentChatReadsRefusal(t *testing.T) {
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, body map[string]any) {
		writeJSON(w, 200, map[string]any{
			"id": "resp_5", "status": "completed", "model": "gpt-4.1",
			"output": []map[string]any{{"type": "message", "role": "assistant",
				"content": []map[string]any{{"type": "refusal", "refusal": "I can't help with that"}}}},
		})
	})
	p := newAzureAgentProvider(t, srv)
	resp, err := p.Chat(context.Background(), Request{Model: "agent:Helper", Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "I can't help with that" {
		t.Errorf("content = %q, want the refusal", resp.Content)
	}
}

// D-40: the served model is seeded from the first event that carries the
// response object (response.created) and rides on every text delta and on
// error chunks, so a run cut before response.completed is still priced by
// the model that answered instead of the fallback ceiling.
func TestParseResponsesSSE_ServedModelRidesOnDeltasAndErrors(t *testing.T) {
	created := `{"type":"response.created","response":{"id":"resp_1","status":"in_progress","model":"gpt-4.1"}}`
	delta := `{"type":"response.output_text.delta","item_id":"msg_1","delta":"hi"}`
	var got []Chunk
	err := parseResponsesSSE(strings.NewReader(sseEvents(created, delta)), func(c Chunk) { got = append(got, c) })
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated stream error = %v", err)
	}
	if len(got) != 2 || !isKeepalive(got[0]) || got[1].Text != "hi" || got[1].ServedModel != "gpt-4.1" {
		t.Errorf("chunks = %+v, want a keepalive and a text chunk carrying the model", got)
	}
	got = parseChunks(t, sseEvents(created, `{"type":"error","code":"server_error","message":"boom"}`))
	if len(got) != 2 || got[1].Error == nil || got[1].ServedModel != "gpt-4.1" {
		t.Errorf("error chunks = %+v, want the model on the error chunk", got)
	}
	// The terminal event's own model still wins when it names one.
	got = parseChunks(t, sseEvents(created, `{"type":"response.completed","response":{"status":"completed","model":"gpt-4.1-2025-04-14"}}`))
	if last := got[len(got)-1]; !last.Done || last.ServedModel != "gpt-4.1-2025-04-14" {
		t.Errorf("done = %+v", last)
	}
	// A terminal event without a model keeps the seeded one.
	got = parseChunks(t, sseEvents(created, `{"type":"response.completed","response":{"status":"completed"}}`))
	if last := got[len(got)-1]; !last.Done || last.ServedModel != "gpt-4.1" {
		t.Errorf("done without a model = %+v, want the seeded model", last)
	}
}

// PERF-3 (parser half): every decoded event that produces no chunk —
// argument deltas, reasoning deltas, *.done notices — feeds the idle
// watchdog with one keepalive, so a long silent phase is never cancelled as a
// stalled stream.
func TestParseResponsesSSE_EveryEventFeedsTheWatchdog(t *testing.T) {
	events := []string{itemEvent("response.output_item.added", 0, `{"id":"fc_1","type":"function_call","call_id":"call_1","name":"propose_fix","arguments":""}`)}
	for i := 0; i < 60; i++ {
		events = append(events, `{"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"x"}`)
	}
	for i := 0; i < 40; i++ {
		events = append(events, `{"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"."}`)
	}
	events = append(events,
		`{"type":"response.reasoning_summary_text.done","item_id":"rs_1","text":"..."}`,
		`{"type":"response.function_call_arguments.done","item_id":"fc_1","arguments":"{}"}`,
		`{"type":"response.completed","response":{"status":"completed"}}`)
	chunks := parseChunks(t, sseEvents(events...))
	keepalives := 0
	for _, c := range chunks {
		if isKeepalive(c) {
			keepalives++
		}
	}
	if keepalives < 100 {
		t.Errorf("100 silent deltas produced %d keepalives (%d chunks in all)", keepalives, len(chunks))
	}
	if last := chunks[len(chunks)-1]; !last.Done || len(last.ToolCalls) != 1 {
		t.Errorf("done = %+v", last)
	}
}

// A [DONE] marker without a terminal event is a truncation, not a completion.
func TestParseResponsesSSE_DoneMarkerWithoutTerminalIsTruncated(t *testing.T) {
	body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\ndata: [DONE]\n\n"
	err := parseResponsesSSE(strings.NewReader(body), func(Chunk) {})
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("want a truncation error, got %v", err)
	}
}

func TestHostedToolLabel_FoundryTypes(t *testing.T) {
	for typ, want := range map[string]string{
		"azure_ai_search_call":              "Agent · Azure AI Search",
		"bing_grounding_call":               "Agent · Bing search",
		"bing_custom_search_preview_call":   "Agent · Bing custom search",
		"openapi_call":                      "Agent · OpenAPI tool",
		"azure_function_call":               "Agent · Azure Function",
		"sharepoint_grounding_preview_call": "Agent · SharePoint",
		"fabric_dataagent_preview_call":     "Agent · Fabric data agent",
		"browser_automation_preview_call":   "Agent · browser automation",
		"memory_search_call":                "Agent · memory search",
		"a2a_preview_call":                  "Agent · connected agent",
	} {
		if got := hostedToolLabel(responsesOutputItem{Type: typ}); got != want {
			t.Errorf("%s label = %q, want %q", typ, got, want)
		}
	}
	for _, typ := range []string{"mcp_call", "a2a_preview_call", "openapi_call", "azure_function_call"} {
		if got := hostedToolLabel(responsesOutputItem{Type: typ, Name: "x"}); !strings.HasSuffix(got, ": x") {
			t.Errorf("%s label %q does not name the tool", typ, got)
		}
	}
	if got := hostedToolLabel(responsesOutputItem{Type: "web_search_call", Name: "x"}); got != "Agent · web search" {
		t.Errorf("web search label = %q", got)
	}
	for _, typ := range []string{"a2a_call", "azure_ai_search_call_output", "function_call", "message", "reasoning"} {
		if got := hostedToolLabel(responsesOutputItem{Type: typ}); got != "" {
			t.Errorf("%s must not be a trail step, got %q", typ, got)
		}
	}
}

func TestRawText(t *testing.T) {
	for raw, want := range map[string]string{
		`"plain"`:                  "plain",
		`{ "a": 1,  "b": [1, 2] }`: `{"a":1,"b":[1,2]}`,
		`[ {"x": "y"} ]`:           `[{"x":"y"}]`,
		`null`:                     "",
		``:                         "",
		`  `:                       "",
		`42`:                       "42",
	} {
		if got := rawText(json.RawMessage(raw)); got != want {
			t.Errorf("rawText(%q) = %q, want %q", raw, got, want)
		}
	}
}

func TestHostedToolSummary_ScrubsAndCutsOnRuneBoundary(t *testing.T) {
	jsonString := func(s string) json.RawMessage {
		b, _ := json.Marshal(s)
		return b
	}
	// 'é' occupies bytes 119-120: a byte cut at 120 would split it.
	got := hostedToolSummary(responsesOutputItem{Output: jsonString(strings.Repeat("a", 119) + "é tail")})
	if !utf8.ValidString(got) || got != strings.Repeat("a", 119)+"…" {
		t.Errorf("multibyte cut = %q (valid UTF-8: %v)", got, utf8.ValidString(got))
	}
	if got := hostedToolSummary(responsesOutputItem{Error: jsonString("login failed: Server=x;Password=hunter2")}); strings.Contains(got, "hunter2") {
		t.Errorf("secret not scrubbed: %q", got)
	}
	// A secret straddling the cut is still matched: scrub, then cut.
	if got := hostedToolSummary(responsesOutputItem{Output: jsonString(strings.Repeat("b", 100) + " Password=hunter2secretvalue;")}); strings.Contains(got, "hunter2") {
		t.Errorf("secret straddling the cut leaked: %q", got)
	}
	if got := hostedToolSummary(responsesOutputItem{Error: json.RawMessage(`{"code":32600,"message":"Session terminated"}`)}); got != "Session terminated" {
		t.Errorf("object error summary = %q", got)
	}
	if got := hostedToolSummary(responsesOutputItem{Output: json.RawMessage(`{ "summary": "ok" }`), Error: json.RawMessage(`null`)}); got != `{"summary":"ok"}` {
		t.Errorf("object output summary = %q", got)
	}
}

// TestAzureFoundry_FetchAgentsSkipsKindsTheSharedRouteCannotRun: hosted,
// external and voice agents are not offered (the shared Responses route
// cannot run them) and do not count toward the listing cap.
func TestAzureFoundry_FetchAgentsSkipsKindsTheSharedRouteCannotRun(t *testing.T) {
	kind := func(k string) map[string]any {
		return map[string]any{"latest": map[string]any{"definition": map[string]any{"kind": k}}}
	}
	var data []map[string]any
	for i := 0; i < azureProjectMaxAgents; i++ {
		data = append(data, map[string]any{"name": fmt.Sprintf("hosted%d", i), "versions": kind("hosted")})
	}
	data = append(data,
		map[string]any{"name": "Helper", "versions": map[string]any{"latest": map[string]any{"definition": map[string]any{"kind": "prompt", "model": "gpt-4.1"}}}},
		map[string]any{"name": "flowy", "versions": kind("workflow")},
		map[string]any{"name": "legacy", "definition": map[string]any{"model": "gpt-4o"}},
		map[string]any{"name": "triage", "versions": kind("hosted")},
		map[string]any{"name": "remote", "versions": kind("external")},
		map[string]any{"name": "talk", "definition": map[string]any{"kind": "Voice"}},
	)
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, n int, _ map[string]any) {
		if r.URL.Path != "/api/projects/p1/agents" {
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
			return
		}
		writeJSON(w, 200, map[string]any{"data": data, "has_more": false})
	})
	p := newAzureAgentProvider(t, srv)
	listed, err := p.fetchAgents(context.Background())
	if err != nil {
		t.Fatalf("fetchAgents: %v", err)
	}
	agents := p.agentModels(listed, nil)
	got := map[string]bool{}
	for _, m := range agents {
		got[m.ID] = true
	}
	for _, id := range []string{"agent:Helper", "agent:flowy", "agent:legacy"} {
		if !got[id] {
			t.Errorf("%s missing from %v", id, got)
		}
	}
	if len(agents) != 3 {
		t.Errorf("listed %d agents, want only the 3 runnable ones: %v", len(agents), got)
	}
}
