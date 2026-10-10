package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
)

func writeBaselineFixture(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "baseline.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadBaseline_Versions: version 1 (a bare array) and version 2 (an object
// with keys and a rule set) both read; a newer version is refused as such,
// before its other fields are looked at; an object without version 2 is
// invalid.
func TestLoadBaseline_Versions(t *testing.T) {
	bl, err := loadBaseline(writeBaselineFixture(t, `["r1:aaaa","r2:bbbb"]`))
	if err != nil || !bl.Found || len(bl.Keys) != 2 || bl.RuleSet != nil {
		t.Errorf("v1: %+v err=%v", bl, err)
	}

	rs := analyzer.NewRuleSet([]models.RuleRef{{ID: "unhandled-error"}, {ID: "x", Stage: "preview"}})
	raw, _ := json.Marshal(map[string]any{"schemaVersion": 2, "keys": []string{"r1:aaaa"}, "ruleSet": rs, "writtenBy": "a later bakicli"})
	bl, err = loadBaseline(writeBaselineFixture(t, "  \n"+string(raw)))
	if err != nil || !bl.Found || len(bl.Keys) != 1 || bl.Keys[0] != "r1:aaaa" {
		t.Fatalf("v2: %+v err=%v", bl, err)
	}
	if bl.RuleSet == nil || bl.RuleSet.Digest != rs.Digest || len(bl.RuleSet.Rules) != 2 {
		t.Errorf("v2 rule set = %+v, want %+v", bl.RuleSet, rs)
	}

	for name, c := range map[string]struct{ content, want string }{
		"newer":           {`{"schemaVersion":3,"keys":{"changed":"shape"}}`, "written by a newer bakicli"},
		"no version":      {`{"keys":["a"]}`, `needs "schemaVersion": 2`},
		"version 1 obj":   {`{"schemaVersion":1,"keys":["a"]}`, `needs "schemaVersion": 2`},
		"version string":  {`{"schemaVersion":"2"}`, "invalid baseline JSON"},
		"bad keys":        {`{"schemaVersion":2,"keys":"a"}`, "invalid baseline JSON"},
		"not json":        {`{not json`, "invalid baseline JSON"},
		"array of object": {`[{"a":1}]`, "invalid baseline JSON"},
	} {
		if _, err := loadBaseline(writeBaselineFixture(t, c.content)); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err %v, want %q", name, err, c.want)
		}
	}
}

// TestCLI_InvalidBaselineExits2: analyze used to exit 1 (after writing the
// report) and watch 2; both exit 2 now, and analyze writes nothing.
func TestCLI_InvalidBaselineExits2(t *testing.T) {
	flow := writeTempFlow(t, cliTestFlow)
	for name, content := range map[string]string{
		"invalid": "{not json",
		"newer":   `{"schemaVersion":3}`,
	} {
		bad := writeBaselineFixture(t, content)
		stdout, stderr, code := runBakicli(t, "-config", "", "-format", "sarif", "-baseline", bad, flow)
		if code != 2 || stdout != "" {
			t.Errorf("analyze %s: exit %d, %d bytes on stdout; want 2 and none (stderr=%s)", name, code, len(stdout), stderr)
		}
		out := runBakicliTimeout(t, 2000, "watch", "-config", "", "-baseline", bad, flow)
		if !strings.Contains(out, "baseline") || strings.Contains(out, "gate PASS") || strings.Contains(out, "gate FAIL") {
			t.Errorf("watch %s must refuse the baseline before analysing:\n%s", name, out)
		}
	}
	_, stderr, _ := runBakicli(t, "-config", "", "-baseline", writeBaselineFixture(t, `{"schemaVersion":3}`), flow)
	if !strings.Contains(stderr, "newer bakicli") {
		t.Errorf("a newer baseline must say so: %s", stderr)
	}
}

// TestCLI_V2BaselineCountsNewerRules: with a version 2 baseline, the run says
// how many new findings come from rules the baseline did not have. They still
// gate (D5: only preview is exempt).
func TestCLI_V2BaselineCountsNewerRules(t *testing.T) {
	var without []analyzer.Rule
	for _, r := range analyzer.AllRules() {
		if r.ID() != "hardcoded-credential" {
			without = append(without, r)
		}
	}
	raw, _ := json.Marshal(map[string]any{"schemaVersion": 2, "keys": []string{}, "ruleSet": analyzer.RecordRuleSet(without)})
	base := writeBaselineFixture(t, string(raw))
	flow := writeTempFlow(t, cliTestFlow)

	stdout, stderr, code := runBakicli(t, "-config", "", "-baseline", base, flow)
	if code != 1 {
		t.Errorf("a new rule's error still gates against an older baseline: exit %d", code)
	}
	if !strings.Contains(stdout, "1 findings from rules newer than this baseline\n") {
		t.Errorf("text: newer-rules line missing from stdout:\n%s\nstderr=%s", stdout, stderr)
	}
	stdout, stderr, _ = runBakicli(t, "-config", "", "-format", "json", "-baseline", base, flow)
	parsesAs(t, "json", stdout)
	if !strings.Contains(stderr, "1 findings from rules newer than this baseline") {
		t.Errorf("json: the line belongs on stderr: %s", stderr)
	}

	// A baseline taken with every current rule names none as newer; a v1 file
	// prints no such line.
	raw, _ = json.Marshal(map[string]any{"schemaVersion": 2, "keys": []string{}, "ruleSet": analyzer.RecordRuleSet(analyzer.AllRules())})
	stdout, _, _ = runBakicli(t, "-config", "", "-baseline", writeBaselineFixture(t, string(raw)), flow)
	if !strings.Contains(stdout, "0 findings from rules newer than this baseline") {
		t.Errorf("same rule set: %s", stdout)
	}
	stdout, _, _ = runBakicli(t, "-config", "", "-baseline", writeBaselineFixture(t, "[]"), flow)
	if strings.Contains(stdout, "newer than this baseline") {
		t.Errorf("a v1 baseline has no rule set to compare: %s", stdout)
	}
}

// TestWriteBaseline_SeveralFlowsAreScoped: the file stays version 1 (a bare
// array); one flow's keys are written as they are, several flows' keys each
// under its flow's path, so keysInScope hands every flow only its own keys;
// "[]" when empty.
func TestWriteBaseline_SeveralFlowsAreScoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "baseline.json")
	a := &models.AnalysisReport{Findings: []models.Finding{{RuleID: "r1", Fingerprint: "r1:a"}, {RuleID: "r1", Fingerprint: "r1:a"}}}
	b := &models.AnalysisReport{Findings: []models.Finding{{RuleID: "r1", Fingerprint: "r1:a"}, {RuleID: "r2", Fingerprint: "r2:b"}}}
	if got := baselineKeys([]flowRun{{target: "./flows/a.txt", report: a}}); len(got) != 1 || got[0] != "r1:a" {
		t.Errorf("one flow: keys %v, want unscoped [r1:a]", got)
	}
	runs := []flowRun{{target: "./flows/a.txt", report: a}, {target: filepath.Join("flows", "b.txt"), report: b}}
	if err := writeBaseline(path, baselineKeys(runs)); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if string(data) != "[\n  \"flows/a.txt#r1:a\",\n  \"flows/b.txt#r1:a\",\n  \"flows/b.txt#r2:b\"\n]" {
		t.Errorf("baseline file = %s", data)
	}
	bl, err := loadBaseline(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := keysInScope(bl.Keys, "flows/a.txt"); len(got) != 1 || got[0] != "r1:a" {
		t.Errorf("a's keys = %v", got)
	}
	if got := keysInScope(bl.Keys, "flows/b.txt"); len(got) != 2 || got[1] != "r2:b" {
		t.Errorf("b's keys = %v", got)
	}
	if got := keysInScope(bl.Keys, "flows/c.txt"); len(got) != 0 {
		t.Errorf("a flow the baseline never saw: %v", got)
	}
	if err := writeBaseline(path, baselineKeys([]flowRun{{target: "x", report: &models.AnalysisReport{}}})); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "[]" {
		t.Errorf("empty baseline = %s, want []", data)
	}
}

// TestCLI_SeveralFlowsBaselineIsPerFlow: two flows from one template carry
// the same fingerprint for the same finding, so a finding baselined in one
// flow must not hide the same new finding in the other.
func TestCLI_SeveralFlowsBaselineIsPerFlow(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a.txt"), filepath.Join(dir, "b.txt")
	write := func(path, content string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(a, cliTestFlow)
	write(b, cliCleanFlow)
	base := filepath.Join(dir, "baseline.json")
	if _, stderr, code := runBakicli(t, "-config", "", "-update-baseline", base, a, b); code != 0 {
		t.Fatalf("update-baseline: exit %d %s", code, stderr)
	}
	write(b, cliTestFlow)
	stdout, _, code := runBakicli(t, "-config", "", "-baseline", base, a, b)
	if code != 1 || !strings.Contains(stdout, "baseline drift: 1 new (1 errors") {
		t.Errorf("b's new credential is hidden by a's baselined one: exit %d\n%s", code, stdout)
	}
}

// TestCLI_UpdateBaselineRewritesABrokenFile: -update-baseline wins over
// -baseline, so a corrupt baseline can be rewritten in place.
func TestCLI_UpdateBaselineRewritesABrokenFile(t *testing.T) {
	broken := writeBaselineFixture(t, "{not json")
	if _, stderr, code := runBakicli(t, "-config", "", "-baseline", broken, "-update-baseline", broken, writeTempFlow(t, cliTestFlow)); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if bl, err := loadBaseline(broken); err != nil || len(bl.Keys) != 1 {
		t.Errorf("rewritten baseline: %+v err=%v", bl, err)
	}
}
