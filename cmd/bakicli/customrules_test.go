package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-core/analyzer"
)

// setFlow trips a SET-matching custom rule and nothing at error severity.
const setFlow = "#Region \"Main\"\n    SET Token TO 'abc'\n#EndRegion\n"

func writeConfig(t *testing.T, dir string, cfg map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".bakirc.json")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCLI_CustomRulesTolerantReads (ADR 0015 read paths): an unknown field in
// a -custom-rules file or an inline rule is a warning naming it, and the rule
// still gates; so are a missing id and a built-in id.
func TestCLI_CustomRulesTolerantReads(t *testing.T) {
	dir := t.TempDir()
	flow := filepath.Join(dir, "Main.txt")
	if err := os.WriteFile(flow, []byte(setFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	file := writeRulesFixture(t, `[{"id":"acme-no-set","name":"No SET","severity":"error","rawTypeMatch":"^SET$","owner":"team-a"}]`)
	stdout, stderr, code := runBakicli(t, "-config", "", "-custom-rules", file, "-quiet", flow)
	if code != 1 {
		t.Errorf("the file's rule must still gate: exit %d\n%s%s", code, stdout, stderr)
	}
	if !strings.Contains(stderr, `warning`) || !strings.Contains(stderr, `unknown field "owner" ignored`) || !strings.Contains(stderr, `"acme-no-set"`) {
		t.Errorf("unknown field not reported: %s", stderr)
	}

	cfg := writeConfig(t, dir, map[string]any{"customRulesInline": []map[string]any{
		{"id": "acme-no-set", "name": "No SET", "severity": "error", "rawTypeMatch": "^SET$", "nameMatches": "typo"},
		{"name": "anonymous", "severity": "info", "rawTypeMatch": "^SET$"},
	}})
	_, stderr, code = runBakicli(t, "-config", cfg, "-quiet", flow)
	if code != 1 {
		t.Errorf("the inline rule must still gate: exit %d (%s)", code, stderr)
	}
	for _, want := range []string{`unknown field "nameMatches" ignored`, "has no id"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q: %s", want, stderr)
		}
	}
}

// TestCLI_CustomRulesTooNewExit2: a rule of a kind or schemaVersion this
// bakicli does not run is refused (running it as a block rule matches the
// wrong blocks; skipping it weakens the gate), in a file and inline.
func TestCLI_CustomRulesTooNewExit2(t *testing.T) {
	dir := t.TempDir()
	flow := writeTempFlow(t, setFlow)
	file := writeRulesFixture(t, `[{"id":"ok","name":"x","rawTypeMatch":"^SET$"},{"schemaVersion":2,"id":"v2","name":"y","rawTypeMatch":"^SET$"}]`)
	stdout, stderr, code := runBakicli(t, "-config", "", "-format", "sarif", "-custom-rules", file, flow)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "needs a newer bakicli") || !strings.Contains(stderr, `"v2"`) {
		t.Errorf("file v2 rule: exit %d stdout %d bytes stderr=%s", code, len(stdout), stderr)
	}

	cfg := writeConfig(t, dir, map[string]any{"customRulesInline": []map[string]any{
		{"kind": "flow", "id": "needs-retry", "name": "x", "severity": "error"},
	}})
	_, stderr, code = runBakicli(t, "-config", cfg, flow)
	if code != 2 || !strings.Contains(stderr, `unknown kind "flow"`) || !strings.Contains(stderr, "needs a newer bakicli") {
		t.Errorf("inline flow kind: exit %d stderr=%s", code, stderr)
	}
	for _, sub := range [][]string{{"fix", "-config", cfg, flow}, {"suppressions", "-config", cfg, flow}} {
		if _, stderr, code := runBakicli(t, sub...); code != 2 {
			t.Errorf("%s: exit %d, want 2 (stderr=%s)", sub[0], code, stderr)
		}
	}
	// watch runs until killed, so bound it: it must refuse before its first run.
	if out := runBakicliTimeout(t, 5000, "watch", "-config", cfg, flow); !strings.Contains(out, "needs a newer bakicli") || strings.Contains(out, "gate ") {
		t.Errorf("watch must refuse the rule before analysing:\n%s", out)
	}

	// A newer release's rule whose field has another JSON type is still "newer", not a type error.
	retyped := writeRulesFixture(t, `[{"id":"ok","name":"x","rawTypeMatch":"^SET$"},{"schemaVersion":2,"id":"v2","nameMatch":["^Set"]}]`)
	_, stderr, code = runBakicli(t, "-config", "", "-custom-rules", retyped, flow)
	if code != 2 || !strings.Contains(stderr, "needs a newer bakicli") || !strings.Contains(stderr, `"v2"`) {
		t.Errorf("retyped v2 rule: exit %d stderr=%s", code, stderr)
	}

	// A negative schemaVersion is not "newer": the rule is invalid, skipped with a warning.
	negative := writeRulesFixture(t, `[{"schemaVersion":-1,"id":"neg","name":"x","severity":"error","rawTypeMatch":"^SET$"}]`)
	_, stderr, code = runBakicli(t, "-config", "", "-custom-rules", negative, flow)
	if code != 0 || !strings.Contains(stderr, `(rule "neg") skipped`) || strings.Contains(stderr, "newer bakicli") {
		t.Errorf("negative schemaVersion: exit %d stderr=%s", code, stderr)
	}
}

// TestCLI_InlineRetypedNewerRule: a .bakirc.json whose inline rule is a newer
// release's and gives a field another JSON type is read like the same rule in
// a -custom-rules file: that rule needs a newer bakicli (exit 2, ADR 0015) and
// the rest of the config is still read and checked. The config used to fail as
// a whole ("invalid config JSON") without naming the rule. A JSON type error in
// a rule of this version still fails the config as a whole.
func TestCLI_InlineRetypedNewerRule(t *testing.T) {
	flow := writeTempFlow(t, setFlow)
	cfg := writeConfig(t, t.TempDir(), map[string]any{"failOn": "error", "customRulesInline": []map[string]any{
		{"id": "ok", "name": "x", "rawTypeMatch": "^SET$", "owner": "team-a"},
		{"schemaVersion": 2, "id": "v2", "nameMatch": []string{"^Set"}},
	}})
	stdout, stderr, code := runBakicli(t, "-config", cfg, "-format", "sarif", flow)
	if code != 2 || stdout != "" {
		t.Errorf("exit %d, stdout %d bytes, want 2 and no report (stderr=%s)", code, len(stdout), stderr)
	}
	for _, want := range []string{`entry 1 (rule "v2")`, newerBakicliHint, `entry 0 (rule "ok"): custom rule "ok": unknown field "owner" ignored`,
		"1 custom rule(s) need a newer bakicli"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("stderr lacks %q:\n%s", want, stderr)
		}
	}
	if strings.Contains(stderr, "invalid config JSON") {
		t.Errorf("the config failed as a whole:\n%s", stderr)
	}
	for _, sub := range [][]string{{"fix", "-config", cfg, flow}, {"suppressions", "-config", cfg, flow}} {
		if _, stderr, code := runBakicli(t, sub...); code != 2 || !strings.Contains(stderr, newerBakicliHint) {
			t.Errorf("%s: exit %d, want 2 naming the rule (stderr=%s)", sub[0], code, stderr)
		}
	}

	typed := writeConfig(t, t.TempDir(), map[string]any{"customRulesInline": []map[string]any{
		{"id": "ok", "name": "x", "rawTypeMatch": "^SET$"},
		{"id": "sev", "name": "y", "severity": 3, "rawTypeMatch": "^SET$"},
	}})
	_, stderr, code = runBakicli(t, "-config", typed, flow)
	if code != 2 || !strings.Contains(stderr, "invalid config JSON") || !strings.Contains(stderr, "customRulesInline.severity") ||
		strings.Contains(stderr, newerBakicliHint) {
		t.Errorf("a type error in a version-1 rule: exit %d stderr=%s", code, stderr)
	}
}

// TestInlineRules_FailsAsBefore: apart from a newer release's rule, a config
// whose customRulesInline does not decode fails with the error it had when the
// field was a []CustomRuleConfig.
func TestInlineRules_FailsAsBefore(t *testing.T) {
	type oldConfig struct {
		FailOn            string                      `json:"failOn"`
		CustomRulesInline []analyzer.CustomRuleConfig `json:"customRulesInline"`
	}
	type newConfig struct {
		FailOn            string      `json:"failOn"`
		CustomRulesInline inlineRules `json:"customRulesInline"`
	}
	for _, doc := range []string{
		`{"customRulesInline": {"id": "x"}}`,
		`{"customRulesInline": ["x"]}`,
		`{"customRulesInline": [{"id": "ok"}, {"id": "sev", "severity": 3}]}`,
		`{"customRulesInline": [{"id": "neg", "schemaVersion": -1, "nameMatch": ["^Set"]}]}`,
		`{"failOn": 1, "customRulesInline": [{"id": "ok"}]}`,
	} {
		var o oldConfig
		var n newConfig
		oerr, nerr := json.Unmarshal([]byte(doc), &o), json.Unmarshal([]byte(doc), &n)
		if oerr == nil || nerr == nil || strings.ReplaceAll(nerr.Error(), "newConfig", "oldConfig") != oerr.Error() {
			t.Errorf("%s:\n new: %v\n old: %v", doc, nerr, oerr)
		}
	}
	var n newConfig
	if err := json.Unmarshal([]byte(`{"customRulesInline": null}`), &n); err != nil || n.CustomRulesInline != nil {
		t.Errorf("null: %v, %v", n.CustomRulesInline, err)
	}
}

// TestCLI_CustomRulesNamedFileMustExist: a stale -custom-rules path (or the
// config's customRules) used to run silently without the org's rules.
func TestCLI_CustomRulesNamedFileMustExist(t *testing.T) {
	dir := t.TempDir()
	flow := writeTempFlow(t, setFlow)
	missing := filepath.Join(dir, "nope.json")
	stdout, stderr, code := runBakicli(t, "-config", "", "-format", "sarif", "-custom-rules", missing, flow)
	if code != 2 || stdout != "" || !strings.Contains(stderr, "does not exist") {
		t.Errorf("-custom-rules missing: exit %d stdout %d bytes stderr=%s", code, len(stdout), stderr)
	}
	cfg := writeConfig(t, dir, map[string]any{"customRules": missing})
	if _, stderr, code := runBakicli(t, "-config", cfg, flow); code != 2 || !strings.Contains(stderr, "does not exist") {
		t.Errorf("config customRules missing: exit %d stderr=%s", code, stderr)
	}
}

// TestRulesTestCmd_Strict: `rules test` checks like the API's test endpoint:
// an unknown field or a built-in id is an error (exit 2, named); a rule without
// a matcher is a warning and runs.
func TestRulesTestCmd_Strict(t *testing.T) {
	flow := writeTempFlow(t, rulesTestFlow)
	for name, c := range map[string]struct{ rules, want string }{
		"unknown field": {`[{"id":"no-label","name":"x","rawTypeMatch":"Labels\\.","nameMatches":"y"}]`, `unknown field "nameMatches"`},
		"built-in id":   {`[{"id":"unhandled-error","name":"x","rawTypeMatch":"Labels\\."}]`, "built-in rule's id"},
		"newer kind":    {`{"kind":"flow","id":"f","name":"x"}`, "needs a newer bakicli"},
		"bad type":      {`[{"id":"a","name":"x","severity":3}]`, `entry 0 (id "a") invalid`},
	} {
		_, stderr, code := runBakicli(t, "rules", "test", writeRulesFixture(t, c.rules), flow)
		if code != 2 || !strings.Contains(stderr, c.want) {
			t.Errorf("%s: exit %d stderr=%s, want 2 and %q", name, code, stderr, c.want)
		}
	}
	stdout, stderr, code := runBakicli(t, "rules", "test", writeRulesFixture(t, `{"rules":[{"id":"every-block","name":"x"}]}`), flow)
	if code != 0 || !strings.Contains(stderr, "matches every block") || !strings.Contains(stdout, "1 finding(s)") {
		t.Errorf("matcher-less rule: exit %d stdout=%s stderr=%s", code, stdout, stderr)
	}
}

// TestRulesLint covers `bakicli rules lint`: every problem listed (warnings
// too), exit 0 clean, 1 on errors, 2 when the file cannot be read; a
// .bakirc.json's inline rules and customRules file are checked.
func TestRulesLint(t *testing.T) {
	clean := writeRulesFixture(t, `[{"kind":"block","schemaVersion":1,"id":"acme-no-set","name":"No SET","severity":"error","rawTypeMatch":"^SET$"}]`)
	stdout, _, code := runBakicli(t, "rules", "lint", clean)
	if code != 0 || !strings.Contains(stdout, "1 rule(s), 0 error(s), 0 warning(s)") {
		t.Errorf("clean: exit %d\n%s", code, stdout)
	}

	dirty := writeRulesFixture(t, `[
		{"id":"a","name":"x","rawTypeMatch":"^SET$","nameMatches":"typo"},
		{"id":"b","name":"x","nameMatch":"*["},
		{"kind":"flow","id":"c","name":"x"},
		{"id":"d","name":"no matcher"},
		{"id":"e","name":"x","severity":3}
	]`)
	stdout, _, code = runBakicli(t, "rules", "lint", dirty)
	if code != 1 {
		t.Errorf("dirty: exit %d, want 1", code)
	}
	for _, want := range []string{
		"entry 0 (rule a): error UNKNOWN_FIELD nameMatches",
		"entry 1 (rule b): error INVALID_PATTERN nameMatch",
		"entry 2 (rule c): error UNKNOWN_KIND kind",
		"needs a newer bakicli",
		"entry 3 (rule d): warning NO_MATCHER",
		"entry 4 (rule e): error INVALID_JSON severity",
		"5 rule(s), 4 error(s), 1 warning(s)",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("lint output lacks %q:\n%s", want, stdout)
		}
	}

	stdout, _, code = runBakicli(t, "rules", "lint", "-format", "json", dirty)
	var res struct {
		Rules, Errors, Warnings int
		Problems                []struct{ File, ID, Field, Reason, Severity, Message string }
	}
	if err := json.Unmarshal([]byte(stdout), &res); err != nil || code != 1 {
		t.Fatalf("json: exit %d err %v\n%s", code, err, stdout)
	}
	if res.Rules != 5 || res.Errors != 4 || res.Warnings != 1 || len(res.Problems) != 5 || res.Problems[3].Reason != "NO_MATCHER" || res.Problems[3].Severity != "warning" {
		t.Errorf("json result = %+v", res)
	}

	// Warnings alone pass.
	if _, _, code := runBakicli(t, "rules", "lint", writeRulesFixture(t, `{"id":"d","name":"no matcher"}`)); code != 0 {
		t.Errorf("warnings only: exit %d, want 0", code)
	}

	dir := t.TempDir()
	cfg := writeConfig(t, dir, map[string]any{
		"failOn":            "error",
		"customRules":       dirty,
		"customRulesInline": []any{map[string]any{"id": "inline-ok", "name": "x", "rawTypeMatch": "^SET$"}, map[string]any{"id": "", "name": "x", "rawTypeMatch": "^SET$"}},
	})
	stdout, _, code = runBakicli(t, "rules", "lint", cfg)
	if code != 1 || !strings.Contains(stdout, ".bakirc.json: entry 1 (rule (no id)): error MISSING_ID") ||
		!strings.Contains(stdout, "rules.json: entry 0 (rule a)") || !strings.Contains(stdout, "7 rule(s), 5 error(s), 1 warning(s)") {
		t.Errorf("config: exit %d\n%s", code, stdout)
	}

	if _, _, code := runBakicli(t, "rules", "lint", filepath.Join(dir, "missing.json")); code != 2 {
		t.Errorf("missing file: exit %d, want 2", code)
	}
	if _, _, code := runBakicli(t, "rules", "lint", writeConfig(t, t.TempDir(), map[string]any{"customRules": filepath.Join(dir, "gone.json")})); code != 2 {
		t.Errorf("config naming a missing customRules file: exit %d, want 2", code)
	}
	stdout, _, code = runBakicli(t, "rules", "lint", writeRulesFixture(t, `{"not":"rules"}`))
	if code != 1 || !strings.Contains(stdout, "error INVALID_JSON") {
		t.Errorf("not a rules document: exit %d\n%s", code, stdout)
	}
}

// TestRulesLint_ConfigWithoutCustomRules: a .bakirc.json with no custom rules
// (what `bakicli init` writes) lints clean; it used to be "not a custom-rule
// JSON array or object", exit 1.
func TestRulesLint_ConfigWithoutCustomRules(t *testing.T) {
	dir := t.TempDir()
	initCfg := filepath.Join(dir, "init.json")
	if _, stderr, code := runBakicli(t, "init", "-o", initCfg); code != 0 {
		t.Fatalf("init: exit %d %s", code, stderr)
	}
	other := filepath.Join(dir, "ci.json")
	if err := os.WriteFile(other, []byte(`{"rules":"hardcoded-credential","policy":"p.json"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		writeConfig(t, t.TempDir(), map[string]any{"failOn": "error", "format": "sarif"}),
		writeConfig(t, t.TempDir(), map[string]any{}),
		initCfg,
		other,
	} {
		stdout, _, code := runBakicli(t, "rules", "lint", path)
		if code != 0 || !strings.Contains(stdout, "0 rule(s), 0 error(s), 0 warning(s)") {
			t.Errorf("%s: exit %d\n%s", path, code, stdout)
		}
	}
	// A "rules" array is a rules envelope, wherever it sits.
	envelope := writeRulesFixture(t, `{"failOn":"error","rules":[{"id":"x","name":"x","nameMatch":"*["}]}`)
	if stdout, _, code := runBakicli(t, "rules", "lint", envelope); code != 1 || !strings.Contains(stdout, "INVALID_PATTERN") {
		t.Errorf("envelope: exit %d\n%s", code, stdout)
	}
}
