package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestMain builds the bakicli binary once for all binary-level tests, then
// runs the test suite. The binary is placed in a temp dir and cleaned up.
var bakicliBin string

func TestMain(m *testing.M) {
	tmp, err := os.MkdirTemp("", "bakicli-test")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(tmp)

	bakicliBin = filepath.Join(tmp, "bakicli")
	cmd := exec.Command("go", "build", "-o", bakicliBin, ".")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("failed to build bakicli: " + err.Error())
	}

	os.Exit(m.Run())
}

// runBakicli executes the built binary with the given args, returning stdout,
// stderr, and exit code.
func runBakicli(t *testing.T, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bakicliBin, args...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	cmd.Run()
	return outBuf.String(), errBuf.String(), cmd.ProcessState.ExitCode()
}

// writeTempFlow creates a temp .txt file with the given PAD source content.
func writeTempFlow(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "flow-*.txt")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	f.WriteString(content)
	f.Close()
	return f.Name()
}

// A minimal PAD flow with a known finding (hardcoded credential).
const cliTestFlow = `#Region "Main"
Variables.SetVariable Name: %ApiKey% Value: 'AKIAIOSFODNN7EXAMPLE'
#EndRegion
`

// A clean PAD flow with no findings.
const cliCleanFlow = `#Region "Main"
Variables.SetVariable Name: %Greeting% Value: 'hello'
#EndRegion
`

func TestCLI_Version(t *testing.T) {
	stdout, _, code := runBakicli(t, "--version")
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "bakicli") {
		t.Errorf("expected 'bakicli' in version output, got: %s", stdout)
	}
}

func TestCLI_AnalyzeClean_Exit0(t *testing.T) {
	f := writeTempFlow(t, cliCleanFlow)
	stdout, _, code := runBakicli(t, "-format", "json", "-fail-on", "error", f)
	if code != 0 {
		t.Errorf("expected exit 0 for clean flow, got %d (stdout: %s)", code, stdout[:min(200, len(stdout))])
	}
	// Verify stdout is valid JSON
	var v map[string]any
	if err := json.Unmarshal([]byte(stdout), &v); err != nil {
		t.Errorf("stdout is not valid JSON: %v", err)
	}
}

func TestCLI_AnalyzeWithFindings_Exit1(t *testing.T) {
	f := writeTempFlow(t, cliTestFlow)
	_, _, code := runBakicli(t, "-fail-on", "info", f)
	if code != 1 {
		t.Errorf("expected exit 1 for flow with findings at info threshold, got %d", code)
	}
}

func TestCLI_AnalyzeSARIF(t *testing.T) {
	f := writeTempFlow(t, cliTestFlow)
	stdout, _, code := runBakicli(t, "-format", "sarif", "-fail-on", "info", f)
	// SARIF is emitted to stdout BEFORE the gate fires, so stdout should have content
	// even when exit code is 1 (findings at info threshold).
	if code != 1 {
		t.Errorf("expected exit 1 for flow with findings, got %d", code)
	}
	if !strings.Contains(stdout, `"runs"`) {
		t.Errorf("expected SARIF output with runs, got: %s", stdout[:min(200, len(stdout))])
	}
}

func TestCLI_RulesList(t *testing.T) {
	stdout, _, code := runBakicli(t, "rules")
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	if !strings.Contains(stdout, "RULE") || !strings.Contains(stdout, "SEVERITY") {
		t.Errorf("expected rules table, got: %s", stdout[:min(200, len(stdout))])
	}
}

func TestCLI_RulesJSON(t *testing.T) {
	stdout, _, code := runBakicli(t, "rules", "-format", "json")
	if code != 0 {
		t.Errorf("expected exit 0, got %d", code)
	}
	var entries []map[string]any
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
		t.Errorf("expected valid JSON array, got error: %v (stdout: %s)", err, stdout[:min(200, len(stdout))])
	}
	if len(entries) == 0 {
		t.Error("expected non-empty rules array")
	}
	// Verify a known rule is present
	found := false
	for _, e := range entries {
		if e["id"] == "unhandled-error" {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected 'unhandled-error' in rules JSON")
	}
}

func TestCLI_NonexistentFile_Exit2(t *testing.T) {
	_, _, code := runBakicli(t, "/nonexistent/file.txt")
	if code != 2 {
		t.Errorf("expected exit 2 for nonexistent file, got %d", code)
	}
}

func TestCLI_FixDryRun(t *testing.T) {
	f := writeTempFlow(t, cliTestFlow)
	stdout, _, code := runBakicli(t, "fix", f)
	if code != 0 {
		t.Errorf("expected exit 0 for fix dry-run, got %d", code)
	}
	// Dry-run prints the patched source to stdout
	if stdout == "" {
		t.Error("expected non-empty stdout from fix dry-run")
	}
}

// A PAD flow with an extra block (for structural diff testing).
const cliExtraBlockFlow = `#Region "Main"
Variables.SetVariable Name: %Greeting% Value: 'hello'
Variables.SetVariable Name: %Farewell% Value: 'bye'
#EndRegion
`

func TestCLI_DiffFailOnDiff(t *testing.T) {
	old := writeTempFlow(t, cliCleanFlow)
	new := writeTempFlow(t, cliExtraBlockFlow)
	_, _, code := runBakicli(t, "diff", "--fail-on-diff", old, new)
	// The new flow has an extra block → structural change → exit 1.
	if code != 1 {
		t.Errorf("expected exit 1 for diff with structural changes + --fail-on-diff, got %d", code)
	}
}

func TestCLI_DiffNoChanges_Exit0(t *testing.T) {
	f := writeTempFlow(t, cliCleanFlow)
	_, _, code := runBakicli(t, "diff", "--fail-on-diff", f, f)
	if code != 0 {
		t.Errorf("expected exit 0 for identical flows diff, got %d", code)
	}
}

func TestCLI_Init(t *testing.T) {
	dir := t.TempDir()
	outPath := filepath.Join(dir, ".bakirc.json")
	_, _, code := runBakicli(t, "init", "-o", outPath)
	if code != 0 {
		t.Errorf("expected exit 0 for init, got %d", code)
	}
	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatalf("config file not created: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("invalid config JSON: %v", err)
	}
}
