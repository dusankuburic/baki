package analyzer

import (
	"strings"

	"pad-core/models"
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
	// Bounded counter loop: the loop header references a variable that is
	// reassigned (SET) inside the loop body. The auto-generated retry loop
	// (LOOP WHILE %RetryCount% < 3 … SET RetryCount TO %RetryCount% + 1) is the
	// canonical case — without this recognition the missing-retry fixer would
	// trade one finding for an infinite-loop-risk finding.
	if loopConditionVarModified(loop) {
		return true
	}
	// An explicit Exit Loop / Break in the loop's OWN body. EXIT LOOP only
	// exits the innermost enclosing loop, so an exit inside a nested loop does
	// NOT terminate this one — walkLoopBody skips nested-loop subtrees.
	found := false
	walkLoopBody(loop, func(b *models.Block) {
		if isExitLoop(b) {
			found = true
			return
		}
		rt := strings.ToLower(b.RawType)
		if strings.Contains(rt, "exit") || strings.Contains(rt, "break") {
			found = true
			return
		}
		for _, v := range b.Properties {
			if strings.Contains(v, "Exit") || strings.Contains(v, "Break") {
				found = true
				return
			}
		}
	})
	return found
}

// walkLoopBody visits descendants of loop that are NOT inside a nested loop.
// EXIT LOOP only exits the innermost enclosing loop, so an exit (or counter
// modification) inside a nested loop must not count for an outer loop — the old
// full-subtree walk let a nested-loop exit wrongly satisfy the outer loop.
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

// loopConditionVarModified reports whether the loop's header references a
// variable that is reassigned (via SET) somewhere in the loop's own body. Such
// a loop is bounded: each iteration advances the referenced variable toward the
// condition's bound. Recognizing this keeps generated retry/range loops from
// being flagged as infinite.
func loopConditionVarModified(loop *models.Block) bool {
	if len(loop.Variables) == 0 {
		return false
	}
	condVars := make(map[string]bool, len(loop.Variables))
	for _, v := range loop.Variables {
		condVars[v] = true
	}
	modified := false
	walkLoopBody(loop, func(b *models.Block) {
		if modified {
			return
		}
		// A SET action writes its _output variable; if that variable appears in
		// the loop condition the loop MIGHT be bounded — but only if the write
		// actually progresses toward the bound.
		out := b.Properties["_output"]
		if out == "" || !condVars[out] {
			return
		}
		val := b.Properties["_value"]
		wrapped := "%" + out + "%"
		// Exclude non-progressing writes:
		//  - a constant reset like `SET Found TO FALSE` (value doesn't reference
		//    the var at all → the loop `WHILE %Found%=FALSE` never terminates); and
		//  - a bare self-no-op `SET X TO %X%`.
		// A real counter advance (`SET RetryCount TO %RetryCount% + 1`) references
		// the var PLUS an operand, so it counts.
		if strings.Contains(val, wrapped) && strings.TrimSpace(val) != wrapped {
			modified = true
		}
	})
	return modified
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&InfiniteLoopRiskRule{}) }
