package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// R-provider-32: Gemini thought signatures ride the replay record (Chunk/Response.ProviderItems) and go back on the
// matching parts; a provider-supplied functionCall.id is honoured; a missing-signature refusal is classified.

// geminiStub serves scripted answers in order and records each request body.
type geminiStub struct {
	mu      sync.Mutex
	bodies  []geminiRequest
	answers []func(w http.ResponseWriter)
}

func (s *geminiStub) serve(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body geminiRequest
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Errorf("request body: %v", err)
		}
		s.mu.Lock()
		n := len(s.bodies)
		s.bodies = append(s.bodies, body)
		s.mu.Unlock()
		if n >= len(s.answers) {
			t.Errorf("unexpected request %d", n+1)
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		s.answers[n](w)
	}))
	t.Cleanup(srv.Close)
	orig := geminiURL
	t.Cleanup(func() { geminiURL = orig })
	geminiURL = func(_, _, _ string) string { return srv.URL }
	return srv
}

func geminiSSE(events ...string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, e := range events {
			_, _ = w.Write([]byte("data: " + e + "\r\n\r\n"))
		}
	}
}

func geminiJSON(status int, body string) func(w http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}
}

func newStubbedGemini(t *testing.T, stub *geminiStub) *GeminiProvider {
	srv := stub.serve(t)
	p := NewGeminiProvider("test-key")
	p.client = srv.Client()
	return p
}

// TestGeminiStream_ToolLoopReplaysThoughtSignature: a two-step tool loop against a stub Gemini server. The first
// call's thoughtSignature and its provider-supplied id go back on the replayed functionCall part (and the id on its
// functionResponse); the second call, which carried neither, gets neither.
func TestGeminiStream_ToolLoopReplaysThoughtSignature(t *testing.T) {
	stub := &geminiStub{answers: []func(http.ResponseWriter){
		geminiSSE(`{"candidates":[{"content":{"role":"model","parts":[` +
			`{"functionCall":{"id":"fc-1","name":"get_block","args":{"blockId":"b-12"}},"thoughtSignature":"sig-A"},` +
			`{"functionCall":{"name":"list_findings","args":{}}}]},"finishReason":"STOP"}],` +
			`"usageMetadata":{"promptTokenCount":40,"candidatesTokenCount":9,"thoughtsTokenCount":30},"modelVersion":"gemini-3-pro-preview"}`),
		geminiSSE(`{"candidates":[{"content":{"role":"model","parts":[{"text":"Block 12 loops."}]}}],"modelVersion":"gemini-3-pro-preview"}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"","thoughtSignature":"sig-T"}]},"finishReason":"STOP"}],"modelVersion":"gemini-3-pro-preview"}`),
	}}
	p := newStubbedGemini(t, stub)

	stream := func(msgs []Message) Chunk {
		t.Helper()
		var done Chunk
		if err := p.Stream(context.Background(), Request{Model: "gemini-3-pro-preview", Messages: msgs, Tools: []ToolDefinition{{Name: "get_block", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}}, func(c Chunk) {
			if c.Error != nil {
				t.Fatalf("error chunk: %v", c.Error)
			}
			if c.Done {
				done = c
			}
		}); err != nil {
			t.Fatal(err)
		}
		return done
	}

	msgs := []Message{{Role: "user", Content: "Does the loop exit?"}}
	first := stream(msgs)
	if len(first.ToolCalls) != 2 || first.ToolCalls[0].ID != "fc-1" {
		t.Fatalf("want the provider's id fc-1 on the first call, got %+v", first.ToolCalls)
	}
	if first.ToolCalls[1].ID == "" || first.ToolCalls[1].ID == "fc-1" {
		t.Fatalf("the second call needs its own minted id, got %q", first.ToolCalls[1].ID)
	}
	if len(first.ProviderItems) == 0 {
		t.Fatal("the Done chunk carries no replay record: the signature is lost")
	}

	msgs = append(msgs,
		Message{Role: "assistant", ToolCalls: first.ToolCalls, ProviderItems: first.ProviderItems, ProviderItemsFrom: geminiSpec.ID},
		Message{Role: "tool", Content: "block 12: Loop", ToolCallID: first.ToolCalls[0].ID},
		Message{Role: "tool", Content: "no findings", ToolCallID: first.ToolCalls[1].ID},
	)
	second := stream(msgs)
	if len(second.ProviderItems) == 0 || !strings.Contains(string(second.ProviderItems), "sig-T") {
		t.Errorf("a text answer's trailing signature belongs in its record, got %s", second.ProviderItems)
	}

	if len(stub.bodies) != 2 {
		t.Fatalf("want 2 requests, got %d", len(stub.bodies))
	}
	contents := stub.bodies[1].Contents
	if len(contents) != 3 {
		t.Fatalf("want user, model, user contents, got %+v", contents)
	}
	model := contents[1]
	if len(model.Parts) != 2 || model.Parts[0].FunctionCall == nil || model.Parts[1].FunctionCall == nil {
		t.Fatalf("model turn parts: %+v", model.Parts)
	}
	if model.Parts[0].ThoughtSignature != "sig-A" || model.Parts[0].FunctionCall.ID != "fc-1" {
		t.Errorf("first call replayed without its signature or id: %+v / %+v", model.Parts[0], model.Parts[0].FunctionCall)
	}
	if model.Parts[1].ThoughtSignature != "" || model.Parts[1].FunctionCall.ID != "" {
		t.Errorf("the second call carried neither a signature nor an id, got %+v / %+v", model.Parts[1], model.Parts[1].FunctionCall)
	}
	results := contents[2].Parts
	if len(results) != 2 || results[0].FunctionResponse == nil || results[0].FunctionResponse.ID != "fc-1" || results[1].FunctionResponse.ID != "" {
		t.Errorf("function responses must echo the provider's id only: %+v", results)
	}
}

// TestGeminiChat_RecordsSignatureAndProviderID: the non-streaming path records the same replay record and honours
// the provider's id.
func TestGeminiChat_RecordsSignatureAndProviderID(t *testing.T) {
	stub := &geminiStub{answers: []func(http.ResponseWriter){
		geminiJSON(200, `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"fc-9","name":"get_block","args":{"blockId":"b-1"}},"thoughtSignature":"sig-C"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":4}}`),
	}}
	p := newStubbedGemini(t, stub)
	resp, err := p.Chat(context.Background(), Request{Model: "gemini-3-pro-preview", Messages: []Message{{Role: "user", Content: "q"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "fc-9" {
		t.Fatalf("want the provider's id fc-9, got %+v", resp.ToolCalls)
	}
	contents := toGeminiContents([]Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", ToolCalls: resp.ToolCalls, ProviderItems: resp.ProviderItems, ProviderItemsFrom: geminiSpec.ID},
	})
	if got := contents[1].Parts[0]; got.ThoughtSignature != "sig-C" {
		t.Errorf("Chat's record must replay the signature, got %+v", got)
	}
}

// TestToGeminiContents_ForeignHistoryCarriesNoGeminiSignatures: replay records written by another provider (or
// naming calls this turn does not have) put no thoughtSignature or id on any part.
func TestToGeminiContents_ForeignHistoryCarriesNoGeminiSignatures(t *testing.T) {
	calls := []ToolCall{{ID: "toolu_1", Name: "get_block", Input: json.RawMessage(`{"blockId":"b-1"}`)}}
	geminiRecord := json.RawMessage(`{"calls":[{"id":"toolu_1","signature":"sig-X","providerId":true}],"textSignature":"sig-Y"}`)
	claudeRecord := json.RawMessage(`[{"type":"thinking","thinking":"t","signature":"claude-sig"},{"type":"tool_use","id":"toolu_1","name":"get_block","input":{"blockId":"b-1"}}]`)
	for _, tc := range []struct {
		name  string
		items json.RawMessage
		from  string
	}{
		{"claude record from claude", claudeRecord, "claude"},
		{"gemini-shaped record from another provider", geminiRecord, "azure-foundry"},
		{"claude record claiming gemini", claudeRecord, geminiSpec.ID},
		{"gemini record for other calls", json.RawMessage(`{"calls":[{"id":"call_other","signature":"sig-Z","providerId":true}]}`), geminiSpec.ID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contents := toGeminiContents([]Message{
				{Role: "user", Content: "q"},
				{Role: "assistant", Content: "Looking.", ToolCalls: calls, ProviderItems: tc.items, ProviderItemsFrom: tc.from},
				{Role: "tool", Content: "r", ToolCallID: "toolu_1"},
			})
			raw, _ := json.Marshal(contents)
			if strings.Contains(string(raw), "thoughtSignature") || strings.Contains(string(raw), `"id"`) {
				t.Errorf("foreign history leaked a Gemini signature or id: %s", raw)
			}
		})
	}
}

// TestToGeminiContents_TextSignatureOnTheTextPart: a tool-call turn that opened with text replays the text part's
// signature on that part.
func TestToGeminiContents_TextSignatureOnTheTextPart(t *testing.T) {
	calls := []ToolCall{{ID: "call_1", Name: "list_findings", Input: json.RawMessage(`{}`)}}
	contents := toGeminiContents([]Message{
		{Role: "user", Content: "q"},
		{Role: "assistant", Content: "Let me check.", ToolCalls: calls, ProviderItems: json.RawMessage(`{"textSignature":"sig-T"}`), ProviderItemsFrom: geminiSpec.ID},
	})
	parts := contents[1].Parts
	if len(parts) != 2 || parts[0].Text != "Let me check." || parts[0].ThoughtSignature != "sig-T" || parts[1].ThoughtSignature != "" {
		t.Errorf("want the text signature on the text part only, got %+v", parts)
	}
}

// The missing-signature refusal's shapes are modelled on Google's documentation (Gemini 3 thought signatures; the
// MISSING_THOUGHT_SIGNATURE FinishReason), not a live capture.
const geminiMissingSignature400 = `{"error":{"code":400,"message":"Function call is missing a thought_signature in functionCall parts. This is required for tools to work correctly, and missing thought_signature may lead to degraded model performance. Additional data, function call ` + "`default_api:get_block`" + ` , position 2. Please refer to https://ai.google.dev/gemini-api/docs/thought-signatures for more details.","status":"INVALID_ARGUMENT"}}`

// TestGemini_MissingThoughtSignatureIsClassified: both documented shapes, on both paths, are
// ErrThoughtSignatureMissing — not retried, not an empty answer, not a generic error.
func TestGemini_MissingThoughtSignatureIsClassified(t *testing.T) {
	finish := `{"candidates":[{"content":{"role":"model","parts":[{"text":""}]},"finishReason":"MISSING_THOUGHT_SIGNATURE"}],"usageMetadata":{"promptTokenCount":50},"modelVersion":"gemini-3-pro-preview"}`
	req := Request{Model: "gemini-3-pro-preview", Messages: []Message{{Role: "user", Content: "q"}}}
	check := func(t *testing.T, err error) {
		t.Helper()
		if !errors.Is(err, ErrThoughtSignatureMissing) {
			t.Fatalf("want ErrThoughtSignatureMissing, got %v", err)
		}
		if isRetryable(err) {
			t.Errorf("a missing signature is deterministic: must not be retried")
		}
	}
	t.Run("stream 400", func(t *testing.T) {
		p := newStubbedGemini(t, &geminiStub{answers: []func(http.ResponseWriter){geminiJSON(400, geminiMissingSignature400)}})
		check(t, p.Stream(context.Background(), req, func(Chunk) {}))
	})
	t.Run("chat 400", func(t *testing.T) {
		p := newStubbedGemini(t, &geminiStub{answers: []func(http.ResponseWriter){geminiJSON(400, geminiMissingSignature400)}})
		_, err := p.Chat(context.Background(), req)
		check(t, err)
	})
	t.Run("stream finish reason", func(t *testing.T) {
		p := newStubbedGemini(t, &geminiStub{answers: []func(http.ResponseWriter){geminiSSE(finish)}})
		var chunkErr error
		var sawDone bool
		if err := p.Stream(context.Background(), req, func(c Chunk) {
			if c.Error != nil {
				chunkErr = c.Error
			}
			sawDone = sawDone || c.Done
		}); err != nil {
			t.Fatal(err)
		}
		if sawDone {
			t.Error("a refusal must not end as an empty Done")
		}
		check(t, chunkErr)
	})
	t.Run("chat finish reason", func(t *testing.T) {
		p := newStubbedGemini(t, &geminiStub{answers: []func(http.ResponseWriter){geminiJSON(200, finish)}})
		_, err := p.Chat(context.Background(), req)
		check(t, err)
	})
}
