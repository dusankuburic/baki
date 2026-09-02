package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pad-core/ai/scrubber"
	"pad-core/models"
)

// fixFakeAnalysis implements ToolAnalysis with a canned report.
type fixFakeAnalysis struct {
	report *models.AnalysisReport
}

func (f fixFakeAnalysis) AnalyzeFlow(_ context.Context, _ *models.FlowDocument) (*models.AnalysisReport, error) {
	return f.report, nil
}
func (f fixFakeAnalysis) GetVariableLineage(_ *models.FlowDocument, _ string) (*models.VariableHistory, error) {
	return nil, nil
}

// fixToolDoc builds a doc with one HTTP action that fires fixable rules.
func fixToolDoc(t *testing.T) (*models.FlowDocument, string) {
	t.Helper()
	doc := toolFixtureDoc()
	block := doc.BlocksByID["b1"]
	if block == nil {
		t.Fatal("fixture block b1 missing")
	}
	return doc, "b1"
}

// fixReport returns a report with one auto-fixable finding on the block.
func fixReport(blockID string) *models.AnalysisReport {
	return &models.AnalysisReport{Findings: []models.Finding{{
		RuleID:      "unhandled-error",
		BlockID:     blockID,
		Title:       "Action has no error handler",
		AutoFix:     "wrap-error-handler",
		Fingerprint: "fp-unhandled-b1",
	}}}
}

func TestProposeFix_RendersPatchPreview(t *testing.T) {
	doc, blockID := fixToolDoc(t)
	tctx := ToolContext{Ctx: context.Background(), Doc: doc, Analysis: fixFakeAnalysis{fixReport(blockID)}}

	out := ExecuteTool("propose_fix", json.RawMessage(`{"blockId":"`+blockID+`","ruleId":"unhandled-error"}`), tctx)
	for _, want := range []string{
		`Fix "wrap-error-handler" for rule "unhandled-error"`,
		"wrap lines 3-3",
		"+ ON BLOCK ERROR",
		"+ END",
		"Preview only",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("preview missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "error:") {
		t.Errorf("unexpected error in preview: %s", out)
	}
}

func TestProposeFix_NoFindingOrNoFix(t *testing.T) {
	doc, blockID := fixToolDoc(t)
	tctx := ToolContext{Ctx: context.Background(), Doc: doc, Analysis: fixFakeAnalysis{fixReport(blockID)}}

	if got := ExecuteTool("propose_fix", json.RawMessage(`{"blockId":"`+blockID+`","ruleId":"missing-rule"}`), tctx); !strings.Contains(got, "no finding for rule") {
		t.Errorf("unknown rule: want 'no finding for rule', got %q", got)
	}
	if got := ExecuteTool("propose_fix", json.RawMessage(`{"blockId":"nope","ruleId":"unhandled-error"}`), tctx); !strings.Contains(got, "no finding for rule") {
		t.Errorf("unknown block: want 'no finding for rule', got %q", got)
	}
	if got := ExecuteTool("propose_fix", json.RawMessage(`{"blockId":"`+blockID+`"}`), tctx); !strings.Contains(got, "blockId and ruleId are required") {
		t.Errorf("missing ruleId: want required-error, got %q", got)
	}

	noFix := &models.AnalysisReport{Findings: []models.Finding{{RuleID: "style", BlockID: blockID, Title: "x"}}}
	tctx.Analysis = fixFakeAnalysis{noFix}
	if got := ExecuteTool("propose_fix", json.RawMessage(`{"blockId":"`+blockID+`","ruleId":"style"}`), tctx); !strings.Contains(got, "no auto-fix") {
		t.Errorf("no-autofix finding: want 'no auto-fix', got %q", got)
	}
}

func TestApplyFix_NoApplierWired(t *testing.T) {
	doc, blockID := fixToolDoc(t)
	tctx := ToolContext{Ctx: context.Background(), Doc: doc, Analysis: fixFakeAnalysis{fixReport(blockID)}, Fixes: nil}

	got := ExecuteTool("apply_fix", json.RawMessage(`{"blockId":"`+blockID+`","ruleId":"unhandled-error"}`), tctx)
	if !strings.Contains(got, "not available in this session") {
		t.Errorf("want 'not available' explanation, got %q", got)
	}
}

// stubFixApplier records the proposal and returns a canned outcome.
type stubFixApplier struct {
	got []FixProposal
	ret string
	err error
}

func (s *stubFixApplier) ApplyFixesWithApproval(_ context.Context, props []FixProposal) (string, error) {
	s.got = append(s.got, props...)
	return s.ret, s.err
}

func (s *stubFixApplier) ApplyFixWithApproval(_ context.Context, prop FixProposal) (string, error) {
	s.got = append(s.got, prop)
	return s.ret, s.err
}

func TestApplyFix_DelegatesToApplier(t *testing.T) {
	doc, blockID := fixToolDoc(t)
	ap := &stubFixApplier{ret: "APPLIED and verified: the finding no longer appears"}
	tctx := ToolContext{Ctx: context.Background(), Doc: doc, Analysis: fixFakeAnalysis{fixReport(blockID)}, Fixes: ap}

	got := ExecuteTool("apply_fix", json.RawMessage(`{"blockId":"`+blockID+`","ruleId":"unhandled-error"}`), tctx)
	if got != ap.ret {
		t.Errorf("want applier outcome passthrough %q, got %q", ap.ret, got)
	}
	if len(ap.got) != 1 {
		t.Fatalf("want 1 proposal, got %d", len(ap.got))
	}
	p := ap.got[0]
	if p.FixType != "wrap-error-handler" || p.BlockID != blockID || p.RuleID != "unhandled-error" {
		t.Errorf("proposal fields wrong: %+v", p)
	}
	if p.Fingerprint != "fp-unhandled-b1" {
		t.Errorf("proposal must carry the finding fingerprint, got %q", p.Fingerprint)
	}
	if p.ProposalID == "" || !strings.Contains(p.Summary, "wrap lines") {
		t.Errorf("proposal must carry an ID and the rendered preview: %+v", p)
	}
}

func TestListFindings_IncludesRuleAndFixType(t *testing.T) {
	doc, blockID := fixToolDoc(t)
	report := fixReport(blockID)
	report.Findings[0].Category = "Reliability"
	report.Findings = append(report.Findings, models.Finding{RuleID: "style-nit", BlockID: blockID, Title: "naming", Category: "Style"})
	tctx := ToolContext{Ctx: context.Background(), Doc: doc, Analysis: fixFakeAnalysis{report}}

	out := ExecuteTool("list_findings", nil, tctx)
	if !strings.Contains(out, "unhandled-error: Action has no error handler") {
		t.Errorf("line must carry the ruleId, got:\n%s", out)
	}
	if !strings.Contains(out, "fix=wrap-error-handler") {
		t.Errorf("fixable finding must carry fix=<type>, got:\n%s", out)
	}
	if strings.Contains(out, "style-nit: naming") && strings.Contains(strings.Split(out, "style-nit")[1], "fix=") {
		t.Errorf("non-fixable finding must not carry fix=, got:\n%s", out)
	}
}

// realDocOnlyAnalysis returns findings ONLY when asked to analyze the real
// document — modeling value-dependent rules (hardcoded credentials vanish on
// the redacted copy because [REDACTED] matches no credential pattern).
type realDocOnlyAnalysis struct {
	realDoc *models.FlowDocument
	report  *models.AnalysisReport
}

func (a realDocOnlyAnalysis) AnalyzeFlow(_ context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
	if doc == a.realDoc {
		return a.report, nil
	}
	return &models.AnalysisReport{}, nil
}
func (a realDocOnlyAnalysis) GetVariableLineage(_ *models.FlowDocument, _ string) (*models.VariableHistory, error) {
	return nil, nil
}

// TestFixTools_AnalyzeRealDoc pins A2: analyses and fix resolution run against
// RealDoc, not the scrubbed copy. Value-dependent findings exist only on the
// real document — before A2, list_findings missed them and apply_fix failed
// with "no finding" for exactly the security findings the UI lists.
func TestFixTools_AnalyzeRealDoc(t *testing.T) {
	realDoc, blockID := fixToolDoc(t)
	scrubbed, err := scrubber.ScrubDocument(realDoc)
	if err != nil {
		t.Fatalf("scrub: %v", err)
	}
	analysis := realDocOnlyAnalysis{realDoc: realDoc, report: fixReport(blockID)}

	// RealDoc wired: the finding is listed and resolvable.
	tctx := ToolContext{Ctx: context.Background(), Doc: scrubbed, RealDoc: realDoc, Analysis: analysis}
	if got := ExecuteTool("list_findings", json.RawMessage(`{}`), tctx); !strings.Contains(got, "unhandled-error") {
		t.Errorf("real-doc finding missing from list_findings: %q", got)
	}
	if got := ExecuteTool("propose_fix", json.RawMessage(`{"blockId":"`+blockID+`","ruleId":"unhandled-error"}`), tctx); !strings.Contains(got, `Fix "wrap-error-handler"`) {
		t.Errorf("propose_fix failed against the real doc: %q", got)
	}

	// Control (pre-A2 behaviour): without RealDoc, the scrubbed analysis hides
	// the finding entirely.
	legacy := ToolContext{Ctx: context.Background(), Doc: scrubbed, Analysis: analysis}
	if got := ExecuteTool("list_findings", json.RawMessage(`{}`), legacy); !strings.Contains(got, "No findings match") {
		t.Errorf("scrubbed analysis should hide the value-dependent finding: %q", got)
	}
}

// TestListFindings_ScrubsRealDocTitles: finding titles are rendered through
// ScrubText because real-doc analysis can embed property evidence in them.
func TestListFindings_ScrubsRealDocTitles(t *testing.T) {
	realDoc, blockID := fixToolDoc(t)
	report := fixReport(blockID)
	report.Findings[0].Title = `Hardcoded credential detected: password: "supersecret42"`
	tctx := ToolContext{
		Ctx:      context.Background(),
		Doc:      realDoc,
		RealDoc:  realDoc,
		Analysis: realDocOnlyAnalysis{realDoc: realDoc, report: report},
	}
	got := ExecuteTool("list_findings", json.RawMessage(`{}`), tctx)
	if strings.Contains(got, "supersecret42") {
		t.Errorf("finding title leaked the secret: %q", got)
	}
	if !strings.Contains(got, "[REDACTED]") {
		t.Errorf("title not scrubbed: %q", got)
	}
}

// TestToolApplyFixes_InputValidation drives the batch tool's bounds directly:
// empty batch, over-limit batch, unresolvable target — all refuse to REQUEST
// approval (all-or-nothing preview; the user never sees a partial batch).
func TestToolApplyFixes_InputValidation(t *testing.T) {
	stub := &stubFixApplier{ret: "batch ok"}
	doc, blockID := fixToolDoc(t)
	tctx := ToolContext{Ctx: context.Background(), Doc: doc, RealDoc: doc, Analysis: fixFakeAnalysis{fixReport(blockID)}, Fixes: stub}

	if got := ExecuteTool("apply_fixes", []byte(`{"targets":[]}`), tctx); !strings.Contains(got, "targets is required") {
		t.Errorf("empty batch: %q", got)
	}
	many := make([]map[string]string, 11)
	for i := range many {
		many[i] = map[string]string{"blockId": blockID, "ruleId": "unhandled-error"}
	}
	in, _ := json.Marshal(map[string]any{"targets": many})
	if got := ExecuteTool("apply_fixes", in, tctx); !strings.Contains(got, "at most 10") {
		t.Errorf("over-limit: %q", got)
	}
	if got := ExecuteTool("apply_fixes", []byte(`{"targets":[{"blockId":"`+blockID+`","ruleId":"missing-rule"}]}`), tctx); !strings.Contains(got, "could not be prepared") {
		t.Errorf("unresolvable target: %q", got)
	}
	if len(stub.got) != 0 {
		t.Errorf("no approval must be requested on refused batches, got %d", len(stub.got))
	}

	// A resolvable batch forwards to the applier.
	ok, _ := json.Marshal(map[string]any{"targets": []map[string]string{{"blockId": blockID, "ruleId": "unhandled-error"}}})
	if got := ExecuteTool("apply_fixes", ok, tctx); got != "batch ok" {
		t.Errorf("valid batch: %q", got)
	}
	if len(stub.got) != 1 {
		t.Errorf("applier called %d times, want 1", len(stub.got))
	}
}

// manyFindingsReport builds a report with N distinct findings on one block.
func manyFindingsReport(blockID string, n int) *models.AnalysisReport {
	report := &models.AnalysisReport{Findings: make([]models.Finding, n)}
	for i := range report.Findings {
		report.Findings[i] = models.Finding{
			RuleID:      fmt.Sprintf("rule-%02d", i),
			BlockID:     blockID,
			Title:       fmt.Sprintf("Finding %d", i),
			Severity:    "warning",
			Fingerprint: fmt.Sprintf("fp-%02d", i),
			AutoFix:     "wrap-error-handler",
		}
	}
	return report
}

// TestListFindings_PaginationAndKeys pins the enumerate-everything contract:
// pages of 40 with an "…and N more — offset=" trailer, finding keys on every
// line (so finding:<key> deep links work from tool output), and a clean
// out-of-range answer instead of an empty page.
func TestListFindings_PaginationAndKeys(t *testing.T) {
	doc, blockID := fixToolDoc(t)
	tctx := ToolContext{Ctx: context.Background(), Doc: doc, RealDoc: doc, Analysis: fixFakeAnalysis{manyFindingsReport(blockID, 95)}}

	page1 := ExecuteTool("list_findings", []byte(`{}`), tctx)
	if !strings.Contains(page1, "showing 1-40 of 95") {
		t.Errorf("page 1 header wrong: %q", strings.SplitN(page1, "\n", 2)[0])
	}
	if !strings.Contains(page1, "…and 55 more — call again with offset=40") {
		t.Errorf("page 1 trailer wrong: %q", page1[len(page1)-120:])
	}
	if !strings.Contains(page1, "key=fp-00") || !strings.Contains(page1, "key=fp-39") {
		t.Errorf("finding keys missing from page 1")
	}

	page2 := ExecuteTool("list_findings", []byte(`{"offset":40}`), tctx)
	if !strings.Contains(page2, "showing 41-80 of 95") || !strings.Contains(page2, "key=fp-40") {
		t.Errorf("page 2 wrong: %q", strings.SplitN(page2, "\n", 2)[0])
	}
	page3 := ExecuteTool("list_findings", []byte(`{"offset":80}`), tctx)
	if !strings.Contains(page3, "showing 81-95 of 95") || strings.Contains(page3, "…and") {
		t.Errorf("last page wrong: %q", page3)
	}
	if got := ExecuteTool("list_findings", []byte(`{"offset":200}`), tctx); !strings.Contains(got, "beyond the 95 matching") {
		t.Errorf("out-of-range offset: %q", got)
	}
}
