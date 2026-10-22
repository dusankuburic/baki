package ai

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"pad-core/models"
)

// uuidLike returns a deterministic 36-character id shaped like the parser's block and subflow UUIDs.
func uuidLike(prefix string, i int) string {
	s := fmt.Sprintf("%s%032d", prefix, i)
	s = s[len(s)-32:]
	return s[:8] + "-" + s[8:12] + "-" + s[12:16] + "-" + s[16:20] + "-" + s[20:]
}

// realisticFindingsReport is n findings of one category with the line shape a real flow produces: UUID block
// and subflow ids, a 60-character title, a rule:16-hex key and a fix type.
func realisticFindingsReport(n int) *models.AnalysisReport {
	report := &models.AnalysisReport{Findings: make([]models.Finding, n)}
	for i := range report.Findings {
		report.Findings[i] = models.Finding{
			RuleID:      "unhandled-error",
			Category:    "Reliability",
			Severity:    "warning",
			Title:       fmt.Sprintf("%-60s", fmt.Sprintf("HTTP action %d has no error handling around the request", i))[:60],
			BlockID:     uuidLike("b", i),
			SubflowID:   uuidLike("s", i%3),
			Fingerprint: fmt.Sprintf("unhandled-error:%016x", 0xa000+i),
			AutoFix:     "wrap-error-handler",
		}
	}
	return report
}

var (
	findingKeyRe = regexp.MustCompile(`key=(\S+)`)
	nextOffsetRe = regexp.MustCompile(`offset=(\d+)`)
)

// TestListFindings_PagesFitTheResultCap pins R-tool-loop-2: a page of real-shaped lines used to run to 8-10 KB,
// so ExecuteTool cut it at maxToolResultBytes — dropping the tail findings and the "call again with offset="
// hint appended last. Following the offsets must now enumerate every finding, no page may be truncated, and
// the next offset is on the first line, where truncation could never reach it.
func TestListFindings_PagesFitTheResultCap(t *testing.T) {
	const total = 120
	tctx := ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc(), Analysis: fixFakeAnalysis{realisticFindingsReport(total)}}

	seen := map[string]bool{}
	offset := 0
	for page := 1; page <= total; page++ {
		res := ExecuteTool("list_findings", fmt.Appendf(nil, `{"category":"Reliability","offset":%d}`, offset), tctx)
		if len(res) > maxToolResultBytes {
			t.Fatalf("page %d is %d bytes, over the %d-byte result cap", page, len(res), maxToolResultBytes)
		}
		if strings.Contains(res, "(result truncated)") {
			t.Fatalf("page %d was truncated:\n%s", page, res)
		}
		for _, m := range findingKeyRe.FindAllStringSubmatch(res, -1) {
			seen[m[1]] = true
		}
		first, _, _ := strings.Cut(res, "\n")
		m := nextOffsetRe.FindStringSubmatch(first)
		if len(seen) < total && m == nil {
			t.Fatalf("page %d leaves %d finding(s) unlisted but its first line names no next offset: %q", page, total-len(seen), first)
		}
		if m == nil {
			break
		}
		next, _ := strconv.Atoi(m[1])
		if next <= offset {
			t.Fatalf("page %d: next offset %d does not advance past %d", page, next, offset)
		}
		offset = next
	}
	if len(seen) != total {
		t.Errorf("following the offsets listed %d distinct findings, want %d", len(seen), total)
	}
}

// TestListFindings_SubflowFilterDropsTheSubflowColumn: when the call filters by subflow every line would
// repeat the same 36-character id, so it is left out.
func TestListFindings_SubflowFilterDropsTheSubflowColumn(t *testing.T) {
	report := realisticFindingsReport(9)
	tctx := ToolContext{Ctx: context.Background(), Doc: toolFixtureDoc(), Analysis: fixFakeAnalysis{report}}
	sf := uuidLike("s", 1)
	res := ExecuteTool("list_findings", []byte(`{"subflowId":"`+sf+`"}`), tctx)
	if !strings.Contains(res, "key=") {
		t.Fatalf("filtered listing is empty: %q", res)
	}
	if strings.Contains(res, "subflow="+sf) {
		t.Errorf("a subflow-filtered listing repeats the subflow id on every line:\n%s", res)
	}
	if all := ExecuteTool("list_findings", []byte(`{}`), tctx); !strings.Contains(all, "subflow=") {
		t.Errorf("an unfiltered listing must still say which subflow each finding is in:\n%s", all)
	}
}

// blockingAnalysis answers only when its context ends.
type blockingAnalysis struct{}

func (blockingAnalysis) AnalyzeFlow(ctx context.Context, _ *models.FlowDocument) (*models.AnalysisReport, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}
func (blockingAnalysis) GetVariableLineage(*models.FlowDocument, string) (*models.VariableHistory, error) {
	return nil, nil
}

// TestExecuteTool_ListFindingsReportsATimeout pins the R-tool-loop-1 sub-item: an analysis that outlives the
// tool's deadline used to come back as "analysis not available", a sentinel the loop memory then served for
// the rest of the run. It is ExecuteTool's "timed out" error instead.
func TestExecuteTool_ListFindingsReportsATimeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	tctx := ToolContext{Ctx: ctx, Doc: toolFixtureDoc(), Analysis: blockingAnalysis{}}
	got := ExecuteTool("list_findings", []byte(`{}`), tctx)
	if !strings.HasPrefix(got, "error: tool list_findings timed out") {
		t.Errorf("a list_findings that hit its deadline answered %q", got)
	}
}
