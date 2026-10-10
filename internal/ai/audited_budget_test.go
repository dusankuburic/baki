package ai

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// budgetGate is a BudgetChecker that refuses with err and records every call it was asked about.
type budgetGate struct {
	err   error
	mu    sync.Mutex
	calls []BudgetCall
}

func (g *budgetGate) Check(_ context.Context, call BudgetCall) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.calls = append(g.calls, call)
	return g.err
}

func (g *budgetGate) seen() []BudgetCall {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]BudgetCall(nil), g.calls...)
}

// budgetSpent is the kind of refusal a checker returns: it names its class for the metric.
type budgetSpent struct{}

func (budgetSpent) Error() string        { return "daily AI budget exceeded ($9.00 / $5.00)" }
func (budgetSpent) BudgetReason() string { return "exceeded" }

// gatedStub is a raw provider counting the calls that reached it.
type gatedStub struct {
	Provider
	id    string
	chats int
	strms int
	embs  int
}

func (s *gatedStub) ID() string                  { return s.id }
func (s *gatedStub) EstimateTokens(t string) int { return len(t) }
func (s *gatedStub) DefaultModel() string        { return "m" }
func (s *gatedStub) PricePerMillionTokens() Pricing {
	return Pricing{InputCostPerM: 1, OutputCostPerM: 1}
}
func (s *gatedStub) Models(context.Context) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "m", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}}, nil
}
func (s *gatedStub) Chat(context.Context, Request) (*Response, error) {
	s.chats++
	return &Response{Content: "ok", TokensIn: 10, TokensOut: 2}, nil
}
func (s *gatedStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	s.strms++
	onChunk(Chunk{Text: "ok"})
	onChunk(Chunk{Done: true, TokensIn: 10, TokensOut: 2})
	return nil
}
func (s *gatedStub) Embed(_ context.Context, text []string) ([][]float32, error) {
	s.embs++
	out := make([][]float32, len(text))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// gatedChain builds the factory chain for stub on the gate, with every usage row forwarded to the channel.
func gatedChain(t *testing.T, stub *gatedStub, gate BudgetChecker, resilience *ResilienceConfig) (Provider, <-chan *interfaces.UsageMetric) {
	t.Helper()
	RegisterProvider(stub.id, func(string) Provider { return stub })
	ch := make(chan *interfaces.UsageMetric, 8)
	rec := func(_ context.Context, m *interfaces.UsageMetric) error { ch <- m; return nil }
	f := NewProviderFactory(func(string, string) (string, error) { return "k", nil }, nil, rec, resilience, WithBudgetChecker(gate))
	p, err := f.For(context.Background(), "u1", stub.id)
	if err != nil {
		t.Fatal(err)
	}
	return p, ch
}

func noUsageRow(t *testing.T, ch <-chan *interfaces.UsageMetric) {
	t.Helper()
	select {
	case m := <-ch:
		t.Errorf("a usage row was recorded for a refused call: %+v", m)
	case <-time.After(50 * time.Millisecond):
	}
}

// ADR 0028: the checker is the first thing Chat, Stream and Embed do. A refusal returns the checker's error as
// is (errors.Is on the service's sentinel keeps working), the inner provider is never called, no usage row is
// written, and the refusal is counted per call kind.
func TestAudited_RefusesOverBudgetBeforeTheCall(t *testing.T) {
	gate := &budgetGate{err: budgetSpent{}}
	stub := &gatedStub{id: "budget-hook-stub"}
	p, rows := gatedChain(t, stub, gate, nil)
	ctx := context.Background()
	counter := func(kind string) float64 {
		return testutil.ToFloat64(aiBudgetRefusalsTotal.WithLabelValues(stub.id, kind, "exceeded"))
	}
	before := map[string]float64{"chat": counter("chat"), "stream": counter("stream"), "embed": counter("embed")}

	if _, err := p.Chat(ctx, Request{Model: "m", OrgID: "org-1"}); !errors.Is(err, budgetSpent{}) {
		t.Errorf("Chat over budget: %v, want the checker's own error", err)
	}
	if err := p.Stream(ctx, Request{Model: "m"}, func(Chunk) { t.Error("a chunk was delivered for a refused stream") }); !errors.Is(err, budgetSpent{}) {
		t.Errorf("Stream over budget: %v", err)
	}
	if _, err := p.Embed(ctx, []string{"text"}); !errors.Is(err, budgetSpent{}) {
		t.Errorf("Embed over budget: %v", err)
	}
	if stub.chats+stub.strms+stub.embs != 0 {
		t.Errorf("the provider was called %d/%d/%d times (chat/stream/embed) over budget", stub.chats, stub.strms, stub.embs)
	}
	noUsageRow(t, rows)
	for _, kind := range []string{"chat", "stream", "embed"} {
		if got := counter(kind) - before[kind]; got != 1 {
			t.Errorf("pad_ai_budget_refusals_total{call=%s} += %v, want 1", kind, got)
		}
	}
	calls := gate.seen()
	if len(calls) != 3 {
		t.Fatalf("checker asked %d times, want 3: %+v", len(calls), calls)
	}
	// The chat call carried its request's org; the embed, with no request, the chain's (a member key: none).
	if c := calls[0]; c.Kind != BudgetCallChat || c.Scope != "u1" || c.OrgID != "org-1" || c.ProviderID != stub.id || c.Model != "m" {
		t.Errorf("chat call = %+v", c)
	}
	if c := calls[2]; c.Kind != BudgetCallEmbed || c.Scope != "u1" || c.OrgID != "" {
		t.Errorf("embed call = %+v", c)
	}

	// Under budget the same chain answers and records.
	gate.err = nil
	if _, err := p.Chat(ctx, Request{Model: "m"}); err != nil || stub.chats != 1 {
		t.Fatalf("Chat under budget: %v (calls %d)", err, stub.chats)
	}
	select {
	case <-rows:
	case <-time.After(2 * time.Second):
		t.Error("no usage row for the admitted call")
	}
}

// An alias-model provider (Azure AI Foundry) is exempt from the budget in the hook, as it is in the chat and
// knowledge-base gates — baki neither prices nor counts its usage — whether the chain's id or the provider's
// own says so (the service tests register Foundry-like stubs under other ids).
func TestAudited_AliasProviderSkipsTheBudget(t *testing.T) {
	gate := &budgetGate{err: budgetSpent{}}
	stub := &gatedStub{id: AzureFoundryProviderID}
	ap := newAuditedProvider(stub, nil, "u1", "budget-alias-stub")
	ap.budget = gate
	if _, err := ap.Chat(context.Background(), Request{Model: "dep"}); err != nil {
		t.Errorf("alias Chat over budget: %v, want answered", err)
	}
	if err := ap.Stream(context.Background(), Request{Model: "dep"}, func(Chunk) {}); err != nil {
		t.Errorf("alias Stream over budget: %v", err)
	}
	if _, err := ap.Embed(context.Background(), []string{"x"}); err != nil {
		t.Errorf("alias Embed over budget: %v", err)
	}
	if len(gate.seen()) != 0 {
		t.Errorf("the checker was asked about an alias provider: %+v", gate.seen())
	}
	// Exempt by the chain's id too.
	byChain := newAuditedProvider(&gatedStub{id: "budget-alias-inner"}, nil, "u1", AzureFoundryProviderID)
	byChain.budget = gate
	if _, err := byChain.Chat(context.Background(), Request{Model: "dep"}); err != nil || len(gate.seen()) != 0 {
		t.Errorf("alias chain over budget: %v, checker asked %d times", err, len(gate.seen()))
	}
}

// The context's budget scope (WithBudgetScope) names who an Embed is charged to, over the chain's own scope and
// credential: an org corpus embedded through a MEMBER's key is gated on the org and its usage row names the
// org (SURVEY_budget bug 4). Without one, the chain's resolution stands.
func TestAudited_BudgetScopeFromContextBeatsTheChain(t *testing.T) {
	gate := &budgetGate{}
	stub := &gatedStub{id: "budget-scope-stub"}
	p, rows := gatedChain(t, stub, gate, nil)

	ctx := WithBudgetScope(context.Background(), "u1", "org-7")
	if _, err := p.Embed(ctx, []string{"chunk"}); err != nil {
		t.Fatal(err)
	}
	if calls := gate.seen(); len(calls) != 1 || calls[0].Scope != "u1" || calls[0].OrgID != "org-7" {
		t.Errorf("gated as %+v, want u1 in org-7", calls)
	}
	select {
	case m := <-rows:
		if m.OrgID != "org-7" || m.UserID != "u1" {
			t.Errorf("usage row = org %q user %q, want org-7 / u1 (the scope the call was gated on)", m.OrgID, m.UserID)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no usage row")
	}

	// A request org beats the chain's credential, and the context beats both.
	if _, err := p.Chat(WithBudgetScope(context.Background(), "u1", "org-8"), Request{Model: "m", OrgID: "org-1"}); err != nil {
		t.Fatal(err)
	}
	if calls := gate.seen(); calls[len(calls)-1].OrgID != "org-8" {
		t.Errorf("chat gated as %+v, want the context's org-8", calls[len(calls)-1])
	}
	<-rows
	if _, err := p.Embed(context.Background(), []string{"chunk"}); err != nil {
		t.Fatal(err)
	}
	if calls := gate.seen(); calls[len(calls)-1].OrgID != "" {
		t.Errorf("embed without a context scope gated as %+v, want the member's own budget", calls[len(calls)-1])
	}
}

// A refusal happens above the breaker and the retry policy: it is not a provider failure, so the circuit stays
// closed and nothing is retried.
func TestAudited_RefusalNeverTripsTheBreaker(t *testing.T) {
	gate := &budgetGate{err: budgetSpent{}}
	stub := &gatedStub{id: "budget-breaker-stub"}
	p, _ := gatedChain(t, stub, gate, &ResilienceConfig{Threshold: 1, OpenFor: time.Hour, Attempts: 3, BaseDelay: time.Millisecond})
	for range 3 {
		if _, err := p.Chat(context.Background(), Request{Model: "m"}); !errors.Is(err, budgetSpent{}) {
			t.Fatalf("Chat: %v", err)
		}
	}
	cb := p.(*auditedProvider).inner.(*TracedProvider).Provider.(*RetryingProvider).Provider.(*CircuitBreakerProvider)
	st := cb.stateFor("m")
	st.mu.Lock()
	state, failures := st.state, st.failures
	st.mu.Unlock()
	if state != circuitClosed || failures != 0 {
		t.Errorf("breaker state %v with %d failures after three refusals, want closed and 0", state, failures)
	}
	if stub.chats != 0 {
		t.Errorf("the provider was called %d times (a retried refusal?)", stub.chats)
	}
	// And once admitted, the breaker sees the call as any other.
	gate.err = nil
	if _, err := p.Chat(context.Background(), Request{Model: "m"}); err != nil || stub.chats != 1 {
		t.Errorf("Chat after the refusals: %v, calls %d", err, stub.chats)
	}
}
