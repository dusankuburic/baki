//go:build live

package service

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
)

// The live eval runner (R-testing-12): the cases of internal/ai/testdata/evals asked of real models, through the same
// stream, tools and grader as TestChatEval_Offline. Built only with -tags live and gated per provider on the live
// smoke's BAKI_LIVE_<PROVIDER>_KEY (internal/testutil/live.go); without keys every subtest skips. It prints a pass
// rate per model and, for each failing case, the failed checks as want/got — a measurement, not a gate: the provider's
// subtest fails only when its pass rate is below BAKI_LIVE_EVAL_MIN_PASS percent (default 0).
//
//	go test -tags live ./internal/service -run TestLiveEval -count=1 -v
//	go test -tags live ./internal/service -run TestLiveEval/claude -count=1 -v -record-evals
//
// -record-evals rewrites the transcript of every case the model passed (testdata/evals/transcripts/<case>.json), for
// a provider that takes tools natively — a marker-mode transcript would not replay as native tool calls. Record one
// provider at a time; review the diff, then run the offline replay.
//
// Cost: every call is capped at liveEvalMaxTokensPerCall output tokens (plus testutil.LiveReasoningAllowance at "low"
// effort for a model that thinks — the service's 16 384-token reasoning floor is clamped away), each provider's run at
// BAKI_LIVE_EVAL_MAX_TOKENS tokens (default 400 000, input + output) and BAKI_LIVE_EVAL_MAX_CALLS calls (default 100).
// The chain makes one attempt per call (evalLiveFactory: retries off), so a case is at most the loop's own
// maxToolIterations + 1 calls and a recorded transcript holds no discarded attempt. Usage goes to no database: the
// chain's recorder is nil. Logged failures pass through LiveProvider.Redact.

var recordEvals = flag.Bool("record-evals", false, "rewrite internal/ai/testdata/evals/transcripts/ from the live run (passing cases, native-tool providers)")

const (
	liveEvalMaxTokensPerCall = 1024
	liveEvalTimeout          = 3 * time.Minute
)

// liveRecorder sits between the chat chain and the live provider: it keeps every request (the grader reads what the
// model was shown) and every streamed turn (the transcript -record-evals writes).
type liveRecorder struct {
	ai.Provider
	mu       sync.Mutex
	requests []ai.Request
	turns    []evalTurn
}

func (r *liveRecorder) Requests() []ai.Request {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]ai.Request(nil), r.requests...)
}

func (r *liveRecorder) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	kept := req
	kept.Messages = append([]ai.Message(nil), req.Messages...)
	r.mu.Lock()
	r.requests = append(r.requests, kept)
	r.mu.Unlock()
	var turn evalTurn
	var text strings.Builder
	err := r.Provider.Stream(ctx, req, func(c ai.Chunk) {
		text.WriteString(c.Text)
		if c.Done {
			turn.TokensIn, turn.TokensOut, turn.FinishReason, turn.ServedModel = c.TokensIn, c.TokensOut, c.FinishReason, c.ServedModel
			for _, tc := range c.ToolCalls {
				turn.ToolCalls = append(turn.ToolCalls, evalToolCall{ID: tc.ID, Name: tc.Name, Input: append([]byte(nil), tc.Input...)})
			}
		}
		onChunk(c)
	})
	turn.Text = text.String()
	r.mu.Lock()
	r.turns = append(r.turns, turn)
	r.mu.Unlock()
	return err
}

// writeEvalTranscript stores a transcript as the offline replay reads it (-record-evals).
func writeEvalTranscript(name string, tr evalTranscript) error {
	data, err := json.MarshalIndent(tr, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(evalTranscriptDir, name+".json"), append(data, '\n'), 0o600)
}

func (r *liveRecorder) transcript() []evalTurn {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]evalTurn(nil), r.turns...)
}

func TestLiveEval(t *testing.T) {
	cases := loadEvalCases(t)
	ran := false
	for _, spec := range ai.Specs() {
		t.Run(spec.ID, func(t *testing.T) {
			lp := testutil.RequireLiveProvider(t, spec.ID)
			ran = true
			runLiveEval(t, lp, cases)
		})
	}
	if !ran {
		testutil.LiveSummaryf("\n### AI live eval\n\nNo `BAKI_LIVE_<PROVIDER>_KEY` is set: nothing ran.\n")
	}
}

var liveEvalHeader sync.Once

func runLiveEval(t *testing.T, lp testutil.LiveProvider, cases []evalCase) {
	budget := testutil.NewLiveBudget("the live eval of "+lp.Spec.ID,
		testutil.LiveIntEnv("BAKI_LIVE_EVAL_MAX_TOKENS", 400000), testutil.LiveIntEnv("BAKI_LIVE_EVAL_MAX_CALLS", 100))
	native := lp.Raw().SupportsTools()
	passed := 0
	var failedNames []string
	for _, c := range cases {
		rec := &liveRecorder{Provider: budget.Wrap(lp.Raw(), liveEvalMaxTokensPerCall)}
		var (
			checks []evalCheck
			answer string
		)
		t.Run(c.name, func(t *testing.T) {
			run := runEvalCase(t, c, evalLiveFactory(), "live-eval-"+lp.Spec.ID, lp.Model, rec, rec.Requests, liveEvalTimeout)
			checks, answer = gradeEval(c, run), run.answer
		})
		fails := evalFailures(checks)
		if checks == nil {
			fails = []string{"the case did not run to a graded end (see its subtest)"}
		}
		if len(fails) > 0 {
			failedNames = append(failedNames, c.name)
			t.Logf("FAIL %s on %s/%s:\n  %s\n  answer: %s", c.name, lp.Spec.ID, lp.Model, lp.Redact(strings.Join(fails, "\n  ")), lp.Redact(excerpt(answer)))
			continue
		}
		passed++
		t.Logf("pass %s on %s/%s", c.name, lp.Spec.ID, lp.Model)
		if *recordEvals && native {
			tr := evalTranscript{
				Recorded: fmt.Sprintf("live %s/%s %s", lp.Spec.ID, lp.Model, time.Now().UTC().Format("2006-01-02")),
				Turns:    rec.transcript(),
			}
			if err := writeEvalTranscript(c.name, tr); err != nil {
				t.Errorf("record %s: %v", c.name, err)
			}
		}
	}
	rate := 100 * passed / max(len(cases), 1)
	tokens, calls := budget.Used()
	t.Logf("%s/%s: %d of %d cases pass (%d%%); %d tokens in %d calls", lp.Spec.ID, lp.Model, passed, len(cases), rate, tokens, calls)
	liveEvalHeader.Do(func() {
		testutil.LiveSummaryf("\n### AI live eval\n\n| Provider | Model | Pass rate | Failing cases | Spend |\n|---|---|---|---|---|")
	})
	failing := "none"
	if len(failedNames) > 0 {
		failing = strings.Join(failedNames, ", ")
	}
	testutil.LiveSummaryf("| %s | %s | %d/%d (%d%%) | %s | %d tokens, %d calls |", lp.Spec.ID, lp.Model, passed, len(cases), rate, failing, tokens, calls)
	if minPass := testutil.LiveIntEnv("BAKI_LIVE_EVAL_MIN_PASS", 0); rate < minPass {
		t.Errorf("%s/%s passes %d%% of the eval set, below BAKI_LIVE_EVAL_MIN_PASS=%d%%", lp.Spec.ID, lp.Model, rate, minPass)
	}
}
