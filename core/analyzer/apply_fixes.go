package analyzer

import (
	"fmt"

	"pad-core/models"
	"pad-core/parser"
)

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
// ruleFilter nil ⇒ all auto-fixable rules; otherwise only findings whose RuleID
// is in the set are fixed (used by "fix all selected rules"). onFix (nullable)
// is invoked per applied fix for progress logging.
func ApplyFixesToSource(source *string, fileName string, ruleFilter map[string]bool, limit int, onFix func(ruleID, fixType string, line int)) (int, error) {
	fixed := 0
	// No-progress guard: track the last applied fix's signature. If the next
	// iteration picks a finding with the same signature, the previous fix
	// didn't resolve it → break to avoid a wasteful loop.
	var lastFixSig string
	// skipped records findings whose fixer declined/errored this run (keyed by
	// content fingerprint) so they aren't retried every iteration. A declined
	// fixer won't resolve its finding, but other fixable findings still can.
	skipped := make(map[string]bool)
	for iter := 0; iter < limit; iter++ {
		doc, err := parser.ParseText(*source, fileName, int64(len(*source)))
		if err != nil {
			return fixed, fmt.Errorf("parse error during fix loop: %w", err)
		}
		report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)

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
			if f.AutoFix == "" {
				continue
			}
			if ruleFilter != nil && !ruleFilter[f.RuleID] {
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

		if onFix != nil {
			onFix(pick.RuleID, pick.AutoFix, pickBlock.LineNumber)
		}
		*source = ApplyPatch(*source, patch)
		fixed++
	}
	return fixed, nil
}
