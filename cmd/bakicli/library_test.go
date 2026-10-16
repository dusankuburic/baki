package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// libraryFlow has a subflow Login whose body is the same in every flow it is
// written into, and one of its own.
func libraryFlow(own string) string {
	return `#Region "Login"
    Display.ShowMessageBox Message: $'''Logging in''' Icon: Display.Icon.None
    SET Attempts TO 0
    SET User TO $'''svc'''
    SET Done TO 1
#EndRegion
#Region "` + own + `"
    SET X TO 1
#EndRegion
`
}

func writeLibraryFlows(t *testing.T) (a, b string) {
	t.Helper()
	dir := t.TempDir()
	a, b = filepath.Join(dir, "Invoices.txt"), filepath.Join(dir, "Orders.txt")
	for path, own := range map[string]string{a: "Bill", b: "Ship"} {
		if err := os.WriteFile(path, []byte(libraryFlow(own)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

type multiReportJSON struct {
	Flows []struct {
		Path     string `json:"path"`
		FlowName string `json:"flowName"`
		Stats    struct {
			Errors int `json:"errors"`
		} `json:"stats"`
	} `json:"flows"`
	Library *struct {
		Flows    int `json:"flows"`
		Findings []struct {
			RuleID      string `json:"ruleId"`
			Stage       string `json:"stage"`
			FlowName    string `json:"flowName"`
			SubflowName string `json:"subflowName"`
			Fingerprint string `json:"fingerprint"`
		} `json:"findings"`
	} `json:"library"`
}

// TestCLI_SeveralFlowsRunLibraryRules: two path arguments are two flows
// (bakicli used to analyze the first and drop the rest silently); their
// cross-flow findings come in "library" and never touch the exit code.
func TestCLI_SeveralFlowsRunLibraryRules(t *testing.T) {
	a, b := writeLibraryFlows(t)
	side := filepath.Join(t.TempDir(), "report.json")
	// -rules names only the library rule: no per-flow findings, so the exit
	// code shows what the library findings contribute (nothing).
	stdout, stderr, code := runBakicli(t, "-config", "", "-rules", "duplicate-subflow-across-flows", "-fail-on", "info",
		"-format", "json", "-report-json", side, a, b)
	if code != 0 {
		t.Errorf("library findings must never gate: exit %d (stderr=%s)", code, stderr)
	}
	var rep multiReportJSON
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatalf("%v\n%s", err, stdout)
	}
	if len(rep.Flows) != 2 || rep.Flows[0].Path != a || rep.Flows[1].FlowName != "Orders" {
		t.Errorf("flows = %+v", rep.Flows)
	}
	if rep.Library == nil || rep.Library.Flows != 2 || len(rep.Library.Findings) != 2 {
		t.Fatalf("library = %+v", rep.Library)
	}
	for _, f := range rep.Library.Findings {
		if f.RuleID != "duplicate-subflow-across-flows" || f.Stage != "preview" || f.SubflowName != "Login" || f.Fingerprint == "" {
			t.Errorf("library finding = %+v", f)
		}
	}
	if data, _ := os.ReadFile(side); !strings.Contains(string(data), `"library"`) || !strings.Contains(string(data), `"flows"`) {
		t.Errorf("-report-json lacks the several-flows document:\n%s", data)
	}

	// Text: one section per flow, then the library section.
	stdout, _, code = runBakicli(t, "-config", "", "-fail-on", "info", a, b)
	if code != 1 {
		t.Errorf("text with every rule: per-flow findings gate: exit %d", code)
	}
	for _, want := range []string{"== " + a + " ==", "== " + b + " ==", "Library (preview): 2 finding(s) across 2 flows; never gated", "flow: Invoices  subflow: Login", "  also in: Orders (Login)\n"} {
		if !strings.Contains(stdout, want) {
			t.Errorf("text output lacks %q:\n%s", want, stdout)
		}
	}
}

// TestCLI_SamePathTwiceIsOneFlow: a flow named twice, however the path is
// spelled (with ./, relative or absolute, and on Windows in another case), is
// analyzed once. It was two flows: listed twice, and its subflows reported by
// the library rule as duplicated across the two. A folder named twice is one
// flow too, and so a single-flow report, as when it is named once.
func TestCLI_SamePathTwiceIsOneFlow(t *testing.T) {
	a, b := writeLibraryFlows(t)
	dir := filepath.Dir(a)
	sep := string(filepath.Separator)
	spellings := []string{a, dir + sep + "." + sep + filepath.Base(a)}
	if cwd, err := os.Getwd(); err == nil {
		if rel, err := filepath.Rel(cwd, a); err == nil {
			spellings = append(spellings, rel)
		}
	}
	if runtime.GOOS == "windows" {
		spellings = append(spellings, strings.ToUpper(a))
	}
	for _, again := range spellings {
		stdout, stderr, code := runBakicli(t, "-config", "", "-rules", "duplicate-subflow-across-flows", "-format", "json", a, again, b)
		var rep multiReportJSON
		if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
			t.Fatalf("%s: %v\n%s", again, err, stdout)
		}
		if code != 0 || len(rep.Flows) != 2 || rep.Library == nil || rep.Library.Flows != 2 || len(rep.Library.Findings) != 2 {
			t.Errorf("%s given after %s: exit %d, %d flows, library %+v (stderr=%s)", again, a, code, len(rep.Flows), rep.Library, stderr)
		}
		if !strings.Contains(stderr, "warning: "+again+" names the same flow as "+a) {
			t.Errorf("%s: the dropped path is not named on stderr: %s", again, stderr)
		}
	}

	// SARIF renders one flow: two flows exit 2 before any output.
	stdout, stderr, code := runBakicli(t, "-config", "", "-format", "sarif", dir, dir+sep)
	if code == 2 || !strings.Contains(stdout, `"runs"`) {
		t.Errorf("a folder named twice with -format sarif: exit %d (stderr=%s)", code, stderr)
	}
}

// TestDistinctTargets_KeepsFlowsThatDifferOnlyInCase: in a case-sensitive
// directory (Linux; on Windows one with NTFS's per-directory flag, as WSL
// sets) Flow.txt and flow.txt are two flows. Lowercasing every path on
// Windows dropped the second, with only a warning.
func TestDistinctTargets_KeepsFlowsThatDifferOnlyInCase(t *testing.T) {
	dir := t.TempDir()
	if runtime.GOOS == "windows" {
		if out, err := exec.Command("fsutil.exe", "file", "setCaseSensitiveInfo", dir, "enable").CombinedOutput(); err != nil {
			t.Skipf("cannot make a case-sensitive directory: %v (%s)", err, out)
		}
	}
	upper, lower := filepath.Join(dir, "Flow.txt"), filepath.Join(dir, "flow.txt")
	writeFlow(t, upper)
	writeFlow(t, lower)
	a, aerr := os.Stat(upper)
	b, berr := os.Stat(lower)
	if aerr != nil || berr != nil || os.SameFile(a, b) {
		t.Skipf("this directory is not case-sensitive (%v, %v)", aerr, berr)
	}

	if got := distinctTargets([]string{upper, lower, upper}); !slices.Equal(got, []string{upper, lower}) {
		t.Errorf("distinctTargets(Flow.txt, flow.txt, Flow.txt) = %v, want both flows once", got)
	}
}

// TestCLI_SeveralFlowsGateOnEveryFlow: the exit code covers every flow, not
// only the first (a gate that passes on the first flow's findings would pass
// a broken second one).
func TestCLI_SeveralFlowsGateOnEveryFlow(t *testing.T) {
	clean := writeTempFlow(t, cliCleanFlow)
	dirty := writeTempFlow(t, cliTestFlow)
	if _, _, code := runBakicli(t, "-config", "", "-quiet", clean, dirty); code != 1 {
		t.Errorf("the second flow's error must fail the gate: exit %d", code)
	}
	base := filepath.Join(t.TempDir(), "baseline.json")
	if _, stderr, code := runBakicli(t, "-config", "", "-update-baseline", base, clean, dirty); code != 0 {
		t.Fatalf("update-baseline: exit %d %s", code, stderr)
	}
	stdout, _, code := runBakicli(t, "-config", "", "-baseline", base, clean, dirty)
	if code != 0 || !strings.Contains(stdout, "baseline drift: 0 new") {
		t.Errorf("a baseline of both flows: exit %d\n%s", code, stdout)
	}
	if _, stderr, code := runBakicli(t, "-config", "", "-format", "sarif", clean, dirty); code != 2 || !strings.Contains(stderr, "renders one flow") {
		t.Errorf("sarif with two flows: exit %d stderr=%s", code, stderr)
	}
}

// TestCLI_OneFlowHasNoLibrary: a single flow keeps the one-report JSON shape.
func TestCLI_OneFlowHasNoLibrary(t *testing.T) {
	a, _ := writeLibraryFlows(t)
	stdout, _, _ := runBakicli(t, "-config", "", "-format", "json", a)
	var rep map[string]any
	if err := json.Unmarshal([]byte(stdout), &rep); err != nil {
		t.Fatal(err)
	}
	if _, ok := rep["library"]; ok {
		t.Error("one flow: no library key")
	}
	if _, ok := rep["findings"]; !ok {
		t.Error("one flow: the report shape changed")
	}

	// Disabled in the config: no library pass.
	dir := t.TempDir()
	cfg := writeConfig(t, dir, map[string]any{"ruleConfig": map[string]any{"duplicate-subflow-across-flows": map[string]any{"enabled": false}}})
	a, b := writeLibraryFlows(t)
	stdout, _, _ = runBakicli(t, "-config", cfg, "-format", "json", a, b)
	var multi multiReportJSON
	if err := json.Unmarshal([]byte(stdout), &multi); err != nil || multi.Library != nil || len(multi.Flows) != 2 {
		t.Errorf("library rule disabled: err %v library %+v flows %d", err, multi.Library, len(multi.Flows))
	}
}

// TestCLI_DiffReportsRefusesSeveralFlows: the several-flows JSON decodes as an
// empty report, so diff-reports -fail-on-new over it found nothing added and
// passed whatever changed; it exits 2 instead.
func TestCLI_DiffReportsRefusesSeveralFlows(t *testing.T) {
	clean, dirty := writeTempFlow(t, cliCleanFlow), writeTempFlow(t, cliCleanFlow)
	dir := t.TempDir()
	oldJSON, newJSON := filepath.Join(dir, "old.json"), filepath.Join(dir, "new.json")
	runBakicli(t, "-config", "", "-report-json", oldJSON, clean, dirty)
	if err := os.WriteFile(dirty, []byte(cliTestFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	runBakicli(t, "-config", "", "-report-json", newJSON, clean, dirty)
	_, stderr, code := runBakicli(t, "diff-reports", "-fail-on-new", oldJSON, newJSON)
	if code != 2 || !strings.Contains(stderr, "several-flows report") {
		t.Errorf("diff-reports over several-flows reports: exit %d, want 2 (stderr=%s)", code, stderr)
	}
	if _, stderr, code := runBakicli(t, "report", "-endpoint", "http://127.0.0.1:1", "-secret", "s", newJSON); code != 2 || !strings.Contains(stderr, "several-flows report") {
		t.Errorf("report with a several-flows report.json: exit %d (stderr=%s)", code, stderr)
	}
}
