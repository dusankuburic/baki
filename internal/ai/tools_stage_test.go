package ai

import (
	"context"
	"strings"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
)

// TestListFindings_LabelsPreviewFindings: the model is told which findings come
// from preview rules (stamped, or unstamped of a preview built-in, as analyzer.Gates
// decides), so it can say they do not gate; stable findings carry no label.
func TestListFindings_LabelsPreviewFindings(t *testing.T) {
	restore := analyzer.SetRuleMetaForTesting("insecure-http-url", analyzer.RuleMeta{
		Confidence: models.ConfidenceMedium, IntroducedIn: 1, Stage: analyzer.StagePreview,
	})
	defer restore()

	doc, blockID := fixToolDoc(t)
	report := &models.AnalysisReport{Findings: []models.Finding{
		{RuleID: "unhandled-error", BlockID: blockID, Title: "stable one"},
		{RuleID: "hardcoded-url", BlockID: blockID, Title: "stamped one", Stage: "preview"},
		{RuleID: "insecure-http-url", BlockID: blockID, Title: "unstamped one"},
	}}
	out := ExecuteTool("list_findings", nil, ToolContext{Ctx: context.Background(), Doc: doc, Analysis: fixFakeAnalysis{report}})

	lines := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		for _, title := range []string{"stable one", "stamped one", "unstamped one"} {
			if strings.Contains(l, title) {
				lines[title] = l
			}
		}
	}
	if len(lines) != 3 {
		t.Fatalf("want 3 finding lines, got:\n%s", out)
	}
	if strings.Contains(lines["stable one"], "stage=") {
		t.Errorf("stable finding labelled: %q", lines["stable one"])
	}
	for _, title := range []string{"stamped one", "unstamped one"} {
		if !strings.HasSuffix(lines[title], " stage=preview") {
			t.Errorf("preview finding not labelled: %q", lines[title])
		}
	}
}
