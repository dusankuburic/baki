package analyzer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// redundantSets is a flow of n redundant self-assignments: n fixable findings
// (redundant-action, auto-fix removes the line), each fix one iteration.
func redundantSets(n int) string {
	var b strings.Builder
	b.WriteString("#Region \"Main\"\n")
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "SET X%d TO %%X%d%%\n", i, i)
	}
	b.WriteString("#EndRegion\n")
	return b.String()
}

// passCancelRule cancels the context during the onPass-th analysis pass of a
// fix loop. A pass is one RunAnalysisCtx over one parsed document, and every
// loop iteration parses afresh, so distinct *FlowDocument values count the
// passes — independent of what the fixes do to the source. The wrapped rule
// keeps its ID, so the loop still dispatches it (AutoFixableRules keys on ID).
type passCancelRule struct {
	BlockRule
	cancel func()
	onPass int
	mu     sync.Mutex
	seen   map[*models.FlowDocument]bool
}

func (r *passCancelRule) Check(b *models.Block, ctx *RuleContext) []models.Finding {
	r.mu.Lock()
	if !r.seen[ctx.Flow] {
		r.seen[ctx.Flow] = true
		if len(r.seen) == r.onPass {
			r.cancel()
		}
	}
	r.mu.Unlock()
	return r.BlockRule.Check(b, ctx)
}

func rulesCancellingOnPass(cancel func(), pass int) []Rule {
	rules := AllRules()
	for i, r := range rules {
		if r.ID() == "redundant-action" {
			rules[i] = &passCancelRule{BlockRule: r.(BlockRule), cancel: cancel, onPass: pass, seen: map[*models.FlowDocument]bool{}}
		}
	}
	return rules
}

// assertPartialAfterTwo is the contract of a loop cut short by ctx after its
// second fix: ctx.Err() is still returned, the result is marked Partial and
// is complete for those two fixes — *source is exactly the reference loop's
// first two fixes, Doc is the parse of that source (not the previous
// iteration's) and the error counts are the reference's.
func assertPartialAfterTwo(t *testing.T, res FixLoopResult, err error, src, orig string) {
	t.Helper()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled returned alongside the partial result", err)
	}
	if !res.Partial {
		t.Fatal("Partial = false after two applied fixes and a cancel: a caller would discard them")
	}
	if res.Fixed != 2 {
		t.Fatalf("Fixed = %d, want 2", res.Fixed)
	}
	if src == orig {
		t.Fatal("*source is unchanged: the applied fixes were lost")
	}
	want := orig
	ref, rerr := refApplyFixesLoop(&want, "Main.txt", FixOptions{Limit: 2}, map[string]bool{})
	if rerr != nil || ref.Fixed != 2 {
		t.Fatalf("reference loop: fixed=%d err=%v, want 2", ref.Fixed, rerr)
	}
	if src != want {
		t.Fatalf("*source is not the first two fixes:\n got %q\nwant %q", src, want)
	}
	cur, perr := parser.ParseText(src, "Main.txt", int64(len(src)))
	if perr != nil {
		t.Fatal(perr)
	}
	if res.Doc == nil {
		t.Fatal("Doc = nil: the caller cannot reuse the final parse")
	}
	if res.Doc.Metadata.BlockCount != cur.Metadata.BlockCount {
		t.Fatalf("Doc has %d blocks but the current source parses to %d: Doc is a stale iteration's parse", res.Doc.Metadata.BlockCount, cur.Metadata.BlockCount)
	}
	if res.AfterErrors != countParseErrors(cur) || res.AfterErrors != ref.AfterErrors || res.BeforeErrors != ref.BeforeErrors {
		t.Fatalf("errors before/after = %d/%d, want %d/%d (current source: %d)", res.BeforeErrors, res.AfterErrors, ref.BeforeErrors, ref.AfterErrors, countParseErrors(cur))
	}
}

// TestApplyFixesToSourceDocCtx_PartialKeepsAppliedFixes: ctx cancelled right
// after the second applied fix (noticed at the top of the next iteration).
// The loop used to return there with *source holding both patches but Doc and
// AfterErrors stale, documented as "discard it" — so a request deadline threw
// away every fix it had paid for.
func TestApplyFixesToSourceDocCtx_PartialKeepsAppliedFixes(t *testing.T) {
	orig := buildFixableSource(10)
	src := orig
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fixes := 0
	opts := FixOptions{Limit: 50, OnFix: func(string, string, int) {
		fixes++
		if fixes == 2 {
			cancel()
		}
	}}
	res, err := ApplyFixesToSourceDocCtx(ctx, &src, "Main.txt", opts)
	assertPartialAfterTwo(t, res, err, src, orig)
	if fixes != 2 {
		t.Fatalf("OnFix ran %d times after the cancel, want 2", fixes)
	}
}

// TestApplyFixesToSourceDocCtx_PartialWhenCancelledMidAnalysis: ctx cancelled
// inside the third pass's analysis, after two fixes. That pass's report is
// partial and must not be acted on (no third fix), but the two fixes stay and
// the result is complete for them — here the pass's own parse is already the
// current source, so no extra parse is needed.
func TestApplyFixesToSourceDocCtx_PartialWhenCancelledMidAnalysis(t *testing.T) {
	orig := redundantSets(6)
	src := orig
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := ApplyFixesToSourceDocCtx(ctx, &src, "Main.txt", FixOptions{Limit: 50, Rules: rulesCancellingOnPass(cancel, 3)})
	assertPartialAfterTwo(t, res, err, src, orig)
	if n := strings.Count(src, "SET X"); n != 4 {
		t.Fatalf("%d redundant SETs remain, want 4 (two removed, none from the cancelled pass):\n%s", n, src)
	}
}

// TestApplyFixesToSourceDocCtx_NothingAppliedIsNotPartial: a context that is
// done before the first fix lands (here: during the first analysis) leaves
// nothing to keep — *source untouched, Partial false, ctx.Err() returned, and
// no final parse spent on a dead context.
func TestApplyFixesToSourceDocCtx_NothingAppliedIsNotPartial(t *testing.T) {
	orig := redundantSets(3)
	src := orig
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	res, err := ApplyFixesToSourceDocCtx(ctx, &src, "Main.txt", FixOptions{Limit: 50, Rules: rulesCancellingOnPass(cancel, 1)})
	if !errors.Is(err, context.Canceled) || res.Partial || res.Fixed != 0 || src != orig {
		t.Fatalf("err=%v partial=%v fixed=%d changed=%v, want Canceled/false/0/false", err, res.Partial, res.Fixed, src != orig)
	}
	if res.AfterErrors != 0 {
		t.Fatalf("AfterErrors = %d on a result with nothing applied", res.AfterErrors)
	}
}
