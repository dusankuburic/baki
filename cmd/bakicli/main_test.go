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
	keys, hadBaseline, err := loadBaseline(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("missing baseline should not error, got %v", err)
	}
	if hadBaseline {
		t.Error("a missing file must report hadBaseline=false")
	}
	if keys != nil {
		t.Errorf("a missing file must return nil keys, got %v", keys)
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
	if err := writeBaseline(path, report); err != nil {
		t.Fatalf("writeBaseline: %v", err)
	}
	keys, hadBaseline, err := loadBaseline(path)
	if err != nil || !hadBaseline {
		t.Fatalf("loadBaseline after write: keys=%v had=%v err=%v", keys, hadBaseline, err)
	}
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
	if err := writeBaseline(path, report); err != nil {
		t.Fatalf("writeBaseline: %v", err)
	}
	keys, _, _ := loadBaseline(path)
	if len(keys) != 1 || keys[0] != "r3:b3" {
		t.Errorf("expected fallback to Key() 'r3:b3', got %v", keys)
	}
}
