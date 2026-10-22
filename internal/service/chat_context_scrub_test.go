package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-core/models"
)

// TestComputeContextCore_ScrubsSelectedSourceFiles pins S1: selected source
// files are AST-scrubbed before injection into the model context. PAD's
// `SET Password TO $”'secret”'` syntax matches no key=value regex, so raw
// disk bytes used to reach the model verbatim — bypassing the property-name
// masking the document path gets.
func TestComputeContextCore_ScrubsSelectedSourceFiles(t *testing.T) {
	dir := t.TempDir()
	const secret = "super-secret-password"
	src := "FUNCTION Main\n    SET Password TO $'''" + secret + "'''\nEND"
	if err := os.WriteFile(filepath.Join(dir, "sub.txt"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	svc := &ChatService{flowCache: &FlowService{}}
	doc := &models.FlowDocument{
		ID:       "f1",
		FilePath: filepath.Join(dir, "flow.txt"),
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{
			{ID: "b1", Name: "Xavier", Type: models.BlockTypeAction, RawType: "Foo.Bar"},
		}}},
	}
	doc.RebuildIndexes()

	cv := svc.computeContextCore(ctxCacheStubProvider{}, doc, nil, models.ChatRequest{
		SelectedSourceFiles: []string{"sub.txt"},
	})

	if strings.Contains(cv.contextText, secret) {
		t.Errorf("source-file secret leaked into model context:\n%s", cv.contextText)
	}
	if !strings.Contains(cv.contextText, "sub.txt") {
		t.Errorf("source file context missing entirely (read path broken?):\n%s", cv.contextText)
	}
}

// TestComputeContextCore_SourceFilesExcludedWithoutFlowPath: the FilePath
// guard must keep the source injection off for cloud docs (empty FilePath) —
// and the S1 scrub must not change that.
func TestComputeContextCore_SourceFilesExcludedWithoutFlowPath(t *testing.T) {
	svc := &ChatService{flowCache: &FlowService{}}
	doc := &models.FlowDocument{ID: "f1"}

	cv := svc.computeContextCore(ctxCacheStubProvider{}, doc, nil, models.ChatRequest{
		SelectedSourceFiles: []string{"sub.txt"},
	})

	if strings.Contains(cv.contextText, "sub.txt") {
		t.Errorf("source file read without a flow directory — path traversal guard regressed")
	}
	_ = context.Background()
}

// TestComputeContextCore_ScrubsURLCredentialsInFindings pins R-security-c12's second channel: the context is
// grounded in the REAL document's analysis, and the hardcoded-url rule copies the raw URL into the finding's
// Description and Metadata["url"], which BuildContext prints for the selected block. Only the context scrub
// stands between those credentials and the provider.
func TestComputeContextCore_ScrubsURLCredentialsInFindings(t *testing.T) {
	const url = "https://svc:Pr0d!Pass@portal.example.com/in?sv=2022-11-02&sig=Ab12Cd34Ef56%2BGh78%3D"
	svc := &ChatService{flowCache: &FlowService{}}
	doc := &models.FlowDocument{
		ID: "f1",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{
			{ID: "b1", Name: "Launch", Type: models.BlockTypeAction, RawType: "WebAutomation.LaunchEdge",
				Properties: map[string]string{"Url": url}},
		}}},
	}
	doc.RebuildIndexes()
	report := &models.AnalysisReport{Findings: []models.Finding{{
		ID: "hardcoded-url:b1", RuleID: "hardcoded-url", BlockID: "b1", SubflowID: "sf1",
		Title:       "Hardcoded URL",
		Description: "Action contains a hardcoded URL: " + url[:60],
		Metadata:    map[string]any{"url": url},
	}}}

	cv := svc.computeContextCore(ctxCacheStubProvider{}, doc, report, models.ChatRequest{ContextBlockID: "b1"})

	if !strings.Contains(cv.contextText, "Known Issues with This Block") {
		t.Fatalf("the finding was not rendered at all — the test is not exercising the channel:\n%s", cv.contextText)
	}
	for _, secret := range []string{"Pr0d!Pass", "Ab12Cd34Ef56"} {
		if strings.Contains(cv.contextText, secret) {
			t.Errorf("URL credential %q reached the model context:\n%s", secret, cv.contextText)
		}
	}
	if !strings.Contains(cv.contextText, "portal.example.com") {
		t.Errorf("the URL's host was masked too — the model loses what the finding is about:\n%s", cv.contextText)
	}
}
