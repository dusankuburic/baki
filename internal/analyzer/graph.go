package analyzer

import (
	"strings"

	"pad-analyzer/internal/models"
	"pad-analyzer/internal/parser"
)

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

	for _, sf := range doc.Subflows {
		nodes = append(nodes, models.GraphNode{
			ID:         sf.ID,
			Label:      sf.Name,
			Type:       "subflow",
			BlockCount: countSubflowBlocks(&sf),
			ErrorCount: errCounts[sf.ID],
			WarnCount:  warnCounts[sf.ID],
		})

		// Find calls to other subflows
		walkSubflowBlocks(&sf, func(b *models.Block) {
			// Check RawType first as it's the most common indicator for "Call Subflow" actions
			isCall := b.RawType == "CALL" || b.RawType == "DISABLED_CALL" || b.Type == models.BlockTypeSubflow
			
			if isCall {
				targetName := ""
				
				// 1. Try to extract from Name (e.g. "Call MySubflow")
				if strings.HasPrefix(b.Name, "Call ") {
					targetName = strings.TrimPrefix(b.Name, "Call ")
					targetName = strings.TrimSuffix(targetName, " (disabled)")
				}
				
				// 2. Try to find a subflow token in the block
				if targetName == "" {
					for _, t := range b.Tokens {
						if t.Type == "subflow" && t.Target != "" {
							targetName = t.Target
							break
						}
					}
				}
				
				// 3. Fallback to the block name itself if it's a subflow type
				if targetName == "" && b.Type == models.BlockTypeSubflow {
					targetName = b.Name
				}

				if targetName != "" {
					// Find target subflow ID
					for _, targetSf := range doc.Subflows {
						if targetSf.Name == targetName || targetSf.ID == targetName {
							edgeKey := sf.ID + "->" + targetSf.ID
							if !edgeMap[edgeKey] {
								edges = append(edges, models.GraphEdge{
									Source: sf.ID,
									Target: targetSf.ID,
								})
								edgeMap[edgeKey] = true
							}
							break
						}
					}
				}
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
