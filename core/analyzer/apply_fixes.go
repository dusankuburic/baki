package analyzer

import (
	"context"
	"fmt"

	"pad-core/models"
	"pad-core/parser"
)

// FixLoopResult reports what ApplyFixesToSourceDoc did and learned in passing, so callers need not re-parse.
type FixLoopResult struct {
	// Fixed is the number of fixes applied.
	Fixed int
	// Doc is the parser's output for the FINAL source (post-fixes); nil only
	// when the loop aborted before completing an iteration.
	Doc *models.FlowDocument
	// BeforeErrors / AfterErrors are the error-severity parse-error counts of
	// the original and fixed source, so "no new parse errors" gates need not
	// parse both versions.
	BeforeErrors, AfterErrors int
	// Partial reports ctx ended the loop after at least one fix (*source
	// holds them, Doc is their parse); the call still returns ctx.Err()
	// alongside. Never set when nothing was applied.
	Partial bool
}

// fixLoopStallLimit bounds consecutive applied fixes that fail to shrink the remaining-finding pool:
// alternating fixers can keep the pool flat forever, and the per-signature no-progress guard can't
// catch that (each insert shifts line numbers, re-keying content fingerprints).
const fixLoopStallLimit = 3

// FixOptions carries the analysis configuration the fix loop runs under — the
// SAME configuration the analyze pass saw, or it "resolves" findings a
// disabled rule never reported. The zero value reproduces pre-options behaviour.
type FixOptions struct {
	// RuleFilter nil ⇒ every auto-fixable rule; otherwise only findings whose RuleID is in the set.
	RuleFilter map[string]bool
	// Settings gates which rules run and at what severity/options, exactly as
	// in the analysis path; nil ⇒ models.DefaultSettings().
	Settings *models.AppSettings
	// Rules is the COMPLETE rule set exactly as assembled for the analysis
	// pass; nil ⇒ AllRules(). Not "extra rules appended to the built-ins":
	// appending the resolved set would dispatch every built-in twice.
	Rules []Rule
	// Limit bounds the number of fix iterations (safety cap); <= 0 ⇒ no fixes attempted.
	Limit int
	// IncludeBehaviourChanging opts into fixes that alter what the flow DOES
	// at runtime (see IsBehaviourChangingFix); off by default — unattended loop.
	IncludeBehaviourChanging bool
	// OnFix (nullable) is invoked per applied fix for progress logging.
	OnFix func(ruleID, fixType string, line int)
}

// settingsOrDefault is the loop's single read of opts.Settings; nil means "no opinion" (defaults), not "no settings".
func (o FixOptions) settingsOrDefault() *models.AppSettings {
	if o.Settings != nil {
		return o.Settings
	}
	return models.DefaultSettings()
}

// ApplyFixesToSourceDoc runs the fix loop on source in place (mutating
// *source) and returns the final parse plus the parse-error counts — see
// ApplyFixesToSource for semantics; server/CLI callers reuse the final doc.
func ApplyFixesToSourceDoc(source *string, fileName string, opts FixOptions) (FixLoopResult, error) {
	return ApplyFixesToSourceDocCtx(context.Background(), source, fileName, opts)
}

// ApplyFixesToSourceDocCtx is ApplyFixesToSourceDoc bounded by ctx: every
// iteration is a full parse and analysis, so the loop checks ctx before each
// one. Once ctx is done it stops after the current iteration; with fixes
// already applied it still finishes the final parse and marks Partial.
func ApplyFixesToSourceDocCtx(ctx context.Context, source *string, fileName string, opts FixOptions) (FixLoopResult, error) {
	return applyFixesLoop(ctx, source, fileName, opts, make(map[string]bool))
}

// applyFixesLoop is the fix loop; skipped is the caller's (empty) set of declined-finding fingerprints.
func applyFixesLoop(ctx context.Context, source *string, fileName string, opts FixOptions, skipped map[string]bool) (FixLoopResult, error) {
	var res FixLoopResult
	// No-progress guard: the next iteration picking a finding with the same signature as the last applied fix → break.
	var lastFixSig string
	// skipped records findings whose fixer declined/errored this run (keyed
	// by content fingerprint) so they aren't retried every iteration; it also
	// mirrors every filter the pick scan applies (eligible below), so the
	// stall guard counts the exact pool a fix has to shrink.
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
	// the previous iteration, catches an oscillator (3→2→3→2…); a cascade still passes.
	stall := 0
	minFixable := -1
	// The caller's rule set (nil ⇒ the built-ins), narrowed to the rules that
	// can emit an AutoFix — the loop only selects findings with one. Settings
	// then gate which actually run, keeping the loop in step with the analysis
	// that produced the findings.
	ruleSet := opts.Rules
	if ruleSet == nil {
		ruleSet = AllRules()
	}
	rules := AutoFixableRules(ruleSet)
	settings := opts.settingsOrDefault()
	// docIsCurrent tracks whether Doc is the parse of the current *source; applying a patch invalidates it.
	docIsCurrent := false
	// interrupted is ctx's error once it ends the loop early; the loop falls
	// through to the final parse below rather than returning from inside, so
	// fixes already in *source are kept — see FixLoopResult.Partial.
	var interrupted error
	for iter := 0; iter < opts.Limit; iter++ {
		if err := ctx.Err(); err != nil {
			interrupted = err
			break
		}
		doc, err := parser.ParseText(*source, fileName, int64(len(*source)))
		if err != nil {
			return res, fmt.Errorf("parse error during fix loop: %w", err)
		}
		res.Doc = doc
		docIsCurrent = true
		if iter == 0 {
			res.BeforeErrors = countParseErrors(doc)
		}
		// No-profile variant: the loop never reads RuleProfiles and re-analyzes per iteration.
		report := RunAnalysisCtxNoProfile(ctx, doc, rules, settings, nil)
		if err := ctx.Err(); err != nil {
			// The walk stopped early: the report is partial, never act on it.
			interrupted = err
			break
		}

		// Stall check BEFORE picking: count the un-skipped fixable candidates —
		// exactly the pool a fix has to shrink (the pick scan mirrors these filters).
		fixable := 0
		for i := range report.Findings {
			f := &report.Findings[i]
			if !eligible(f) {
				continue
			}
			if !skipped[diffKey(*f)] {
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

		// Pick the first auto-fixable finding (matching the filter, not already skipped) whose fixer accepts the work.
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
			sig := diffKey(*f)
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
				// Fixer declined or errored: skip this finding for the rest of the run.
				skipped[sig] = true
				continue
			}
			pick, pickBlock, patch = f, block, candidate
			break
		}
		if pick == nil {
			break // no more un-skipped, fixable findings
		}
		// No-progress guard: the same finding (by content fingerprint) picked twice
		// in a row means the fix didn't resolve it — break instead of re-applying.
		curSig := diffKey(*pick)
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
	if interrupted != nil && res.Fixed == 0 {
		// Nothing applied: nothing to keep, and no parse on a dead context.
		return res, interrupted
	}
	// The loop exited with a patch applied but not re-parsed — parse once so Doc matches *source.
	if res.Doc == nil || (res.Fixed > 0 && !docIsCurrent) {
		doc, err := parser.ParseText(*source, fileName, int64(len(*source)))
		if err != nil {
			return res, fmt.Errorf("parse error during fix loop: %w", err)
		}
		res.Doc = doc
	}
	res.AfterErrors = countParseErrors(res.Doc)
	if interrupted != nil {
		res.Partial = true
		return res, interrupted
	}
	return res, nil
}

// ApplyFixesToSource runs the iterative auto-fix loop on source in place (mutating *source); shared
// by bakicli fix and the server's batch apply-fix endpoint.
//
// Iterative — parse → analyze → apply the first accepted fixable patch → re-parse → repeat: a patch
// shifts line numbers and re-mints block IDs, so batch-applying fixes computed against the original
// source would target wrong lines. Stops when no un-skipped fixable finding remains, the same finding
// reappears unchanged (no-progress guard), or the limit is hit; ApplyFixesToSourceDocCtx bounds it.
//
// Batching non-overlapping patches per iteration does NOT reproduce this loop — whether a finding is
// still there, still first, or still counted depends on the fixes applied before it
// (TestApplyFixes_BatchingChangesTheResult).
func ApplyFixesToSource(source *string, fileName string, opts FixOptions) (int, error) {
	res, err := ApplyFixesToSourceDoc(source, fileName, opts)
	return res.Fixed, err
}

// ApplyFixesToSourceCtx is ApplyFixesToSource bounded by ctx (see ApplyFixesToSourceDocCtx).
func ApplyFixesToSourceCtx(ctx context.Context, source *string, fileName string, opts FixOptions) (int, error) {
	res, err := ApplyFixesToSourceDocCtx(ctx, source, fileName, opts)
	return res.Fixed, err
}

// CountParseErrors counts error-severity (not warning) parse problems; exported so service gates share one implementation.
func CountParseErrors(doc *models.FlowDocument) int {
	if doc == nil {
		return 1 << 30
	}
	n := 0
	for _, e := range doc.ParseErrors {
		if e.Severity == "error" {
			n++
		}
	}
	return n
}

// countParseErrors is the internal alias used by the fix loop.
func countParseErrors(doc *models.FlowDocument) int { return CountParseErrors(doc) }
