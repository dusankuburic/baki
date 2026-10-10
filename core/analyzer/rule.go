package analyzer

import "pad-core/models"

// Rule is the catalog identity every rule kind shares. The engine dispatches a
// rule by its kind (BlockRule or FlowRule); a Rule that is neither is skipped
// and counted in Stats.RulesSkipped (see docs/architecture/analyzer.md).
type Rule interface {
	ID() string
	Name() string
	Description() string
	DefaultSeverity() models.Severity
	Category() string
}

// BlockRule is checked once per non-END block of the document, in parallel
// shards, with a read-only context.
type BlockRule interface {
	Rule
	Check(block *models.Block, ctx *RuleContext) []models.Finding
}

// FlowRule is checked once per document, on the walk's calling goroutine while
// the block shards run. Its findings anchor on blocks of ctx.Flow (or are
// block-less); the engine stable-sorts them into document order, so they sit in
// the rule's slot exactly as a block rule's findings would.
type FlowRule interface {
	Rule
	CheckFlow(ctx *RuleContext) []models.Finding
}

type ruleKind uint8

const (
	kindUnknown ruleKind = iota
	kindBlock
	kindFlow
)

// kindOf classifies r; FlowRule wins when a type implements both.
func kindOf(r Rule) ruleKind {
	switch r.(type) {
	case FlowRule:
		return kindFlow
	case BlockRule:
		return kindBlock
	}
	return kindUnknown
}

// Rule kinds as the catalog names them (models.Rule.Kind).
const (
	RuleKindBlock   = "block"
	RuleKindFlow    = "flow"
	RuleKindLibrary = "library"
)

// RuleKind names r's kind for the catalog: a LibraryRule is "library", else
// kindOf decides; "" for a rule of no known kind.
func RuleKind(r Rule) string {
	if _, ok := r.(LibraryRule); ok {
		return RuleKindLibrary
	}
	switch kindOf(r) {
	case kindFlow:
		return RuleKindFlow
	case kindBlock:
		return RuleKindBlock
	}
	return ""
}

// subflowAnchor is the block a per-subflow finding anchors on: sf's first
// block, when the block walk would have dispatched it as sf's own (not END,
// its SubflowID resolving to sf; on duplicate subflow IDs the last subflow
// wins, as in SubflowByID). The converted flow rules fired on exactly this
// block when they were block rules, so their fingerprints are unchanged.
func subflowAnchor(ctx *RuleContext, sf *models.Subflow) *models.Block {
	if len(sf.Blocks) == 0 {
		return nil
	}
	return ownAnchor(ctx, sf, &sf.Blocks[0])
}

// actionableAnchor is subflowAnchor for the first top-level block that is
// neither a comment nor END: per-subflow rules (large-subflow,
// subflow-no-error-handler) attach there so the finding's line and inline
// suppression target are an actionable block, not a leading comment.
func actionableAnchor(ctx *RuleContext, sf *models.Subflow) *models.Block {
	for i := range sf.Blocks {
		if t := sf.Blocks[i].Type; t != models.BlockTypeComment && t != models.BlockTypeEnd {
			return ownAnchor(ctx, sf, &sf.Blocks[i])
		}
	}
	return nil
}

func ownAnchor(ctx *RuleContext, sf *models.Subflow, b *models.Block) *models.Block {
	if b.Type == models.BlockTypeEnd || b.SubflowID != sf.ID || ctx.SubflowByID[sf.ID] != sf {
		return nil
	}
	return b
}
