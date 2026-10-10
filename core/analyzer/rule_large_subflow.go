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

var largeSubflowMaxBlocks = OptionSpec{Key: "maxBlocks", Default: defaultLargeSubflowThreshold, Min: 1}

func (r *LargeSubflowRule) CheckFlow(ctx *RuleContext) []models.Finding {
	threshold := ruleOption(ctx.Settings, r.ID(), largeSubflowMaxBlocks)
	var out []models.Finding
	for i := range ctx.Flow.Subflows {
		sf := &ctx.Flow.Subflows[i]
		// Fire once per subflow on the first non-comment top-level block (see
		// actionableAnchor).
		anchor := actionableAnchor(ctx, sf)
		if anchor == nil || anchor.ParentID != "" {
			continue
		}
		if count := countAllBlocks(sf.Blocks); count > threshold {
			out = append(out, models.Finding{
				RuleID:      r.ID(),
				Severity:    r.DefaultSeverity(),
				Title:       "Large subflow",
				Description: "Subflow '" + sf.Name + "' has " + strconv.Itoa(count) + " blocks (threshold: " + strconv.Itoa(threshold) + ").",
				BlockID:     anchor.ID,
				SubflowID:   sf.ID,
				Suggestion:  "Break the subflow into smaller, focused subflows that each handle a single responsibility.",
			})
		}
	}
	return out
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

func init() {
	registerRule(&LargeSubflowRule{}, RuleMeta{
		Confidence:   models.ConfidenceLow,
		Options:      []OptionSpec{largeSubflowMaxBlocks},
		IntroducedIn: 1,
		Revision:     1,
		Stage:        StageStable,
	})
}
