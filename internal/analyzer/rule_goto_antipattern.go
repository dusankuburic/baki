package analyzer

import (
	"strings"

	"pad-analyzer/internal/models"
)

type GotoAntipatternRule struct{}

func (r *GotoAntipatternRule) ID() string                    { return "goto-antipattern" }
func (r *GotoAntipatternRule) Name() string                   { return "GOTO anti-pattern detected" }
func (r *GotoAntipatternRule) Description() string            { return "GOTO jumps that break scope boundaries, spaghetti control flow, or orphaned labels." }
func (r *GotoAntipatternRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (r *GotoAntipatternRule) Category() string               { return "Logic" }

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
	return ""
}

func isScopeBreakingGoto(gotoBlock *models.Block, targetLabel string, ctx *RuleContext) bool {
	gotoDepth := ctx.BlockDepth[gotoBlock.ID]

	var labelBlock *models.Block
	for _, b := range ctx.AllBlocks {
		if b.RawType == "LABEL" && (b.Name == targetLabel || strings.EqualFold(b.Name, targetLabel)) {
			labelBlock = b
			break
		}
	}

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
