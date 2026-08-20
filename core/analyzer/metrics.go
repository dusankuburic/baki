package analyzer

import (
	"math"

	"pad-core/models"
)

func ComputeFlowMetrics(doc *models.FlowDocument, report *models.AnalysisReport) *models.FlowMetrics {
	if doc == nil {
		return &models.FlowMetrics{}
	}

	subflowMetrics := make([]models.SubflowMetrics, 0, len(doc.Subflows))
	callGraph := buildCallGraph(doc)

	// Invert the call graph once to get fan-in for every subflow in O(edges),
	// instead of rescanning the whole graph per subflow (was O(subflows²)).
	fanInByID := make(map[string]int)
	for src, targets := range callGraph {
		for tgt := range targets {
			if tgt != src {
				fanInByID[tgt]++
			}
		}
	}
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		subflowMetrics = append(subflowMetrics, computeSubflowMetrics(sf, callGraph, fanInByID))
	}
	circular := detectCircularDeps(doc, callGraph)
	return assembleFlowMetrics(subflowMetrics, len(doc.Subflows), circular, report)
}

// ComputeFlowMetricsFromCtx assembles the report metrics from the artifacts
// buildContext already computed (call graph, fan-in, cycles, per-subflow
// complexity) — the analysis path calls this so the graph/metrics phase runs
// once per analysis instead of twice.
func ComputeFlowMetricsFromCtx(ctx *RuleContext, report *models.AnalysisReport) *models.FlowMetrics {
	return assembleFlowMetrics(ctx.subflowMetrics, len(ctx.Flow.Subflows), ctx.circularDeps, report)
}

// assembleFlowMetrics reduces per-subflow metrics (already computed) into the
// aggregate FlowMetrics. Shared by the from-scratch and from-ctx paths so the
// two can never drift.
func assembleFlowMetrics(subflowMetrics []models.SubflowMetrics, subflowCount int, circular []string, report *models.AnalysisReport) *models.FlowMetrics {
	var totalBlocks, totalVars int
	var maxCyclo, maxCog int
	var sumCyclo, sumCog float64

	for _, m := range subflowMetrics {
		totalBlocks += m.BlockCount
		totalVars += m.VariableCount
		if m.CyclomaticComplexity > maxCyclo {
			maxCyclo = m.CyclomaticComplexity
		}
		if m.CognitiveComplexity > maxCog {
			maxCog = m.CognitiveComplexity
		}
		sumCyclo += float64(m.CyclomaticComplexity)
		sumCog += float64(m.CognitiveComplexity)
	}

	n := subflowCount
	avgCyclo := 0.0
	avgCog := 0.0
	if n > 0 {
		avgCyclo = sumCyclo / float64(n)
		avgCog = sumCog / float64(n)
	}

	varDensity := 0.0
	if totalBlocks > 0 {
		varDensity = float64(totalVars) / float64(totalBlocks)
	}

	healthScore := computeHealthScore(report, maxCyclo, maxCog, circular)

	return &models.FlowMetrics{
		Subflows:             subflowMetrics,
		TotalBlocks:          totalBlocks,
		TotalVariables:       totalVars,
		MaxCyclomatic:        maxCyclo,
		AvgCyclomatic:        avgCyclo,
		MaxCognitive:         maxCog,
		AvgCognitive:         avgCog,
		HealthScore:          healthScore,
		VariableDensity:      varDensity,
		SubflowCount:         n,
		CircularDependencies: circular,
	}
}

// computeSubflowMetrics walks the subflow tree exactly once, carrying the
// nesting depth, to derive block count, cyclomatic/cognitive complexity and max
// depth. The previous version re-walked the whole tree per decision block (via
// nestingDepthForBlock) plus a separate pass for max depth — O(decisions·blocks).
func computeSubflowMetrics(sf *models.Subflow, callGraph map[string]map[string]bool, fanInByID map[string]int) models.SubflowMetrics {
	blockCount := 0
	cyclo := 1
	cog := 0
	maxDepth := 0
	varCount := len(sf.Variables)

	var walk func(blocks []models.Block, depth int)
	walk = func(blocks []models.Block, depth int) {
		if depth > maxDepth {
			maxDepth = depth
		}
		for i := range blocks {
			b := &blocks[i]
			if b.Type != models.BlockTypeEnd && b.Type != models.BlockTypeComment {
				blockCount++
				switch b.Type {
				case models.BlockTypeCondition:
					cyclo++
					cog += depth + 1
					for ci := range b.Children {
						child := &b.Children[ci]
						if child.Type == models.BlockTypeElse {
							cyclo++
							cog += depth + 1
						}
					}
				case models.BlockTypeLoop:
					cyclo++
					cog += depth + 1
				case models.BlockTypeSwitch:
					for ci := range b.Children {
						child := &b.Children[ci]
						if child.Type == models.BlockTypeCase || child.Type == models.BlockTypeDefault {
							cyclo++
							cog += depth + 1
						}
					}
				}
			}
			if len(b.Children) > 0 {
				walk(b.Children, depth+1)
			}
		}
	}
	walk(sf.Blocks, 0)

	fanOut := 0
	if targets, ok := callGraph[sf.ID]; ok {
		fanOut = len(targets)
	}

	return models.SubflowMetrics{
		SubflowID:            sf.ID,
		SubflowName:          sf.Name,
		BlockCount:           blockCount,
		CyclomaticComplexity: cyclo,
		CognitiveComplexity:  cog,
		MaxNestingDepth:      maxDepth,
		VariableCount:        varCount,
		FanIn:                fanInByID[sf.ID],
		FanOut:               fanOut,
	}
}

func buildCallGraph(doc *models.FlowDocument) map[string]map[string]bool {
	nameIndex := buildSubflowNameIndex(doc)
	graph := make(map[string]map[string]bool)
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		graph[sf.ID] = make(map[string]bool)
		// Uses the same call resolver as the execution graph so fan-in/out and
		// circular-dependency detection stay consistent with the rendered graph.
		walkSubflowBlocks(sf, func(b *models.Block) {
			if !isSubflowCall(b) {
				return
			}
			if targetID := resolveCallTargetID(b, nameIndex); targetID != "" {
				graph[sf.ID][targetID] = true
			}
		})
	}
	return graph
}

func detectCircularDeps(doc *models.FlowDocument, callGraph map[string]map[string]bool) []string {
	visited := make(map[string]int)
	var cycles []string
	var path []string

	var dfs func(node string)
	dfs = func(node string) {
		visited[node] = 1
		path = append(path, node)
		for target := range callGraph[node] {
			if visited[target] == 1 {
				for i, p := range path {
					if p == target {
						cycles = append(cycles, path[i:]...)
						break
					}
				}
			} else if visited[target] == 0 {
				dfs(target)
			}
		}
		path = path[:len(path)-1]
		visited[node] = 2
	}

	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		if visited[sf.ID] == 0 {
			dfs(sf.ID)
		}
	}
	return cycles
}

func computeHealthScore(report *models.AnalysisReport, maxCyclo int, maxCog int, circular []string) int {
	if report == nil {
		return 100
	}

	score := 100.0

	for _, f := range report.Findings {
		switch f.Severity {
		case models.SeverityError:
			score -= 12
		case models.SeverityWarning:
			score -= 4
		case models.SeverityInfo:
			score -= 1
		}
	}

	if maxCyclo > 20 {
		score -= float64(maxCyclo-20) * 2
	}
	if maxCog > 30 {
		score -= float64(maxCog-30) * 1.5
	}
	if len(circular) > 0 {
		score -= 10
	}

	return int(math.Max(0, math.Min(100, score)))
}
