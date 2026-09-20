package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// runBakicliIn is runBakicli with a working directory (SARIF source roots are
// derived from it) and extra environment.
func runBakicliIn(t *testing.T, dir string, env []string, args ...string) (stdout, stderr string, exitCode int) {
	t.Helper()
	cmd := exec.Command(bakicliBin, args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	var outBuf, errBuf strings.Builder
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf
	_ = cmd.Run()
	return outBuf.String(), errBuf.String(), cmd.ProcessState.ExitCode()
}

const ciPolicyJSON = `{"id":"pol-ci","name":"CI policy","gateSeverity":"info",
  "rules":[{"ruleId":"hardcoded-credential","enabled":true}]}`

// TestCLI_HelpListsSubcommandsAndExitCodes: -h is the first thing a CI author
// reads; it used to print the analyze flags and nothing else.
func TestCLI_HelpListsSubcommandsAndExitCodes(t *testing.T) {
	stdout, stderr, code := runBakicli(t, "-h")
	out := stdout + stderr
	if code != 0 {
		t.Errorf("-h exit %d, want 0", code)
	}
	for _, want := range []string{"bakicli fix", "bakicli suppressions", "bakicli report", "Exit codes:", "  2  usage error", "-format): text, json, sarif, junit, csv, html", "-fail-on string"} {
		if !strings.Contains(out, want) {
			t.Errorf("-h output lacks %q:\n%s", want, out)
		}
	}
}

// TestCLI_JSONCarriesPolicyVerdict: the JSON report embeds the -policy result
// (and omits the key without one), and -report-json writes the same document
// from the same run whatever -format is.
func TestCLI_JSONCarriesPolicyVerdict(t *testing.T) {
	dir := t.TempDir()
	flow := writeTempFlow(t, cliTestFlow)
	policy := filepath.Join(dir, "policy.json")
	if err := os.WriteFile(policy, []byte(ciPolicyJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	side := filepath.Join(dir, "report.json")

	stdout, stderr, code := runBakicli(t, "-format", "sarif", "-policy", policy, "-report-json", side, flow)
	if code != 1 {
		t.Fatalf("policy violated: exit %d, want 1 (stderr=%s)", code, stderr)
	}
	var sarif struct {
		Runs []struct {
			Invocations []struct {
				ExecutionSuccessful bool `json:"executionSuccessful"`
			} `json:"invocations"`
			Properties map[string]any `json:"properties"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(stdout), &sarif); err != nil {
		t.Fatalf("sarif: %v", err)
	}
	if sarif.Runs[0].Invocations[0].ExecutionSuccessful || sarif.Runs[0].Properties["policy"] == nil {
		t.Errorf("SARIF must carry the failed policy verdict: %+v", sarif.Runs[0])
	}

	data, err := os.ReadFile(side)
	if err != nil {
		t.Fatalf("-report-json not written: %v", err)
	}
	var rep map[string]any
	if err := json.Unmarshal(data, &rep); err != nil {
		t.Fatal(err)
	}
	pol, _ := rep["policy"].(map[string]any)
	if pol == nil || pol["passed"] != false || pol["policyName"] != "CI policy" {
		t.Errorf("report.json policy = %+v", rep["policy"])
	}
	if _, ok := rep["stats"]; !ok {
		t.Error("report.json lost the AnalysisReport fields")
	}

	stdout, _, _ = runBakicli(t, "-format", "json", flow)
	var plain map[string]any
	if err := json.Unmarshal([]byte(stdout), &plain); err != nil {
		t.Fatal(err)
	}
	if _, ok := plain["policy"]; ok {
		t.Error("no -policy → no policy key")
	}
}

// TestCLI_SARIFURIsAreRepoRelative: analysing ./flows from the checkout root
// must place results at flows/<member> against %SRCROOT% — what GitHub needs
// to annotate a PR — instead of a bare basename.
func TestCLI_SARIFURIsAreRepoRelative(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "flows", "teamA"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "flows", "teamA", "Main.txt"), []byte(cliTestFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code := runBakicliIn(t, root, nil, "-format", "sarif", "-fail-on", "info", "-config", "", "flows")
	if code != 1 {
		t.Fatalf("exit %d want 1: %s", code, stderr)
	}
	var sarif struct {
		Runs []struct {
			AutomationDetails  map[string]string            `json:"automationDetails"`
			OriginalURIBaseIDs map[string]map[string]string `json:"originalUriBaseIds"`
			Results            []struct {
				Locations []struct {
					PhysicalLocation struct {
						ArtifactLocation map[string]string `json:"artifactLocation"`
					} `json:"physicalLocation"`
				} `json:"locations"`
			} `json:"results"`
		} `json:"runs"`
	}
	if err := json.Unmarshal([]byte(stdout), &sarif); err != nil {
		t.Fatal(err)
	}
	run := sarif.Runs[0]
	if run.AutomationDetails["id"] != "pad-analysis" {
		t.Errorf("automationDetails = %v", run.AutomationDetails)
	}
	if got := run.OriginalURIBaseIDs["%SRCROOT%"]["uri"]; !strings.HasPrefix(got, "file:///") || !strings.HasSuffix(got, "/") {
		t.Errorf("originalUriBaseIds[%%SRCROOT%%] = %q", got)
	}
	art := run.Results[0].Locations[0].PhysicalLocation.ArtifactLocation
	if art["uri"] != "flows/teamA/Main.txt" || art["uriBaseId"] != "%SRCROOT%" {
		t.Errorf("artifactLocation = %v, want flows/teamA/Main.txt against %%SRCROOT%%", art)
	}
}

// TestCLI_SuppressionsHonourConfig: a directive for a rule the config disables
// is stale under that config — and not without it. Judging against built-in
// defaults regardless of .bakirc.json made -fail-on-stale gate on false verdicts.
func TestCLI_SuppressionsHonourConfig(t *testing.T) {
	flow := writeTempFlow(t, suppressionFlow)
	cfg := filepath.Join(t.TempDir(), ".bakirc.json")
	if err := os.WriteFile(cfg, []byte(`{"ruleConfig":{"missing-timeout":{"enabled":false}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	staleCount := func(args ...string) int {
		out, stderr, code := runBakicli(t, append([]string{"suppressions", "-format", "json"}, args...)...)
		if code != 0 {
			t.Fatalf("exit %d: %s", code, stderr)
		}
		var entries []map[string]any
		if err := json.Unmarshal([]byte(out), &entries); err != nil {
			t.Fatal(err)
		}
		n := 0
		for _, e := range entries {
			if e["stale"] == true {
				n++
			}
		}
		return n
	}
	if n := staleCount("-config", "", flow); n != 2 {
		t.Errorf("without config: stale = %d, want 2", n)
	}
	if n := staleCount("-config", cfg, flow); n != 3 {
		t.Errorf("with missing-timeout disabled: stale = %d, want 3 (the directive no longer guards anything)", n)
	}
	if _, _, code := runBakicli(t, "suppressions", "-config", filepath.Join(t.TempDir(), "nope", "x.json"), flow); code != 0 {
		t.Errorf("a missing config path is not an error (optional config), got exit %d", code)
	}
}

// TestCLI_FixBackup: -apply -backup keeps the original next to the rewritten
// file; an unchanged file gets no .bak.
func TestCLI_FixBackup(t *testing.T) {
	flow := writeTempFlow(t, cliTestFlow)
	_, stderr, code := runBakicli(t, "fix", "-apply", "-backup", "-unsafe", "-config", "", flow)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	bak, err := os.ReadFile(flow + ".bak")
	if err != nil {
		t.Fatalf("no backup written: %v", err)
	}
	if string(bak) != cliTestFlow {
		t.Errorf("backup content differs from the original:\n%s", bak)
	}
	fixed, _ := os.ReadFile(flow)
	if string(fixed) == cliTestFlow {
		t.Error("expected the flow to be rewritten")
	}
	clean := writeTempFlow(t, cliCleanFlow)
	runBakicli(t, "fix", "-apply", "-backup", "-unsafe", "-config", "", clean)
	if _, err := os.Stat(clean + ".bak"); err == nil {
		t.Error("an unchanged file must not leave a .bak behind")
	}
}

// TestCLI_RulesJSONCarriesOptions: thresholds are part of the rule contract a
// docgen/CI consumer needs; they were absent from the catalog.
func TestCLI_RulesJSONCarriesOptions(t *testing.T) {
	stdout, _, _ := runBakicli(t, "rules", "-format", "json")
	var entries []struct {
		ID      string         `json:"id"`
		Options map[string]any `json:"options"`
	}
	if err := json.Unmarshal([]byte(stdout), &entries); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.ID == "deep-nesting" {
			found = true
			if _, ok := e.Options["maxDepth"]; !ok {
				t.Errorf("deep-nesting options = %v, want maxDepth", e.Options)
			}
		}
	}
	if !found {
		t.Error("deep-nesting missing from the catalog")
	}
}

// seenRequest records what the fake webhook received. The handler runs on the
// server's goroutine while the test reads it, so every access takes the mutex
// (the plain map this used to be was a data race under -race).
type seenRequest struct {
	mu     sync.Mutex
	fields map[string]string
}

func (s *seenRequest) set(key, value string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fields[key] = value
}

func (s *seenRequest) get(key string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fields[key]
}

// ciWebhookServer mirrors internal/api.verifySignature: HMAC-SHA256 over
// "<ts>." + body, constant-time compare, ±5 min timestamp window.
func ciWebhookServer(t *testing.T, secret string, passed bool) (*httptest.Server, *seenRequest) {
	t.Helper()
	seen := &seenRequest{fields: map[string]string{}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen.set("path", r.URL.Path)
		seen.set("body", string(body))
		ts := r.Header.Get("X-Baki-Timestamp")
		sec, err := strconv.ParseInt(ts, 10, 64)
		if err != nil || time.Since(time.Unix(sec, 0)).Abs() > 5*time.Minute {
			http.Error(w, "bad timestamp", http.StatusUnauthorized)
			return
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write([]byte(ts + "."))
		mac.Write(body)
		want := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(r.Header.Get("X-Baki-Signature")), []byte(want)) {
			http.Error(w, "invalid or missing X-Baki-Signature", http.StatusUnauthorized)
			return
		}
		var req map[string]string
		_ = json.Unmarshal(body, &req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"flowId": req["flowId"], "flowName": "Billing", "passed": passed, "gate": req["failOn"],
			"errors": 1, "warnings": 0, "info": 2,
		})
	}))
	t.Cleanup(srv.Close)
	return srv, seen
}

// TestCLI_ReportSignsLikeTheServerVerifies drives `bakicli report` against a
// verifier that recomputes the server's canonical string.
func TestCLI_ReportSignsLikeTheServerVerifies(t *testing.T) {
	srv, seen := ciWebhookServer(t, "topsecret", false)

	stdout, stderr, code := runBakicli(t, "report", "-endpoint", srv.URL, "-secret", "topsecret", "-flow-id", "flow-1", "-fail-on", "warning")
	if code != 1 {
		t.Errorf("gate failed on the server → exit 1, got %d (stderr=%s)", code, stderr)
	}
	if got := seen.get("path"); got != "/api/integrations/ci" {
		t.Errorf("posted to %q", got)
	}
	var body map[string]string
	_ = json.Unmarshal([]byte(seen.get("body")), &body)
	if body["flowId"] != "flow-1" || body["failOn"] != "warning" || body["format"] != "summary" {
		t.Errorf("request body = %s", seen.get("body"))
	}
	if !strings.Contains(stdout, `"passed":false`) || !strings.Contains(stderr, "gate FAILED") {
		t.Errorf("stdout=%q stderr=%q", stdout, stderr)
	}

	// Wrong secret → 401 → exit 2 (request error, not a gate verdict).
	if _, _, code := runBakicli(t, "report", "-endpoint", srv.URL+"/api/integrations/ci", "-secret", "nope", "-flow-id", "flow-1"); code != 2 {
		t.Errorf("bad secret: exit %d, want 2", code)
	}

	// Passing gate, secret from the environment, flow ID from a report.json.
	ok, _ := ciWebhookServer(t, "envsecret", true)
	rep := filepath.Join(t.TempDir(), "report.json")
	if err := os.WriteFile(rep, []byte(`{"flowId":"flow-from-report","findings":[],"stats":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, code = runBakicliIn(t, ".", []string{"PAD_CI_WEBHOOK_SECRET=envsecret"}, "report", "-endpoint", ok.URL, rep)
	if code != 0 || !strings.Contains(stdout, `"flowId":"flow-from-report"`) {
		t.Errorf("exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}

	// No secret anywhere → usage error.
	if _, _, code := runBakicliIn(t, ".", []string{"PAD_CI_WEBHOOK_SECRET="}, "report", "-endpoint", ok.URL, "-flow-id", "x"); code != 2 {
		t.Errorf("missing secret: exit %d, want 2", code)
	}
}

func TestSignCIRequest_CanonicalString(t *testing.T) {
	body := []byte(`{"flowId":"f"}`)
	mac := hmac.New(sha256.New, []byte("k"))
	mac.Write([]byte("1700000000." + string(body)))
	if got, want := signCIRequest("k", 1700000000, body), "sha256="+hex.EncodeToString(mac.Sum(nil)); got != want {
		t.Errorf("signCIRequest = %s, want %s", got, want)
	}
	if got := ciWebhookURL("https://baki.example.com/"); got != "https://baki.example.com/api/integrations/ci" {
		t.Errorf("ciWebhookURL = %q", got)
	}
	if got := ciWebhookURL("https://baki.example.com/api/integrations/ci"); got != "https://baki.example.com/api/integrations/ci" {
		t.Errorf("ciWebhookURL(full) = %q", got)
	}
}
