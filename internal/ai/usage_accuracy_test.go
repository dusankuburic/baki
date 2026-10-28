package ai

// Step 7 of the AI roadmap: the token counts the usage views show.
//   - R-budget-1: a retried attempt's usage is recorded, not lost to the attempt that followed it.
//   - R-budget-3: the no-usage estimate counts the tool schemas and the tool-call arguments, and estimates the
//     side a provider left at zero.
//   - R-budget-12: a failed usage save is retried once and counted.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// attemptsProvider replays one scripted Stream (or Chat) per call.
type attemptsProvider struct {
	servedFakeProvider
	streams [][]Chunk
	chats   []func() (*Response, error)
	calls   int
}

func (p *attemptsProvider) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	i := p.calls
	p.calls++
	for _, c := range p.streams[i] {
		onChunk(c)
	}
	return nil
}

func (p *attemptsProvider) Chat(context.Context, Request) (*Response, error) {
	i := p.calls
	p.calls++
	return p.chats[i]()
}

// chain is the factory's wrapper order, without the breaker: audited → retry → p.
func usageChain(p Provider, sink *usageSink) Provider {
	rp := NewRetryingProviderWithConfig(p, 3, time.Millisecond)
	ap := newAuditedProvider(rp, sink.recorder(), "u1", p.ID())
	ap.adoptRawHints(p)
	return ap
}

// drain collects every row recorded within a short wait.
func (s *usageSink) drain(t *testing.T) []interfaces.UsageMetric {
	t.Helper()
	var rows []interfaces.UsageMetric
	for {
		select {
		case m := <-s.ch:
			rows = append(rows, m)
		case <-time.After(300 * time.Millisecond):
			return rows
		}
	}
}

func sumRows(rows []interfaces.UsageMetric) (in, out int) {
	for _, r := range rows {
		in += r.PromptTokens
		out += r.CompletionTokens
	}
	return in, out
}

// R-budget-1, done when: a first attempt's pre-content error with 1000 input tokens and a second attempt ending
// Done(500,100) record 1500 input tokens in total.
func TestUsage_RetriedStreamAttemptIsRecorded(t *testing.T) {
	p := &attemptsProvider{servedFakeProvider: servedFakeProvider{id: "openai"}, streams: [][]Chunk{
		{{TokensIn: 1000}, {Error: ErrProviderDown, TokensIn: 1000}},
		{{TokensIn: 500}, {Text: "ok"}, {Done: true, TokensIn: 500, TokensOut: 100}},
	}}
	sink := newUsageSink()
	var forwarded []Chunk
	if err := usageChain(p, sink).Stream(context.Background(), Request{Model: "gpt-4o"}, func(c Chunk) { forwarded = append(forwarded, c) }); err != nil {
		t.Fatal(err)
	}
	in, out := sumRows(sink.drain(t))
	if in != 1500 || out != 100 {
		t.Errorf("recorded %d in / %d out, want 1500 / 100", in, out)
	}
	for _, c := range forwarded {
		if c.attemptDiscarded {
			t.Error("the discarded-attempt marker reached the caller")
		}
	}
}

// R-budget-1, done when: a Chat that fails with 300 held input tokens and then succeeds records the 300 too.
func TestUsage_RetriedChatAttemptIsRecorded(t *testing.T) {
	p := &attemptsProvider{servedFakeProvider: servedFakeProvider{id: "openai"}, chats: []func() (*Response, error){
		func() (*Response, error) { return nil, &heldUsageError{err: ErrProviderDown, tokensIn: 300} },
		func() (*Response, error) { return &Response{Content: "ok", TokensIn: 100, TokensOut: 10}, nil },
	}}
	sink := newUsageSink()
	if _, err := usageChain(p, sink).Chat(context.Background(), Request{Model: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	if in, out := sumRows(sink.drain(t)); in != 400 || out != 10 {
		t.Errorf("recorded %d in / %d out, want 400 / 10", in, out)
	}
}

// R-budget-1, done when: the sequences that worked keep their usage — failed then failed, and a held error
// without usage followed by a success — with no double count of the last attempt.
func TestUsage_RetryKeepsTheOtherSequences(t *testing.T) {
	p := &attemptsProvider{servedFakeProvider: servedFakeProvider{id: "openai"}, streams: [][]Chunk{
		{{TokensIn: 200}, {Error: ErrProviderDown, TokensIn: 200}},
		{{TokensIn: 200}, {Error: ErrProviderDown, TokensIn: 200}},
		{{TokensIn: 200}, {Error: ErrProviderDown, TokensIn: 200}},
	}}
	sink := newUsageSink()
	_ = usageChain(p, sink).Stream(context.Background(), Request{Model: "gpt-4o"}, func(Chunk) {})
	if in, _ := sumRows(sink.drain(t)); in != 600 {
		t.Errorf("three failed attempts recorded %d input tokens, want 600", in)
	}

	p = &attemptsProvider{servedFakeProvider: servedFakeProvider{id: "openai"}, streams: [][]Chunk{
		{{Error: ErrProviderDown}},
		{{Text: "ok"}, {Done: true, TokensIn: 50, TokensOut: 5}},
	}}
	sink = newUsageSink()
	_ = usageChain(p, sink).Stream(context.Background(), Request{Model: "gpt-4o"}, func(Chunk) {})
	if in, out := sumRows(sink.drain(t)); in != 50 || out != 5 {
		t.Errorf("a usage-less failure then success recorded %d / %d, want 50 / 5", in, out)
	}
}

func eightTools() []ToolDefinition {
	tools := make([]ToolDefinition, 8)
	for i := range tools {
		tools[i] = ToolDefinition{Name: "tool_" + string(rune('a'+i)), Description: "does a thing to the flow",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"blockId":{"type":"string","description":"the block"}}}`)}
	}
	return tools
}

// R-budget-3, done when: the request estimate counts the tool schemas, and the memo tells the two apart.
func TestUsage_EstimateCountsToolSchemas(t *testing.T) {
	ap := &auditedProvider{inner: &servedFakeProvider{id: "openai"}, providerID: "openai"}
	base := Request{SystemPrompt: "sys", Messages: []Message{{Role: "user", Content: "hi"}}}
	withTools := base
	withTools.Tools = eightTools()
	schemas := 0
	for _, td := range withTools.Tools {
		schemas += len(td.InputSchema)
	}
	if got, without := ap.estimateRequestTokens(withTools), ap.estimateRequestTokens(base); got-without < schemas {
		t.Errorf("estimate with tools %d, without %d: want at least the schemas' %d more", got, without, schemas)
	}
	if requestEstimateHash("openai", base) == requestEstimateHash("openai", withTools) {
		t.Error("the estimate memo keys a request with tools like one without")
	}
}

// R-budget-3, done when: a Done(0,0) stream ending in one 2 KB tool call records the call's estimate as output and
// the schemas in the input; a Done(500, 0) stream with text gets an output estimate.
func TestUsage_EstimateCountsToolCallsAndTheMissingSide(t *testing.T) {
	args := json.RawMessage(`{"fixes":"` + strings.Repeat("x", 2048) + `"}`)
	p := &attemptsProvider{servedFakeProvider: servedFakeProvider{id: "glm"}, streams: [][]Chunk{
		{{Done: true, ToolCalls: []ToolCall{{ID: "c1", Name: "apply_fixes", Input: args}}}},
		{{Text: strings.Repeat("y", 1024)}, {Done: true, TokensIn: 500}},
	}}
	sink := newUsageSink()
	chain := usageChain(p, sink)
	req := Request{Model: "glm-5", Tools: eightTools(), Messages: []Message{{Role: "user", Content: "fix"}}}
	_ = chain.Stream(context.Background(), req, func(Chunk) {})
	rows := sink.drain(t)
	if len(rows) != 1 || rows[0].CompletionTokens < len(args) || rows[0].PromptTokens < len(req.Tools[0].InputSchema)*8 {
		t.Errorf("tool-call turn recorded %+v, want the arguments as output and the schemas in the input", rows)
	}
	_ = chain.Stream(context.Background(), req, func(Chunk) {})
	rows = sink.drain(t)
	if len(rows) != 1 || rows[0].PromptTokens != 500 || rows[0].CompletionTokens < 1024 {
		t.Errorf("Done(500, 0) recorded %+v, want 500 in and the text's estimate out", rows)
	}
}

// R-budget-12, done when: a failed save is retried once and counted in pad_usage_save_failed_total.
func TestUsage_FailedSaveIsRetriedAndCounted(t *testing.T) {
	var calls atomic.Int32
	failures := testutil.ToFloat64(usageSaveFailedTotal.WithLabelValues("retried"))
	lost := testutil.ToFloat64(usageSaveFailedTotal.WithLabelValues("lost"))
	recorded := make(chan struct{}, 1)
	ap := newAuditedProvider(&servedFakeProvider{id: "openai"}, func(context.Context, *interfaces.UsageMetric) error {
		if calls.Add(1) == 1 {
			return errors.New("database is restarting")
		}
		recorded <- struct{}{}
		return nil
	}, "u1", "openai")
	ap.saveRetryDelay = time.Millisecond
	if _, err := ap.Chat(context.Background(), Request{Model: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-recorded:
	case <-time.After(3 * time.Second):
		t.Fatal("the row was not saved on the retry")
	}
	if got := testutil.ToFloat64(usageSaveFailedTotal.WithLabelValues("retried")); got != failures+1 {
		t.Errorf("retried saves counted %v, want %v", got, failures+1)
	}

	ap.recorder = func(context.Context, *interfaces.UsageMetric) error { return errors.New("down") }
	if _, err := ap.Chat(context.Background(), Request{Model: "gpt-4o"}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for testutil.ToFloat64(usageSaveFailedTotal.WithLabelValues("lost")) != lost+1 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if got := testutil.ToFloat64(usageSaveFailedTotal.WithLabelValues("lost")); got != lost+1 {
		t.Errorf("lost saves counted %v, want %v", got, lost+1)
	}
}

// Review: a marker followed by an attempt that never reached the provider records no estimate on top.
func TestUsage_DiscardedThenUnreachedRecordsNoEstimate(t *testing.T) {
	p := &attemptsProvider{servedFakeProvider: servedFakeProvider{id: "claude"}, streams: [][]Chunk{
		{{TokensIn: 1000}, {Error: ErrProviderDown, TokensIn: 1000}},
		{}, // nothing at all: a refused connection, an open circuit
		{},
	}}
	sink := newUsageSink()
	req := Request{Model: "claude-x", SystemPrompt: strings.Repeat("s", 5000)}
	_ = usageChain(p, sink).Stream(context.Background(), req, func(Chunk) {})
	if in, _ := sumRows(sink.drain(t)); in != 1000 {
		t.Errorf("recorded %d input tokens, want the 1000 billed (no estimate for unreached attempts)", in)
	}
}

// Review: Chat counts each attempt once — when retries run out, and when the backoff gives up early.
func TestUsage_ChatCountsEachAttemptOnce(t *testing.T) {
	fail := func() (*Response, error) { return nil, &heldUsageError{err: ErrProviderDown, tokensIn: 100} }
	p := &attemptsProvider{servedFakeProvider: servedFakeProvider{id: "openai"}, chats: []func() (*Response, error){fail, fail, fail}}
	sink := newUsageSink()
	_, _ = usageChain(p, sink).Chat(context.Background(), Request{Model: "gpt-4o"})
	if in, _ := sumRows(sink.drain(t)); in != 300 {
		t.Errorf("three failed attempts recorded %d, want 300", in)
	}

	longWait := func() (*Response, error) {
		return nil, &heldUsageError{err: &RateLimitError{RetryAfter: time.Hour}, tokensIn: 100}
	}
	p = &attemptsProvider{servedFakeProvider: servedFakeProvider{id: "openai"}, chats: []func() (*Response, error){longWait}}
	sink = newUsageSink()
	_, _ = usageChain(p, sink).Chat(context.Background(), Request{Model: "gpt-4o"})
	if in, _ := sumRows(sink.drain(t)); in != 100 {
		t.Errorf("a backoff that gave up recorded %d, want 100", in)
	}
}

// Review: a Stop during the backoff keeps the held error's usage.
func TestUsage_StreamCancelDuringBackoffKeepsHeldUsage(t *testing.T) {
	p := &attemptsProvider{servedFakeProvider: servedFakeProvider{id: "openai"}, streams: [][]Chunk{
		{{Error: ErrProviderDown, TokensIn: 1000}},
		{{Text: "never"}},
	}}
	sink := newUsageSink()
	rp := NewRetryingProviderWithConfig(p, 3, time.Second)
	ap := newAuditedProvider(rp, sink.recorder(), "u1", "openai")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_ = ap.Stream(ctx, Request{Model: "gpt-4o"}, func(Chunk) {})
	if in, _ := sumRows(sink.drain(t)); in != 1000 {
		t.Errorf("recorded %d, want the held 1000", in)
	}
}
