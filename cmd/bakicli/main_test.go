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
