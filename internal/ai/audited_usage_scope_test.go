package ai

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// scopeStub is a raw provider that answers every call with fixed usage.
type scopeStub struct {
	Provider
	id string
}

func (s *scopeStub) ID() string                  { return s.id }
func (s *scopeStub) EstimateTokens(t string) int { return len(t) }
func (s *scopeStub) DefaultModel() string        { return "m" }
func (s *scopeStub) Models(context.Context) ([]ModelInfo, error) {
	return []ModelInfo{{ID: "m"}}, nil
}
func (s *scopeStub) Chat(context.Context, Request) (*Response, error) {
	return &Response{Content: "ok", TokensIn: 10, TokensOut: 2}, nil
}
func (s *scopeStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	onChunk(Chunk{Text: "ok"})
	onChunk(Chunk{Done: true, TokensIn: 10, TokensOut: 2})
	return nil
}
func (s *scopeStub) Embed(_ context.Context, text []string) ([][]float32, error) {
	out := make([][]float32, len(text))
	for i := range out {
		out[i] = []float32{1, 0}
	}
	return out, nil
}

// scopeChain builds the factory chain for stub, with every usage row forwarded to the channel.
func scopeChain(t *testing.T, stub *scopeStub) (Provider, <-chan *interfaces.UsageMetric) {
	t.Helper()
	RegisterProvider(stub.id, func(string) Provider { return stub })
	ch := make(chan *interfaces.UsageMetric, 8)
	rec := func(_ context.Context, m *interfaces.UsageMetric) error { ch <- m; return nil }
	f := NewProviderFactory(func(string, string) (string, error) { return "k", nil }, nil, rec, nil)
	p, err := f.For(context.Background(), "u1", stub.id)
	if err != nil {
		t.Fatal(err)
	}
	return p, ch
}

func nextRow(t *testing.T, ch <-chan *interfaces.UsageMetric) *interfaces.UsageMetric {
	t.Helper()
	select {
	case m := <-ch:
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("no usage row")
		return nil
	}
}

// The context's usage scope names who a call is for, whatever the chain's credential: an org corpus embedded
// through a MEMBER's key records the org. A request org beats the chain's credential, and the context beats
// both, on every call kind: the row records the scope and org the call was attributed to (R-budget-15). Without a
// context scope, the chain's resolution stands.
func TestAudited_UsageScopeFromContextBeatsTheChain(t *testing.T) {
	p, rows := scopeChain(t, &scopeStub{id: "usage-scope-stub"})

	if _, err := p.Embed(WithUsageScope(context.Background(), "u1", "org-7"), []string{"chunk"}); err != nil {
		t.Fatal(err)
	}
	if m := nextRow(t, rows); m.OrgID != "org-7" || m.UserID != "u1" {
		t.Errorf("usage row = org %q user %q, want org-7 / u1", m.OrgID, m.UserID)
	}

	if _, err := p.Chat(WithUsageScope(context.Background(), "u1", "org-8"), Request{Model: "m", OrgID: "org-1"}); err != nil {
		t.Fatal(err)
	}
	if m := nextRow(t, rows); m.OrgID != "org-8" || m.UserID != "u1" {
		// The context's scope beats the Request's org: the row names who the call was attributed to.
		t.Errorf("chat usage row = org %q user %q, want the context's org-8 / u1", m.OrgID, m.UserID)
	}

	if _, err := p.Chat(context.Background(), Request{Model: "m", OrgID: "org-1"}); err != nil {
		t.Fatal(err)
	}
	if m := nextRow(t, rows); m.OrgID != "org-1" || m.UserID != "u1" {
		t.Errorf("chat without a context scope recorded org %q user %q, want the request's org-1 / u1", m.OrgID, m.UserID)
	}

	if _, err := p.Embed(context.Background(), []string{"chunk"}); err != nil {
		t.Fatal(err)
	}
	if m := nextRow(t, rows); m.OrgID != "" || m.UserID != "u1" {
		t.Errorf("embed without a context scope recorded org %q user %q, want the member's own row", m.OrgID, m.UserID)
	}
}

// A usage scope naming another scope than the chain's (a service scope running on a member-built chain) is what
// the row records, scope and org, on Chat, Stream and Embed alike (R-budget-15): the row says who the call was
// attributed to, never the chain's own scope beside the context's org.
func TestAudited_UsageScopeNamesTheRowsUserAndOrg(t *testing.T) {
	p, rows := scopeChain(t, &scopeStub{id: "usage-scope-svc-stub"})
	ctx := WithUsageScope(context.Background(), "svc", "org-5")

	if _, err := p.Chat(ctx, Request{Model: "m", OrgID: "org-1"}); err != nil {
		t.Fatal(err)
	}
	if m := nextRow(t, rows); m.UserID != "svc" || m.OrgID != "org-5" {
		t.Errorf("chat row = user %q org %q, want svc / org-5", m.UserID, m.OrgID)
	}
	if err := p.Stream(ctx, Request{Model: "m", OrgID: "org-1"}, func(Chunk) {}); err != nil {
		t.Fatal(err)
	}
	if m := nextRow(t, rows); m.UserID != "svc" || m.OrgID != "org-5" {
		t.Errorf("stream row = user %q org %q, want svc / org-5", m.UserID, m.OrgID)
	}
	if _, err := p.Embed(ctx, []string{"chunk"}); err != nil {
		t.Fatal(err)
	}
	if m := nextRow(t, rows); m.UserID != "svc" || m.OrgID != "org-5" {
		t.Errorf("embed row = user %q org %q, want svc / org-5", m.UserID, m.OrgID)
	}
}
