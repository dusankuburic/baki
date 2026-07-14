package testutil

import (
	"context"

	"pad-analyzer/internal/ai"
)

// FakeTurn scripts one streamed assistant turn for FakeProvider: optional text
// (or multi-part text via Parts, e.g. to split a secret across chunks) streamed
// first, then a terminal Done chunk carrying any tool calls and token counts.
type FakeTurn struct {
	Text      string
	Parts     []string // multiple text chunks in one turn; overrides Text when set
	ToolCalls []ai.ToolCall
	TokensIn  int
	TokensOut int
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
	calls    int
}

// Calls reports how many times Stream has been invoked.
func (p *FakeProvider) Calls() int { return p.calls }

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

func (p *FakeProvider) Stream(_ context.Context, _ ai.Request, onChunk func(ai.Chunk)) error {
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
	onChunk(ai.Chunk{Done: true, ToolCalls: turn.ToolCalls, TokensIn: turn.TokensIn, TokensOut: turn.TokensOut})
	return nil
}
