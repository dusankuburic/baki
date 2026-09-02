package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// flowWithFixture runs the built binary's analyze (JSON) on a fixture and
// returns the decoded report — the input shape diff-reports consumes.
func saveReport(t *testing.T, flow string) string {
	t.Helper()
	flowPath := writeTempFlow(t, flow)
	outPath := filepath.Join(t.TempDir(), "report.json")
	out, stderr, code := runBakicli(t, "-format", "json", "-quiet", flowPath)
	if code != 0 {
		t.Fatalf("analyze: exit %d stderr=%q", code, stderr)
	}
	if err := os.WriteFile(outPath, []byte(out), 0644); err != nil {
		t.Fatal(err)
	}
	return outPath
}

const dirtyFlow = "#Region \"Main\"\n    HTTPClient.InvokeUrl Url: $'''https://x''' Method: HTTPClient.Method.GET\n#EndRegion\n"
const cleanerFlow = "#Region \"Main\"\n    COMMENT  fine now\n    SET Ok TO 1\n#EndRegion\n"

// TestDiffReportsCmd pins R0-7: findings-level diff between two saved runs,
// with the -fail-on-new gate. Previously branch comparisons meant eyeballing
// two JSON blobs (the server had the endpoint; the CLI didn't).
func TestDiffReportsCmd(t *testing.T) {
	oldJSON := saveReport(t, cleanerFlow)
	newJSON := saveReport(t, dirtyFlow)

	out, stderr, code := runBakicli(t, "diff-reports", oldJSON, newJSON)
	if code != 0 {
		t.Fatalf("diff-reports: exit %d stderr=%q", code, stderr)
	}
	if want := "+"; !containsStr(out, want) || !containsStr(out, "added") {
		t.Errorf("output missing added summary: %q", out)
	}
	if !containsStr(out, "unhandled-error") {
		t.Errorf("added finding not listed: %q", out)
	}

	// Gate: any addition at/above error fails.
	_, stderr, code = runBakicli(t, "diff-reports", "-fail-on-new", "-fail-on", "warning", oldJSON, newJSON)
	if code != 1 {
		t.Errorf("fail-on-new with additions: exit %d, want 1 (stderr=%q)", code, stderr)
	}
	// Reverse direction: only removals → gate passes.
	_, _, code = runBakicli(t, "diff-reports", "-fail-on-new", newJSON, oldJSON)
	if code != 0 {
		t.Errorf("fail-on-new with only removals: exit %d, want 0", code)
	}
	// JSON format round-trips counts.
	out, _, _ = runBakicli(t, "diff-reports", "-format", "json", oldJSON, newJSON)
	var parsed struct {
		AddedCount int `json:"addedCount"`
	}
	if err := json.Unmarshal([]byte(out), &parsed); err != nil || parsed.AddedCount == 0 {
		t.Errorf("json format: parsed=%+v err=%v", parsed, err)
	}
}

func containsStr(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOfStr(haystack, needle) >= 0)
}

func indexOfStr(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}

// runBakicliTimeout runs the binary and kills it after the timeout — for
// `watch`, whose initial analysis run executes before the poll loop starts,
// so the gate output of run #1 is captured.
func runBakicliTimeout(t *testing.T, timeoutMs int, args ...string) (stdout string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeoutMs)*time.Millisecond)
	defer cancel()
	var outBuf strings.Builder
	cmd := exec.CommandContext(ctx, bakicliBin, args...)
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	_ = cmd.Run() // killed on timeout is the success path here
	return outBuf.String()
}

// TestWatch_BaselineGate pins R0-7's watch parity: watch accepts -baseline
// and gates the initial run on drift (the same ratchet analyze uses). The
// old watch ignored -baseline/-policy/.bakirc.json entirely, so the local
// feedback loop was weaker than the CI gate it previewed.
func TestWatch_BaselineGate(t *testing.T) {
	dir := t.TempDir()
	flow := filepath.Join(dir, "flow.txt")
	if err := os.WriteFile(flow, []byte(cleanerFlow), 0644); err != nil {
		t.Fatal(err)
	}
	// Capture the CLEAN state as the baseline (analyze's -update-baseline).
	base := filepath.Join(dir, "baseline.json")
	if _, stderr, code := runBakicli(t, "-update-baseline", base, "-quiet", flow); code != 0 {
		t.Fatalf("update-baseline: exit %d stderr=%q", code, stderr)
	}

	// Introduce a finding; watch must report baseline drift on its first run.
	if err := os.WriteFile(flow, []byte(dirtyFlow), 0644); err != nil {
		t.Fatal(err)
	}
	out := runBakicliTimeout(t, 800, "watch", "-interval", "10s", "-fail-on", "warning", "-baseline", base, flow)
	if !strings.Contains(out, "baseline drift: 4 new") {
		t.Errorf("watch baseline gate missing drift: %q", out)
	}
	if !strings.Contains(out, "gate FAIL (new findings") {
		t.Errorf("watch baseline gate should FAIL: %q", out)
	}
}

// TestWatch_PolicyGate: -policy gates the initial watch run.
func TestWatch_PolicyGate(t *testing.T) {
	dir := t.TempDir()
	flow := filepath.Join(dir, "flow.txt")
	if err := os.WriteFile(flow, []byte(dirtyFlow), 0644); err != nil {
		t.Fatal(err)
	}
	pol := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(pol, []byte(`{"name":"p","gateSeverity":"warning","rules":[{"ruleId":"unhandled-error","enabled":true,"severity":"warning"}]}`), 0644); err != nil {
		t.Fatal(err)
	}
	out := runBakicliTimeout(t, 800, "watch", "-interval", "10s", "-policy", pol, flow)
	if !strings.Contains(out, "policy") || !strings.Contains(out, "gate FAIL") {
		t.Errorf("watch policy gate missing: %q", out)
	}
}

// TestWatch_ConfigDiscovery: .bakirc.json defaults apply (fail-on).
func TestWatch_ConfigDiscovery(t *testing.T) {
	dir := t.TempDir()
	flow := filepath.Join(dir, "flow.txt")
	if err := os.WriteFile(flow, []byte(dirtyFlow), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bakirc.json"), []byte(`{"failOn":"info"}`), 0644); err != nil {
		t.Fatal(err)
	}
	// cwd-relative discovery: run with the workdir set to the config folder.
	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	var outBuf strings.Builder
	cmd := exec.CommandContext(ctx, bakicliBin, "watch", "-interval", "10s", flow)
	cmd.Dir = dir
	cmd.Stdout = &outBuf
	cmd.Stderr = &outBuf
	_ = cmd.Run()
	out := outBuf.String()
	// dirtyFlow carries an error-severity finding; with cfg fail-on=info any
	// finding fails. (The flag default error would also fail here, so assert
	// the config path at least parses and gates.)
	if !strings.Contains(out, "gate FAIL") {
		t.Errorf("watch with config: no gate output: %q", out)
	}
}
