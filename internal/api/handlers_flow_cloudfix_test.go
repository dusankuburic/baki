package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/models"
	"pad-core/parser"
)

// seedCloudFlowWithSource parses source, then seeds a library flow carrying
// BOTH the parsed Content and the raw Source text (the shape cloud apply-fix /
// preview-fix operate on). Returns the parsed doc so the test can read block IDs.
func seedCloudFlowWithSource(t *testing.T, rt *Router, id, ownerID, source string) *models.FlowDocument {
	t.Helper()
	doc, err := parser.ParseText(source, id+".txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse seed source: %v", err)
	}
	content, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal seed doc: %v", err)
	}
	storageDoc := &storageif.FlowDocument{
		ID:      id,
		Name:    id,
		OwnerID: ownerID,
		Content: content,
		Source:  source,
	}
	if err := rt.security.Backend.SaveFlow(context.Background(), storageDoc); err != nil {
		t.Fatalf("seed flow %s: %v", id, err)
	}
	return doc
}

// firstFixableFinding runs analysis on a parsed doc and returns the first
// finding that has an auto-fix, so the test can drive apply-fix/preview-fix.
func firstFixableFinding(t *testing.T, doc *models.FlowDocument) models.Finding {
	t.Helper()
	report := analyzer.RunAnalysis(doc, analyzer.AllRules(), models.DefaultSettings(), nil)
	for _, f := range report.Findings {
		if f.AutoFix != "" {
			return f
		}
	}
	t.Fatalf("no auto-fixable finding in seeded flow; findings: %+v", report.Findings)
	return models.Finding{}
}

// TestApplyFix_CloudMode_Works verifies cloud-mode apply-fix (previously 403)
// now patches the stored raw source, persists it, and resolves the finding.
func TestApplyFix_CloudMode_Works(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	const source = "#Region \"Main\"\nSET X TO %X%\n#EndRegion\n"
	doc := seedCloudFlowWithSource(t, rt, "flow1", "alice", source)
	finding := firstFixableFinding(t, doc)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	resp := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/apply-fix", bearer, map[string]any{
		"flowId": "flow1", "blockId": finding.BlockID, "fixType": finding.AutoFix, "ruleId": finding.RuleID,
	})
	checkStatus(t, resp, http.StatusOK)

	// Reload the flow's stored source — the redundant SET must be gone.
	reloaded, err := rt.security.Backend.LoadFlow(context.Background(), "flow1")
	if err != nil {
		t.Fatalf("reload flow: %v", err)
	}
	if strings.Contains(reloaded.Source, "SET X TO %X%") {
		t.Errorf("expected the redundant SET removed from stored source, still present:\n%s", reloaded.Source)
	}
}

// TestPreviewFix_CloudMode_Works verifies cloud-mode preview-fix (previously
// 403) returns the before/after source text for a dry-run diff.
func TestPreviewFix_CloudMode_Works(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	const source = "#Region \"Main\"\nSET X TO %X%\n#EndRegion\n"
	doc := seedCloudFlowWithSource(t, rt, "flow1", "alice", source)
	finding := firstFixableFinding(t, doc)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	resp := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/preview-fix", bearer, map[string]any{
		"flowId": "flow1", "blockId": finding.BlockID, "fixType": finding.AutoFix, "ruleId": finding.RuleID,
	})
	checkStatus(t, resp, http.StatusOK)
	var result struct {
		Original string `json:"original"`
		Patched  string `json:"patched"`
	}
	decodeJSON(t, resp, &result)
	if result.Original == "" || result.Patched == "" {
		t.Fatalf("expected non-empty original/patched, got: %+v", result)
	}
	if result.Original == result.Patched {
		t.Errorf("preview did not change the source (original == patched)")
	}
	if strings.Contains(result.Patched, "SET X TO %X%") {
		t.Errorf("patched source still contains the redundant SET:\n%s", result.Patched)
	}
}

// TestApplyFixBatch_CloudMode_Works verifies the batch endpoint fixes multiple
// findings in one pass (previously cloud had no fix at all). Seeds a flow with
// two redundant-action findings, batches the rule, and asserts both resolve.
func TestApplyFixBatch_CloudMode_Works(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	// Two self-assignments → two redundant-action findings.
	const source = "#Region \"Main\"\nSET X TO %X%\nSET Y TO %Y%\n#EndRegion\n"
	seedCloudFlowWithSource(t, rt, "flow1", "alice", source)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	resp := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/apply-fix-batch", bearer, map[string]any{
		"flowId": "flow1", "rules": []string{"redundant-action"},
	})
	checkStatus(t, resp, http.StatusOK)
	var result struct {
		Applied int `json:"applied"`
	}
	decodeJSON(t, resp, &result)
	if result.Applied < 2 {
		t.Errorf("expected ≥2 fixes applied, got %d", result.Applied)
	}

	// Both redundant SETs must be gone from the stored source.
	reloaded, err := rt.security.Backend.LoadFlow(context.Background(), "flow1")
	if err != nil {
		t.Fatalf("reload flow: %v", err)
	}
	if strings.Contains(reloaded.Source, "SET X TO %X%") || strings.Contains(reloaded.Source, "SET Y TO %Y%") {
		t.Errorf("expected both redundant SETs removed, source still contains one:\n%s", reloaded.Source)
	}
}
