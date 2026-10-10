package analyzer

import (
	"strings"

	"pad-core/models"
	"pad-core/parser"
)

type InfiniteLoopRiskRule struct{}

func (r *InfiniteLoopRiskRule) ID() string   { return "infinite-loop-risk" }
func (r *InfiniteLoopRiskRule) Name() string { return "Loop may run forever" }
func (r *InfiniteLoopRiskRule) Description() string {
	return "LOOP blocks with no recognizable exit condition."
}
func (r *InfiniteLoopRiskRule) DefaultSeverity() models.Severity { return models.SeverityError }
func (r *InfiniteLoopRiskRule) Category() string                 { return "Reliability" }

func (r *InfiniteLoopRiskRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeLoop {
		return nil
	}

	// A loop that terminates BY CONSTRUCTION cannot run forever: FOREACH
	// consumes a fixed collection, a counted range has an end. Judging every
	// loop by hasExitCondition alone produced 204 of 204 Error findings on the
	// corpus, all on bounded for-each loops.
	switch block.Properties[parser.LoopKindProperty] {
	case parser.LoopKindForEach, parser.LoopKindCounted:
		return nil
	}

	if hasExitCondition(block) {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Loop may run forever",
		Description: "This loop has no recognizable exit condition such as Exit Loop, Break, or modifications to the loop variable.",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Add an 'Exit loop' action or ensure the loop variable is modified to guarantee termination.",
		AutoFixHint: "Add an IF block inside the loop that checks a counter or sentinel condition, then use 'Exit loop' to break out. Example: IF %LoopIndex% >= %MaxIterations% THEN Exit loop.",
	}}
}

func hasExitCondition(loop *models.Block) bool {
	// Two predicates, computed in ONE fused walk of the body: 1 — a bounded
	// counter loop (the loop header references a variable reassigned inside
	// the body; the auto-generated retry loop is the canonical case); 2 — an
	// explicit Exit Loop/Break in the loop's OWN body — EXIT LOOP only exits
	// the innermost enclosing loop, so walkLoopBody skips nested-loop subtrees
	// for BOTH predicates. Only real control-flow constructs count (exact
	// name/RawType token): the rule used to accept any value containing "Exit"
	// or "Break", so `Display.ShowMessageBox Message: 'Exit code received'`
	// declared the loop safe.
	condVars := make(map[string]bool, len(loop.Variables))
	for _, v := range loop.Variables {
		condVars[v] = true
	}
	condModified := false
	exitFound := false
	walkLoopBody(loop, func(b *models.Block) {
		if !exitFound && (isExitLoop(b) || isExitLoopRawType(b.RawType)) {
			exitFound = true
		}
		// Once a predicate is answered, its per-block work stops; the shared
		// walk itself is the same single pass either way.
		if condModified || len(condVars) == 0 {
			return
		}
		// A SET writes its _output variable; if it appears in the loop
		// condition the loop MIGHT be bounded — but only if the write
		// progresses toward the bound.
		out := b.Properties["_output"]
		if out == "" || !condVars[out] {
			return
		}
		// PAD's idiomatic counter step is an ACTION, not a SET
		// (`Variables.IncreaseVariable Value: 1 Name: Counter` — no _value
		// referencing the variable); Increase/Decrease move monotonically,
		// precisely progress.
		switch b.RawType {
		case "Variables.IncreaseVariable", "Variables.DecreaseVariable":
			condModified = true
			return
		}
		val := b.Properties["_value"]
		wrapped := "%" + out + "%"
		// Exclude non-progressing writes: a constant reset (`SET Found TO FALSE`
		// — `WHILE %Found%=FALSE` never terminates) and a bare self-no-op
		// `SET X TO %X%`; a real advance (`SET RetryCount TO %RetryCount% + 1`)
		// references the var PLUS an operand.
		if strings.Contains(val, wrapped) && strings.TrimSpace(val) != wrapped {
			condModified = true
		}
	})
	return condModified || exitFound
}

// isExitLoopRawType reports whether rawType is exactly one of the parser's
// loop-exit statements (case-insensitive, whole token — no substring match).
func isExitLoopRawType(rawType string) bool {
	switch strings.ToUpper(strings.TrimSpace(rawType)) {
	case "EXIT LOOP", "EXIT_LOOP", "EXITLOOP", "BREAK":
		return true
	}
	return false
}

// walkLoopBody visits descendants of loop NOT inside a nested loop: EXIT LOOP
// only exits the innermost enclosing loop, so a nested-loop exit must not
// count for an outer loop (the old full-subtree walk let it).
func walkLoopBody(loop *models.Block, fn func(*models.Block)) {
	var walk func(b *models.Block)
	walk = func(b *models.Block) {
		for i := range b.Children {
			c := &b.Children[i]
			fn(c)
			// Don't descend into nested loops — their exits belong to them.
			if c.Type != models.BlockTypeLoop {
				walk(c)
			}
		}
	}
	walk(loop)
}

func init() {
	registerRule(&InfiniteLoopRiskRule{}, RuleMeta{
		Confidence:   models.ConfidenceHigh,
		AutoFix:      "insert-exit-condition",
		IntroducedIn: 1,
		Revision:     1,
		Stage:        StageStable,
	})
}
