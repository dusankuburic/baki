package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// The context budget was a flat 4000 tokens whatever the model's window (R-config-9): a model with a 200k window
// got the same 4k slice of a large flow as an 8k one. Without an explicit per-provider setting the budget is
// now derived from the window, with 4000 as the floor and MaxContextTokenBudget as the cap; an explicit setting
// still caps it.
func TestContextTokenBudget_DerivedFromTheWindow(t *testing.T) {
	ctx := context.Background()
	unset := &ChatService{settings: &staticSettings{s: models.AppSettings{}}}
	cases := []struct {
		window int
		check  func(t *testing.T, got int)
	}{
		{8192, func(t *testing.T, got int) {
			if got != defaultContextTokenBudget {
				t.Errorf("8k window: budget %d, want the floor %d", got, defaultContextTokenBudget)
			}
		}},
		{2048, func(t *testing.T, got int) {
			if got != 1024 {
				t.Errorf("2k window: budget %d, want half the window", got)
			}
		}},
		{200_000, func(t *testing.T, got int) {
			if got <= defaultContextTokenBudget || got > 200_000/4 {
				t.Errorf("200k window: budget %d, want above %d and at most a quarter of the window", got, defaultContextTokenBudget)
			}
		}},
		{1 << 20, func(t *testing.T, got int) {
			if got != MaxContextTokenBudget {
				t.Errorf("1M window: budget %d, want the cap %d", got, MaxContextTokenBudget)
			}
		}},
	}
	for _, tc := range cases {
		p := &testutil.FakeProvider{Ctx: tc.window}
		tc.check(t, unset.contextTokenBudget(ctx, p, "fake-model"))
	}
	// Monotonic in the window.
	if a, b := unset.contextTokenBudget(ctx, &testutil.FakeProvider{Ctx: 32_000}, ""), unset.contextTokenBudget(ctx, &testutil.FakeProvider{Ctx: 128_000}, ""); a > b {
		t.Errorf("budget shrinks with a larger window: %d (32k) > %d (128k)", a, b)
	}
	explicit := &ChatService{settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{Providers: map[string]models.AIProviderConfig{"fake": {ContextTokenBudget: 6000}}}}}}
	if got := explicit.contextTokenBudget(ctx, &testutil.FakeProvider{Ctx: 200_000}, "fake-model"); got != 6000 {
		t.Errorf("an explicit per-provider budget is not the cap: got %d, want 6000", got)
	}
}

// A large fixture flow with a 200k-window provider and no explicit setting: the built context exceeds the old
// flat 4000 tokens and stays under the derived cap.
func TestComputeContextCore_LargeFlowFillsTheDerivedBudget(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("..", "..", "core", "parser", "testdata", "samples", "synthetic_10k.txt"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "Main.txt")
	if err := os.WriteFile(path, src, 0o600); err != nil {
		t.Fatal(err)
	}
	notifier := &testutil.CountingNotifier{}
	flows := NewFlowService(notifier, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, nil)
	doc, err := flows.LoadFlowFromPath(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := &ChatService{flowCache: flows, settings: &staticSettings{s: models.AppSettings{}}}
	p := &testutil.FakeProvider{Ctx: 200_000}
	budget := svc.contextTokenBudget(context.Background(), p, "fake-model")
	cv, err := svc.computeContextCore(p, "fake-model", doc, nil, models.ChatRequest{UserMessage: "q", SelectedSourceFiles: []string{"Main.txt"}}, ai.ToolAccessNone)
	if err != nil {
		t.Fatal(err)
	}
	n := ai.EstimateTokens(cv.contextText)
	if n <= defaultContextTokenBudget {
		t.Errorf("context is %d tokens, no more than the old flat budget %d", n, defaultContextTokenBudget)
	}
	if n > budget {
		t.Errorf("context is %d tokens, over the derived budget %d", n, budget)
	}
}
