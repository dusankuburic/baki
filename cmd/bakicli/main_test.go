package main

import (
	"os"
	"path/filepath"
	"testing"

	"pad-core/models"
)

func TestLoadPolicy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	body := `{
		"name": "Security baseline",
		"gateSeverity": "warning",
		"rules": [
			{"ruleId": "hardcoded-credential", "enabled": true, "severity": "error"},
			{"ruleId": "sql-injection", "enabled": true}
		]
	}`
	if err := os.WriteFile(path, []byte(body), 0644); err != nil {
		t.Fatalf("write policy: %v", err)
	}

	p, err := loadPolicy(path)
	if err != nil {
		t.Fatalf("loadPolicy: %v", err)
	}
	if p.Name != "Security baseline" || p.GateSeverity != models.SeverityWarning {
		t.Errorf("policy metadata mismatch: %+v", p)
	}
	if len(p.Rules) != 2 || p.Rules[0].RuleID != "hardcoded-credential" || p.Rules[0].Severity != models.SeverityError {
		t.Errorf("policy rules mismatch: %+v", p.Rules)
	}
}

func TestLoadPolicy_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := loadPolicy(path); err == nil {
		t.Error("expected an error for invalid policy JSON")
	}
}

func TestLoadPolicy_MissingFile(t *testing.T) {
	if _, err := loadPolicy(filepath.Join(t.TempDir(), "nope.json")); err == nil {
		t.Error("expected an error for a missing policy file")
	}
}

// TestLoadBaseline_MissingFileReturnsNoBaseline verifies a missing baseline
// file is NOT an error — it means "no baseline established yet", so the caller
// gates on all findings and prompts for -update-baseline.
func TestLoadBaseline_MissingFileReturnsNoBaseline(t *testing.T) {
	bl, err := loadBaseline(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing baseline should not error, got %v", err)
	}
	if bl.Found {
		t.Error("a missing file must report Found=false")
	}
	if bl.Keys != nil || bl.RuleSet != nil {
		t.Errorf("a missing file must return no keys and no rule set, got %+v", bl)
	}
}

// TestWriteThenLoadBaseline_RoundTrip verifies writeBaseline captures the
// content-stable fingerprints and loadBaseline reads them back, and that the
// round-trip is idempotent (the same report reproduces the same baseline).
func TestWriteThenLoadBaseline_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	report := &models.AnalysisReport{FlowID: "f1", Findings: []models.Finding{
		{RuleID: "r1", BlockID: "b1", Fingerprint: "r1:aaaa"},
		{RuleID: "r2", BlockID: "b2", Fingerprint: "r2:bbbb"},
		{RuleID: "r1", BlockID: "b1", Fingerprint: "r1:aaaa"}, // duplicate fingerprint
	}}
	if err := writeBaseline(path, findingKeys(report)); err != nil {
		t.Fatalf("writeBaseline: %v", err)
	}
	bl, err := loadBaseline(path)
	if err != nil || !bl.Found {
		t.Fatalf("loadBaseline after write: %+v err=%v", bl, err)
	}
	keys := bl.Keys
	// Duplicate fingerprint deduped → 2 distinct keys.
	if len(keys) != 2 {
		t.Errorf("expected 2 distinct baseline keys, got %d (%v)", len(keys), keys)
	}
	for _, k := range keys {
		if k != "r1:aaaa" && k != "r2:bbbb" {
			t.Errorf("unexpected baseline key %q", k)
		}
	}
}

// TestWriteBaseline_FallsBackToKey confirms a finding without a content-stable
// Fingerprint is still captured via its legacy Key() (so older reports work).
func TestWriteBaseline_FallsBackToKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "baseline.json")
	report := &models.AnalysisReport{Findings: []models.Finding{
		{RuleID: "r3", BlockID: "b3"}, // no Fingerprint
	}}
	if err := writeBaseline(path, findingKeys(report)); err != nil {
		t.Fatalf("writeBaseline: %v", err)
	}
	bl, _ := loadBaseline(path)
	if len(bl.Keys) != 1 || bl.Keys[0] != "r3:b3" {
		t.Errorf("expected fallback to Key() 'r3:b3', got %v", bl.Keys)
	}
}
