package analyzer

import (
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/models"
	"pad-analyzer/internal/parser"
)

func loadFixture(t *testing.T, name string) *models.FlowDocument {
	t.Helper()
	path := filepath.Join("testdata", name)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat fixture %s: %v", name, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	doc, err := parser.ParseText(string(data), name, info.Size())
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return doc
}

func makeBlock(id, name string, bt models.BlockType, rawType string, indent int) *models.Block {
	return &models.Block{
		ID:       id,
		Name:     name,
		Type:     bt,
		RawType:  rawType,
		Indent:   indent,
		Children: []models.Block{},
	}
}

func TestRunAnalysisEmpty(t *testing.T) {
	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{}}},
	}
	report := RunAnalysis(flow, AllRules(), nil, nil)
	if len(report.Findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(report.Findings))
	}
	if report.Stats.BlocksAnalyzed != 0 {
		t.Errorf("expected 0 blocks analyzed, got %d", report.Stats.BlocksAnalyzed)
	}
}

func TestRunAnalysisProgress(t *testing.T) {
	b := makeBlock("b1", "Click button", models.BlockTypeAction, "WebAutomation.Click", 0)
	b.SubflowID = "sf1"
	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}},
	}
	calls := 0
	RunAnalysis(flow, AllRules()[:1], nil, func(current, total int, ruleName string) {
		calls++
	})
	if calls == 0 {
		t.Error("expected progress callbacks")
	}
}

func TestBuildVariableLineage(t *testing.T) {
	t.Run("variable with set and read", func(t *testing.T) {
		setter := makeBlock("b1", "Set X", models.BlockTypeVariable, "SET", 0)
		setter.Properties = map[string]string{"_output": "Counter"}
		setter.SubflowID = "sf1"

		user := makeBlock("b2", "Use X", models.BlockTypeAction, "Display.ShowMessageBox", 0)
		user.Variables = []string{"Counter"}
		user.SubflowID = "sf1"

		doc := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*setter, *user}}},
		}

		h := BuildVariableLineage(doc, "Counter")
		if h == nil {
			t.Fatal("expected non-nil history")
		}
		if h.Name != "Counter" {
			t.Errorf("Name = %q, want %q", h.Name, "Counter")
		}
		initCount, readCount := 0, 0
		for _, e := range h.Events {
			switch e.Type {
			case "init":
				initCount++
			case "read":
				readCount++
			}
		}
		if initCount != 1 {
			t.Errorf("expected 1 init event, got %d", initCount)
		}
		if readCount != 1 {
			t.Errorf("expected 1 read event, got %d", readCount)
		}
	})

	t.Run("unknown variable returns empty events", func(t *testing.T) {
		doc := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{}}},
		}
		h := BuildVariableLineage(doc, "NoSuchVar")
		if h == nil {
			t.Fatal("expected non-nil history for unknown var")
		}
		if len(h.Events) != 0 {
			t.Errorf("expected 0 events, got %d", len(h.Events))
		}
	})
}

func TestBuildExecutionGraph(t *testing.T) {
	t.Run("single subflow has one node and no edges", func(t *testing.T) {
		doc := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{}}},
		}
		g := BuildExecutionGraph(doc, nil)
		if g == nil {
			t.Fatal("expected non-nil graph")
		}
		if len(g.Nodes) != 1 {
			t.Errorf("expected 1 node, got %d", len(g.Nodes))
		}
		if len(g.Edges) != 0 {
			t.Errorf("expected 0 edges, got %d", len(g.Edges))
		}
	})

	t.Run("two subflows with call have edge", func(t *testing.T) {
		callBlock := makeBlock("b1", "Call Helper", models.BlockTypeAction, "CALL", 0)
		callBlock.SubflowID = "sf1"
		doc := &models.FlowDocument{
			ID: "test",
			Subflows: []models.Subflow{
				{ID: "sf1", Name: "Main", Blocks: []models.Block{*callBlock}},
				{ID: "sf2", Name: "Helper", Blocks: []models.Block{}},
			},
		}
		g := BuildExecutionGraph(doc, nil)
		if len(g.Nodes) != 2 {
			t.Errorf("expected 2 nodes, got %d", len(g.Nodes))
		}
		if len(g.Edges) != 1 {
			t.Errorf("expected 1 edge (Main→Helper), got %d", len(g.Edges))
		}
		if g.Edges[0].Source != "sf1" || g.Edges[0].Target != "sf2" {
			t.Errorf("edge = %v→%v, want sf1→sf2", g.Edges[0].Source, g.Edges[0].Target)
		}
	})

	t.Run("duplicate calls produce single edge", func(t *testing.T) {
		c1 := makeBlock("b1", "Call Helper", models.BlockTypeAction, "CALL", 0)
		c1.SubflowID = "sf1"
		c2 := makeBlock("b2", "Call Helper", models.BlockTypeAction, "CALL", 0)
		c2.SubflowID = "sf1"
		doc := &models.FlowDocument{
			ID: "test",
			Subflows: []models.Subflow{
				{ID: "sf1", Name: "Main", Blocks: []models.Block{*c1, *c2}},
				{ID: "sf2", Name: "Helper", Blocks: []models.Block{}},
			},
		}
		g := BuildExecutionGraph(doc, nil)
		if len(g.Edges) != 1 {
			t.Errorf("expected 1 deduplicated edge, got %d", len(g.Edges))
		}
	})

	t.Run("findings annotate node counts", func(t *testing.T) {
		doc := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{}}},
		}
		report := &models.AnalysisReport{
			Findings: []models.Finding{
				{SubflowID: "sf1", Severity: models.SeverityError},
				{SubflowID: "sf1", Severity: models.SeverityWarning},
			},
		}
		g := BuildExecutionGraph(doc, report)
		if g.Nodes[0].ErrorCount != 1 {
			t.Errorf("ErrorCount = %d, want 1", g.Nodes[0].ErrorCount)
		}
		if g.Nodes[0].WarnCount != 1 {
			t.Errorf("WarnCount = %d, want 1", g.Nodes[0].WarnCount)
		}
	})
}

func TestComputeStats(t *testing.T) {
	findings := []models.Finding{
		{Severity: models.SeverityError},
		{Severity: models.SeverityWarning},
		{Severity: models.SeverityWarning},
		{Severity: models.SeverityInfo},
	}
	stats := computeStats(findings)
	if stats.Errors != 1 || stats.Warnings != 2 || stats.Info != 1 {
		t.Errorf("stats mismatch: %+v", stats)
	}
}
