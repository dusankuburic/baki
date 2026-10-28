//go:build live

package ai_test

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// The live provider smoke (R-testing-11). Every other provider test runs on fixtures written from the vendors'
// documentation; this one asks the vendors themselves, so a new SSE event, a renamed usage field, a deprecated
// parameter or a changed tool-call encoding shows up here before a user's stream breaks on it. Built only with
// -tags live and gated per provider on BAKI_LIVE_<PROVIDER>_KEY (internal/testutil/live.go): without keys every
// subtest skips. Per configured provider it lists the models, runs one Chat, one Stream with the search_flow tool
// and a prompt that forces the call, and one Embed where the provider has CapEmbeddings, and asserts invariants, not
// text: one terminal chunk, usage above zero (or carried on the error), a tool call whose input is valid JSON, the
// served model reported and the cache counts inside the input. New entries in the repair memos are reported.
//
// Cost: every call is capped at liveSmokeMaxTokens output tokens — plus testutil.LiveReasoningAllowance at "low"
// effort for a model that thinks, whose reasoning counts against the same cap — and the run as a whole at
// BAKI_LIVE_SMOKE_MAX_TOKENS tokens (default 40 000, input + output) and BAKI_LIVE_SMOKE_MAX_CALLS calls (default 40):
// three calls per provider, plus at most the provider's own parameter repairs (maxSendRepairs); past either ceiling
// every call is refused. Failure messages pass through LiveProvider.Redact: no key, credential field or endpoint. The chain is
// the factory's, retries off (one attempt), and its usage recorder keeps the rows in memory: nothing is written to a
// database. Run: go test -tags live ./internal/ai -run TestLive -count=1 -v (docs/architecture/ai-providers.md).

const (
	// liveSmokeMaxTokens is every smoke call's output cap.
	liveSmokeMaxTokens = 64
	liveSmokeScope     = "live-smoke"
	liveSmokeTimeout   = 90 * time.Second
)

var (
	liveSmokeBudget = testutil.NewLiveBudget("the live smoke",
		testutil.LiveIntEnv("BAKI_LIVE_SMOKE_MAX_TOKENS", 40000), testutil.LiveIntEnv("BAKI_LIVE_SMOKE_MAX_CALLS", 40))
	liveSummaryHeader sync.Once
)

// liveUsage is the in-memory usage recorder of the smoke's chains.
type liveUsage struct {
	mu   sync.Mutex
	rows []interfaces.UsageMetric
}

func (u *liveUsage) record(_ context.Context, m *interfaces.UsageMetric) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rows = append(u.rows, *m)
	return nil
}

// waitRow waits for a usage row past from (the audited wrapper records in the background) and returns it.
func (u *liveUsage) waitRow(from int) (interfaces.UsageMetric, bool) {
	deadline := time.Now().Add(3 * time.Second)
	for {
		u.mu.Lock()
		if len(u.rows) > from {
			row := u.rows[len(u.rows)-1]
			u.mu.Unlock()
			return row, true
		}
		u.mu.Unlock()
		if time.Now().After(deadline) {
			return interfaces.UsageMetric{}, false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (u *liveUsage) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.rows)
}

// liveStep runs one step as a subtest and reports how it ended: pass, FAIL or skip.
func liveStep(t *testing.T, name string, fn func(t *testing.T)) string {
	status := "pass"
	t.Run(name, func(t *testing.T) {
		defer func() {
			switch {
			case t.Failed():
				status = "FAIL"
			case t.Skipped():
				status = "skip"
			}
		}()
		fn(t)
	})
	return status
}

func TestLiveSmoke(t *testing.T) {
	ran := 0
	for _, spec := range ai.Specs() {
		t.Run(spec.ID, func(t *testing.T) {
			lp := testutil.RequireLiveProvider(t, spec.ID)
			ran++
			runLiveSmoke(t, lp)
		})
	}
	if ran == 0 {
		testutil.LiveSummaryf("### AI live smoke\n\nNo `BAKI_LIVE_<PROVIDER>_KEY` is set: nothing ran.\n")
	}
	tokens, calls := liveSmokeBudget.Used()
	t.Logf("the live smoke used %d tokens in %d calls (caps %d / %d)", tokens, calls, liveSmokeBudget.MaxTokens, liveSmokeBudget.MaxCalls)
	if ran > 0 {
		testutil.LiveSummaryf("\nSmoke spend: %d tokens in %d calls (caps %d tokens / %d calls, %d output tokens per call).\n",
			tokens, calls, liveSmokeBudget.MaxTokens, liveSmokeBudget.MaxCalls, liveSmokeMaxTokens)
	}
}

// runLiveSmoke runs the four steps against one provider and writes its summary row.
func runLiveSmoke(t *testing.T, lp testutil.LiveProvider) {
	usage := &liveUsage{}
	factory := ai.NewProviderFactory(lp.KeyLookup(), nil, usage.record, &ai.ResilienceConfig{Attempts: 1})
	ctx := context.Background()
	chain, err := factory.For(ctx, liveSmokeScope, lp.Spec.ID)
	if err != nil {
		t.Fatalf("build the %s chain: %s", lp.Spec.ID, lp.Redact(err.Error()))
	}
	p := liveSmokeBudget.Wrap(chain, liveSmokeMaxTokens)
	before := ai.LiveRepairMemo()

	models := liveStep(t, "models", func(t *testing.T) { liveModels(t, p, lp) })
	chat := liveStep(t, "chat", func(t *testing.T) { liveChat(t, p, lp, usage) })
	stream := liveStep(t, "stream_tool", func(t *testing.T) { liveStreamTool(t, p, lp, usage) })
	embed := liveStep(t, "embed", func(t *testing.T) { liveEmbed(t, factory, lp) })

	repairs := ai.NewLiveRepairEntries(before, ai.LiveRepairMemo())
	for _, r := range repairs {
		t.Logf("new repair-memo entry: %s", r)
	}
	liveSummaryHeader.Do(func() {
		testutil.LiveSummaryf("### AI live smoke\n\n| Provider | Model | Models | Chat | Stream + tool | Embed | New repair-memo entries |\n|---|---|---|---|---|---|---|")
	})
	repairCell := "none"
	if len(repairs) > 0 {
		repairCell = strings.ReplaceAll(strings.Join(repairs, "<br>"), "|", "\\|")
	}
	testutil.LiveSummaryf("| %s | %s | %s | %s | %s | %s | %s |", lp.Spec.ID, lp.Model, models, chat, stream, embed, repairCell)
}

// liveModels: the listing answers, and says whether the run's model is in it (a model a live listing no longer
// carries is logged, not failed: some listings name models by another scheme).
func liveModels(t *testing.T, p ai.Provider, lp testutil.LiveProvider) {
	ctx, cancel := context.WithTimeout(context.Background(), liveSmokeTimeout)
	defer cancel()
	list, err := p.Models(ctx)
	if err != nil {
		t.Fatalf("Models: %s", lp.Redact(err.Error()))
	}
	if len(list) == 0 {
		t.Fatal("Models listed nothing")
	}
	for _, m := range list {
		if m.ID == "" {
			t.Errorf("a listed model has no id: %+v", m)
		}
		if m.ID == lp.Model {
			return
		}
	}
	t.Logf("%s is not among the %d listed models", lp.Model, len(list))
}

// liveChat: one non-streaming turn; usage is recorded (the answer's, or the one carried on its error), the served
// model is reported and the cache counts sit inside the input.
func liveChat(t *testing.T, p ai.Provider, lp testutil.LiveProvider, usage *liveUsage) {
	ctx, cancel := context.WithTimeout(context.Background(), liveSmokeTimeout)
	defer cancel()
	from := usage.count()
	resp, err := p.Chat(ctx, ai.Request{
		Model:     lp.Model,
		MaxTokens: liveSmokeMaxTokens,
		Messages:  []ai.Message{{Role: "user", Content: "Reply with the single word OK."}},
	})
	row, recorded := usage.waitRow(from)
	if err != nil {
		msg := lp.Redact(err.Error())
		switch {
		case errors.Is(err, testutil.ErrLiveBudgetExhausted):
			t.Fatalf("Chat: %s", msg)
		case errors.Is(err, ai.ErrOutputLimit):
			// A thinking model spent the whole cap thinking: the harness's allowance (testutil.LiveReasoning) does not
			// cover this model — a harness gap to fix there, not wire drift.
			t.Fatalf("Chat stopped at its output cap before answering (reasoning model per the harness: %v): %s", testutil.LiveReasoning(lp.Model), msg)
		case !recorded || row.PromptTokens+row.CompletionTokens == 0:
			t.Fatalf("Chat failed and carried no usage on the error: %s", msg)
		}
		t.Fatalf("Chat failed (its usage was carried on the error: %d in / %d out): %s", row.PromptTokens, row.CompletionTokens, msg)
	}
	if resp.TokensIn <= 0 || resp.TokensOut <= 0 {
		t.Errorf("Chat usage %d in / %d out, want both above zero", resp.TokensIn, resp.TokensOut)
	}
	if !recorded || row.PromptTokens <= 0 {
		t.Errorf("the audited chain recorded no usage row for the Chat call (got %+v)", row)
	}
	if resp.ServedModel == "" {
		t.Error("Chat reported no served model")
	}
	checkCache(t, "Chat", resp.TokensIn, resp.CacheReadTokens, resp.CacheWriteTokens)
	if strings.TrimSpace(resp.Content) == "" {
		if testutil.LiveTruncated(resp.FinishReason) {
			t.Errorf("Chat answered nothing: it stopped at its output cap (finish %q; reasoning model per the harness: %v)", resp.FinishReason, testutil.LiveReasoning(lp.Model))
		} else {
			t.Errorf("Chat answered nothing (finish %q)", resp.FinishReason)
		}
	}
	t.Logf("chat: %d in / %d out, served %s, finish %q", resp.TokensIn, resp.TokensOut, resp.ServedModel, resp.FinishReason)
}

// liveStreamTool: one streamed turn offered search_flow and told to call it: exactly one terminal chunk, usage on
// it, a search_flow call whose input is valid JSON with a query, the served model reported.
func liveStreamTool(t *testing.T, p ai.Provider, lp testutil.LiveProvider, usage *liveUsage) {
	if !p.SupportsTools() {
		t.Skipf("%s does not take tools natively (the chat runs it in marker mode)", lp.Spec.ID)
	}
	spec, ok := ai.ToolSpecOf("search_flow")
	if !ok {
		t.Fatal("search_flow is not registered")
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveSmokeTimeout)
	defer cancel()
	from := usage.count()
	var (
		terminal          []ai.Chunk
		served            string
		chunks, textBytes int
	)
	err := p.Stream(ctx, ai.Request{
		Model:        lp.Model,
		MaxTokens:    liveSmokeMaxTokens,
		SystemPrompt: "You are a test harness for tool calling. Answer every request by calling the search_flow tool; never answer in text.",
		Messages:     []ai.Message{{Role: "user", Content: `Call search_flow with the query "Invoke" now.`}},
		Tools:        []ai.ToolDefinition{spec.Def},
	}, func(c ai.Chunk) {
		chunks++
		textBytes += len(c.Text)
		if c.ServedModel != "" {
			served = c.ServedModel
		}
		if c.Done || c.Error != nil {
			terminal = append(terminal, c)
		}
	})
	if err != nil {
		t.Fatalf("Stream: %s", lp.Redact(err.Error()))
	}
	if len(terminal) != 1 {
		t.Fatalf("Stream sent %d terminal chunks in %d chunks, want exactly one", len(terminal), chunks)
	}
	end := terminal[0]
	if end.Error != nil {
		if end.TokensIn+end.TokensOut == 0 {
			t.Fatalf("the stream failed and its error chunk carried no usage: %s", lp.Redact(end.Error.Error()))
		}
		t.Fatalf("the stream failed (usage carried: %d in / %d out): %s", end.TokensIn, end.TokensOut, lp.Redact(end.Error.Error()))
	}
	if end.TokensIn <= 0 || end.TokensOut <= 0 {
		t.Errorf("the terminal chunk carries usage %d in / %d out, want both above zero", end.TokensIn, end.TokensOut)
	}
	if row, ok := usage.waitRow(from); !ok || row.PromptTokens <= 0 {
		t.Errorf("the audited chain recorded no usage row for the stream (got %+v)", row)
	}
	if served == "" {
		t.Error("the stream reported no served model")
	}
	checkCache(t, "Stream", end.TokensIn, end.CacheReadTokens, end.CacheWriteTokens)
	if len(end.ToolCalls) == 0 {
		if testutil.LiveTruncated(end.FinishReason) {
			t.Fatalf("the stream stopped at its output cap before calling search_flow (finish %q, %d text bytes; reasoning model per the harness: %v)", end.FinishReason, textBytes, testutil.LiveReasoning(lp.Model))
		}
		t.Fatalf("the model did not call search_flow (finish %q, %d text bytes)", end.FinishReason, textBytes)
	}
	for _, tc := range end.ToolCalls {
		if tc.Name != "search_flow" {
			t.Errorf("tool call %q, want search_flow", tc.Name)
		}
		if tc.ID == "" {
			t.Errorf("the %s call has no id: the next turn could not answer it", tc.Name)
		}
		var in struct {
			Query string `json:"query"`
		}
		if !json.Valid(tc.Input) {
			t.Errorf("the %s input is not valid JSON: %q", tc.Name, tc.Input)
		} else if err := json.Unmarshal(tc.Input, &in); err != nil || strings.TrimSpace(in.Query) == "" {
			t.Errorf("the %s input has no query: %s", tc.Name, tc.Input)
		}
	}
	t.Logf("stream: %d chunks, %d in / %d out, served %s, finish %q, calls %d", chunks, end.TokensIn, end.TokensOut, served, end.FinishReason, len(end.ToolCalls))
}

// liveEmbed: one embedding through the factory's embedding chain, where the provider has CapEmbeddings.
func liveEmbed(t *testing.T, factory *ai.ProviderFactory, lp testutil.LiveProvider) {
	if !lp.Spec.Caps.Has(ai.CapEmbeddings) {
		t.Skipf("%s has no embeddings", lp.Spec.ID)
	}
	ctx, cancel := context.WithTimeout(context.Background(), liveSmokeTimeout)
	defer cancel()
	chain, err := factory.ForEmbedding(ctx, liveSmokeScope, lp.Spec.ID, "")
	if errors.Is(err, ai.ErrEmbeddingDeploymentMissing) {
		t.Skipf("the credential names no embedding deployment: %s", lp.Redact(err.Error()))
	}
	if err != nil {
		t.Fatalf("ForEmbedding: %s", lp.Redact(err.Error()))
	}
	vecs, err := liveSmokeBudget.Wrap(chain, liveSmokeMaxTokens).Embed(ctx, []string{"Launch Excel, then read the first free row."})
	if err != nil {
		t.Fatalf("Embed: %s", lp.Redact(err.Error()))
	}
	if len(vecs) != 1 || len(vecs[0]) == 0 {
		t.Fatalf("Embed returned %d vectors (first of %d dims), want one non-empty vector", len(vecs), dims(vecs))
	}
	for i, v := range vecs[0] {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			t.Fatalf("component %d of the embedding is %v", i, v)
		}
	}
	t.Logf("embed: %d dims", len(vecs[0]))
}

func dims(vecs [][]float32) int {
	if len(vecs) == 0 {
		return 0
	}
	return len(vecs[0])
}

// checkCache: the prompt-cache counts are a slice INCLUDED in the input (ai.Response), never negative.
func checkCache(t *testing.T, what string, in, read, write int) {
	t.Helper()
	if read < 0 || write < 0 || read+write > in {
		t.Errorf("%s cache counts %d read + %d write do not fit in the %d input tokens", what, read, write, in)
	}
}
