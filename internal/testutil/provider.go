package testutil

import (
	"context"
	"fmt"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-core/ai/scrubber"
)

// FakeTurn scripts one streamed assistant turn for FakeProvider: optional text
// (or multi-part text via Parts, e.g. to split a secret across chunks) streamed
// first, then a terminal Done chunk carrying any tool calls, token counts and
// the finish reason (empty: a normal stop).
type FakeTurn struct {
	Text         string
	Parts        []string // multiple text chunks in one turn; overrides Text when set
	ToolCalls    []ai.ToolCall
	TokensIn     int
	TokensOut    int
	FinishReason string
}

// FakeProvider is a shared, scriptable ai.Provider for tests. Each Stream call
// consumes the next FakeTurn; once Turns is exhausted it emits Fallback
// (repeated) when set, otherwise a terminal Done with no tool calls. Set Err to
// make Stream fail outright. Ctx overrides ContextLimit (0 => a large default
// that never trips truncation).
type FakeProvider struct {
	Turns    []FakeTurn
	Fallback *FakeTurn
	Err      error
	Ctx      int
	Tools    bool
	// RequireScrubbed, when set, fails the test for every string of an outgoing request that is not already
	// its own scrubber.ScrubText (UnscrubbedStrings): the chat scrubs each string where it is built and the
	// provider boundary does not scrub again, so a string the scrubber would still change here is one no
	// call site scrubbed. Reported from Stream's goroutine, so the test must wait for its stream to finish.
	RequireScrubbed testing.TB
	calls           int
	requests        []ai.Request
}

// Calls reports how many times Stream has been invoked.
func (p *FakeProvider) Calls() int { return p.calls }

// Requests returns every request Stream received, in order (the messages slice is copied: callers reuse it).
func (p *FakeProvider) Requests() []ai.Request { return p.requests }

func (p *FakeProvider) ID() string                                     { return "fake" }
func (p *FakeProvider) Name() string                                   { return "Fake" }
func (p *FakeProvider) Models(context.Context) ([]ai.ModelInfo, error) { return nil, nil }
func (p *FakeProvider) DefaultModel() string                           { return "fake-model" }
func (p *FakeProvider) FreeModel() string                              { return "fake-model" }
func (p *FakeProvider) PricePerMillionTokens() ai.Pricing              { return ai.Pricing{} }
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

func (p *FakeProvider) Stream(_ context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	recorded := req
	recorded.Messages = append([]ai.Message(nil), req.Messages...)
	p.requests = append(p.requests, recorded)
	if p.RequireScrubbed != nil {
		for _, s := range UnscrubbedStrings(req) {
			p.RequireScrubbed.Errorf("request %d carries a string the scrubber would still change: %s", len(p.requests), s)
		}
	}
	if p.Err != nil {
		return p.Err
	}
	i := p.calls
	p.calls++

	var turn FakeTurn
	switch {
	case i < len(p.Turns):
		turn = p.Turns[i]
	case p.Fallback != nil:
		turn = *p.Fallback
	}

	switch {
	case len(turn.Parts) > 0:
		for _, part := range turn.Parts {
			onChunk(ai.Chunk{Text: part})
		}
	case turn.Text != "":
		onChunk(ai.Chunk{Text: turn.Text})
	}
	onChunk(ai.Chunk{Done: true, ToolCalls: turn.ToolCalls, TokensIn: turn.TokensIn, TokensOut: turn.TokensOut, FinishReason: turn.FinishReason})
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
