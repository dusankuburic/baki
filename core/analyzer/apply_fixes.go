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
// Iterative: parse → analyze → apply the first auto-fixable finding's patch →
// re-parse → repeat. Re-parsing each iteration is what makes it correct: a
// patch shifts line numbers (and the parser mints fresh block IDs), so fixes
// computed against the original source would target the wrong lines if batch-
// applied. Stops when no fixable finding remains, a fixer declines (empty
// patch), the same finding reappears unchanged after a fix (no-progress guard),
// or limit is hit.
//
// ruleFilter nil ⇒ all auto-fixable rules; otherwise only findings whose RuleID
// is in the set are fixed (used by "fix all selected rules"). onFix (nullable)
// is invoked per applied fix for progress logging.
func ApplyFixesToSource(source *string, fileName string, ruleFilter map[string]bool, limit int, onFix func(ruleID, fixType string, line int)) (int, error) {
	fixed := 0
	// No-progress guard: track the last applied fix's (ruleID, fixType, lineNumber).
	// If the next iteration picks a finding with the same signature on the same line,
	// the previous fix didn't resolve it → break to avoid a wasteful loop.
	var lastFixSig string
	for iter := 0; iter < limit; iter++ {
		doc, err := parser.ParseText(*source, fileName, int64(len(*source)))
		if err != nil {
			return fixed, fmt.Errorf("parse error during fix loop: %w", err)
		}
		report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)

		// Pick the first auto-fixable finding matching the rule filter.
		var pick *models.Finding
		for i := range report.Findings {
			f := &report.Findings[i]
			if f.AutoFix == "" {
				continue
			}
			if ruleFilter != nil && !ruleFilter[f.RuleID] {
				continue
			}
			pick = f
			break
		}
		if pick == nil {
			break // no more fixable findings
		}
		block := doc.BlocksByID[pick.BlockID]
		if block == nil {
			break // stale finding (block gone) — shouldn't happen post-reparse
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

		variable, _ := pick.Metadata["variable"].(string)
		property, _ := pick.Metadata["property"].(string)
		patch, err := PatchForFix(block, pick.AutoFix, pick.RuleID, variable, property)
		if err != nil || len(patch.Ops) == 0 {
			break // unknown fix or fixer declined (e.g. no output var) — stop
		}
		if onFix != nil {
			onFix(pick.RuleID, pick.AutoFix, block.LineNumber)
		}
		*source = ApplyPatch(*source, patch)
		fixed++
	}
	return fixed, nil
}
