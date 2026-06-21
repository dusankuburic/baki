package export

import (
	"bytes"
	"encoding/json"
	"testing"

	"pad-core/models"
)

// parseSARIF runs the exporter and unmarshals the result, failing the test on
// any error so each case can focus on assertions.
func parseSARIF(t *testing.T, report *models.AnalysisReport, doc *models.FlowDocument) (sarifLog, []byte) {
	t.Helper()
	raw, err := ReportToSARIF(report, doc)
	if err != nil {
		t.Fatalf("ReportToSARIF returned error: %v", err)
	}
	var log sarifLog
	if err := json.Unmarshal(raw, &log); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, raw)
	}
	return log, raw
}

func TestReportToSARIF_EnvelopeAndEmpty(t *testing.T) {
	log, raw := parseSARIF(t, makeReport(), makeDoc("Flow", ""))

	if log.Schema == "" {
		t.Error("expected $schema to be set")
	}
	if log.Version != "2.1.0" {
		t.Errorf("version = %q, want 2.1.0", log.Version)
	}
	if len(log.Runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(log.Runs))
	}
	if log.Runs[0].Tool.Driver.Name != sarifTool {
		t.Errorf("driver name = %q, want %q", log.Runs[0].Tool.Driver.Name, sarifTool)
	}
	if len(log.Runs[0].Results) != 0 {
		t.Errorf("expected 0 results, got %d", len(log.Runs[0].Results))
	}

	// Empty collections must serialize as [] (not null) so strict SARIF
	// consumers don't choke.
	if !bytes.Contains(raw, []byte(`"results": []`)) {
		t.Errorf("expected results to serialize as [], got:\n%s", raw)
	}
	if !bytes.Contains(raw, []byte(`"rules": []`)) {
		t.Errorf("expected rules to serialize as [], got:\n%s", raw)
	}
}

func TestReportToSARIF_LevelMapping(t *testing.T) {
	report := makeReport(
		models.Finding{RuleID: "r-err", Severity: models.SeverityError, Title: "E", BlockID: "b1", SubflowID: "s1"},
		models.Finding{RuleID: "r-warn", Severity: models.SeverityWarning, Title: "W", BlockID: "b2", SubflowID: "s1"},
		models.Finding{RuleID: "r-info", Severity: models.SeverityInfo, Title: "I", BlockID: "b3", SubflowID: "s1"},
	)
	log, _ := parseSARIF(t, report, makeDoc("Flow", ""))

	want := map[string]string{"r-err": "error", "r-warn": "warning", "r-info": "note"}
	for _, res := range log.Runs[0].Results {
		if got := res.Level; got != want[res.RuleID] {
			t.Errorf("level for %s = %q, want %q", res.RuleID, got, want[res.RuleID])
		}
	}
}

func TestReportToSARIF_RuleDedup(t *testing.T) {
	report := makeReport(
		models.Finding{RuleID: "dup", Severity: models.SeverityWarning, Title: "first", BlockID: "b1", SubflowID: "s1"},
		models.Finding{RuleID: "dup", Severity: models.SeverityWarning, Title: "second", BlockID: "b2", SubflowID: "s1"},
		models.Finding{RuleID: "other", Severity: models.SeverityError, Title: "x", BlockID: "b3", SubflowID: "s1"},
	)
	log, _ := parseSARIF(t, report, makeDoc("Flow", ""))

	rules := log.Runs[0].Tool.Driver.Rules
	if len(rules) != 2 {
		t.Fatalf("expected 2 unique rule descriptors, got %d", len(rules))
	}
	// Every result's ruleIndex must point at the matching descriptor.
	for _, res := range log.Runs[0].Results {
		if res.RuleIndex < 0 || res.RuleIndex >= len(rules) {
			t.Fatalf("ruleIndex %d out of range", res.RuleIndex)
		}
		if rules[res.RuleIndex].ID != res.RuleID {
			t.Errorf("result %s points at descriptor %s", res.RuleID, rules[res.RuleIndex].ID)
		}
	}
}

func docWithBlock(subflowID, sourceFile, blockID string, line int) *models.FlowDocument {
	return &models.FlowDocument{
		Name: "Flow",
		Subflows: []models.Subflow{{
			ID:         subflowID,
			SourceFile: sourceFile,
			Blocks:     []models.Block{{ID: blockID, LineNumber: line, SubflowID: subflowID}},
		}},
	}
}

func TestReportToSARIF_PhysicalLocationFromDoc(t *testing.T) {
	doc := docWithBlock("Main", "Main.txt", "blk-42", 17)
	report := makeReport(models.Finding{
		RuleID: "r1", Severity: models.SeverityError, Title: "boom",
		BlockID: "blk-42", SubflowID: "Main",
	})
	log, _ := parseSARIF(t, report, doc)

	locs := log.Runs[0].Results[0].Locations
	if len(locs) != 1 || locs[0].PhysicalLocation == nil {
		t.Fatalf("expected one physical location, got %+v", locs)
	}
	phys := locs[0].PhysicalLocation
	if phys.ArtifactLocation.URI != "Main.txt" {
		t.Errorf("uri = %q, want Main.txt", phys.ArtifactLocation.URI)
	}
	if phys.Region == nil || phys.Region.StartLine != 17 {
		t.Errorf("expected region startLine 17, got %+v", phys.Region)
	}
	if len(locs[0].LogicalLocations) != 1 || locs[0].LogicalLocations[0].Name != "blk-42" {
		t.Errorf("expected logical location for block, got %+v", locs[0].LogicalLocations)
	}
}

func TestReportToSARIF_NilDocSynthesizesURI(t *testing.T) {
	report := makeReport(models.Finding{
		RuleID: "r1", Severity: models.SeverityWarning, Title: "t",
		BlockID: "b1", SubflowID: "Sub",
	})
	log, _ := parseSARIF(t, report, nil) // must not panic

	loc := log.Runs[0].Results[0].Locations[0]
	if loc.PhysicalLocation.ArtifactLocation.URI != "Sub.txt" {
		t.Errorf("uri = %q, want synthesized Sub.txt", loc.PhysicalLocation.ArtifactLocation.URI)
	}
	// No doc means no resolvable line, so the region must be omitted.
	if loc.PhysicalLocation.Region != nil {
		t.Errorf("expected no region without a doc, got %+v", loc.PhysicalLocation.Region)
	}
}

func TestReportToSARIF_MessageFallbackAndSuggestion(t *testing.T) {
	report := makeReport(models.Finding{
		RuleID: "r1", Severity: models.SeverityInfo, Title: "Use the vault",
		BlockID: "b1", SubflowID: "s1", Suggestion: "Store secrets in Key Vault.",
	})
	log, _ := parseSARIF(t, report, makeDoc("Flow", ""))

	msg := log.Runs[0].Results[0].Message.Text
	if !bytes.Contains([]byte(msg), []byte("Use the vault")) {
		t.Errorf("expected title fallback in message, got %q", msg)
	}
	if !bytes.Contains([]byte(msg), []byte("Store secrets in Key Vault.")) {
		t.Errorf("expected suggestion appended to message, got %q", msg)
	}
}

func TestReportToSARIF_FingerprintStableAndDistinct(t *testing.T) {
	a := models.Finding{RuleID: "r1", SubflowID: "s1", BlockID: "b1", Title: "t"}
	b := models.Finding{RuleID: "r1", SubflowID: "s1", BlockID: "b2", Title: "t"}

	fpA1, fpA2 := fingerprint(a), fingerprint(a)
	if fpA1 == "" {
		t.Fatal("fingerprint should not be empty")
	}
	if fpA1 != fpA2 {
		t.Error("fingerprint should be deterministic")
	}
	if fpA1 == fingerprint(b) {
		t.Error("findings at different blocks should have different fingerprints")
	}

	report := makeReport(a)
	log, _ := parseSARIF(t, report, makeDoc("Flow", ""))
	if log.Runs[0].Results[0].PartialFingerprints["padFindingId/v1"] != fingerprint(a) {
		t.Error("expected partial fingerprint to match computed value")
	}
}
