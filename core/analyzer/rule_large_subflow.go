package analyzer

import (
	"strconv"

	"pad-core/models"
)

type LargeSubflowRule struct{}

func (r *LargeSubflowRule) ID() string   { return "large-subflow" }
func (r *LargeSubflowRule) Name() string { return "Large subflow" }
func (r *LargeSubflowRule) Description() string {
	return "Subflows with too many blocks are hard to maintain, test, and debug."
}
func (r *LargeSubflowRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *LargeSubflowRule) Category() string                 { return "Style" }

const defaultLargeSubflowThreshold = 50

func (r *LargeSubflowRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.ParentID != "" {
		return nil
	}
	if block.Type == models.BlockTypeComment {
		return nil
	}

	threshold := defaultLargeSubflowThreshold
	if ctx.Settings != nil {
		if rc, ok := ctx.Settings.Analysis.Rules[r.ID()]; ok {
			if mb, ok := rc.Options["maxBlocks"]; ok {
				if f, ok := mb.(float64); ok && f > 0 {
					threshold = int(f)
				}
			}
		}
	}

	sf := ctx.SubflowByID[block.SubflowID]
	if sf == nil {
		return nil
	}
	// Fire once per subflow on the first non-comment top-level block, so the
	// finding's line/suppression target points at an actionable block rather
	// than a leading comment (whose BlockID would point at the wrong line).
	if firstActionableBlockID(sf.Blocks) != block.ID {
		return nil
	}
	count := countAllBlocks(sf.Blocks)
	if count > threshold {
		return []models.Finding{{
			RuleID:      r.ID(),
			Severity:    r.DefaultSeverity(),
			Title:       "Large subflow",
			Description: "Subflow '" + sf.Name + "' has " + strconv.Itoa(count) + " blocks (threshold: " + strconv.Itoa(threshold) + ").",
			BlockID:     block.ID,
			SubflowID:   block.SubflowID,
			Suggestion:  "Break the subflow into smaller, focused subflows that each handle a single responsibility.",
		}}
	}

	return nil
}

func countAllBlocks(blocks []models.Block) int {
	count := 0
	for i := range blocks {
		if blocks[i].Type == models.BlockTypeEnd || blocks[i].Type == models.BlockTypeComment {
			continue
		}
		count++
		count += countAllBlocks(blocks[i].Children)
	}
	return count
}

// firstActionableBlockID returns the ID of the first top-level block that is
// neither a comment nor an END marker, or "" if there is none. Per-subflow
// rules (large-subflow, subflow-no-error-handler) attach their finding to this
// block so the finding's line number / inline-suppression target points at an
// actionable block rather than a leading comment (whose BlockID would point at
// the wrong line).
func firstActionableBlockID(blocks []models.Block) string {
	for i := range blocks {
		t := blocks[i].Type
		if t != models.BlockTypeComment && t != models.BlockTypeEnd {
			return blocks[i].ID
		}
	}
	return ""
}

// init self-registers this rule with the analyzer's rule catalog
// (see registry.go) — no separate registration step required.
func init() { registerRule(&LargeSubflowRule{}) }
