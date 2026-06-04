package analyzer

import (
	"strings"

	"pad-analyzer/internal/models"
	"pad-analyzer/internal/parser"
)

// buildSubflowNameIndex maps each subflow's Name and ID to its ID, so a call
// target can be resolved in O(1) instead of scanning all subflows per call.
func buildSubflowNameIndex(doc *models.FlowDocument) map[string]string {
	idx := make(map[string]string, len(doc.Subflows)*2)
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		if sf.Name != "" {
			idx[sf.Name] = sf.ID
		}
		idx[sf.ID] = sf.ID
	}
	return idx
}

// isSubflowCall reports whether b invokes another subflow.
func isSubflowCall(b *models.Block) bool {
	return b.RawType == "CALL" || b.RawType == "DISABLED_CALL" || b.Type == models.BlockTypeSubflow
}

// resolveCallTargetID returns the target subflow ID for a call block using the
// name index, or "" when the target can't be resolved. Centralizes the call
// target heuristics so the execution graph and the metrics call graph agree.
func resolveCallTargetID(b *models.Block, nameIndex map[string]string) string {
	targetName := ""
	// 1. "Call X" / "Call X (disabled)" naming convention.
	if strings.HasPrefix(b.Name, "Call ") {
		targetName = strings.TrimSuffix(strings.TrimPrefix(b.Name, "Call "), " (disabled)")
	}
	// 2. An explicit subflow token.
	if targetName == "" {
		for _, t := range b.Tokens {
			if t.Type == "subflow" && t.Target != "" {
				targetName = t.Target
				break
			}
		}
	}
	// 3. Fallback to the block name for SUBFLOW-typed blocks.
	if targetName == "" && b.Type == models.BlockTypeSubflow {
		targetName = b.Name
	}
	if targetName == "" {
		return ""
	}
	return nameIndex[targetName]
}

func BuildExecutionGraph(doc *models.FlowDocument, report *models.AnalysisReport) *models.GraphData {
	nodes := make([]models.GraphNode, 0, len(doc.Subflows))
	edges := make([]models.GraphEdge, 0)
	edgeMap := make(map[string]bool)

	// Subflow error/warn counts
	errCounts := make(map[string]int)
	warnCounts := make(map[string]int)

	if report != nil {
		for _, f := range report.Findings {
			if f.Severity == models.SeverityError {
				errCounts[f.SubflowID]++
			} else if f.Severity == models.SeverityWarning {
				warnCounts[f.SubflowID]++
			}
		}
	}

	nameIndex := buildSubflowNameIndex(doc)

	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		nodes = append(nodes, models.GraphNode{
			ID:         sf.ID,
			Label:      sf.Name,
			Type:       "subflow",
			BlockCount: countSubflowBlocks(sf),
			ErrorCount: errCounts[sf.ID],
			WarnCount:  warnCounts[sf.ID],
		})

		// Find calls to other subflows using the shared resolver.
		walkSubflowBlocks(sf, func(b *models.Block) {
			if !isSubflowCall(b) {
				return
			}
			targetID := resolveCallTargetID(b, nameIndex)
			if targetID == "" {
				return
			}
			edgeKey := sf.ID + "->" + targetID
			if !edgeMap[edgeKey] {
				edges = append(edges, models.GraphEdge{Source: sf.ID, Target: targetID})
				edgeMap[edgeKey] = true
			}
		})
	}

	return &models.GraphData{
		Nodes: nodes,
		Edges: edges,
	}
}

func countSubflowBlocks(sf *models.Subflow) int {
	count := 0
	for i := range sf.Blocks {
		count++
		count += parser.CountDescendants(&sf.Blocks[i])
	}
	return count
}
