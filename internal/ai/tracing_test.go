package ai

// R-observability-9: a provider span says how the call ended — status, error class, served model, usage — for an
// error chunk as well as a returned error, and through the factory chain where the retry wrapper holds a
// pre-content error chunk back.

import (
	"context"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// recordSpans installs an in-memory tracer provider for the test and returns its recorder.
func recordSpans(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	prev := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() {
		otel.SetTracerProvider(prev)
		_ = tp.Shutdown(context.Background())
	})
	return sr
}

func spanAttrs(s sdktrace.ReadOnlySpan) map[attribute.Key]attribute.Value {
	out := map[attribute.Key]attribute.Value{}
	for _, kv := range s.Attributes() {
		out[kv.Key] = kv.Value
	}
	return out
}

func lastSpan(t *testing.T, sr *tracetest.SpanRecorder, name string) sdktrace.ReadOnlySpan {
	t.Helper()
	spans := sr.Ended()
	for i := len(spans) - 1; i >= 0; i-- {
		if spans[i].Name() == name {
			return spans[i]
		}
	}
	t.Fatalf("no span %q among %d", name, len(spans))
	return nil
}

func exceptionEvents(s sdktrace.ReadOnlySpan) int {
	n := 0
	for _, e := range s.Events() {
		if e.Name == "exception" {
			n++
		}
	}
	return n
}

func TestTracedProvider_StreamErrorChunk(t *testing.T) {
	sr := recordSpans(t)
	stub := &cbStreamStub{chunks: []Chunk{{Error: ErrContentFiltered, TokensIn: 10}}}
	tp := NewTracedProvider(stub)
	if err := tp.Stream(context.Background(), Request{Model: "m"}, func(Chunk) {}); err != nil {
		t.Fatalf("Stream returned %v, want nil (the error is a chunk)", err)
	}
	s := lastSpan(t, sr, "stream-stub.Stream")
	a := spanAttrs(s)
	if s.Status().Code != codes.Error || exceptionEvents(s) != 1 {
		t.Errorf("status %v with %d exception events, want Error and 1", s.Status(), exceptionEvents(s))
	}
	if a["ai.error_class"].AsString() != "content_filtered" || a["ai.tokens_in"].AsInt64() != 10 {
		t.Errorf("attributes = %v, want error_class content_filtered and tokens_in 10", a)
	}
}

func TestTracedProvider_StreamSuccessCarriesServedModelAndUsage(t *testing.T) {
	sr := recordSpans(t)
	stub := &cbStreamStub{chunks: []Chunk{{Text: "hi"}, {Done: true, TokensIn: 7, TokensOut: 3, CacheReadTokens: 4, ServedModel: "gpt-4o-2024-11-20", FinishReason: "stop"}}}
	if err := NewTracedProvider(stub).Stream(context.Background(), Request{Model: "gpt-4o"}, func(Chunk) {}); err != nil {
		t.Fatal(err)
	}
	s := lastSpan(t, sr, "stream-stub.Stream")
	a := spanAttrs(s)
	if a["ai.served_model"].AsString() != "gpt-4o-2024-11-20" || a["ai.tokens_in"].AsInt64() != 7 || a["ai.tokens_out"].AsInt64() != 3 ||
		a["ai.cache_read_tokens"].AsInt64() != 4 || a["ai.finish_reason"].AsString() != "stop" {
		t.Errorf("attributes = %v", a)
	}
	if s.Status().Code == codes.Error {
		t.Errorf("a successful stream has status %v", s.Status())
	}
}

// Through the factory chain the retry wrapper holds a pre-content error chunk back and returns it as the error,
// with its usage on heldUsageError: the span still says content_filtered and carries the tokens.
func TestTracedProvider_HeldErrorThroughTheChain(t *testing.T) {
	sr := recordSpans(t)
	resetBreakerRegistry()
	t.Cleanup(resetBreakerRegistry)
	stub := &estimatingStreamStub{cbStreamStub{chunks: []Chunk{{Error: ErrContentFiltered, TokensIn: 12}}}}
	f := NewProviderFactory(nil, nil, nil, nil)
	chain := f.wrapChainScoped(stub, "u1", "u1", "", "stream-stub")
	_ = chain.Stream(context.Background(), Request{Model: "m"}, func(Chunk) {})
	s := lastSpan(t, sr, "stream-stub.Stream")
	a := spanAttrs(s)
	if s.Status().Code != codes.Error || a["ai.error_class"].AsString() != "content_filtered" || a["ai.tokens_in"].AsInt64() != 12 {
		t.Errorf("status %v, attributes %v; want Error, content_filtered and tokens_in 12", s.Status(), a)
	}
}

func TestTracedProvider_ChatEmbedAndModels(t *testing.T) {
	sr := recordSpans(t)
	tp := NewTracedProvider(newCBStub(ErrRateLimited))
	if _, err := tp.Chat(context.Background(), Request{Model: "m"}); err == nil {
		t.Fatal("want the stub's error")
	}
	s := lastSpan(t, sr, "stub.Chat")
	if s.Status().Code != codes.Error || spanAttrs(s)["ai.error_class"].AsString() != "rate_limited" {
		t.Errorf("Chat span: status %v, attributes %v", s.Status(), spanAttrs(s))
	}

	tpOK := NewTracedProvider(okEmbedStub{})
	if _, err := tpOK.Embed(context.Background(), []string{"x"}); err != nil {
		t.Fatal(err)
	}
	if s := lastSpan(t, sr, "embed-ok.Embed"); s.Status().Code == codes.Error {
		t.Errorf("Embed span status %v", s.Status())
	}
	_, _ = tpOK.Models(context.Background())
	lastSpan(t, sr, "embed-ok.Models")
}

type okEmbedStub struct{ Provider }

func (okEmbedStub) ID() string { return "embed-ok" }
func (okEmbedStub) Embed(context.Context, []string) ([][]float32, error) {
	return [][]float32{{1}}, nil
}
func (okEmbedStub) Models(context.Context) ([]ModelInfo, error) { return nil, nil }

type countStub struct{ n int }

func (c countStub) CountTokens(context.Context, Request) (int, error) { return c.n, nil }

// CountTokens runs on the raw provider, outside the traced chain: the audited wrapper spans it.
func TestAudited_CountTokensIsSpanned(t *testing.T) {
	sr := recordSpans(t)
	ap := &auditedProvider{providerID: "count-test", tokenCounter: countStub{n: 42}}
	if n, err := ap.CountTokens(context.Background(), Request{Model: "m"}); err != nil || n != 42 {
		t.Fatalf("CountTokens = %d, %v", n, err)
	}
	s := lastSpan(t, sr, "count-test.CountTokens")
	if spanAttrs(s)["ai.tokens_in"].AsInt64() != 42 {
		t.Errorf("attributes = %v", spanAttrs(s))
	}
}

// A user's Stop keeps its class on the span but is not an error status.
func TestTracedProvider_CancelIsNotAnError(t *testing.T) {
	sr := recordSpans(t)
	tp := NewTracedProvider(newCBStub(context.Canceled))
	_, _ = tp.Chat(context.Background(), Request{})
	s := lastSpan(t, sr, "stub.Chat")
	if s.Status().Code == codes.Error || spanAttrs(s)["ai.error_class"].AsString() != "canceled" {
		t.Errorf("cancelled Chat span: status %v, attributes %v", s.Status(), spanAttrs(s))
	}
}
