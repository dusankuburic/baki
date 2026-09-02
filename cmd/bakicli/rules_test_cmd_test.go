package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-core/analyzer"
)

const rulesTestFlow = "#Region \"Main\"\n    Labels.ApplyTo Window: $'''x'''\n#EndRegion\n"

func writeRulesFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "rules.json")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestRulesTestCmd_MatchAndNone pins R0-5's `bakicli rules test`: a matching
// rule prints its findings (exit 0); --fail-on-none flips a no-match run to
// exit 1 so CI can assert a fixture still trips the rule.
func TestRulesTestCmd_MatchAndNone(t *testing.T) {
	good := writeRulesFixture(t, `[{"id":"no-label","name":"Labels must be named","severity":"warning","rawTypeMatch":"Labels\\."}]`)
	flow := writeTempFlow(t, rulesTestFlow)

	out, stderr, code := runBakicli(t, "rules", "test", good, flow)
	if code != 0 {
		t.Fatalf("match run: exit %d stderr=%q", code, stderr)
	}
	if !strings.Contains(out, "no-label") || !strings.Contains(out, "1 finding(s)") {
		t.Errorf("match output missing rule/finding: %q", out)
	}
	if !strings.Contains(out, "no-label") || !strings.Contains(out, "1 match(es)") {
		t.Errorf("per-rule count missing: %q", out)
	}

	// No-match: exit 0 by default, 1 with --fail-on-none.
	other := writeTempFlow(t, "#Region \"Main\"\n    COMMENT  nothing to see\n#EndRegion\n")
	_, _, code = runBakicli(t, "rules", "test", good, other)
	if code != 0 {
		t.Errorf("no-match default: exit %d, want 0", code)
	}
	_, stderr, code = runBakicli(t, "rules", "test", "-fail-on-none", good, other)
	if code != 1 {
		t.Errorf("no-match with --fail-on-none: exit %d, want 1 (stderr=%q)", code, stderr)
	}
}

// TestRulesTestCmd_InvalidRuleExits2: authoring feedback is the point — a
// broken entry (bad regex) is fatal with the index/id on stderr.
func TestRulesTestCmd_InvalidRuleExits2(t *testing.T) {
	bad := writeRulesFixture(t, `[
		{"id":"ok","name":"fine","rawTypeMatch":"Labels\\."},
		{"id":"broken","name":"bad regex","nameMatch":"*invalid["}
	]`)
	flow := writeTempFlow(t, rulesTestFlow)
	_, stderr, code := runBakicli(t, "rules", "test", bad, flow)
	if code != 2 {
		t.Fatalf("invalid rule: exit %d, want 2", code)
	}
	if !strings.Contains(stderr, `"broken"`) {
		t.Errorf("stderr missing offending rule id: %q", stderr)
	}
}

// TestRulesTestCmd_SingleObjectShape: one rule object (no array) loads —
// handier while iterating on a single rule.
func TestRulesTestCmd_SingleObjectShape(t *testing.T) {
	one := writeRulesFixture(t, `{"id":"no-label","name":"x","rawTypeMatch":"Labels\\."}`)
	flow := writeTempFlow(t, rulesTestFlow)
	out, _, code := runBakicli(t, "rules", "test", one, flow)
	if code != 0 || !strings.Contains(out, "1 finding(s)") {
		t.Errorf("single-object: exit %d out=%q", code, out)
	}
}

// TestLoadCustomRules_Warnings pins the core half of R0-5: skipped entries
// are REPORTED (index + file + reason) instead of silently dropped.
func TestLoadCustomRules_Warnings(t *testing.T) {
	path := writeRulesFixture(t, `[
		{"id":"ok","name":"fine","rawTypeMatch":"Labels\\."},
		{"id":"bad-autofix","name":"x","autoFix":"not-a-fixer"},
		{"id":"bad-regex","name":"y","nameMatch":"*["}
	]`)
	rules, warnings, err := analyzer.LoadCustomRules(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(rules) != 1 || rules[0].ID() != "ok" {
		t.Errorf("valid rules = %d, want just 'ok'", len(rules))
	}
	if len(warnings) != 2 {
		t.Fatalf("warnings = %d, want 2: %v", len(warnings), warnings)
	}
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "bad-autofix") || !strings.Contains(joined, "bad-regex") {
		t.Errorf("warnings missing offending ids: %v", warnings)
	}
	if !strings.Contains(joined, "rules.json") {
		t.Errorf("warnings missing file context: %v", warnings)
	}
}
