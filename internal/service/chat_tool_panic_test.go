package service

import (
	"context"
	"strings"
	"testing"

	promtest "github.com/prometheus/client_golang/prometheus/testutil"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// R-tool-loop-7: guardedToolRun turned a tool panic into the error-shaped result only on the parallel read-only
// path; a one-call turn, a turn with a fix tool and the prompt loop ran the tool bare, so a panic reached the stream
// worker's recover, the stream ended as interrupted and the model never read the failure.

// panickingAnalysis is the tools' analysis slice; its lineage walk panics, as a bug reached by model input would.
type panickingAnalysis struct{}

func (panickingAnalysis) AnalyzeFlow(context.Context, *models.FlowDocument) (*models.AnalysisReport, error) {
	return &models.AnalysisReport{}, nil
}

func (panickingAnalysis) GetVariableLineage(*models.FlowDocument, string) (*models.VariableHistory, error) {
	panic("lineage boom")
}

func lineageCall() ai.ToolCall {
	return ai.ToolCall{ID: "c1", Name: "get_variable_lineage", Input: []byte(`{"name":"X"}`)}
}

func TestRunToolLoop_SingleCallPanicIsAToolResult(t *testing.T) {
	prov := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{lineageCall()}},
		{Text: "The lineage tool failed; here is what I know."},
	}}
	before := promtest.ToFloat64(chatToolResultsTotal.WithLabelValues("get_variable_lineage", "false"))
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, events := captureEvents()
	svc.runToolLoop(context.Background(), prov, ai.Request{Messages: []ai.Message{{Role: "user", Content: "trace X"}}}, "user-1",
		&ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc(), Analysis: panickingAnalysis{}}, ctl,
		func() bool { return true }, emit, func() int { return 0 })

	if !ctl.done {
		t.Fatalf("the loop did not finish: err %q, events %+v", ctl.errMsg, events())
	}
	reqs := prov.Requests()
	if len(reqs) != 2 || !strings.HasPrefix(lastToolResult(reqs[1]), "error: tool get_variable_lineage failed unexpectedly") {
		t.Errorf("the model did not read the failure: %+v", reqs)
	}
	if after := promtest.ToFloat64(chatToolResultsTotal.WithLabelValues("get_variable_lineage", "false")); after != before+1 {
		t.Errorf("failed tool results went %v -> %v, want one more", before, after)
	}
}

func TestRunPromptToolLoop_PanickingToolIsAToolResult(t *testing.T) {
	prov := &testutil.FakeProvider{Turns: []testutil.FakeTurn{
		{Text: "<tool_call>\n{\"name\": \"get_variable_lineage\", \"input\": {\"name\": \"X\"}}\n</tool_call>"},
		{Text: "The lineage tool failed."},
	}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, events := captureEvents()
	svc.runPromptToolLoop(context.Background(), prov, ai.Request{Messages: []ai.Message{{Role: "user", Content: "trace X"}}}, "user-1",
		&ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc(), Analysis: panickingAnalysis{}}, ctl,
		func() bool { return true }, emit, func() int { return 0 })

	if !ctl.done {
		t.Fatalf("the loop did not finish: err %q, events %+v", ctl.errMsg, events())
	}
	reqs := prov.Requests()
	if len(reqs) != 2 || !strings.Contains(reqs[1].Messages[len(reqs[1].Messages)-1].Content, "failed unexpectedly") {
		t.Errorf("the model did not read the failure: %+v", reqs)
	}
}

// Review of step 3: a recovered panic value is model-visible text and must pass the scrubber like every other
// tool result.
func TestGuardedToolRun_ScrubsThePanicValue(t *testing.T) {
	res := guardedToolRun(lineageCall(), func() toolExecResult { panic("bad input password=supersecret123") })
	if !strings.HasPrefix(res.result, "error: tool get_variable_lineage failed unexpectedly") || strings.Contains(res.result, "supersecret123") {
		t.Errorf("result = %q, want the error shape with the secret scrubbed", res.result)
	}
}

// The batch applier recovers a panic like the single one: the card is resolved as error and the model reads an
// error-shaped, scrubbed result.
func TestApplyFixesWithApproval_PanicResolvesTheCard(t *testing.T) {
	doc, props := tenCallDoc()
	svc, _ := newFixTestService(doc, func(context.Context, *models.FlowDocument, string, string, string, string, string) (*models.FlowDocument, error) {
		panic("fixer boom password=supersecret123")
	})
	rec := &fixEventRecorder{}
	ap := svc.newFixApplier(doc, "stream-1", rec.emit, nil, nil)
	approveStreamAfter(svc, "stream-1", 0)

	out, err := ap.ApplyFixesWithApproval(context.Background(), props[:2])
	if err != nil || !strings.HasPrefix(out, "error: tool apply_fixes failed unexpectedly") {
		t.Errorf("ApplyFixesWithApproval = %q, %v; want the error-shaped result and no error", out, err)
	}
	if strings.Contains(out, "supersecret123") {
		t.Errorf("the panic value reached the model unscrubbed: %q", out)
	}
	if evs := rec.snapshot(); len(evs) == 0 || evs[len(evs)-1] != "fix_decision:error" {
		t.Errorf("events = %v, want the card resolved with a fix_decision error", evs)
	}
}
