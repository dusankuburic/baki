package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/parser"
)

func makeTestAnalysisService(t *testing.T, text string) (*FlowService, *AnalysisService) {
	t.Helper()
	doc, err := parser.ParseText(text, "test.txt", int64(len(text)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	flow := &FlowService{ctx: context.Background()}
	flow.currentDoc = doc
	analysis := &AnalysisService{ctx: context.Background(), flow: flow}
	return flow, analysis
}

func TestAnalysisService_LastReport_nil(t *testing.T) {
	_, svc := makeTestAnalysisService(t, simpleFlow)
	if svc.LastReport() != nil {
		t.Fatal("expected nil before any analysis run")
	}
}

func TestAnalysisService_GetVariableLineage_no_doc(t *testing.T) {
	flow := &FlowService{ctx: context.Background()}
	svc := &AnalysisService{ctx: context.Background(), flow: flow}
	_, err := svc.GetVariableLineage("MyVar")
	if err == nil {
		t.Fatal("expected error when no doc loaded")
	}
}

func TestAnalysisService_GetVariableLineage_found(t *testing.T) {
	_, svc := makeTestAnalysisService(t, simpleFlow)
	history, err := svc.GetVariableLineage("MyVar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if history == nil {
		t.Fatal("expected non-nil history")
	}
	if history.Name != "MyVar" {
		t.Errorf("history.Name = %q, want %q", history.Name, "MyVar")
	}
	// simpleFlow assigns MyVar via SET, so there should be at least one event
	if len(history.Events) == 0 {
		t.Error("expected at least one event for MyVar")
	}
}

func TestAnalysisService_GetVariableLineage_unknown_var(t *testing.T) {
	_, svc := makeTestAnalysisService(t, simpleFlow)
	history, err := svc.GetVariableLineage("NoSuchVar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Unknown variable → history with empty events, not an error
	if history == nil {
		t.Fatal("expected non-nil history even for unknown var")
	}
	if len(history.Events) != 0 {
		t.Errorf("expected 0 events for unknown var, got %d", len(history.Events))
	}
}

func TestAnalysisService_GetExecutionGraph_no_doc(t *testing.T) {
	flow := &FlowService{ctx: context.Background()}
	svc := &AnalysisService{ctx: context.Background(), flow: flow}
	_, err := svc.GetExecutionGraph()
	if err == nil {
		t.Fatal("expected error when no doc loaded")
	}
}

func TestAnalysisService_GetExecutionGraph_single_subflow(t *testing.T) {
	_, svc := makeTestAnalysisService(t, simpleFlow)
	graph, err := svc.GetExecutionGraph()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if graph == nil {
		t.Fatal("expected non-nil graph")
	}
	// simpleFlow has one implicit subflow → one node, no edges
	if len(graph.Nodes) == 0 {
		t.Error("expected at least one node")
	}
}

func TestAnalysisService_GetExecutionGraph_two_subflows(t *testing.T) {
	_, svc := makeTestAnalysisService(t, twoSubflowFlow)
	graph, err := svc.GetExecutionGraph()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Main calls Helper → should have an edge
	if len(graph.Nodes) < 2 {
		t.Errorf("expected 2 nodes, got %d", len(graph.Nodes))
	}
	if len(graph.Edges) == 0 {
		t.Error("expected at least one edge (Main → Helper)")
	}
}

func TestAnalysisService_GetRules_returns_all(t *testing.T) {
	_, svc := makeTestAnalysisService(t, simpleFlow)
	rules := svc.GetRules()
	if len(rules) == 0 {
		t.Fatal("expected non-empty rule list")
	}
	for _, r := range rules {
		if r.ID == "" {
			t.Error("rule with empty ID")
		}
		if r.Name == "" {
			t.Errorf("rule %q has empty Name", r.ID)
		}
	}
}
