package analyzer

import (
	"fmt"

	"pad-core/models"
	"pad-core/parser"
)

// FixLoopResult reports what ApplyFixesToSourceDoc did and what it learned in
// passing, so callers don't re-parse source the loop already parsed.
type FixLoopResult struct {
	// Fixed is the number of fixes applied.
	Fixed int
	// Doc is the parser's output for the FINAL source (post-fixes). It is the
	// exact parse the caller would get from re-parsing *source themselves —
	// reuse it instead of paying another full parse. Nil only when the loop
	// aborted with an error before completing an iteration.
	Doc *models.FlowDocument
	// BeforeErrors / AfterErrors are the error-severity parse-error counts of
	// the original and the fixed source, taken from the loop's own first and
	// last parses (the loop parses once per iteration regardless). Callers
	// that gate persistence on "the fix didn't introduce parse errors" can
	// compare these two instead of parsing both versions again.
	BeforeErrors, AfterErrors int
}

// fixLoopStallLimit bounds how many consecutive applied fixes may fail to
// shrink the pool of remaining fixable findings before the loop gives up.
// A well-behaved fix strictly resolves its finding (pool -1), possibly
// revealing a new one (net 0) — a small window tolerates that. But a fixer
// whose patch doesn't resolve its finding (e.g. a mis-indented insert the
// parser won't nest), or two fixers that alternately create each other's
// findings, keeps the pool flat forever while rewriting the file up to
// `limit` times. The per-signature no-progress guard can't catch that class:
// each insert shifts line numbers, so content fingerprints never repeat.
const fixLoopStallLimit = 3

// FixOptions carries the analysis configuration the fix loop runs under.
//
// It exists because the loop used to hardcode models.DefaultSettings() and the
// built-in rule set, which made it disagree with the analysis that produced the
// findings in the first place: a rule disabled in .bakirc.json (or in an org's
// profile) reported nothing, yet `bakicli fix` still rewrote the flow to
// "resolve" it, and a custom rule carrying an autoFix was never applied at all.
// The loop must see the SAME configuration the analyze pass saw.
//
// The zero value reproduces the pre-options behaviour exactly (built-in rules,
// default settings, no filter), so a caller with nothing to configure can pass
// FixOptions{Limit: n}.
type FixOptions struct {
	// RuleFilter nil ⇒ every auto-fixable rule; otherwise only findings whose
	// RuleID is in the set are fixed (the UI's "fix all selected rules",
	// `bakicli fix -rule`).
	RuleFilter map[string]bool
	// Settings gates which rules run and at what severity/options, exactly as
	// in the analysis path. Nil ⇒ models.DefaultSettings().
	Settings *models.AppSettings
	// Rules is the COMPLETE rule set to run — built-ins plus any configured
	// custom rules, exactly as assembled for the analysis pass. Nil ⇒
	// AllRules(). It is not "extra rules appended to the built-ins": the
	// resolved set a caller already holds (an org profile, the CLI's
	// selectRules) contains the built-ins, and appending it would dispatch
	// every built-in twice.
	Rules []Rule
	// Limit bounds the number of fix iterations (safety cap against a fixer
	// that never resolves its finding). <= 0 ⇒ no fixes are attempted.
	Limit int
	// IncludeBehaviourChanging opts into fixes that alter what the flow DOES at
	// runtime rather than how it handles failure — see IsBehaviourChangingFix.
	// Off by default, because this loop runs unattended from `bakicli fix` and
	// the UI's "fix all", and those fixes need a human to accept the trade.
	IncludeBehaviourChanging bool
	// OnFix (nullable) is invoked per applied fix for progress logging.
	OnFix func(ruleID, fixType string, line int)
}

// settingsOrDefault is the loop's single read of opts.Settings. Nil means "the
// caller has no opinion", which is the built-in defaults — NOT "no settings",
// which would leave every rule's severity and options unresolved.
func (o FixOptions) settingsOrDefault() *models.AppSettings {
	if o.Settings != nil {
		return o.Settings
	}
	return models.DefaultSettings()
}

// ApplyFixesToSourceDoc runs the iterative auto-fix loop on source in place
// (mutating *source) and returns the loop's final parse plus the parse-error
// counts it already computed. See ApplyFixesToSource for the loop's semantics;
// this variant exists so server/CLI callers can reuse the final doc and the
// before/after error gate instead of re-parsing the source two extra times.
func ApplyFixesToSourceDoc(source *string, fileName string, opts FixOptions) (FixLoopResult, error) {
	var res FixLoopResult
	// No-progress guard: track the last applied fix's signature. If the next
	// iteration picks a finding with the same signature, the previous fix
	// didn't resolve it → break to avoid a wasteful loop.
	var lastFixSig string
	// skipped records findings whose fixer declined/errored this run (keyed by
	// content fingerprint) so they aren't retried every iteration. A declined
	// fixer won't resolve its finding, but other fixable findings still can.
	skipped := make(map[string]bool)
	// eligible mirrors every filter the pick scan applies. The stall guard counts
	// "the pool a fix has to shrink", so if it counted candidates the picker
	// then skipped, the pool could never reach zero and the loop would spin to
	// its limit.
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
	// Stall detection (see fixLoopStallLimit): applied fixes that fail to
	// drive the remaining-fixable pool to a NEW MINIMUM. The guard must count
	// from the minimum, not from the previous iteration: an oscillator whose
	// pool dips and regrows (3→2→3→2…) resets a consecutive-flat counter on
	// every dip and runs to `limit` — exactly the duplicate-action/remove
	// livelock this guard exists to stop. A legitimate cascade still passes:
	// each reveal keeps the pool flat for at most a couple of steps before the
	// next shrink (6,5,4,3,2,2,1,0 stalls at most once).
	stall := 0
	minFixable := -1
	// The caller's rule set (nil ⇒ the built-ins), narrowed to the rules that
	// can emit an AutoFix: the loop only ever selects findings with one, so
	// dispatching the rest is wasted work — this cuts per-iteration rule
	// dispatch by more than half with no behavior change (the dropped rules'
	// findings are never selected, and dedup keys on block+title+subject, so
	// dropping them cannot reorder or fold the fixable findings).
	//
	// Settings then gate which of those actually run: RunAnalysis skips a rule
	// the caller disabled, which is what keeps the loop in step with the
	// analysis that produced the findings.
	ruleSet := opts.Rules
	if ruleSet == nil {
		ruleSet = AllRules()
	}
	rules := AutoFixableRules(ruleSet)
	settings := opts.settingsOrDefault()
	// docIsCurrent tracks whether Doc is the parse of the current *source.
	// Each iteration's parse sets it; applying a patch invalidates it (the
	// post-patch source hasn't been parsed yet). If the loop exits right
	// after an apply (limit exhausted), one final parse brings Doc current.
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

		// Stall check BEFORE picking: count the un-skipped fixable candidates
		// this iteration. The pick scan below mirrors these filters, so the
		// count is exactly the pool a fix has to shrink.
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

		// Pick the first auto-fixable finding (matching the filter, not already
		// skipped) whose fixer accepts the work. Trying each candidate's patch
		// in-line means a single declined fixer no longer aborts the whole run.
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
				// Fixer declined (e.g. no output var) or errored. Skip this
				// finding for the rest of the run; other findings may still be
				// fixable.
				skipped[sig] = true
				continue
			}
			pick, pickBlock, patch = f, block, candidate
			break
		}
		if pick == nil {
			break // no more un-skipped, fixable findings
		}
		// No-progress guard: if the same finding (by content fingerprint) was
		// picked in the previous iteration, the fix didn't resolve it. Break to
		// avoid re-applying the same non-resolving patch up to `limit`.
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
	// The loop exited with a patch applied but not re-parsed (limit exhausted
	// or the last iteration's apply) — parse once so Doc matches *source.
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

// ApplyFixesToSource runs the iterative auto-fix loop on source in place
// (mutating *source), returning the number of fixes applied. Shared by the
// headless CLI (bakicli fix) and the server's batch apply-fix endpoint.
//
// Iterative: parse → analyze → apply the first fixable finding's patch that the
// fixer actually accepts → re-parse → repeat. Re-parsing each iteration is what
// makes it correct: a patch shifts line numbers (and the parser mints fresh
// block IDs), so fixes computed against the original source would target the
// wrong lines if batch-applied.
//
// Within one iteration each fixable finding's patch is tried in turn: a fixer
// that declines (empty patch, e.g. no _output variable) or errors is recorded
// in `skipped` and the next candidate is tried, so a single declined fixer can
// no longer block the rest of the fixes in the flow. The loop stops when no
// un-skipped fixable finding remains, the same finding reappears unchanged
// after an applied fix (no-progress guard), or the limit is hit.
//
// See FixOptions for what the loop can be configured with; its zero value plus
// a Limit reproduces the built-in rules at their default settings.
func ApplyFixesToSource(source *string, fileName string, opts FixOptions) (int, error) {
	res, err := ApplyFixesToSourceDoc(source, fileName, opts)
	return res.Fixed, err
}

// CountParseErrors counts error-severity (not warning) parse problems in a
// parsed doc. Exported so service-layer gates ("a fix must not introduce
// parse errors") share one implementation instead of a mirrored copy.
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
