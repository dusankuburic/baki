package analyzer

import (
	"pad-analyzer/internal/models"
)

type DeadDataRule struct{}

func (r *DeadDataRule) ID() string                    { return "dead-data" }
func (r *DeadDataRule) Name() string                   { return "Dead data path" }
func (r *DeadDataRule) Description() string            { return "Variables set but only read by blocks that are themselves dead code (after terminators)." }
func (r *DeadDataRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (r *DeadDataRule) Category() string               { return "Logic" }

func (r *DeadDataRule) Check(block *models.Block, ctx *RuleContext) []models.Finding {
	if block.Type != models.BlockTypeVariable {
		return nil
	}

	outVar := outputVar(block)
	if outVar == "" {
		return nil
	}

	parentID := ctx.ParentMap[block.ID]
	terminatorIdx, hasTerminator := ctx.TerminatorIndex[parentID]
	if !hasTerminator {
		return nil
	}

	myIdx, ok := ctx.BlockIndex[block.ID]
	if !ok || myIdx > terminatorIdx {
		return nil
	}

	readers := findReaders(outVar, ctx)
	if len(readers) == 0 {
		return nil
	}

	allReadersDead := true
	for _, readerID := range readers {
		readerIdx, ok := ctx.BlockIndex[readerID]
		if !ok {
			continue
		}
		readerParentID := ctx.ParentMap[readerID]

		readerTermIdx, hasTerm := ctx.TerminatorIndex[readerParentID]
		if hasTerm && readerIdx > readerTermIdx {
			continue
		}
		allReadersDead = false
		break
	}

	if !allReadersDead {
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

// findReaders returns the IDs of blocks that read varName (it appears in their
// Variables), excluding blocks that also write the same variable (a self
// assignment is not counted as a read). O(readers) via ctx.ReadersByVar.
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

func AnalyzeDataFlow(doc *models.FlowDocument) *models.DataFlowAnalysis {
	result := &models.DataFlowAnalysis{
		Blocks: make(map[string]*models.BlockDataFlow),
	}

	ctx := buildContext(doc, nil)

	for id, block := range ctx.AllBlocks {
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

	for id, df := range result.Blocks {
		for _, readVar := range df.Reads {
			upstream := findWriters(readVar, ctx, id)
			df.UpstreamBlocks = append(df.UpstreamBlocks, upstream...)
		}
	}

	for id, df := range result.Blocks {
		for _, writeVar := range df.Writes {
			downstream := findReadersAfter(writeVar, ctx, id)
			df.DownstreamBlocks = append(df.DownstreamBlocks, downstream...)
		}
	}

	result.TaintPaths = computeTaintPaths(doc, ctx)
	result.DeadData = computeDeadDataPaths(ctx)

	return result
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

func computeTaintPaths(doc *models.FlowDocument, ctx *RuleContext) []models.TaintPath {
	var sources []string
	for _, b := range ctx.AllBlocks {
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

	var paths []models.TaintPath
	for _, source := range sources {
		for _, b := range ctx.AllBlocks {
			sink := findSink(b.RawType)
			if sink == "" {
				continue
			}
			for _, v := range b.Variables {
				if v == source {
					paths = append(paths, models.TaintPath{
						SourceVar: source,
						SinkBlock: b.ID,
						SinkType:  sink,
						Path:      []string{source, b.ID},
					})
					break
				}
			}
		}
	}
	return paths
}

func computeDeadDataPaths(ctx *RuleContext) []models.DeadDataPath {
	var dead []models.DeadDataPath

	for _, b := range ctx.AllBlocks {
		outVar := outputVar(b)
		if outVar == "" {
			continue
		}

		if !ctx.UsedVariables[outVar] {
			continue
		}

		parentID := ctx.ParentMap[b.ID]
		termIdx, hasTerm := ctx.TerminatorIndex[parentID]
		if !hasTerm {
			continue
		}
		blockIdx, ok := ctx.BlockIndex[b.ID]
		if !ok || blockIdx > termIdx {
			continue
		}

		readers := findReaders(outVar, ctx)
		for _, readerID := range readers {
			readerIdx, ok := ctx.BlockIndex[readerID]
			if !ok {
				continue
			}
			readerParentID := ctx.ParentMap[readerID]
			readerTermIdx, hasTerm := ctx.TerminatorIndex[readerParentID]
			if hasTerm && readerIdx > readerTermIdx {
				dead = append(dead, models.DeadDataPath{
					Variable:  outVar,
					SetBlock:  b.ID,
					ReadBlock: readerID,
					Reason:    "reader is in dead code (after terminator)",
				})
			}
		}
	}
	return dead
}
