package analyzer

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadCustomRules_ValidFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rules.json")
	content := `[
	  {
	    "id": "custom-no-http-get",
	    "name": "No HTTP GET",
	    "description": "HTTP GET actions are not allowed",
	    "severity": "warning",
	    "category": "Security",
	    "rawTypeMatch": "HTTPClient\\.Invoke",
	    "propertyHas": {"method": "GET"},
	    "suggestion": "Use POST instead of GET."
	  }
	]`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	rules, _, err := LoadCustomRules(path)
	if err != nil {
		t.Fatalf("LoadCustomRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(rules))
	}
	if rules[0].ID() != "custom-no-http-get" {
		t.Errorf("ID = %q, want custom-no-http-get", rules[0].ID())
	}
}

func TestLoadCustomRules_MissingFile(t *testing.T) {
	rules, _, err := LoadCustomRules("/nonexistent/path/rules.json")
	if err != nil {
		t.Fatalf("missing file should return nil,nil; got error: %v", err)
	}
	if rules != nil {
		t.Fatalf("missing file should return nil rules; got %d", len(rules))
	}
}

func TestLoadCustomRules_InvalidJSON(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.json")
	os.WriteFile(path, []byte(`{not valid json`), 0644)
	_, _, err := LoadCustomRules(path)
	if err == nil {
		t.Fatal("expected error for invalid JSON, got nil")
	}
}

func TestLoadCustomRules_SkipsInvalidRules(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mixed.json")
	content := `[
	  {"id": "valid-rule", "name": "Valid", "severity": "warning"},
	  {"id": "bad-regex", "name": "Bad", "rawTypeMatch": "[invalid"}
	]`
	os.WriteFile(path, []byte(content), 0644)
	rules, _, err := LoadCustomRules(path)
	if err != nil {
		t.Fatalf("LoadCustomRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 valid rule (invalid silently skipped), got %d", len(rules))
	}
	if rules[0].ID() != "valid-rule" {
		t.Errorf("expected valid-rule, got %s", rules[0].ID())
	}
}

func TestLoadCustomRules_EmptyArray(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "empty.json")
	os.WriteFile(path, []byte(`[]`), 0644)
	rules, _, err := LoadCustomRules(path)
	if err != nil {
		t.Fatalf("LoadCustomRules: %v", err)
	}
	if len(rules) != 0 {
		t.Fatalf("expected 0 rules, got %d", len(rules))
	}
}
