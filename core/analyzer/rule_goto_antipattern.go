package analyzer

import (
	"strings"

	"pad-core/models"
)

type GotoAntipatternRule struct{}

func (r *GotoAntipatternRule) ID() string   { return "goto-antipattern" }
func (r *GotoAntipatternRule) Name() string { return "GOTO anti-pattern detected" }
func (r *GotoAntipatternRule) Description() string {
	return "GOTO jumps that break scope boundaries, spaghetti control flow, or orphaned labels."
}
func (r *GotoAntipatternRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *GotoAntipatternRule) Category() string                 { return "Logic" }

func (r *GotoAntipatternRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.RawType != "GOTO" {
		return nil
	}

	targetLabel := extractGotoTarget(block)
	if targetLabel == "" {
		return nil
	}

	var findings []models.Finding

	if isScopeBreakingGoto(block, targetLabel, ctx) {
		findings = append(findings, models.Finding{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "GOTO breaks scope boundary",
			Description: "This GOTO jumps to a label outside its current scope (across loop/condition/switch boundaries), which creates fragile and hard-to-maintain control flow.",
			BlockID:     block.ID,
			SubflowID:   block.SubflowID,
			Suggestion:  "Restructure the flow to avoid cross-scope GOTO. Use Exit Loop, flags, or extract logic into subflows instead.",
			Metadata:    map[string]interface{}{"targetLabel": targetLabel},
		})
	}

	return findings
}

func extractGotoTarget(block *models.Block) string {
	if block.Properties != nil {
		if t, ok := block.Properties["_target"]; ok && t != "" {
			return t
		}
		if t, ok := block.Properties["labelName"]; ok && t != "" {
			return t
		}
		if t, ok := block.Properties["target"]; ok && t != "" {
			return t
		}
	}
	name := block.Name
	if after, ok := strings.CutPrefix(name, "Goto "); ok {
		return strings.TrimSpace(after)
	}
	if after, ok := strings.CutPrefix(name, "Go to "); ok {
		return strings.TrimSpace(after)
	}
	// Parser-produced GOTO blocks expose the label as a "label" token (the
	// tokenizer strips the "GOTO " verb, leaving the label in Name/Tokens).
	for _, t := range block.Tokens {
		if t.Type == "label" {
			if t.Target != "" {
				return t.Target
			}
			if t.Value != "" {
				return t.Value
			}
		}
	}
	if name := strings.TrimSpace(block.Name); name != "" {
		return name
	}
	return ""
}

// buildLabelIndex maps each label name (lowercased, so lookups are
// case-insensitive like the previous EqualFold scan) to its first LABEL block in
// document order. Walking in document order keeps the result deterministic when
// duplicate label names exist; the old per-GOTO map scan picked an arbitrary one.
func buildLabelIndex(ctx *RuleContext) map[string]*models.Block {
	idx := make(map[string]*models.Block)
	walkBlocks(ctx.Flow, func(b *models.Block) {
		if b.RawType == "LABEL" {
			key := strings.ToLower(b.Name)
			if _, ok := idx[key]; !ok {
				idx[key] = b
			}
			// Count per (lowercased) name in the same pass — duplicate-label
			// reads this instead of re-scanning AllBlocks per LABEL block
			// (which made the rule O(labels × blocks)).
			ctx.LabelNameCount[key]++
		}
	})
	return idx
}

func isScopeBreakingGoto(gotoBlock *models.Block, targetLabel string, ctx *RuleContext) bool {
	gotoDepth := ctx.BlockDepth[gotoBlock.ID]

	labelBlock := ctx.LabelByName[strings.ToLower(targetLabel)]
	if labelBlock == nil {
		return false
	}

	labelDepth := ctx.BlockDepth[labelBlock.ID]
	if labelDepth != gotoDepth {
		return true
	}

	gotoParent := ctx.ParentMap[gotoBlock.ID]
	labelParent := ctx.ParentMap[labelBlock.ID]

	if gotoParent != labelParent {
		gotoAncestors := ancestorSet(gotoBlock.ID, ctx)
		labelAncestors := ancestorSet(labelBlock.ID, ctx)
		for a := range gotoAncestors {
			if !labelAncestors[a] {
				parentBlock := ctx.AllBlocks[a]
				if parentBlock != nil && isScopeContainer(parentBlock) {
					return true
				}
			}
		}
		for a := range labelAncestors {
			if !gotoAncestors[a] {
				parentBlock := ctx.AllBlocks[a]
				if parentBlock != nil && isScopeContainer(parentBlock) {
					return true
				}
			}
		}
	}

	return false
}

func ancestorSet(blockID string, ctx *RuleContext) map[string]bool {
	seen := make(map[string]bool)
	cur := blockID
	for {
		pid, ok := ctx.ParentMap[cur]
		if !ok {
			break
		}
		seen[pid] = true
		cur = pid
	}
	return seen
}

func isScopeContainer(b *models.Block) bool {
	switch b.Type {
	case models.BlockTypeLoop, models.BlockTypeCondition, models.BlockTypeSwitch,
		models.BlockTypeErrorHandler:
		return true
	}
	return false
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&GotoAntipatternRule{}) }
