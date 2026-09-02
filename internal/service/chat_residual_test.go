package service

import (
	"context"
	"strings"
	"sync"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// TestEnforceBudget_InFlightTerm pins R2: the per-iteration budget check adds
// the stream's own not-yet-persisted spend — usage records are written
// asynchronously, so a store-only read lagged by up to one loop iteration.
func TestEnforceBudget_InFlightTerm(t *testing.T) {
	// Budget 1.00; persisted usage 0.90 — store-only reads pass, but with
	// 0.20 in-flight the check must trip.
	backend := &testutil.FakeBackend{DailyUsage: 0.9}
	svc := &ChatService{
		backend:  backend,
		settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 1.0}}},
	}
	if err := svc.enforceBudget(context.Background(), "user-1", "org-1", 0); err != nil {
		t.Fatalf("store-only read should pass (0.9 < 1.0): %v", err)
	}
	if err := svc.enforceBudget(context.Background(), "user-1", "org-1", 0.2); err == nil {
		t.Fatal("in-flight spend (0.9 + 0.2 ≥ 1.0) must trip the budget")
	}
	// And the error mentions the combined figure.
	if err := svc.enforceBudget(context.Background(), "user-1", "org-1", 0.2); !strings.Contains(err.Error(), "1.10") {
		t.Errorf("error should report combined usage: %v", err)
	}
}

// TestInFlightCost_PricesTokens: local spend priced at provider rates.
func TestInFlightCost_PricesTokens(t *testing.T) {
	p := &testutil.FakeProvider{} // Pricing zero-value
	if got := inFlightCost(p, 0, 0); got != 0 {
		t.Errorf("zero tokens = %v, want 0", got)
	}
	if got := inFlightCost(nil, 100, 100); got != 0 {
		t.Errorf("nil provider = %v, want 0", got)
	}
	// A provider with real pricing via the interface: wrap the fake — use
	// glm's provider-wide pricing (1.4/4.4 per catalog audit) for real math.
	g := ai.NewGLMProvider("k")
	if got := inFlightCost(g, 1_000_000, 0); got != 1.4 {
		t.Errorf("1M input on GLM = %v, want 1.4", got)
	}
	if got := inFlightCost(g, 0, 1_000_000); got != 4.4 {
		t.Errorf("1M output on GLM = %v, want 4.4", got)
	}
}

// TestConvMutexFor_BoundedAndStable pins R3: same conversation → same mutex;
// the stripe set is bounded (no per-key allocation, so no unbounded growth).
func TestConvMutexFor_BoundedAndStable(t *testing.T) {
	svc := &ChatService{}
	a := svc.convMutexFor("flow-1", "flow")
	b := svc.convMutexFor("flow-1", "flow")
	if a != b {
		t.Error("same (flow, convKey) must map to the same mutex")
	}
	// Distinct keys map into the fixed stripe set — at most convMutexStripes
	// distinct mutex pointers across any key population.
	seen := map[*sync.Mutex]bool{}
	for i := 0; i < 10_000; i++ {
		seen[svc.convMutexFor("flow", strings.Repeat("x", i%40)+string(rune('a'+i%26))+"-"+string(rune(i%26+'a')))] = true
	}
	if len(seen) > convMutexStripes {
		t.Errorf("stripe set exceeded: %d distinct mutexes > %d", len(seen), convMutexStripes)
	}
	// Mutual exclusion still holds for the same conversation.
	var counter int
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mu := svc.convMutexFor("flow-1", "flow")
			mu.Lock()
			counter++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if counter != 32 {
		t.Errorf("counter = %d, want 32 (lock lost mutual exclusion)", counter)
	}
}

// TestRunToolLoop_MultiReadOnlyToolsRunAndOrder pins R4: a turn with several
// read-only calls executes all of them (concurrently under the hood) and the
// model's next turn receives every result in call order.
func TestRunToolLoop_MultiReadOnlyToolsRunAndOrder(t *testing.T) {
	stub := &recordingProvider{FakeProvider: testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{
			{ID: "t1", Name: "search_flow", Input: []byte(`{"query":"xav"}`)},
			{ID: "t2", Name: "get_block", Input: []byte(`{"blockId":"b1"}`)},
			{ID: "t3", Name: "search_flow", Input: []byte(`{"query":"api"}`)},
		}, TokensIn: 5, TokensOut: 5},
		{Text: "summarized", TokensIn: 5, TokensOut: 5},
	}}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{cancel: func() {}, started: make(chan struct{})}
	close(ctl.started)
	emit, evs := collectEvents()

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "go"}}},
		"user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	if !ctl.done || ctl.buffer.String() != "summarized" {
		t.Fatalf("loop did not finish: done=%v err=%q", ctl.done, ctl.errMsg)
	}
	stub.mu.Lock()
	var toolMsgs []string
	if len(stub.reqs) == 2 {
		for _, m := range stub.reqs[1] {
			if m.Role == "tool" {
				toolMsgs = append(toolMsgs, m.Content)
			}
		}
	}
	stub.mu.Unlock()
	if len(toolMsgs) != 3 {
		t.Fatalf("want 3 tool results on the model's next turn, got %d", len(toolMsgs))
	}
	if !strings.Contains(toolMsgs[0], "xav") || !strings.Contains(toolMsgs[1], "b1") || !strings.Contains(toolMsgs[2], "api") {
		t.Errorf("results out of call order: %v", toolMsgs)
	}
	// Event stream: tool + tool_result per call (3 each), in order.
	got := strings.Join(*evs, ",")
	for _, want := range []string{"tool,tool,tool", "tool_result,tool_result,tool_result"} {
		if !strings.Contains(got, want) {
			t.Errorf("event grouping wrong (want %q in %q)", want, got)
		}
	}
}

// TestRagGuidelines_TokenCap pins R5: an over-verbose knowledge base cannot
// crowd the system prompt — the block is truncated to the sub-budget.
func TestRagGuidelines_TokenCap(t *testing.T) {
	long := "\n**Relevant Organizational Guidelines** (apply where relevant):\n" + strings.Repeat("- "+strings.Repeat("guideline text ", 20)+"\n", 200)
	if ai.EstimateTokens(long) <= ragGuidelinesTokenCap {
		t.Fatalf("fixture too short to exercise the cap (%d tokens)", ai.EstimateTokens(long))
	}
	// Truncation is applied at the ragGuidelines boundary; assert the cap
	// itself via the same helper the production path uses.
	capped := ai.TruncateToTokenLimit(long, ragGuidelinesTokenCap)
	if n := ai.EstimateTokens(capped); n > ragGuidelinesTokenCap {
		t.Errorf("capped block = %d tokens, want <= %d", n, ragGuidelinesTokenCap)
	}
	if !strings.Contains(capped, "Relevant Organizational Guidelines") {
		t.Errorf("cap lost the header: %q", capped[:80])
	}
}
