package service

import (
	"testing"
	"time"

	"pad-core/models"
	"pad-core/parser"
)

// makeTestAnalysisService parses text into a FlowDocument and returns it alongside
// a freshly-constructed AnalysisService. Used by analysis_test.go and flow_extra_test.go.
func makeTestAnalysisService(t *testing.T, text string) (*models.FlowDocument, *AnalysisService) {
	t.Helper()
	doc, err := parser.ParseText(text, "test.txt", int64(len(text)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	svc := &AnalysisService{}
	return doc, svc
}

func TestAnalysisService_GetVariableLineage_no_doc(t *testing.T) {
	svc := &AnalysisService{}
	_, err := svc.GetVariableLineage(nil, "MyVar")
	if err == nil {
		t.Fatal("expected error when no doc loaded")
	}
}

func TestAnalysisService_GetVariableLineage_found(t *testing.T) {
	doc, svc := makeTestAnalysisService(t, simpleFlow)
	history, err := svc.GetVariableLineage(doc, "MyVar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if history == nil {
		t.Fatal("expected non-nil history")
	}
	if history.Name != "MyVar" {
		t.Errorf("history.Name = %q, want %q", history.Name, "MyVar")
	}
	if len(history.Events) == 0 {
		t.Error("expected at least one event for MyVar")
	}
}

func TestAnalysisService_GetVariableLineage_unknown_var(t *testing.T) {
	doc, svc := makeTestAnalysisService(t, simpleFlow)
	history, err := svc.GetVariableLineage(doc, "NoSuchVar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if history == nil {
		t.Fatal("expected non-nil history even for unknown var")
	}
	if len(history.Events) != 0 {
		t.Errorf("expected 0 events for unknown var, got %d", len(history.Events))
	}
}

func TestAnalysisService_GetExecutionGraph_no_doc(t *testing.T) {
	svc := &AnalysisService{}
	_, err := svc.GetExecutionGraph(nil)
	if err == nil {
		t.Fatal("expected error when no doc loaded")
	}
}

func TestAnalysisService_GetExecutionGraph_single_subflow(t *testing.T) {
	doc, svc := makeTestAnalysisService(t, simpleFlow)
	graph, err := svc.GetExecutionGraph(doc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if graph == nil {
		t.Fatal("expected non-nil graph")
	}
	if len(graph.Nodes) == 0 {
		t.Error("expected at least one node")
	}
}

func TestAnalysisService_GetExecutionGraph_two_subflows(t *testing.T) {
	doc, svc := makeTestAnalysisService(t, twoSubflowFlow)
	graph, err := svc.GetExecutionGraph(doc)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(graph.Nodes) < 2 {
		t.Errorf("expected 2 nodes, got %d", len(graph.Nodes))
	}
	if len(graph.Edges) == 0 {
		t.Error("expected at least one edge (Main → Helper)")
	}
}

func TestAnalysisService_GetRules_returns_all(t *testing.T) {
	svc := &AnalysisService{}
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

func TestSortedReports_OrdersByFlowIDAndHandlesNil(t *testing.T) {
	ts := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	reports := []*models.AnalysisReport{
		{FlowID: "f2", GeneratedAt: ts, FlowName: "gamma"},
		{FlowID: "f1", GeneratedAt: ts, FlowName: "alpha"},
	}
	sorted := sortedReports(reports)
	if len(sorted) != 2 || sorted[0].FlowID != "f1" || sorted[1].FlowID != "f2" {
		t.Errorf("expected [f1 f2] ordering, got %+v", sorted)
	}
	if got := sortedReports(nil); len(got) != 0 {
		t.Fatalf("expected empty result for nil input, got %d", len(got))
	}
}
