package service

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
)

// TestTruncateForContextWindow_ToolOverhead pins R4b: the window estimate
// counts the tool schemas riding every request AND each assistant tool_use's
// argument JSON — a content-only estimate undercounted a tool loop by the
// schema (~1000 tokens) plus accumulated args, letting requests pass the
// guard and 400 at the provider.
func TestTruncateForContextWindow_ToolOverhead(t *testing.T) {
	// FakeProvider.EstimateTokens = len/4.
	mkReq := func() *ai.Request {
		return &ai.Request{
			Messages: []ai.Message{
				{Role: "user", Content: stringRepeat("q", 400)}, // 100 tokens
				{Role: "assistant", Content: "", ToolCalls: []ai.ToolCall{{ID: "t1", Name: "search_flow", Input: []byte(stringRepeat(`{"query":"x"}`, 150))}}}, // args ~300 tokens
				{Role: "tool", Content: stringRepeat("r", 1200)}, // 300 tokens
			},
		}
	}
	provider := &testutil.FakeProvider{}
	const ctxLimit = 5000

	// Control: without tools the conversation fits untouched.
	req := mkReq()
	if _, err := truncateForContextWindow(context.Background(), provider, req, ctxLimit, 0); err != nil {
		t.Fatalf("no-tools case should fit: %v", err)
	}
	if len(req.Messages) != 3 {
		t.Errorf("no-tools case must not truncate, got %d messages", len(req.Messages))
	}

	// With a tool schema attached (~500 tokens), the same conversation must
	// now overflow and shed the assistant+tool turn (the junction repair
	// drops the orphaned tool result with its assistant). The schema is sized
	// so the pinned user turn + schemas still fit — a request whose TOOLS
	// alone overflow the window fails outright (different branch).
	req = mkReq()
	req.Tools = []ai.ToolDefinition{{Name: "search_flow", InputSchema: []byte(stringRepeat("s", 2000))}}
	if _, err := truncateForContextWindow(context.Background(), provider, req, ctxLimit, 0); err != nil {
		t.Fatalf("with-tools case should truncate, not fail: %v", err)
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
		t.Errorf("with-tools case must shed history down to the pinned user turn, got %+v", req.Messages)
	}
}

// errCounting is the forced counting error for the fallback case below.
var errCounting = errors.New("count_tokens unavailable")

func stringRepeat(s string, n int) string {
	out := make([]byte, 0, len(s)*n)
	for i := 0; i < n; i++ {
		out = append(out, s...)
	}
	return string(out)
}

// countTokenProvider stubs ai.TokenCounter for truncateForContextWindow's
// exact re-check: its estimator prices one token per byte by default
// (so the byte ceiling and the estimate agree — the len/4 fake would leave a
// 4x margin no 30% undercount can cross; bytesPerToken > 1 prices like cl100k
// on English text instead), and CountTokens answers a fixed multiple of the
// estimate of the system prompt and the messages, or a forced error.
type countTokenProvider struct {
	testutil.FakeProvider
	multiple      float64
	bytesPerToken int
	forceErr      error
	calls         int
}

func (p *countTokenProvider) EstimateTokens(s string) int {
	return len(s) / max(p.bytesPerToken, 1)
}

func (p *countTokenProvider) CountTokens(_ context.Context, req ai.Request) (int, error) {
	p.calls++
	if p.forceErr != nil {
		return 0, p.forceErr
	}
	n := p.EstimateTokens(req.SystemPrompt)
	for _, m := range req.Messages {
		n += p.EstimateTokens(m.Content) + 4
	}
	return int(float64(n) * p.multiple), nil
}

// TestTruncateForContextWindow_ExactCountNearTheWindow: a prompt whose
// estimate the window admits with less than 30% to spare is re-checked with
// the provider's own tokenizer; a count that says over (the ~30% Claude 4.7+/
// Fable/Mythos undercount) triggers the drop loop against costs scaled to the
// count, a counting error silently keeps the estimate's verdict, and a prompt
// comfortably below the band never pays a count call.
func TestTruncateForContextWindow_ExactCountNearTheWindow(t *testing.T) {
	mkReq := func() *ai.Request {
		return &ai.Request{Messages: []ai.Message{
			{Role: "user", Content: stringRepeat("q", 40000)},
			{Role: "assistant", Content: stringRepeat("a", 20000)},
			{Role: "user", Content: stringRepeat("u", 20000)}, // the pin
		}}
	}
	// estimate = 80012 tokens; +4000 reserve = 84012 — inside the window
	// (100000) but above 70% of it, so the exact re-check decides.
	const ctxLimit = 100000

	t.Run("a 1.3x count trims what the estimate admitted", func(t *testing.T) {
		p := &countTokenProvider{multiple: 1.3}
		req := mkReq()
		if _, err := truncateForContextWindow(context.Background(), p, req, ctxLimit, 2); err != nil {
			t.Fatal(err)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" {
			t.Errorf("kept %d messages, want the pinned turn only (the count said the prompt is over)", len(req.Messages))
		}
		if p.calls == 0 || p.calls > 2 {
			t.Errorf("count calls = %d, want 1 or 2 (the cap is two)", p.calls)
		}
	})

	t.Run("a counting error keeps the estimate verdict", func(t *testing.T) {
		p := &countTokenProvider{multiple: 1.3, forceErr: errCounting}
		req := mkReq()
		if _, err := truncateForContextWindow(context.Background(), p, req, ctxLimit, 2); err != nil {
			t.Fatal(err)
		}
		if len(req.Messages) != 3 {
			t.Errorf("a counting error must not trim: kept %d messages, want 3", len(req.Messages))
		}
		if p.calls != 1 {
			t.Errorf("count calls = %d, want 1", p.calls)
		}
	})

	t.Run("a count that fits keeps the history", func(t *testing.T) {
		p := &countTokenProvider{multiple: 1.0}
		req := mkReq()
		if _, err := truncateForContextWindow(context.Background(), p, req, ctxLimit, 2); err != nil {
			t.Fatal(err)
		}
		if len(req.Messages) != 3 {
			t.Errorf("kept %d messages, want all 3 (the count confirmed the estimate)", len(req.Messages))
		}
	})

	t.Run("below the band no count call is made", func(t *testing.T) {
		p := &countTokenProvider{multiple: 1.3}
		req := mkReq()
		// 84012 estimate+reserve against a 200000 window: under 70%, so even a
		// 30% undercount still fits — no count call.
		if _, err := truncateForContextWindow(context.Background(), p, req, 200000, 2); err != nil {
			t.Fatal(err)
		}
		if p.calls != 0 {
			t.Errorf("count calls = %d, want 0 (the prompt is not near the window)", p.calls)
		}
		if len(req.Messages) != 3 {
			t.Errorf("kept %d messages, want all 3", len(req.Messages))
		}
	})
}

// TestTruncateForContextWindow_ExactCountAfterAnEstimateTrim: the exact
// re-check also runs once the ESTIMATE had to trim, and scales the system
// prompt and tool schemas with the messages. Without it, a 1M Claude
// 4.7+/5.x/Fable/Mythos thread long enough to need trimming is trimmed by
// the ~30%-undercounting estimate alone and refused as CONTEXT_LIMIT on
// every turn.
func TestTruncateForContextWindow_ExactCountAfterAnEstimateTrim(t *testing.T) {
	const ctxLimit = 100000

	t.Run("an estimate trim that the count says is still over is trimmed again", func(t *testing.T) {
		// estimate 100020 + 4000 reserve > window: the estimate keeps
		// u1/a1/pin (80012 — 84% of the window with the reserve), which a
		// 1.3x count puts at 104015 + 4000, still over.
		p := &countTokenProvider{multiple: 1.3}
		req := &ai.Request{Messages: []ai.Message{
			{Role: "user", Content: stringRepeat("q", 10000)},
			{Role: "assistant", Content: stringRepeat("a", 10000)},
			{Role: "user", Content: stringRepeat("u", 40000)},
			{Role: "assistant", Content: stringRepeat("b", 20000)},
			{Role: "user", Content: stringRepeat("c", 20000)}, // the pin
		}}
		pin, err := truncateForContextWindow(context.Background(), p, req, ctxLimit, 4)
		if err != nil {
			t.Fatal(err)
		}
		if len(req.Messages) != 1 || req.Messages[0].Content[0] != 'c' || pin != 0 {
			t.Errorf("kept %d messages (pin %d), want the pinned turn only — the estimate's own trim is over by the count", len(req.Messages), pin)
		}
		if p.calls != 2 {
			t.Errorf("count calls = %d, want 2 (the trim's check and the verification of the second trim)", p.calls)
		}
	})

	t.Run("the scale covers the system prompt and tool schemas", func(t *testing.T) {
		// estimate 45000 system + 36012 messages = 81012 (85% of the window
		// with the reserve); count 1.3x = 105315, over. Scaling only the
		// messages kept all three (45000 + 46816 + 4000 fits); the scaled
		// system prompt (58500) leaves room for the pinned turn alone.
		p := &countTokenProvider{multiple: 1.3}
		req := &ai.Request{SystemPrompt: stringRepeat("s", 45000), Messages: []ai.Message{
			{Role: "user", Content: stringRepeat("q", 16000)},
			{Role: "assistant", Content: stringRepeat("a", 10000)},
			{Role: "user", Content: stringRepeat("u", 10000)}, // the pin
		}}
		if _, err := truncateForContextWindow(context.Background(), p, req, ctxLimit, 2); err != nil {
			t.Fatal(err)
		}
		if len(req.Messages) != 1 || req.Messages[0].Role != "user" || req.Messages[0].Content[0] != 'u' {
			t.Errorf("kept %d messages, want the pinned turn only (the fixed part scaled with the rest)", len(req.Messages))
		}
		if p.calls != 2 {
			t.Errorf("count calls = %d, want 2", p.calls)
		}
	})

	t.Run("no count call for a mid-size prompt well under the window", func(t *testing.T) {
		// cl100k-like pricing (4 bytes per token): 90 KB and 150 KB prompts
		// cross the 80% BYTE bound of a 100k window (and the second the
		// window itself), but estimate at ~22k and ~37k tokens — no Claude
		// undercount brings them near the window, so no round trip.
		for _, size := range []int{90000, 150000} {
			p := &countTokenProvider{multiple: 1.3, bytesPerToken: 4}
			req := &ai.Request{Messages: []ai.Message{
				{Role: "user", Content: stringRepeat("q", size/2)},
				{Role: "assistant", Content: stringRepeat("a", size/4)},
				{Role: "user", Content: stringRepeat("u", size/4)},
			}}
			if _, err := truncateForContextWindow(context.Background(), p, req, ctxLimit, 2); err != nil {
				t.Fatal(err)
			}
			if p.calls != 0 {
				t.Errorf("%d bytes: count calls = %d, want 0 (the estimate is far from the window)", size, p.calls)
			}
			if len(req.Messages) != 3 {
				t.Errorf("%d bytes: kept %d messages, want all 3", size, len(req.Messages))
			}
		}
	})
}
