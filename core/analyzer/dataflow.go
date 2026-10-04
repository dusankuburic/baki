package analyzer

import (
	"context"
	"math"
	"sort"

	"pad-core/models"
)

type DeadDataRule struct{}

func (r *DeadDataRule) ID() string   { return "dead-data" }
func (r *DeadDataRule) Name() string { return "Dead data path" }
func (r *DeadDataRule) Description() string {
	return "Variables set but only read by blocks that are themselves dead code (after terminators)."
}
func (r *DeadDataRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *DeadDataRule) Category() string                 { return "Logic" }

func (r *DeadDataRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeVariable {
		return nil
	}

	outVar := outputVar(block)
	if outVar == "" {
		return nil
	}

	siblingKey := ctx.SiblingKey[block.ID]
	terminatorIdx, hasTerminator := ctx.TerminatorIndex[siblingKey]
	if !hasTerminator {
		return nil
	}

	myIdx, ok := ctx.BlockIndex[block.ID]
	if !ok || myIdx > terminatorIdx {
		return nil
	}

	// Classified once per variable in buildContext (see buildAllReadersDead);
	// absent means no readers, or at least one live one.
	if !ctx.AllReadersDead[outVar] {
		return nil
	}

	return []models.Finding{{
		RuleID:      r.ID(),
		Severity:    r.DefaultSeverity(),
		Title:       "Dead data path",
		Description: "Variable '" + outVar + "' is set here but all blocks that read it are themselves unreachable (after exit actions).",
		BlockID:     block.ID,
		SubflowID:   block.SubflowID,
		Suggestion:  "Remove this variable assignment or move it before the exit action.",
		Metadata:    map[string]any{"variable": outVar},
	}}
}

// findReaders returns the IDs of blocks that read varName, excluding blocks
// that also write it (a self assignment is not a read); O(readers) via ctx.ReadersByVar.
func findReaders(varName string, ctx *RuleContext) []string {
	candidates := ctx.ReadersByVar[varName]
	if len(candidates) == 0 {
		return nil
	}
	readers := make([]string, 0, len(candidates))
	for _, id := range candidates {
		b := ctx.AllBlocks[id]
		if b == nil {
			continue
		}
		if b.Properties != nil {
			if b.Properties["_output"] == varName || b.Properties["_var"] == varName {
				continue
			}
		}
		readers = append(readers, id)
	}
	return readers
}

// buildAllReadersDead holds varName iff it has at least one reader (excluding
// self-writes, as findReaders does) and none is live — live means a BlockIndex
// at or before its group's terminator, or no terminator; no BlockIndex counts
// as dead, exactly as the rule's per-writer walk did. O(total reads).
func buildAllReadersDead(ctx *RuleContext) map[string]bool {
	dead := make(map[string]bool)
	for varName, candidates := range ctx.ReadersByVar {
		n := 0
		allDead := true
		for _, id := range candidates {
			b := ctx.AllBlocks[id]
			if b == nil {
				continue
			}
			if b.Properties != nil {
				if b.Properties["_output"] == varName || b.Properties["_var"] == varName {
					continue
				}
			}
			n++
			readerIdx, ok := ctx.BlockIndex[id]
			if !ok {
				continue
			}
			readerTermIdx, hasTerm := ctx.TerminatorIndex[ctx.SiblingKey[id]]
			if hasTerm && readerIdx > readerTermIdx {
				continue
			}
			allDead = false
			break
		}
		if n > 0 && allDead {
			dead[varName] = true
		}
	}
	return dead
}

// DataFlowLimits bounds the data-flow response; PAD variables are flow-global,
// so materialized edges are O(readers × writers) per variable (a 10.6 MB flow
// hit 26M edges, 4.6 GB). A limit <= 0 means unlimited.
type DataFlowLimits struct {
	// MaxEdgesPerVariable caps the upstream and downstream blocks listed for
	// one variable, so one hot variable cannot take the whole edge budget.
	MaxEdgesPerVariable int `json:"maxEdgesPerVariable"`
	// MaxEdges caps the upstream entries, and separately the downstream
	// entries, across all blocks.
	MaxEdges int `json:"maxEdges"`
	// MaxTaintPaths and MaxDeadData cap those lists.
	MaxTaintPaths int `json:"maxTaintPaths"`
	MaxDeadData   int `json:"maxDeadData"`
}

// DefaultDataFlowLimits leaves typical flows exact and holds a pathological one
// to a few MB of JSON (one variable with 5,000 writers and 5,000 readers:
// 5.6 MB, was 25M edges each way).
var DefaultDataFlowLimits = DataFlowLimits{
	MaxEdgesPerVariable: 200,
	MaxEdges:            100_000,
	MaxTaintPaths:       10_000,
	MaxDeadData:         10_000,
}

// DataFlowResult is models.DataFlowAnalysis (fields inlined, so the response
// keeps the old shape) plus what the limits cut; Truncated=false means the
// lists are exactly the unlimited analysis.
type DataFlowResult struct {
	*models.DataFlowAnalysis
	Truncated           bool           `json:"truncated"`
	EdgesTruncated      bool           `json:"edgesTruncated,omitempty"`
	TaintPathsTruncated bool           `json:"taintPathsTruncated,omitempty"`
	DeadDataTruncated   bool           `json:"deadDataTruncated,omitempty"`
	Limits              DataFlowLimits `json:"limits"`
}

// AnalyzeDataFlow is AnalyzeDataFlowCtx without cancellation, under
// DefaultDataFlowLimits.
func AnalyzeDataFlow(doc *models.FlowDocument) *models.DataFlowAnalysis {
	res, _ := AnalyzeDataFlowCtx(context.Background(), doc, DefaultDataFlowLimits)
	return res.DataFlowAnalysis
}

const dataFlowCheckEvery = 256

// AnalyzeDataFlowCtx computes each block's reads/writes plus upstream/downstream
// blocks, taint hints and dead data paths, within limits; O(blocks log blocks +
// reads + output), returns ctx.Err() once ctx is done. Blocks are visited in
// sorted-ID order, so which blocks lose edges is deterministic.
func AnalyzeDataFlowCtx(ctx context.Context, doc *models.FlowDocument, limits DataFlowLimits) (*DataFlowResult, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := &models.DataFlowAnalysis{
		Blocks: make(map[string]*models.BlockDataFlow),
	}
	res := &DataFlowResult{DataFlowAnalysis: result, Limits: limits}

	// Structural indexes only: data flow reads none of the rule-only work
	// buildContext adds.
	rctx := buildIndexContext(doc, nil)

	ids := make([]string, 0, len(rctx.AllBlocks))
	for id := range rctx.AllBlocks {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	for _, id := range ids {
		block := rctx.AllBlocks[id]
		df := &models.BlockDataFlow{
			BlockID:   id,
			SubflowID: block.SubflowID,
			Reads:     block.Variables,
		}

		if block.Properties != nil {
			if out, ok := block.Properties["_output"]; ok && out != "" {
				df.Writes = append(df.Writes, out)
			}
			if v, ok := block.Properties["_var"]; ok && v != "" {
				df.Writes = append(df.Writes, v)
			}
		}

		result.Blocks[id] = df
	}

	perVar := limitOrMax(limits.MaxEdgesPerVariable)

	// Upstream: writers of each read variable in WritersByVar order, minus
	// the block itself.
	budget := limitOrMax(limits.MaxEdges)
	for i, id := range ids {
		if i%dataFlowCheckEvery == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		df := result.Blocks[id]
		for _, readVar := range df.Reads {
			ws := rctx.WritersByVar[readVar]
			taken, j := 0, 0
			for ; j < len(ws) && taken < perVar && budget > 0; j++ {
				if ws[j] == id {
					continue
				}
				df.UpstreamBlocks = append(df.UpstreamBlocks, ws[j])
				taken++
				budget--
			}
			// Only the block's own entries may be left unlisted.
			for ; j < len(ws); j++ {
				if ws[j] != id {
					res.EdgesTruncated = true
					break
				}
			}
		}
	}

	// Downstream: readers of each written variable after the writer's line, in
	// ReadersByVar order; a per-variable index sorted by line finds them by
	// binary search.
	budget = limitOrMax(limits.MaxEdges)
	byLine := make(map[string][]readerAt)
	var scratch []readerAt
	for i, id := range ids {
		if i%dataFlowCheckEvery == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		df := result.Blocks[id]
		if len(df.Writes) == 0 {
			continue
		}
		writerLine := rctx.AllBlocks[id].LineNumber
		for _, writeVar := range df.Writes {
			idx, ok := byLine[writeVar]
			if !ok {
				idx = readersByLine(rctx, writeVar)
				byLine[writeVar] = idx
			}
			// The writer's own entries carry its line, so "after" also drops
			// them, as the old id != writer check did.
			k := sort.Search(len(idx), func(n int) bool { return idx[n].line > writerLine })
			after := idx[k:]
			take := min(len(after), perVar, budget)
			if take < len(after) {
				res.EdgesTruncated = true
			}
			if take == 0 {
				continue
			}
			scratch = append(scratch[:0], after[:take]...)
			sort.Slice(scratch, func(a, b int) bool { return scratch[a].pos < scratch[b].pos })
			for _, r := range scratch {
				df.DownstreamBlocks = append(df.DownstreamBlocks, r.id)
			}
			budget -= take
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result.TaintPaths, res.TaintPathsTruncated = computeTaintPaths(doc, rctx, ids, limitOrMax(limits.MaxTaintPaths))
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result.DeadData, res.DeadDataTruncated = computeDeadDataPaths(rctx, ids, limitOrMax(limits.MaxDeadData))
	res.Truncated = res.EdgesTruncated || res.TaintPathsTruncated || res.DeadDataTruncated

	return res, nil
}

// limitOrMax maps a DataFlowLimits value to a count (<= 0 is unlimited).
func limitOrMax(n int) int {
	if n <= 0 {
		return math.MaxInt
	}
	return n
}

// readerAt is one ReadersByVar entry: reader ID, line, and position in
// ReadersByVar (the order the response lists readers in).
type readerAt struct {
	id   string
	line int
	pos  int
}

// readersByLine returns varName's ReadersByVar entries sorted by line, ties in
// ReadersByVar order; unknown blocks are dropped, as findReadersAfter drops them.
func readersByLine(ctx *RuleContext, varName string) []readerAt {
	ids := ctx.ReadersByVar[varName]
	idx := make([]readerAt, 0, len(ids))
	for pos, id := range ids {
		if b := ctx.AllBlocks[id]; b != nil {
			idx = append(idx, readerAt{id: id, line: b.LineNumber, pos: pos})
		}
	}
	sort.SliceStable(idx, func(a, b int) bool { return idx[a].line < idx[b].line })
	return idx
}

// findWriters returns the IDs of blocks that write varName (via _output or
// _var), excluding excludeID. O(writers) via ctx.WritersByVar.
func findWriters(varName string, ctx *RuleContext, excludeID string) []string {
	candidates := ctx.WritersByVar[varName]
	if len(candidates) == 0 {
		return nil
	}
	writers := make([]string, 0, len(candidates))
	for _, id := range candidates {
		if id == excludeID {
			continue
		}
		writers = append(writers, id)
	}
	return writers
}

// findReadersAfter returns the IDs of blocks that read varName and appear after
// writerID by line number. O(readers) via ctx.ReadersByVar.
func findReadersAfter(varName string, ctx *RuleContext, writerID string) []string {
	writerBlock := ctx.AllBlocks[writerID]
	if writerBlock == nil {
		return nil
	}
	writerLine := writerBlock.LineNumber

	var readers []string
	for _, id := range ctx.ReadersByVar[varName] {
		if id == writerID {
			continue
		}
		b := ctx.AllBlocks[id]
		if b == nil || b.LineNumber <= writerLine {
			continue
		}
		readers = append(readers, id)
	}
	return readers
}

// computeTaintPaths is a NAME-BASED heuristic, not a data-flow analysis (the
// UI labels it as such): a "source" is a dialog/prompt-produced variable or an
// Input_/UserInput_-prefixed name; a "path" is that variable read DIRECTLY by a
// sink — zero hops (one intermediate SET breaks the trail). The name stays for
// compatibility. ids is the visiting order; at most limit paths are returned.
func computeTaintPaths(doc *models.FlowDocument, ctx *RuleContext, ids []string, limit int) (paths []models.TaintPath, truncated bool) {
	var sources []string
	for _, id := range ids {
		b := ctx.AllBlocks[id]
		if b.Properties != nil {
			if rt, ok := b.Properties["_inputType"]; ok && (rt == "dialog" || rt == "prompt") {
				if out, ok := b.Properties["_output"]; ok && out != "" {
					sources = append(sources, out)
				}
			}
		}
	}

	inputPrefixes := []string{"Input_", "input_", "UserInput_", "userinput_"}
	for _, sf := range doc.Subflows {
		for _, v := range sf.Variables {
			for _, prefix := range inputPrefixes {
				if len(v.Name) > len(prefix) && v.Name[:len(prefix)] == prefix {
					sources = append(sources, v.Name)
				}
			}
		}
	}
	if len(sources) == 0 {
		return nil, false
	}

	// Sinks indexed by the variables they read (each sink once per variable)
	// so a source finds its direct sinks without scanning; per variable they
	// stay in visiting order.
	type sinkEntry struct {
		block *models.Block
		styp  string
	}
	sinksByVar := make(map[string][]sinkEntry)
	for _, id := range ids {
		b := ctx.AllBlocks[id]
		st := findSink(b.RawType)
		if st == "" {
			continue
		}
		for _, v := range b.Variables {
			list := sinksByVar[v]
			if len(list) > 0 && list[len(list)-1].block == b {
				continue // b reads v more than once: still one path
			}
			sinksByVar[v] = append(list, sinkEntry{block: b, styp: st})
		}
	}

	for _, source := range sources {
		for _, s := range sinksByVar[source] {
			if len(paths) >= limit {
				return paths, true
			}
			paths = append(paths, models.TaintPath{
				SourceVar: source,
				SinkBlock: s.block.ID,
				SinkType:  s.styp,
				Path:      []string{source, s.block.ID},
			})
		}
	}
	return paths, false
}

// computeDeadDataPaths pairs each live writer of a used variable (before its
// scope's terminator) with every dead reader (after its own terminator); dead
// readers depend only on the variable, so they are found once per variable.
// ids is the visiting order; at most limit paths.
func computeDeadDataPaths(ctx *RuleContext, ids []string, limit int) (dead []models.DeadDataPath, truncated bool) {
	deadReaders := make(map[string][]string)
	for _, id := range ids {
		b := ctx.AllBlocks[id]
		outVar := outputVar(b)
		if outVar == "" {
			continue
		}

		if !ctx.UsedVariables[outVar] {
			continue
		}

		siblingKey := ctx.SiblingKey[b.ID]
		termIdx, hasTerm := ctx.TerminatorIndex[siblingKey]
		if !hasTerm {
			continue
		}
		blockIdx, ok := ctx.BlockIndex[b.ID]
		if !ok || blockIdx > termIdx {
			continue
		}

		readers, ok := deadReaders[outVar]
		if !ok {
			for _, readerID := range findReaders(outVar, ctx) {
				readerIdx, ok := ctx.BlockIndex[readerID]
				if !ok {
					continue
				}
				readerTermIdx, hasTerm := ctx.TerminatorIndex[ctx.SiblingKey[readerID]]
				if hasTerm && readerIdx > readerTermIdx {
					readers = append(readers, readerID)
				}
			}
			deadReaders[outVar] = readers
		}
		for _, readerID := range readers {
			if len(dead) >= limit {
				return dead, true
			}
			dead = append(dead, models.DeadDataPath{
				Variable:  outVar,
				SetBlock:  b.ID,
				ReadBlock: readerID,
				Reason:    "reader is in dead code (after terminator)",
			})
		}
	}
	return dead, false
}

func init() { registerRule(&DeadDataRule{}) }
