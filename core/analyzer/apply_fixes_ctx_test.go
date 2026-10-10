package analyzer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// refApplyFixesLoop is ApplyFixesToSourceDoc as it was before the ctx-aware
// variant, kept verbatim (skipped is passed in so the test can compare it) as
// the oracle the loop must still match exactly.
func refApplyFixesLoop(source *string, fileName string, opts FixOptions, skipped map[string]bool) (FixLoopResult, error) {
	var res FixLoopResult
	// No-progress guard: if the next iteration picks the same signature as the
	// last applied fix, that fix didn't resolve it → break.
	var lastFixSig string
	// skipped records declined/errored findings (by content fingerprint) so
	// they aren't retried; eligible mirrors every filter the pick scan
	// applies, so the stall guard counts the exact pool a fix has to shrink.
	eligible := func(f *models.Finding) bool {
		if f.AutoFix == "" {
			return false
		}
		if opts.RuleFilter != nil && !opts.RuleFilter[f.RuleID] {
			return false
		}
		if !opts.IncludeBehaviourChanging && IsBehaviourChangingFix(f.AutoFix) {
			return false
		}
		return true
	}
	// Stall detection (see fixLoopStallLimit): fixes that fail to drive the
	// remaining-fixable pool to a NEW MINIMUM. Counting from the minimum, not
	// the previous iteration, is what catches an oscillator whose pool dips
	// and regrows (3→2→3→2…); a legitimate cascade still passes.
	stall := 0
	minFixable := -1
	// The caller's rule set (nil ⇒ the built-ins), narrowed to the rules that
	// can emit an AutoFix: the loop only ever selects findings with one, so
	// dispatching the rest is wasted work (and cannot reorder or fold the
	// fixable findings — dedup keys on block+title+subject).
	ruleSet := opts.Rules
	if ruleSet == nil {
		ruleSet = AllRules()
	}
	rules := AutoFixableRules(ruleSet)
	settings := opts.settingsOrDefault()
	// docIsCurrent tracks whether Doc is the parse of the current *source:
	// each iteration's parse sets it, applying a patch invalidates it.
	docIsCurrent := false
	for iter := 0; iter < opts.Limit; iter++ {
		doc, err := parser.ParseText(*source, fileName, int64(len(*source)))
		if err != nil {
			return res, fmt.Errorf("parse error during fix loop: %w", err)
		}
		res.Doc = doc
		docIsCurrent = true
		if iter == 0 {
			res.BeforeErrors = countParseErrors(doc)
		}
		report := RunAnalysis(doc, rules, settings, nil)

		// Stall check BEFORE picking: the pick scan below mirrors these
		// filters, so the count is exactly the pool a fix has to shrink.
		fixable := 0
		for i := range report.Findings {
			f := &report.Findings[i]
			if !eligible(f) {
				continue
			}
			sig := f.Fingerprint
			if sig == "" {
				sig = f.Key()
			}
			if !skipped[sig] {
				fixable++
			}
		}
		if minFixable < 0 || fixable < minFixable {
			minFixable = fixable
			stall = 0
		} else {
			stall++
			if stall >= fixLoopStallLimit {
				break
			}
		}

		// Pick the first auto-fixable finding whose fixer accepts the work —
		// a single declined fixer no longer aborts the whole run.
		var (
			pick      *models.Finding
			pickBlock *models.Block
			patch     models.Patch
		)
		for i := range report.Findings {
			f := &report.Findings[i]
			if !eligible(f) {
				continue
			}
			sig := f.Fingerprint
			if sig == "" {
				sig = f.Key()
			}
			if skipped[sig] {
				continue
			}
			block := doc.BlocksByID[f.BlockID]
			if block == nil {
				continue // stale finding (block gone) — shouldn't happen post-reparse
			}
			variable, _ := f.Metadata["variable"].(string)
			property, _ := f.Metadata["property"].(string)
			candidate, ferr := PatchForFix(block, f.AutoFix, f.RuleID, variable, property)
			if ferr != nil || len(candidate.Ops) == 0 {
				// Fixer declined or errored: skip this finding for the rest
				// of the run; other findings may still be fixable.
				skipped[sig] = true
				continue
			}
			pick, pickBlock, patch = f, block, candidate
			break
		}
		if pick == nil {
			break // no more un-skipped, fixable findings
		}
		// No-progress guard: the same finding picked twice in a row means the
		// fix didn't resolve it — break.
		curSig := pick.Fingerprint
		if curSig == "" {
			curSig = pick.Key()
		}
		if curSig == lastFixSig {
			break
		}
		lastFixSig = curSig

		if opts.OnFix != nil {
			opts.OnFix(pick.RuleID, pick.AutoFix, pickBlock.LineNumber)
		}
		*source = ApplyPatch(*source, patch)
		res.Fixed++
		docIsCurrent = false
	}
	// The loop exited with a patch applied but not re-parsed — parse once so
	// Doc matches *source.
	if res.Doc == nil || (res.Fixed > 0 && !docIsCurrent) {
		doc, err := parser.ParseText(*source, fileName, int64(len(*source)))
		if err != nil {
			return res, fmt.Errorf("parse error during fix loop: %w", err)
		}
		res.Doc = doc
	}
	res.AfterErrors = countParseErrors(res.Doc)
	return res, nil
}

// fixRun is everything observable about one run of the fix loop.
type fixRun struct {
	source        string
	fixed         int
	before, after int
	docBlocks     int
	applied       []string
	skipped       []string
	err           error
}

func countDocBlocks(doc *models.FlowDocument) int {
	if doc == nil {
		return -1
	}
	n := 0
	var walk func(bs []models.Block)
	walk = func(bs []models.Block) {
		for i := range bs {
			n++
			walk(bs[i].Children)
		}
	}
	for i := range doc.Subflows {
		walk(doc.Subflows[i].Blocks)
	}
	return n
}

func runFixLoop(src, name string, opts FixOptions, loop func(*string, string, FixOptions, map[string]bool) (FixLoopResult, error)) fixRun {
	var r fixRun
	opts.OnFix = func(ruleID, fixType string, line int) {
		r.applied = append(r.applied, fmt.Sprintf("%s/%s@%d", ruleID, fixType, line))
	}
	skipped := map[string]bool{}
	res, err := loop(&src, name, opts, skipped)
	r.source, r.fixed, r.before, r.after, r.err = src, res.Fixed, res.BeforeErrors, res.AfterErrors, err
	r.docBlocks = countDocBlocks(res.Doc)
	for k := range skipped {
		r.skipped = append(r.skipped, k)
	}
	sort.Strings(r.skipped)
	return r
}

func fixCorpus(t *testing.T) map[string]string {
	t.Helper()
	files, _ := filepath.Glob("../parser/testdata/samples/*.txt")
	more, _ := filepath.Glob("../parser/testdata/samples/*/*.txt")
	corpus := map[string]string{}
	for _, f := range append(files, more...) {
		if filepath.Base(f) == "synthetic_10k.txt" {
			continue // 1 MB: covered with a small limit below
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		corpus[f] = string(b)
	}
	if len(corpus) < 10 {
		t.Fatalf("fixture rot: %d sample flows", len(corpus))
	}
	corpus["fixable-groups"] = buildFixableSource(12)
	corpus["second-pass"] = "#Region \"Main\"\nSET X TO %X%\nHTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/x' => R\nWAIT 0\n#EndRegion\n"
	corpus["crlf"] = strings.ReplaceAll(buildFixableSource(4), "\n", "\r\n")
	return corpus
}

// TestApplyFixesLoop_MatchesReference is the differential gate: over every
// sample flow, under both fix policies, several limits and a rule filter,
// the loop yields exactly the old loop's final source, fix count, applied
// sequence (rule/fix/line, in order), skipped set, error counts and doc.
func TestApplyFixesLoop_MatchesReference(t *testing.T) {
	ctxLoop := func(src *string, name string, opts FixOptions, skipped map[string]bool) (FixLoopResult, error) {
		return applyFixesLoop(context.Background(), src, name, opts, skipped)
	}
	corpus := fixCorpus(t)
	names := make([]string, 0, len(corpus))
	for n := range corpus {
		names = append(names, n)
	}
	sort.Strings(names)
	applied := 0
	for _, name := range names {
		for _, bc := range []bool{false, true} {
			for _, limit := range []int{1, 3, 500} {
				for _, filter := range []map[string]bool{nil, {"unhandled-error": true, "error-swallow": true, "duplicate-action": true}} {
					opts := FixOptions{Limit: limit, IncludeBehaviourChanging: bc, RuleFilter: filter}
					got := runFixLoop(corpus[name], filepath.Base(name), opts, ctxLoop)
					want := runFixLoop(corpus[name], filepath.Base(name), opts, refApplyFixesLoop)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("%s (behaviourChanging=%v limit=%d filter=%v):\n got %+v\nwant %+v", name, bc, limit, filter, got, want)
					}
					applied += got.fixed
				}
			}
		}
	}
	// And the 1 MB sample, briefly.
	big, err := os.ReadFile("../parser/testdata/samples/synthetic_10k.txt")
	if err != nil {
		t.Fatal(err)
	}
	opts := FixOptions{Limit: 3}
	if got, want := runFixLoop(string(big), "synthetic_10k.txt", opts, ctxLoop), runFixLoop(string(big), "synthetic_10k.txt", opts, refApplyFixesLoop); !reflect.DeepEqual(got, want) {
		t.Fatalf("synthetic_10k: got %d fixes %v, want %d %v", got.fixed, got.applied, want.fixed, want.applied)
	}
	if applied < 100 {
		t.Fatalf("fixture rot: only %d fixes applied across the corpus", applied)
	}
}

// TestApplyFixesToSourceDocCtx_StopsOnCancel: a done context stops the loop
// before the next iteration (and before acting on a partial analysis).
func TestApplyFixesToSourceDocCtx_StopsOnCancel(t *testing.T) {
	src := buildFixableSource(10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	opts := FixOptions{Limit: 100}
	fixes := 0
	opts.OnFix = func(string, string, int) {
		fixes++
		if fixes == 2 {
			cancel()
		}
	}
	res, err := ApplyFixesToSourceDocCtx(ctx, &src, "Main.txt", opts)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.Fixed != 2 || fixes != 2 {
		t.Fatalf("applied %d fixes (OnFix %d) after cancelling at the 2nd, want 2", res.Fixed, fixes)
	}

	// Already done: nothing is parsed or applied.
	src2 := buildFixableSource(2)
	orig := src2
	n, err := ApplyFixesToSourceCtx(ctx, &src2, "Main.txt", FixOptions{Limit: 100})
	if !errors.Is(err, context.Canceled) || n != 0 || src2 != orig {
		t.Fatalf("pre-cancelled: n=%d err=%v changed=%v", n, err, src2 != orig)
	}
}

// cancellingRule cancels the context the first time it runs, then behaves as
// the rule it wraps (same ID, so the fix loop keeps it).
type cancellingRule struct {
	BlockRule
	cancel func()
}

func (r cancellingRule) Check(b *models.Block, ctx *RuleContext) []models.Finding {
	r.cancel()
	return r.BlockRule.Check(b, ctx)
}

// TestApplyFixesToSourceDocCtx_CancelDuringAnalysis: cancelled mid-walk, the
// analysis returns a partial report (here: the first block's findings). The
// loop must not act on it.
func TestApplyFixesToSourceDocCtx_CancelDuringAnalysis(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rules := AllRules()
	for i, r := range rules {
		if r.ID() == "redundant-action" {
			rules[i] = cancellingRule{BlockRule: r.(BlockRule), cancel: cancel}
		}
	}
	src := buildFixableSource(3) // first block: SET X0 TO %X0% (redundant-action)
	orig := src
	res, err := ApplyFixesToSourceDocCtx(ctx, &src, "Main.txt", FixOptions{Limit: 100, Rules: rules})
	if !errors.Is(err, context.Canceled) || res.Fixed != 0 || src != orig {
		t.Fatalf("fixed=%d err=%v changed=%v, want nothing applied from a partial analysis", res.Fixed, err, src != orig)
	}
	// Premise: the partial report holds an eligible finding the loop would
	// otherwise have acted on.
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	for i, r := range rules {
		if c, ok := r.(cancellingRule); ok {
			rules[i] = cancellingRule{BlockRule: c.BlockRule, cancel: cancel2}
		}
	}
	doc, err := parser.ParseText(orig, "Main.txt", int64(len(orig)))
	if err != nil {
		t.Fatal(err)
	}
	partial := RunAnalysisCtx(ctx2, doc, AutoFixableRules(rules), models.DefaultSettings(), nil)
	eligible := 0
	for _, f := range partial.Findings {
		if f.AutoFix != "" && !IsBehaviourChangingFix(f.AutoFix) {
			eligible++
		}
	}
	if eligible == 0 {
		t.Fatal("fixture rot: the partial analysis has no fixable finding")
	}
}

// TestApplyFixes_BatchingChangesTheResult records why the loop is not
// batched. Two file reads share a LOOP. The loop wraps the first in ON BLOCK
// ERROR; the re-analysis then counts its sibling as handled (a sibling is an
// error handler), so the second read is never wrapped. Applying both
// non-overlapping patches from the first analysis in one pass — the batching
// that would save the re-parse — wraps both.
func TestApplyFixes_BatchingChangesTheResult(t *testing.T) {
	const name = "Main.txt"
	src := "#Region \"Main\"\n" +
		"LOOP LoopIndex FROM 1 TO 3 STEP 1\n" +
		"    File.ReadTextFromFile.ReadText File: $'''C:\\\\a.txt''' Encoding: File.TextFileEncoding.UTF8 Content=> A\n" +
		"    File.ReadTextFromFile.ReadText File: $'''C:\\\\b.txt''' Encoding: File.TextFileEncoding.UTF8 Content=> B\n" +
		"END\n" +
		"#EndRegion\n"

	seq := src
	var applied []string
	if _, err := ApplyFixesToSource(&seq, name, FixOptions{Limit: 50, OnFix: func(r, f string, line int) {
		applied = append(applied, fmt.Sprintf("%s@%d", r, line))
	}}); err != nil {
		t.Fatal(err)
	}
	if want := []string{"file-op-no-error-handler@3", "error-swallow@3"}; !reflect.DeepEqual(applied, want) {
		t.Fatalf("sequential loop applied %v, want %v", applied, want)
	}
	if got := strings.Count(seq, "ON BLOCK ERROR"); got != 1 {
		t.Fatalf("sequential loop wrapped %d reads, want 1:\n%s", got, seq)
	}

	// One batched pass over the first analysis.
	doc, err := parser.ParseText(src, name, int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	report := RunAnalysis(doc, AutoFixableRules(AllRules()), models.DefaultSettings(), nil)
	var patches []models.Patch
	lines := map[int]bool{}
	for i := range report.Findings {
		f := &report.Findings[i]
		if f.RuleID != "file-op-no-error-handler" {
			continue
		}
		b := doc.BlocksByID[f.BlockID]
		if b == nil || lines[b.LineNumber] {
			continue
		}
		p, err := PatchForFix(b, f.AutoFix, f.RuleID, "", "")
		if err != nil || len(p.Ops) == 0 {
			t.Fatalf("patch for line %d: %v", b.LineNumber, err)
		}
		lines[b.LineNumber] = true
		patches = append(patches, p)
	}
	if len(patches) != 2 {
		t.Fatalf("fixture rot: the first analysis has %d wrappable reads, want 2", len(patches))
	}
	batched := src
	for i := len(patches) - 1; i >= 0; i-- { // bottom-up, so line numbers hold
		batched = ApplyPatch(batched, patches[i])
	}
	if got := strings.Count(batched, "ON BLOCK ERROR"); got != 2 {
		t.Fatalf("batched pass wrapped %d reads, want 2:\n%s", got, batched)
	}
}
