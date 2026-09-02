package analyzer

import (
	"sort"
	"strings"

	"pad-core/models"
)

// SuppressionEntry is one inline `# pad-ignore` directive in the flow source,
// located and audited: Stale marks a directive whose rule no longer fires on
// its target block (the finding it silenced was fixed, the block changed, or
// the rule was renamed) — the directive now masks NOTHING but stays in the
// file silently suppressing any FUTURE finding of that rule on that block.
type SuppressionEntry struct {
	Line       int    `json:"line"`
	Rule       string `json:"rule"` // "*" = all rules
	BlockLabel string `json:"blockLabel"`
	BlockType  string `json:"blockType,omitempty"`
	Subflow    string `json:"subflow,omitempty"`
	Stale      bool   `json:"stale"`
	Reason     string `json:"reason,omitempty"`
}

// SuppressionInventory locates every pad-ignore directive in the parsed flow
// and audits staleness against the UNSUPPRESSED findings (rules as they would
// fire without any suppression). The governance concern: suppressions are
// write-only today — nothing lists them or flags the ones masking nothing.
func SuppressionInventory(flow *models.FlowDocument, rules []Rule, settings *models.AppSettings) []SuppressionEntry {
	if flow == nil {
		return nil
	}
	// Unsuppressed findings: dispatch every rule over the SAME context the
	// engine builds (runAnalysisCore applies inline suppression at the end;
	// we want the pre-suppression truth). One buildContext + one walk.
	firing := map[string]map[string]bool{} // blockID -> ruleID set
	ctx := buildContext(flow, settings)
	var check func(blocks []models.Block)
	check = func(blocks []models.Block) {
		for i := range blocks {
			b := &blocks[i]
			if b.Type != models.BlockTypeEnd {
				for _, rule := range rules {
					if settings != nil {
						if rc, ok := settings.Analysis.Rules[rule.ID()]; ok && !rc.Enabled {
							continue
						}
					}
					for _, f := range rule.Check(b, ctx) {
						if firing[b.ID] == nil {
							firing[b.ID] = map[string]bool{}
						}
						firing[b.ID][f.RuleID] = true
					}
				}
			}
			if len(b.Children) > 0 {
				check(b.Children)
			}
		}
	}
	for i := range flow.Subflows {
		check(flow.Subflows[i].Blocks)
	}

	var entries []SuppressionEntry
	var walk func(blocks []models.Block, subflowName string)
	walk = func(blocks []models.Block, subflowName string) {
		for i := range blocks {
			b := &blocks[i]
			if b.Type == models.BlockTypeComment {
				if all, ruleIDs, ok := parsePadIgnore(b.Name); ok {
					targets := []string{"*"}
					if !all {
						targets = ruleIDs
					}
					next := nextConcreteSibling(blocks, i)
					for _, rule := range targets {
						e := SuppressionEntry{
							Line:       b.LineNumber,
							Rule:       rule,
							Subflow:    subflowName,
							BlockLabel: "<no following block>",
						}
						if next != nil {
							e.BlockLabel = strings.TrimSpace(next.Name)
							if e.BlockLabel == "" {
								e.BlockLabel = next.RawType
							}
							e.BlockType = string(next.Type)
							fires := firing[next.ID] != nil && firing[next.ID][rule]
							if rule == "*" {
								// Wildcards: stale only when NO rule fires on
								// the block at all.
								e.Stale = len(firing[next.ID]) == 0
								if e.Stale {
									e.Reason = "no rule currently fires on this block"
								}
							} else {
								e.Stale = !fires
								if e.Stale {
									if len(firing[next.ID]) == 0 {
										e.Reason = "no rule currently fires on this block"
									} else {
										e.Reason = "rule does not currently fire here"
									}
								}
							}
						} else {
							e.Stale = true
							e.Reason = "directive has no following block"
						}
						entries = append(entries, e)
					}
				}
				continue
			}
			if len(b.Children) > 0 {
				walk(b.Children, subflowName)
			}
		}
	}
	for i := range flow.Subflows {
		walk(flow.Subflows[i].Blocks, flow.Subflows[i].Name)
	}
	sort.SliceStable(entries, func(a, b int) bool {
		if entries[a].Line != entries[b].Line {
			return entries[a].Line < entries[b].Line
		}
		return entries[a].Rule < entries[b].Rule
	})
	return entries
}

// nextConcreteSibling returns the first non-comment block after index i (the
// directive's target per the suppression semantics).
func nextConcreteSibling(blocks []models.Block, i int) *models.Block {
	for j := i + 1; j < len(blocks); j++ {
		if blocks[j].Type != models.BlockTypeComment {
			return &blocks[j]
		}
	}
	return nil
}
