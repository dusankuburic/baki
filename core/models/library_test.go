package models

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestBatchAnalysis_LibraryIsAdditive(t *testing.T) {
	data, err := json.Marshal(BatchAnalysis{Results: []BatchResult{}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "library") {
		t.Errorf("a batch without a library pass must keep today's JSON: %s", data)
	}
	data, err = json.Marshal(BatchAnalysis{Library: &LibraryReport{Findings: []LibraryFinding{}}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"library":{"findings":[],"flows":0,"rulesRun":0,`) {
		t.Errorf("library report JSON: %s", data)
	}
}

func TestLibraryFinding_JSON(t *testing.T) {
	f := LibraryFinding{
		Finding:     Finding{ID: "L-001", RuleID: "r", Severity: SeverityInfo, Fingerprint: "r:00", Stage: "preview"},
		FlowID:      "f1",
		FlowName:    "Invoices",
		SubflowName: "Login",
		Related:     []LibraryLocation{{FlowID: "f2", FlowName: "Orders", SubflowName: "Login"}},
		Anchor:      "#1",
	}
	data, err := json.Marshal(f)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"id", "ruleId", "severity", "fingerprint", "stage", "flowId", "flowName", "subflowName", "related"} {
		if _, ok := got[k]; !ok {
			t.Errorf("missing %q in %s", k, data)
		}
	}
	if _, ok := got["Anchor"]; ok || strings.Contains(string(data), "#1") {
		t.Errorf("Anchor must not be serialized: %s", data)
	}
}
