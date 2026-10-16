package analyzer

import (
	"context"
	"regexp"
	"strings"
	"sync"

	"pad-core/models"
)

var variableRefRegex = regexp.MustCompile(`%([^%]+)%`)

// RuleContext is what a rule sees of one analysis: the document, the settings,
// the structural indexes buildContext builds eagerly (the walk and most rules
// read them), and the rule-only facts behind accessors, each group built on
// first use under its own sync.Once, so a run whose rules never ask for a fact
// never pays for it. Every fact is a pure function of the document; once
// visible it is read-only (rules run in parallel shards and must not write).
type RuleContext struct {
	Flow     *models.FlowDocument
	Settings *models.AppSettings

	AllBlocks       map[string]*models.Block
	ParentMap       map[string]string
	SiblingMap      map[string][]*models.Block
	SiblingKey      map[string]string
	BlockIndex      map[string]int
	TerminatorIndex map[string]int
	BlockDepth      map[string]int
	// WritersByVar/ReadersByVar map variable to block IDs writing (_output/_var) or reading (Variables) it.
	WritersByVar map[string][]string
	ReadersByVar map[string][]string
	// FirstReaderByVar maps variable to its earliest-reading block ID (lowest LineNumber).
	FirstReaderByVar map[string]string
	// SubflowByID / SubflowByName resolve subflows; on duplicate IDs the last
	// subflow wins, on duplicate names the first.
	SubflowByID   map[string]*models.Subflow
	SubflowByName map[string]*models.Subflow
	// DescendantCount maps block ID to non-End/non-Comment blocks in its subtree, post-order.
	DescendantCount map[string]int
	// GroupHasHandler marks sibling-group keys containing an error-handler block.
	GroupHasHandler map[string]bool
	totalBlocks     int

	gctx context.Context

	usedVariables   lazy[map[string]bool]
	signatures      lazy[map[string]string]
	hasErrorHandler lazy[map[string]bool]
	insideRetryLoop lazy[map[string]bool]
	allReadersDead  lazy[map[string]bool]
	labels          lazy[*labelIndex]
	closedResources lazy[map[string]map[string]bool]
	taintedVars     lazy[map[string]bool]
	graph           lazy[*subflowGraph]
	docPos          lazy[map[string]int]
}

// lazy is one fact group: the first reader builds it, every reader after sees
// the same value (sync.Once orders the build before every return).
type lazy[T any] struct {
	once  sync.Once
	built bool // tests read it after a walk
	v     T
}

func (l *lazy[T]) get(ctx *RuleContext, build func(*RuleContext) T) T {
	l.once.Do(func() {
		l.v = build(ctx)
		l.built = true
	})
	return l.v
}

// Err reports the walk's cancellation, for flow rules that loop over a large
// document; nil outside a walk.
func (ctx *RuleContext) Err() error {
	if ctx.gctx == nil {
		return nil
	}
	return ctx.gctx.Err()
}

// UsedVariables is the set of variable names referenced as %name% in any
// non-END block's property values or Variables.
func (ctx *RuleContext) UsedVariables() map[string]bool {
	return ctx.usedVariables.get(ctx, buildUsedVariables)
}

// HasErrorHandler marks blocks whose ancestor chain contains an error handler
// or a sibling group holding one (see HasErrorHandlerAncestor).
func (ctx *RuleContext) HasErrorHandler() map[string]bool {
	return ctx.hasErrorHandler.get(ctx, buildHasErrorHandler)
}

// InsideRetryLoop marks blocks enclosed by a retry/attempt LOOP (missing-retry).
func (ctx *RuleContext) InsideRetryLoop() map[string]bool {
	return ctx.insideRetryLoop.get(ctx, buildInsideRetryLoop)
}

// AllReadersDead marks variables whose readers are all unreachable (dead-data);
// nil when no sibling group has a terminator.
func (ctx *RuleContext) AllReadersDead() map[string]bool {
	return ctx.allReadersDead.get(ctx, func(c *RuleContext) map[string]bool {
		if len(c.TerminatorIndex) == 0 {
			return nil
		}
		return buildAllReadersDead(c)
	})
}

// LabelByName maps a lowercased label name to its document-order-first LABEL block.
func (ctx *RuleContext) LabelByName() map[string]*models.Block {
	return ctx.labels.get(ctx, buildLabelIndex).byName
}

// LabelNameCount counts LABEL blocks per lowercased name (duplicate-label).
func (ctx *RuleContext) LabelNameCount() map[string]int {
	return ctx.labels.get(ctx, buildLabelIndex).count
}

// ClosedResourceVars maps a close-action prefix to the variables referenced by
// any block performing that close (resource-leak).
func (ctx *RuleContext) ClosedResourceVars() map[string]map[string]bool {
	return ctx.closedResources.get(ctx, buildClosedResourceVars)
}

// TaintedVars marks variables tracing to an untrusted source; a fixed point over WritersByVar.
func (ctx *RuleContext) TaintedVars() map[string]bool {
	return ctx.taintedVars.get(ctx, buildTaintedVars)
}

// CircularSubflows marks subflow IDs on a call-graph cycle.
func (ctx *RuleContext) CircularSubflows() map[string]bool {
	return ctx.subflowGraph().circularSet
}

// SubflowCyclo maps a subflow ID to its cyclomatic complexity.
func (ctx *RuleContext) SubflowCyclo() map[string]int {
	return ctx.subflowGraph().cyclo
}

// SubflowFanIn maps a subflow ID to its distinct-caller count.
func (ctx *RuleContext) SubflowFanIn() map[string]int {
	return ctx.subflowGraph().fanIn
}

// DuplicateSubflowNames is the set of subflow names appearing more than once.
func (ctx *RuleContext) DuplicateSubflowNames() map[string]bool {
	return ctx.subflowGraph().duplicateNames
}

// subflowGraph is the call graph and everything derived from it; the report's
// metrics phase reads it on every analysis.
func (ctx *RuleContext) subflowGraph() *subflowGraph {
	return ctx.graph.get(ctx, buildSubflowGraph)
}

// blockSig is duplicate-action's content signature of b, hashed once per
// block for the whole walk.
func (ctx *RuleContext) blockSig(b *models.Block) string {
	if s, ok := ctx.signatures.get(ctx, buildSignatures)[b.ID]; ok {
		return s
	}
	return blockSignature(b)
}

// docPosition maps a block ID to its pre-order position in the document (END
// blocks included; the first block wins on a duplicate ID).
func (ctx *RuleContext) docPosition() map[string]int {
	return ctx.docPos.get(ctx, func(c *RuleContext) map[string]int {
		pos := make(map[string]int, c.totalBlocks)
		if c.Flow == nil {
			return pos
		}
		n := 0
		walkBlocks(c.Flow, func(b *models.Block) {
			if _, dup := pos[b.ID]; !dup {
				pos[b.ID] = n
			}
			n++
		})
		return pos
	})
}

// buildContext builds the structural indexes every walk needs; the rule-only
// facts above are built when first read (nil-map reads stay safe).
func buildContext(flow *models.FlowDocument, settings *models.AppSettings) *RuleContext {
	ctx := &RuleContext{
		Flow:            flow,
		AllBlocks:       make(map[string]*models.Block),
		ParentMap:       make(map[string]string),
		SiblingMap:      make(map[string][]*models.Block),
		SiblingKey:      make(map[string]string),
		BlockIndex:      make(map[string]int),
		TerminatorIndex: make(map[string]int),
		BlockDepth:      make(map[string]int),
		WritersByVar:    make(map[string][]string),
		ReadersByVar:    make(map[string][]string),
		DescendantCount: make(map[string]int),
		GroupHasHandler: make(map[string]bool),
		SubflowByID:     make(map[string]*models.Subflow, len(flow.Subflows)),
		SubflowByName:   make(map[string]*models.Subflow, len(flow.Subflows)),
		Settings:        settings,
	}

	for i := range flow.Subflows {
		sf := &flow.Subflows[i]
		collectBlocks(ctx, sf.Blocks, "", sf.ID, 0)
		ctx.SubflowByID[sf.ID] = sf
		if _, ok := ctx.SubflowByName[sf.Name]; !ok {
			ctx.SubflowByName[sf.Name] = sf
		}
	}

	// Earliest reader per variable in one O(total reads) pass; tie-break:
	// lowest LineNumber, then first ID in ReadersByVar order.
	ctx.FirstReaderByVar = make(map[string]string, len(ctx.ReadersByVar))
	for vname, ids := range ctx.ReadersByVar {
		lowestLine := -1
		lowestID := ""
		for _, id := range ids {
			b := ctx.AllBlocks[id]
			if b == nil {
				continue
			}
			if lowestLine < 0 || b.LineNumber < lowestLine {
				lowestLine = b.LineNumber
				lowestID = id
			}
		}
		ctx.FirstReaderByVar[vname] = lowestID
	}

	return ctx
}

func collectBlocks(ctx *RuleContext, blocks []models.Block, parentID string, subflowID string, depth int) {
	ptrs := make([]*models.Block, 0, len(blocks))
	for i := range blocks {
		if blocks[i].Type == models.BlockTypeEnd {
			continue
		}
		ptrs = append(ptrs, &blocks[i])
	}

	// Sibling groups key by parentID (nested) or subflowID (top level): sharing
	// "" would overwrite SiblingMap[""] across subflows and index out of range.
	siblingKey := parentID
	if siblingKey == "" {
		siblingKey = subflowID
	}

	terminatorIdx := -1
	groupHasHandler := false
	for i, b := range ptrs {
		ctx.BlockIndex[b.ID] = i
		ctx.SiblingKey[b.ID] = siblingKey
		if terminatorIdx < 0 && isTerminator(b) {
			terminatorIdx = i
		}
		if b.Type == models.BlockTypeErrorHandler {
			groupHasHandler = true
		}

		ctx.AllBlocks[b.ID] = b
		if parentID != "" {
			ctx.ParentMap[b.ID] = parentID
		}
		ctx.BlockDepth[b.ID] = depth
		ctx.totalBlocks++

		// Index variable writers/readers in traversal order so derived results are deterministic.
		if b.Properties != nil {
			if out := b.Properties["_output"]; out != "" {
				ctx.WritersByVar[out] = append(ctx.WritersByVar[out], b.ID)
			}
			if v := b.Properties["_var"]; v != "" {
				ctx.WritersByVar[v] = append(ctx.WritersByVar[v], b.ID)
			}
		}
		for _, v := range b.Variables {
			ctx.ReadersByVar[v] = append(ctx.ReadersByVar[v], b.ID)
		}
	}
	ctx.SiblingMap[siblingKey] = ptrs
	if terminatorIdx >= 0 {
		ctx.TerminatorIndex[siblingKey] = terminatorIdx
	}
	if groupHasHandler {
		ctx.GroupHasHandler[siblingKey] = true
	}

	for i := range blocks {
		b := &blocks[i]
		if len(b.Children) > 0 {
			collectBlocks(ctx, b.Children, b.ID, subflowID, depth+1)
		}
	}

	// Post-order descendant counts credited to the parent of this child-group.
	subtree := 0
	for i := range blocks {
		b := &blocks[i]
		if b.Type != models.BlockTypeEnd && b.Type != models.BlockTypeComment {
			subtree++
		}
		subtree += ctx.DescendantCount[b.ID]
	}
	if parentID != "" {
		ctx.DescendantCount[parentID] = subtree
	}
}

func isTerminator(b *models.Block) bool {
	if b.Type == models.BlockTypeEnd {
		return true
	}
	return matchesAny(b, terminatorNames)
}

func buildUsedVariables(ctx *RuleContext) map[string]bool {
	used := make(map[string]bool)
	if ctx.Flow == nil {
		return used
	}
	scan := func(val string) {
		if !strings.Contains(val, "%") {
			return // %var% is the only shape variableRefRegex matches
		}
		for _, m := range variableRefRegex.FindAllStringSubmatch(val, -1) {
			if len(m) > 1 {
				used[strings.TrimSpace(m[1])] = true
			}
		}
	}
	walkBlocks(ctx.Flow, func(b *models.Block) {
		if b.Type == models.BlockTypeEnd {
			return
		}
		for _, val := range b.Properties {
			scan(val)
		}
		for _, v := range b.Variables {
			scan(v)
		}
	})
	return used
}

func buildSignatures(ctx *RuleContext) map[string]string {
	sigs := make(map[string]string, len(ctx.AllBlocks))
	for id, b := range ctx.AllBlocks {
		sigs[id] = blockSignature(b)
	}
	return sigs
}

// buildHasErrorHandler memoizes, per block: an ancestor is or has an
// error-handler sibling.
func buildHasErrorHandler(ctx *RuleContext) map[string]bool {
	memo := make(map[string]bool, len(ctx.AllBlocks))
	var has func(blockID string) bool
	has = func(blockID string) bool {
		if v, ok := memo[blockID]; ok {
			return v
		}
		memo[blockID] = false // guard against cycles
		pid, ok := ctx.ParentMap[blockID]
		if !ok {
			return false
		}
		parent := ctx.AllBlocks[pid]
		if parent == nil {
			return false
		}
		// GroupHasHandler[pid] means "any sibling of this block is a handler"
		// (nested sibling groups key by the parent's ID).
		direct := parent.Type == models.BlockTypeErrorHandler || ctx.GroupHasHandler[pid]
		result := direct || has(pid)
		memo[blockID] = result
		return result
	}
	for id := range ctx.AllBlocks {
		has(id)
	}
	return memo
}

// buildInsideRetryLoop memoizes, per block: any ancestor is a LOOP whose name
// or properties mention retry or attempt.
func buildInsideRetryLoop(ctx *RuleContext) map[string]bool {
	memo := make(map[string]bool, len(ctx.AllBlocks))
	retryParent := make(map[string]bool)
	var inside func(blockID string) bool
	inside = func(blockID string) bool {
		if v, ok := memo[blockID]; ok {
			return v
		}
		memo[blockID] = false // guard against cycles
		pid, ok := ctx.ParentMap[blockID]
		if !ok {
			return false
		}
		parent := ctx.AllBlocks[pid]
		if parent == nil {
			return false
		}
		isRetry, ok := retryParent[pid]
		if !ok {
			isRetry = parentIsRetryLoop(parent)
			retryParent[pid] = isRetry
		}
		result := isRetry || inside(pid)
		memo[blockID] = result
		return result
	}
	for id := range ctx.AllBlocks {
		inside(id)
	}
	return memo
}

func parentIsRetryLoop(parent *models.Block) bool {
	if parent.Type != models.BlockTypeLoop {
		return false
	}
	nameLower := strings.ToLower(parent.Name)
	if strings.Contains(nameLower, "retry") || strings.Contains(nameLower, "attempt") {
		return true
	}
	for k, v := range parent.Properties {
		if v == "" {
			continue
		}
		kl := strings.ToLower(k)
		if strings.Contains(kl, "retry") || strings.Contains(kl, "attempt") {
			return true
		}
	}
	return false
}

// subflowGraph is the call graph and the per-subflow facts derived from it.
type subflowGraph struct {
	callGraph      map[string]map[string]bool
	circular       []string // subflow IDs on a cycle, document order (Metrics.CircularDependencies)
	circularSet    map[string]bool
	metrics        []models.SubflowMetrics
	cyclo          map[string]int
	fanIn          map[string]int
	duplicateNames map[string]bool
}

func buildSubflowGraph(ctx *RuleContext) *subflowGraph {
	g := &subflowGraph{
		circularSet:    make(map[string]bool),
		cyclo:          make(map[string]int),
		fanIn:          make(map[string]int),
		duplicateNames: make(map[string]bool),
	}
	flow := ctx.Flow
	if flow == nil {
		return g
	}
	g.callGraph = buildCallGraph(flow)
	g.circular = detectCircularDeps(flow, g.callGraph)
	for _, sfID := range g.circular {
		g.circularSet[sfID] = true
	}

	// Per-subflow complexity and fan-in; mirrors ComputeFlowMetrics' inversion.
	fanInByID := make(map[string]int, len(g.callGraph))
	for src, targets := range g.callGraph {
		for tgt := range targets {
			if tgt != src {
				fanInByID[tgt]++
			}
		}
	}
	g.metrics = make([]models.SubflowMetrics, 0, len(flow.Subflows))
	nameCount := make(map[string]int, len(flow.Subflows))
	for i := range flow.Subflows {
		sf := &flow.Subflows[i]
		m := computeSubflowMetrics(sf, g.callGraph, fanInByID)
		g.metrics = append(g.metrics, m)
		g.cyclo[sf.ID] = m.CyclomaticComplexity
		g.fanIn[sf.ID] = m.FanIn
		nameCount[sf.Name]++
	}
	for name, count := range nameCount {
		if count > 1 {
			g.duplicateNames[name] = true
		}
	}
	return g
}

func GetSiblings(ctx *RuleContext, block *models.Block) []*models.Block {
	key := ctx.SiblingKey[block.ID]
	return ctx.SiblingMap[key]
}

// siblingPosition is block's sibling group and its index in it; ok is false
// when the indexes do not hold block's ID there: a document the parser never
// emits repeats a subflow ID or a parent's block ID, so the later group took
// the key. A rule reading a neighbour then skips the block rather than index
// another group's blocks.
func siblingPosition(ctx *RuleContext, block *models.Block) (siblings []*models.Block, idx int, ok bool) {
	siblings = GetSiblings(ctx, block)
	idx, ok = ctx.BlockIndex[block.ID]
	if !ok || idx >= len(siblings) || siblings[idx].ID != block.ID {
		return nil, 0, false
	}
	return siblings, idx, true
}

func GetParent(ctx *RuleContext, block *models.Block) *models.Block {
	pid, ok := ctx.ParentMap[block.ID]
	if !ok {
		return nil
	}
	return ctx.AllBlocks[pid]
}

func HasErrorHandlerAncestor(ctx *RuleContext, block *models.Block) bool {
	return ctx.HasErrorHandler()[block.ID]
}
