package analyzer

import (
	"context"
	"sort"
	"strings"

	"golang.org/x/sync/singleflight"

	"pad-core/cache"
	"pad-core/models"
)

// suppressInvCache memoizes inventories by the same content identity the
// analysis cache uses — the inventory is a pure function of that identity,
// so entries never go stale; the cache saves re-running the ENTIRE rule set
// on every governance view. Bounded like the sibling caches; the shared
// slice is handed to callers read-only.
var suppressInvCache, _ = cache.NewLRUCache(64) // size > 0 ⇒ error impossible
var suppressInvSF singleflight.Group

// SuppressionInventoryCached is SuppressionInventory behind the content-keyed
// cache above, with singleflight collapsing concurrent cold builds (same
// shape as CachedAnalysisCtx).
func SuppressionInventoryCached(flow *models.FlowDocument, rules []Rule, settings *models.AppSettings) []SuppressionEntry {
	if flow == nil || !hasPadIgnoreDirective(flow) {
		return nil
	}
	key := "inv:" + analyzerVersion + ":" + FlowHash(flow) + ":" + settingsDigest(settings) + ":" + ruleSetDigest(rules)
	if v, ok := suppressInvCache.Get(context.Background(), key); ok {
		return v.([]SuppressionEntry)
	}
	v, _, _ := suppressInvSF.Do(key, func() (any, error) {
		entries := SuppressionInventory(flow, rules, settings)
		suppressInvCache.Set(context.Background(), key, entries, 0)
		return entries, nil
	})
	if v == nil {
		return SuppressionInventory(flow, rules, settings)
	}
	return v.([]SuppressionEntry)
}

// SuppressionEntry is one inline `# pad-ignore` directive, located and
// audited: Stale marks a directive whose rule no longer fires on its target
// block (finding fixed, block changed, rule renamed) — it now masks NOTHING
// but stays in the file silently suppressing any FUTURE finding there.
type SuppressionEntry struct {
	Line       int    `json:"line"`
	Rule       string `json:"rule"` // "*" = all rules
	BlockLabel string `json:"blockLabel"`
	BlockType  string `json:"blockType,omitempty"`
	Subflow    string `json:"subflow,omitempty"`
	Stale      bool   `json:"stale"`
	Reason     string `json:"reason,omitempty"`
}

// SuppressionInventory locates every pad-ignore directive and audits
// staleness against the UNSUPPRESSED findings — the governance concern being
// that suppressions are otherwise write-only. A directive in a subflow
// header has two targets (the next concrete block AND the block-less
// findings the header scope covers) and is live if either fires.
func SuppressionInventory(flow *models.FlowDocument, rules []Rule, settings *models.AppSettings) []SuppressionEntry {
	// No directive, no entries: the full single-threaded rule pass below
	// costs more than a cold parallel analysis and returns nothing on a
	// directive-free flow (the common case), so the cheap pre-check skips it.
	if flow == nil || !hasPadIgnoreDirective(flow) {
		return nil
	}
	// Unsuppressed findings: dispatch every rule over the SAME context the
	// engine builds (runAnalysisCore applies inline suppression at the end;
	// we want the pre-suppression truth). One buildContext + one walk.
	firing := map[string]map[string]bool{}    // blockID -> ruleID set
	blockless := map[string]map[string]bool{} // subflowID -> ruleID set, findings with no block
	record := func(m map[string]map[string]bool, key, rule string) {
		if m[key] == nil {
			m[key] = map[string]bool{}
		}
		m[key][rule] = true
	}
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
						if f.BlockID == "" {
							record(blockless, f.SubflowID, f.RuleID)
							continue
						}
						record(firing, b.ID, f.RuleID)
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
	// Mirror the engine's document-level parse-error fallback (engine.go): when
	// no block produced a parse-error finding, the errors are reported with an
	// empty BlockID against the first subflow.
	if len(flow.ParseErrors) > 0 && parseErrorRuleEnabled(settings) {
		fired := false
		for _, set := range firing {
			if set["parse-error"] {
				fired = true
				break
			}
		}
		if !fired {
			sfID := ""
			if len(flow.Subflows) > 0 {
				sfID = flow.Subflows[0].ID
			}
			record(blockless, sfID, "parse-error")
		}
	}

	// setFires reports whether rule ("*" = any) fires within set.
	setFires := func(set map[string]bool, rule string) bool {
		if rule == "*" {
			return len(set) > 0
		}
		return set[rule]
	}
	// headerScope describes the header of one subflow for the walk below.
	type headerScope struct {
		sfID       string
		file       bool     // this header is also the file scope
		commentIDs []string // the leading comment blocks themselves
	}
	headerFires := func(h *headerScope, rule string) bool {
		if h == nil {
			return false
		}
		if setFires(blockless[h.sfID], rule) {
			return true
		}
		if h.file {
			for _, set := range blockless {
				if setFires(set, rule) {
					return true
				}
			}
		}
		for _, id := range h.commentIDs {
			if setFires(firing[id], rule) {
				return true
			}
		}
		return false
	}

	var entries []SuppressionEntry
	var walk func(blocks []models.Block, subflowName string, header *headerScope)
	walk = func(blocks []models.Block, subflowName string, header *headerScope) {
		inHeader := header != nil
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
							if rule == "*" {
								// Wildcards: stale only when NO rule fires on
								// the block at all.
								e.Stale = len(firing[next.ID]) == 0
								if e.Stale {
									e.Reason = "no rule currently fires on this block"
								}
							} else {
								e.Stale = !firing[next.ID][rule]
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
						// A header directive is live when its subflow/file
						// scope currently silences something, whatever the
						// next block does.
						if inHeader && e.Stale && headerFires(header, rule) {
							e.Stale = false
							e.Reason = ""
							if next == nil {
								e.BlockLabel = "<subflow header>"
							}
						}
						entries = append(entries, e)
					}
				}
				continue
			}
			inHeader = false
			if len(b.Children) > 0 {
				walk(b.Children, subflowName, nil)
			}
		}
	}
	fileScoped := false
	for i := range flow.Subflows {
		sf := &flow.Subflows[i]
		h := &headerScope{sfID: sf.ID, commentIDs: leadingCommentIDs(sf.Blocks)}
		if !fileScoped && len(sf.Blocks) > 0 {
			fileScoped = true
			h.file = true
		}
		walk(sf.Blocks, sf.Name, h)
	}
	sort.SliceStable(entries, func(a, b int) bool {
		if entries[a].Line != entries[b].Line {
			return entries[a].Line < entries[b].Line
		}
		return entries[a].Rule < entries[b].Rule
	})
	return entries
}

// hasPadIgnoreDirective reports whether the inventory walk would visit at
// least one `pad-ignore` comment: it mirrors that walk's reach exactly
// (every sibling group, descending into non-comment children only), so nil
// when false is the same result as running the full inventory.
func hasPadIgnoreDirective(flow *models.FlowDocument) bool {
	var scan func(blocks []models.Block) bool
	scan = func(blocks []models.Block) bool {
		for i := range blocks {
			b := &blocks[i]
			if b.Type == models.BlockTypeComment {
				if _, _, ok := parsePadIgnore(b.Name); ok {
					return true
				}
				continue
			}
			if len(b.Children) > 0 && scan(b.Children) {
				return true
			}
		}
		return false
	}
	for i := range flow.Subflows {
		if scan(flow.Subflows[i].Blocks) {
			return true
		}
	}
	return false
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
