package ai

import (
	"context"
	"testing"
	"time"

	"pad-core/models"
)

// R-approvals-3: ExecuteTool wrapped an approval tool in one 120 s deadline that covered both the human's decision
// and the apply + re-analysis, so a slow approval left the write too little time. An approval tool now bounds its
// own phases (the decision wait, then the apply), and ExecuteTool gives its handler the stream's context unchanged.

// deadlineApplier records whether its context carries a deadline.
type deadlineApplier struct{ hadDeadline *bool }

func (d deadlineApplier) ApplyFixWithApproval(ctx context.Context, _ FixProposal) (string, error) {
	_, *d.hadDeadline = ctx.Deadline()
	return "APPLIED", nil
}

func (d deadlineApplier) ApplyFixesWithApproval(ctx context.Context, _ []FixProposal) (string, error) {
	_, *d.hadDeadline = ctx.Deadline()
	return "APPLIED", nil
}

func TestExecuteTool_ApprovalToolsRunWithoutTheToolDeadline(t *testing.T) {
	for name, input := range map[string]string{
		"apply_fix":   `{"blockId":"b1","ruleId":"unhandled-error"}`,
		"apply_fixes": `{"targets":[{"blockId":"b1","ruleId":"unhandled-error"}]}`,
	} {
		had := true
		tctx := ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc(), Fixes: deadlineApplier{&had}, Analysis: fixFakeAnalysis{fixReport("b1")}}
		out := ExecuteTool(name, []byte(input), tctx)
		if out != "APPLIED" {
			t.Fatalf("%s did not reach the applier: %q", name, out)
		}
		if had {
			t.Errorf("%s ran under a tool deadline (result %q): the applier bounds its own phases", name, out)
		}
	}
	// A read-only tool keeps its deadline: list_findings' analysis runs under the default tool timeout.
	var left time.Duration
	tctx := ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc(), Analysis: deadlineAnalysis{&left}}
	ExecuteTool("list_findings", []byte(`{}`), tctx)
	if left <= 0 || left > defaultToolTimeout {
		t.Errorf("list_findings ran with %s left on its context, want the %s tool deadline", left, defaultToolTimeout)
	}
}

// deadlineAnalysis records how long its context had left.
type deadlineAnalysis struct{ left *time.Duration }

func (d deadlineAnalysis) AnalyzeFlow(ctx context.Context, _ *models.FlowDocument) (*models.AnalysisReport, error) {
	if dl, ok := ctx.Deadline(); ok {
		*d.left = time.Until(dl)
	}
	return &models.AnalysisReport{}, nil
}

func (deadlineAnalysis) GetVariableLineage(*models.FlowDocument, string) (*models.VariableHistory, error) {
	return nil, nil
}
