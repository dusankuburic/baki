package analyzer

import (
	"os"
	"path/filepath"
	"strings"
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
	rules, warnings, err := LoadCustomRules(path)
	if err != nil {
		t.Fatalf("LoadCustomRules: %v", err)
	}
	if len(rules) != 1 {
		t.Fatalf("expected 1 valid rule (the invalid one skipped with a warning), got %d", len(rules))
	}
	if rules[0].ID() != "valid-rule" {
		t.Errorf("expected valid-rule, got %s", rules[0].ID())
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], `entry 1 (rule "bad-regex") skipped`) {
		t.Errorf("warnings = %v, want one naming the skipped entry", warnings)
	}
}

// TestLoadCustomRules_OtherDocumentShapes: a rules file may also be one rule object or the {"rules": …} envelope
// the validate endpoint accepts (both used to fail the whole file).
func TestLoadCustomRules_OtherDocumentShapes(t *testing.T) {
	for _, content := range []string{
		`{"id": "one", "nameMatch": "x"}`,
		`{"rules": [{"id": "one", "nameMatch": "x"}]}`,
		`{"rules": {"id": "one", "nameMatch": "x"}}`,
	} {
		path := filepath.Join(t.TempDir(), "rules.json")
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		rules, warnings, err := LoadCustomRules(path)
		if err != nil || len(rules) != 1 || rules[0].ID() != "one" || len(warnings) != 0 {
			t.Errorf("%s: rules %d, warnings %v, err %v", content, len(rules), warnings, err)
		}
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
