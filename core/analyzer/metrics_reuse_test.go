package analyzer

import (
	"reflect"
	"testing"

	"pad-core/models"
)

// TestComputeFlowMetricsFromCtx_MatchesStandalone is the behavior gate for the
// metrics-reuse refactor: the report path assembles metrics from the
// buildContext artifacts, and it must produce EXACTLY what the standalone
// from-scratch computation produces (deep-equal, including health score and
// cycle list).
func TestComputeFlowMetricsFromCtx_MatchesStandalone(t *testing.T) {
	// A flow with calls between subflows (so the call graph, fan-in, and
	// cycle detection all have content) plus nesting (complexity walks).
	flow := buildManySubflowsFlow(50)
	report := &models.AnalysisReport{Findings: []models.Finding{
		{RuleID: "x", Severity: models.SeverityError},
		{RuleID: "y", Severity: models.SeverityWarning},
	}}

	ctx := buildContext(flow, nil)
	fromCtx := ComputeFlowMetricsFromCtx(ctx, report)
	standalone := ComputeFlowMetrics(flow, report)

	if !reflect.DeepEqual(fromCtx, standalone) {
		t.Fatalf("from-ctx metrics diverged from standalone:\nctx:       %+v\nstandalone: %+v", fromCtx, standalone)
	}
}
