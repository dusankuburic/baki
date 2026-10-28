package testutil

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-core/ai/scrubber"
)

// FakeTurn scripts one streamed assistant turn for FakeProvider: optional text
// (or multi-part text via Parts, e.g. to split a secret across chunks) streamed
// first, then a terminal Done chunk carrying any tool calls, token counts, the
// served model and the finish reason (empty: a normal stop).
type FakeTurn struct {
	Text         string
	Parts        []string // multiple text chunks in one turn; overrides Text when set
	ToolCalls    []ai.ToolCall
	TokensIn     int
	TokensOut    int
	FinishReason string
	ServedModel  string
	// ProviderItems is the turn's replay record, on its Done chunk (ai.Chunk.ProviderItems).
	ProviderItems json.RawMessage
	// Err fails this call before any chunk; the next call takes the next turn.
	Err error
	// ChunkErr ends the turn, after its text, with an error chunk carrying the turn's token counts and served
	// model in place of Done: how a provider reports a failure mid-stream.
	ChunkErr error
	// Block parks the turn, after its text, until the stream's context ends, then returns the context's error.
	Block bool
}

// FakeProvider is a shared, scriptable ai.Provider for tests. Each Stream call
// consumes the next FakeTurn; once Turns is exhausted it emits Fallback
// (repeated) when set, otherwise a terminal Done with no tool calls. Set Err to
// make every Stream fail outright. Ctx overrides ContextLimit (0 => a large
// default that never trips truncation). Calls, Requests and Record are safe
// while a chat worker streams on another goroutine.
type FakeProvider struct {
	Turns    []FakeTurn
	Fallback *FakeTurn
	Err      error
	Ctx      int
	Tools    bool
	// HonourCtx makes a call on an ended context fail with the context's error before any chunk, as a real
	// provider's HTTP call does. Off by default: some tests rely on a fake that streams whatever the context.
	HonourCtx bool
	// RequireScrubbed, when set, fails the test for every string of an outgoing request that is not already
	// its own scrubber.ScrubText (UnscrubbedStrings): the chat scrubs each string where it is built and the
	// provider boundary does not scrub again, so a string the scrubber would still change here is one no
	// call site scrubbed. Reported from Stream's goroutine, so the test must wait for its stream to finish.
	RequireScrubbed testing.TB

	mu       sync.Mutex
	calls    int
	requests []ai.Request
}

// Calls reports how many times Stream has consumed a turn.
func (p *FakeProvider) Calls() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls
}

// Requests returns a copy of every request Stream (or Record) received, in order.
func (p *FakeProvider) Requests() []ai.Request {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]ai.Request(nil), p.requests...)
}

// Record keeps a copy of req (its messages slice copied: callers reuse it) and answers its 0-based index. A stub
// that scripts its own chunks calls it so the shared recorder still sees every request.
func (p *FakeProvider) Record(req ai.Request) int {
	recorded := req
	recorded.Messages = append([]ai.Message(nil), req.Messages...)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.requests = append(p.requests, recorded)
	return len(p.requests) - 1
}

func (p *FakeProvider) ID() string                                     { return "fake" }
func (p *FakeProvider) Name() string                                   { return "Fake" }
func (p *FakeProvider) Models(context.Context) ([]ai.ModelInfo, error) { return nil, nil }
func (p *FakeProvider) DefaultModel() string                           { return "fake-model" }
func (p *FakeProvider) FreeModel() string                              { return "fake-model" }
func (p *FakeProvider) SupportsTools() bool                            { return p.Tools }
func (p *FakeProvider) EstimateTokens(t string) int                    { return len(t) / 4 }

func (p *FakeProvider) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }

func (p *FakeProvider) ContextLimit() int {
	if p.Ctx > 0 {
		return p.Ctx
	}
	return 1 << 20
}

func (p *FakeProvider) Chat(context.Context, ai.Request) (*ai.Response, error) {
	return &ai.Response{}, nil
}

func (p *FakeProvider) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	n := p.Record(req) + 1
	if p.RequireScrubbed != nil {
		for _, s := range UnscrubbedStrings(req) {
			p.RequireScrubbed.Errorf("request %d carries a string the scrubber would still change: %s", n, s)
		}
	}
	if p.Err != nil {
		return p.Err
	}
	if p.HonourCtx {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	p.mu.Lock()
	i := p.calls
	p.calls++
	p.mu.Unlock()

	var turn FakeTurn
	switch {
	case i < len(p.Turns):
		turn = p.Turns[i]
	case p.Fallback != nil:
		turn = *p.Fallback
	}
	if turn.Err != nil {
		return turn.Err
	}

	switch {
	case len(turn.Parts) > 0:
		for _, part := range turn.Parts {
			onChunk(ai.Chunk{Text: part})
		}
	case turn.Text != "":
		onChunk(ai.Chunk{Text: turn.Text})
	}
	switch {
	case turn.Block:
		<-ctx.Done()
		return ctx.Err()
	case turn.ChunkErr != nil:
		onChunk(ai.Chunk{Error: turn.ChunkErr, TokensIn: turn.TokensIn, TokensOut: turn.TokensOut, ServedModel: turn.ServedModel})
		return nil
	}
	onChunk(ai.Chunk{Done: true, ToolCalls: turn.ToolCalls, TokensIn: turn.TokensIn, TokensOut: turn.TokensOut,
		FinishReason: turn.FinishReason, ServedModel: turn.ServedModel, ProviderItems: turn.ProviderItems})
	return nil
}

// RequestString is one string of an outgoing request and the path it sits at.
type RequestString struct {
	Path string
	Text string
}

// RequestStrings lists every string of req a provider would send: the system prompt, each message's content
// and tool-call arguments, and the tool definitions.
func RequestStrings(req ai.Request) []RequestString {
	out := []RequestString{{Path: "systemPrompt", Text: req.SystemPrompt}}
	for i, m := range req.Messages {
		out = append(out, RequestString{Path: fmt.Sprintf("messages[%d].content (%s)", i, m.Role), Text: m.Content})
		for j, tc := range m.ToolCalls {
			out = append(out, RequestString{Path: fmt.Sprintf("messages[%d].toolCalls[%d].input (%s)", i, j, tc.Name), Text: string(tc.Input)})
		}
	}
	for i, td := range req.Tools {
		out = append(out,
			RequestString{Path: fmt.Sprintf("tools[%d].description (%s)", i, td.Name), Text: td.Description},
			RequestString{Path: fmt.Sprintf("tools[%d].inputSchema (%s)", i, td.Name), Text: string(td.InputSchema)})
	}
	return out
}

// UnscrubbedStrings reports the strings of req that scrubber.ScrubText would still change — strings no call
// site scrubbed — each as its path and the text around the first change.
func UnscrubbedStrings(req ai.Request) []string {
	var out []string
	for _, s := range RequestStrings(req) {
		if scrubbed := scrubber.ScrubText(s.Text); scrubbed != s.Text {
			out = append(out, s.Path+": "+unscrubbedExcerpt(s.Text, scrubbed))
		}
	}
	return out
}

// unscrubbedExcerpt is the text around the first byte at which scrubbed departs from text.
func unscrubbedExcerpt(text, scrubbed string) string {
	i := 0
	for i < len(text) && i < len(scrubbed) && text[i] == scrubbed[i] {
		i++
	}
	return fmt.Sprintf("%q", text[max(i-40, 0):min(i+80, len(text))])
}
