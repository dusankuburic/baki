package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// A small context window — an Azure AI Foundry registry entry's override (the
// form allows 1024 tokens and up, and invites it for small distilled models),
// a local server's 2k–8k context — keeps room for both the prompt and the
// answer: the max-tokens backstop and the truncation reserve scale with the
// window (review R1). Every window from 16000 tokens up behaves exactly as
// before.

// foundryChatMessages is the conversation part of a chat-completions body the
// fake Foundry resource recorded: its user and assistant messages (the system
// prompt left out).
func foundryChatMessages(t *testing.T, body map[string]any) []map[string]any {
	t.Helper()
	raw, ok := body["messages"].([]any)
	if !ok {
		t.Fatalf("request body has no messages: %v", body)
	}
	var out []map[string]any
	for _, m := range raw {
		msg, _ := m.(map[string]any)
		if role := msg["role"]; role == "user" || role == "assistant" {
			out = append(out, msg)
		}
	}
	return out
}

// Registry entries whose windows are 1024, 4096 and 8192 tokens, capped at 512
// output tokens: a short question is sent — whatever max tokens the client
// asked for, the entry's cap is what goes out — and a thread longer than the
// window loses its oldest turns instead of failing with CONTEXT_LIMIT. Before,
// every turn reserved 4000 tokens whatever the window, so the 1024 and 4096
// entries refused even "hi".
func TestChatAzureRegistry_SmallWindowEntriesRun(t *testing.T) {
	for _, window := range []int{1024, 4096, 8192} {
		t.Run(fmt.Sprintf("window %d", window), func(t *testing.T) {
			e := newAzChatEnv(t)
			foundry := newAzChatFoundry(t)
			c := e.mustConn(t, azmUser, "res", azmKeyConfig("k"))
			m := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "phi-distilled",
				DisplayName: "Small", ContextWindow: window, MaxOutputTokens: 512})
			doc := &models.FlowDocument{ID: "f1", Name: "f1"}

			// The entry's own cap, the web client's default and an unset cap
			// (an API client) all go out as the entry's 512.
			for _, maxTokens := range []int{512, 4096, 0} {
				before := len(foundry.requests())
				snap := e.turn(t, "u1", doc, models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: m.ModelID,
					MaxTokens: maxTokens, UserMessage: "hi", ConversationID: uuid.NewString()})
				if !snap.Done || snap.ErrorCode != "" {
					t.Fatalf("max tokens %d: \"hi\" = %+v, want it sent", maxTokens, snap)
				}
				reqs := foundry.requests()
				if len(reqs) != before+1 {
					t.Fatalf("max tokens %d: the resource received %d request(s), want 1", maxTokens, len(reqs)-before)
				}
				if got := reqs[len(reqs)-1].body["max_completion_tokens"]; got != float64(512) {
					t.Errorf("max tokens %d: max_completion_tokens = %v, want the entry's 512", maxTokens, got)
				}
			}

			// A thread far longer than the window (30 turns of ~300 tokens).
			var history []models.ChatMessage
			for i := 0; i < 30; i++ {
				role := "user"
				if i%2 == 1 {
					role = "assistant"
				}
				history = append(history, models.ChatMessage{Role: role, Content: fmt.Sprintf("turn %d %s", i, strings.Repeat("word ", 300))})
			}
			before := len(foundry.requests())
			snap := e.turn(t, "u1", doc, models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: m.ModelID,
				MaxTokens: 512, UserMessage: "and now?", Messages: history, ConversationID: uuid.NewString()})
			if !snap.Done || snap.ErrorCode != "" {
				t.Fatalf("long thread = %+v, want it trimmed and sent", snap)
			}
			reqs := foundry.requests()
			if len(reqs) != before+1 {
				t.Fatalf("long thread: the resource received %d request(s), want 1", len(reqs)-before)
			}
			sent := foundryChatMessages(t, reqs[len(reqs)-1].body)
			if len(sent) >= len(history)+1 {
				t.Errorf("long thread: %d messages sent, want the oldest of the %d dropped", len(sent), len(history)+1)
			}
			first, last := sent[0], sent[len(sent)-1]
			if first["role"] != "user" {
				t.Errorf("long thread: the kept history starts with %v, want a user turn", first["role"])
			}
			if content, _ := last["content"].(string); last["role"] != "user" || !strings.HasSuffix(content, "and now?") {
				t.Errorf("long thread: the last message is %v %.40q…, want the current question", last["role"], content)
			}
		})
	}
}

// An 8192-token entry capped at 512 output tokens sends a ~4.5k-token question:
// prompt plus max output fit the window with room to spare. The flat 4000-token
// reserve refused it.
func TestChatAzureRegistry_8kEntrySendsAPromptThatFits(t *testing.T) {
	e := newAzChatEnv(t)
	foundry := newAzChatFoundry(t)
	c := e.mustConn(t, azmUser, "res", azmKeyConfig("k"))
	m := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "slm-8k",
		DisplayName: "SLM", ContextWindow: 8192, MaxOutputTokens: 512})
	question := strings.Repeat("word ", 4500) // ~4500 cl100k tokens
	if n := ai.NewAzureFoundryProvider("").EstimateTokens(question); n < 4400 || n+512 >= 8192-1000 {
		t.Fatalf("question estimate %d tokens: the test needs ~4.5k", n)
	}
	snap := e.turn(t, "u1", &models.FlowDocument{ID: "f1", Name: "f1"}, models.ChatRequest{Provider: ai.AzureFoundryProviderID,
		Model: m.ModelID, MaxTokens: 512, UserMessage: question, ConversationID: uuid.NewString()})
	if !snap.Done || snap.ErrorCode != "" {
		t.Fatalf("turn = done %v, code %q; want the question sent", snap.Done, snap.ErrorCode)
	}
	reqs := foundry.requests()
	if len(reqs) != 1 || reqs[0].body["max_completion_tokens"] != float64(512) {
		t.Fatalf("the resource received %d request(s), want one with max_completion_tokens 512", len(reqs))
	}
	sent := foundryChatMessages(t, reqs[0].body)
	if content, _ := sent[len(sent)-1]["content"].(string); !strings.HasSuffix(content, question) {
		t.Error("the question did not arrive whole")
	}
}

// From 16000 tokens up nothing changed: an entry capped at 512 output tokens
// still reserves the full 4000 beside its prompt, so a question that fits only
// with the smaller reserve is refused as before — and one that fits with it is
// sent with the same max tokens.
func TestChatAzureRegistry_16kEntryUnchanged(t *testing.T) {
	e := newAzChatEnv(t)
	foundry := newAzChatFoundry(t)
	c := e.mustConn(t, azmUser, "res", azmKeyConfig("k"))
	m := e.mustModel(t, azmUser, AzureModelInput{ConnectionID: c.ID, Kind: "chat", Deployment: "slm-16k",
		DisplayName: "SLM", ContextWindow: 16384, MaxOutputTokens: 512})
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}

	// ~13k tokens with the system prompt: +512 fits 16384, +4000 does not.
	tooLong := strings.Repeat("word ", 12600)
	snap := e.turn(t, "u1", doc, models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: m.ModelID,
		MaxTokens: 512, UserMessage: tooLong, ConversationID: uuid.NewString()})
	if snap.Done || snap.ErrorCode != ChatErrContextLimit {
		t.Errorf("turn = done %v, code %q; want CONTEXT_LIMIT as before (the reserve stays 4000 on 16k)", snap.Done, snap.ErrorCode)
	}
	if n := len(foundry.requests()); n != 0 {
		t.Errorf("the refused question reached the resource %d time(s)", n)
	}

	fits := strings.Repeat("word ", 11000)
	snap = e.turn(t, "u1", doc, models.ChatRequest{Provider: ai.AzureFoundryProviderID, Model: m.ModelID,
		MaxTokens: 4096, UserMessage: fits, ConversationID: uuid.NewString()})
	if !snap.Done {
		t.Fatalf("turn = done %v, code %q; want it sent", snap.Done, snap.ErrorCode)
	}
	if reqs := foundry.requests(); len(reqs) != 1 || reqs[0].body["max_completion_tokens"] != float64(512) {
		t.Errorf("requests = %d, want one with the entry's 512", len(reqs))
	}
}

// The two formulas are the pre-R1 ones on every window from 16000 tokens up —
// the reserve for every cap, the backstop for every requested max tokens —
// and the backstop is from 8000 up.
func TestContextWindow_LargeWindowsKeepTheFlatReserve(t *testing.T) {
	caps := []int{-1, 0, 1, 256, 512, 1000, 3999, 4000, 4001, 4096, 8000, 16384, 64000, 128000, 1_000_000, 3_000_000}
	for _, window := range []int{16000, 16001, 16384, 20000, 32768, 64000, 128000, 200000, 1_000_000, 2_000_000} {
		for _, maxTokens := range caps {
			if got, want := truncationReserve(window, maxTokens), max(contextReserve, maxTokens); got != want {
				t.Errorf("truncationReserve(%d, %d) = %d, want %d as before", window, maxTokens, got, want)
			}
			wantMax := min(max(maxTokens, 0), window-contextReserve)
			if _, got := normalizeChatParams(0.5, maxTokens, window, 0); got != wantMax {
				t.Errorf("normalizeChatParams(max %d, window %d) = %d, want %d as before", maxTokens, window, got, wantMax)
			}
		}
	}
	for _, window := range []int{8000, 8001, 8096, 8192, 12000, 15999} {
		if got, want := outputBackstop(window), window-contextReserve; got != want {
			t.Errorf("outputBackstop(%d) = %d, want %d as before", window, got, want)
		}
	}
}

// Below that the backstop keeps at most half the window for the prompt and the
// reserve at most a quarter (never less than the request's own cap); an unset
// cap still reserves contextReserve, since the provider then sends its own
// default.
func TestContextWindow_SmallWindowsScale(t *testing.T) {
	for _, tc := range []struct{ window, backstop int }{
		{1024, 512}, {2048, 1024}, {4096, 2048}, {6000, 3000}, {7999, 4000}, {8192, 4192},
	} {
		if got := outputBackstop(tc.window); got != tc.backstop {
			t.Errorf("outputBackstop(%d) = %d, want %d", tc.window, got, tc.backstop)
		}
	}
	for _, tc := range []struct{ window, maxTokens, reserve int }{
		{1024, 512, 512},   // the cap itself
		{1024, 100, 256},   // a quarter of the window
		{4096, 512, 1024},  // a quarter of the window
		{4096, 2048, 2048}, // the cap itself
		{8192, 512, 2048},
		{12000, 512, 3000},
		{1024, 0, contextReserve}, // unset: the provider's own default goes out
		{8192, 0, contextReserve},
	} {
		if got := truncationReserve(tc.window, tc.maxTokens); got != tc.reserve {
			t.Errorf("truncationReserve(%d, %d) = %d, want %d", tc.window, tc.maxTokens, got, tc.reserve)
		}
	}
}

// The truncation's byte fast path, its estimate and its drop loop all read the
// scaled reserve: on a 4096-token window with a 512-token cap (reserve 1024) a
// prompt whose bytes fit beside 1024 skips the tokenizer, one byte more runs it,
// and a thread the window cannot hold is trimmed to what fits beside 1024 —
// not refused outright as it was against 4000. An unset cap keeps the old
// reserve (the provider's own 4096 default would go out) and is refused.
func TestTruncateForContextWindow_SmallWindowScaledReserve(t *testing.T) {
	const window = 4096
	reserve := truncationReserve(window, 512) // 1024
	oneMessage := func(bytes int) *ai.Request {
		return &ai.Request{MaxTokens: 512, Messages: []ai.Message{{Role: "user", Content: strings.Repeat("u", bytes-4)}}}
	}

	p := &countingEstimator{FakeProvider: &testutil.FakeProvider{}}
	req := oneMessage(window - reserve)
	if _, err := truncateForContextWindow(context.Background(), p, req, window, 0); err != nil || p.calls != 0 {
		t.Errorf("a prompt whose bytes fit beside the reserve: err %v, %d tokenizer call(s); want no call", err, p.calls)
	}
	req = oneMessage(window - reserve + 1)
	if _, err := truncateForContextWindow(context.Background(), p, req, window, 0); err != nil || p.calls == 0 {
		t.Errorf("one byte over: err %v, %d tokenizer call(s); want the estimate to run (and fit)", err, p.calls)
	}

	// FakeProvider prices len/4: each 4000-byte turn costs 1004 tokens.
	msg := func(role, tag string) ai.Message {
		return ai.Message{Role: role, Content: tag + strings.Repeat(".", 4000-len(tag))}
	}
	thread := func(maxTokens int) *ai.Request {
		return &ai.Request{MaxTokens: maxTokens, Messages: []ai.Message{
			msg("user", "u0"), msg("assistant", "a0"), msg("user", "u1"), msg("assistant", "a1"), msg("user", "current"),
		}}
	}
	req = thread(512)
	pin, err := truncateForContextWindow(context.Background(), &testutil.FakeProvider{}, req, window, 4)
	if err != nil {
		t.Fatalf("a long thread on a small window: %v, want it trimmed", err)
	}
	if len(req.Messages) != 3 || pin != 2 || !strings.HasPrefix(req.Messages[0].Content, "u1") || !strings.HasPrefix(req.Messages[pin].Content, "current") {
		t.Errorf("kept %d messages (pin %d), want u1, a1 and the current turn (3012 tokens beside the 1024 reserve)", len(req.Messages), pin)
	}
	if _, err := truncateForContextWindow(context.Background(), &testutil.FakeProvider{}, thread(0), window, 4); !errors.Is(err, ai.ErrContextLimit) {
		t.Errorf("an unset cap on a 4096 window: err %v, want ErrContextLimit (the provider's own 4096 default does not fit)", err)
	}
}

// Generation params on a small window: the backstop keeps half the window for the prompt
// instead of all of it (a 4096 window used to cap max tokens at 96, a 1024 one at 0 — the
// provider's own default then went out), the reasoning floor is re-capped by it, and an
// unset max tokens gets the common default capped to the window instead of the provider's
// default, which the window cannot hold. From a backstop of 4096 up (windows of 8096
// tokens and more) an unset cap still goes out as the provider's default.
func TestTurnGenerationParams_SmallWindows(t *testing.T) {
	p := &itemsProvider{id: "local-ish", limit: 1 << 20, models: []ai.ModelInfo{
		{ID: "slm", ContextLimit: 4096},
		{ID: "tiny", ContextLimit: 1024},
		{ID: "slm-capped", ContextLimit: 4096, MaxOutputTokens: 512},
		{ID: "gpt-5-mini", ContextLimit: 4096},
		{ID: "gh-8k", ContextLimit: 8192},
		{ID: "edge", ContextLimit: 8096},
		{ID: "big", ContextLimit: 128000},
	}}
	for _, tc := range []struct {
		model   string
		in, out int
	}{
		{"slm", 4096, 2048},
		{"slm", 1000, 1000},
		{"slm", 0, 2048}, // unset: the common default, capped to the window
		{"tiny", 4096, 512},
		{"tiny", 0, 512},
		{"tiny", 300, 300},
		{"slm-capped", 4096, 512},
		{"slm-capped", 0, 512},
		{"gpt-5-mini", 1000, 2048}, // the reasoning floor, re-capped by the backstop
		{"gpt-5-mini", 0, 2048},
		{"gh-8k", 999999, 4192}, // as before
		{"gh-8k", 0, 0},         // as before: the provider's own default
		{"edge", 0, 0},          // the backstop (4096) holds the common default
		{"big", 0, 0},
		{"big", 4096, 4096},
	} {
		_, got := turnGenerationParams(context.Background(), p, models.ChatRequest{MaxTokens: tc.in}, turnModel{resolved: tc.model})
		if got != tc.out {
			t.Errorf("%s with max tokens %d: %d, want %d", tc.model, tc.in, got, tc.out)
		}
	}
}
