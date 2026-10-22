package analyzer

import (
	"testing"

	"pad-core/models"
)

func TestBuildExecutionGraph_CallCountsAndDisabled(t *testing.T) {
	doc := &models.FlowDocument{
		ID: "flow-1",
		Subflows: []models.Subflow{
			{
				ID:   "sf-main",
				Name: "Main",
				Blocks: []models.Block{
					{
						ID:      "b1",
						Name:    "Call Worker",
						RawType: "CALL",
						Type:    models.BlockTypeSubflow,
					},
					{
						ID:      "b2",
						Name:    "Call Worker (disabled)",
						RawType: "DISABLED_CALL",
						Type:    models.BlockTypeSubflow,
					},
					{
						ID:      "b3",
						Name:    "Call Worker",
						RawType: "CALL",
						Type:    models.BlockTypeSubflow,
					},
					{
						ID:      "b4",
						Name:    "Call Helper",
						RawType: "DISABLED_CALL",
						Type:    models.BlockTypeSubflow,
					},
				},
			},
			{
				ID:     "sf-worker",
				Name:   "Worker",
				Blocks: []models.Block{},
			},
			{
				ID:     "sf-helper",
				Name:   "Helper",
				Blocks: []models.Block{},
			},
		},
	}

	report := &models.AnalysisReport{
		Findings: []models.Finding{
			{
				ID:        "f1",
				SubflowID: "sf-worker",
				Severity:  models.SeverityError,
			},
			{
				ID:        "f2",
				SubflowID: "sf-worker",
				Severity:  models.SeverityWarning,
			},
			{
				ID:        "f3",
				SubflowID: "sf-main",
				Severity:  models.SeverityWarning,
			},
		},
	}

	data := BuildExecutionGraph(doc, report)
	if len(data.Nodes) != 3 {
		t.Fatalf("expected 3 nodes, got %d", len(data.Nodes))
	}
	if len(data.Edges) != 2 {
		t.Fatalf("expected 2 edges, got %d", len(data.Edges))
	}

	// Verify node error/warn counts
	nodeMap := make(map[string]models.GraphNode)
	for _, n := range data.Nodes {
		nodeMap[n.ID] = n
	}
	if nodeMap["sf-worker"].ErrorCount != 1 || nodeMap["sf-worker"].WarnCount != 1 {
		t.Errorf("expected sf-worker to have 1 error and 1 warn, got err=%d warn=%d",
			nodeMap["sf-worker"].ErrorCount, nodeMap["sf-worker"].WarnCount)
	}
	if nodeMap["sf-main"].WarnCount != 1 {
		t.Errorf("expected sf-main to have 1 warn, got warn=%d", nodeMap["sf-main"].WarnCount)
	}

	// Verify edges
	edgeMap := make(map[string]models.GraphEdge)
	for _, e := range data.Edges {
		edgeMap[e.Source+"->"+e.Target] = e
	}

	workerEdge, ok := edgeMap["sf-main->sf-worker"]
	if !ok {
		t.Fatal("expected edge sf-main->sf-worker")
	}
	if workerEdge.CallCount != 3 {
		t.Errorf("expected CallCount 3, got %d", workerEdge.CallCount)
	}
	if workerEdge.DisabledCount != 1 {
		t.Errorf("expected DisabledCount 1, got %d", workerEdge.DisabledCount)
	}

	helperEdge, ok := edgeMap["sf-main->sf-helper"]
	if !ok {
		t.Fatal("expected edge sf-main->sf-helper")
	}
	if helperEdge.CallCount != 1 {
		t.Errorf("expected CallCount 1, got %d", helperEdge.CallCount)
	}
	if helperEdge.DisabledCount != 1 {
		t.Errorf("expected DisabledCount 1, got %d", helperEdge.DisabledCount)
	}
}
