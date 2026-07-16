package api

import (
	"net/http"
	"strings"
	"testing"

	"pad-core/models"
)

// A minimal PAD flow text that triggers at least one finding, reused across the
// analyze-raw tests.
const rawSampleFlow = `#Region "Main"
Variables.SetVariable Name: %ApiKey% Value: 'AKIAIOSFODNN7EXAMPLE'
Excel.LaunchExcel Visible: True LoadAddIns: False => %ExcelInstance%
#EndRegion
`

// TestAnalyzeRaw_ReturnsJSON verifies the headline contract: POST raw flow text,
// get an AnalysisReport back in one call — no pre-stored flow, no CLI.
func TestAnalyzeRaw_ReturnsJSON(t *testing.T) {
	rt := newTestRouter(nil, false) // local mode, no auth
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze-raw", map[string]any{
		"files": map[string]string{"Main.txt": rawSampleFlow},
		"name":  "raw-sample",
	})
	checkStatus(t, rr, http.StatusOK)
	var report models.AnalysisReport
	decodeJSON(t, rr, &report)
	if len(report.Findings) == 0 {
		t.Errorf("expected the raw sample to yield findings, got 0; stats=%+v", report.Stats)
	}
}

// TestAnalyzeRaw_ReturnsSARIF verifies the format=sarif path returns SARIF text
// (so non-GitHub CI can POST flow text and consume SARIF directly).
func TestAnalyzeRaw_ReturnsSARIF(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze-raw", map[string]any{
		"files":  map[string]string{"Main.txt": rawSampleFlow},
		"name":   "raw-sample",
		"format": "sarif",
	})
	checkStatus(t, rr, http.StatusOK)
	body := rr.Body.String()
	if !strings.Contains(body, `"runs"`) || !strings.Contains(body, `"results"`) {
		t.Errorf("expected SARIF output with runs/results, got: %s", body[:min(200, len(body))])
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/sarif") {
		t.Errorf("expected application/sarif+json content-type, got %q", ct)
	}
}

// TestAnalyzeRaw_ReturnsJUnit verifies the format=junit path returns JUnit XML.
func TestAnalyzeRaw_ReturnsJUnit(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze-raw", map[string]any{
		"files":  map[string]string{"Main.txt": rawSampleFlow},
		"name":   "raw-sample",
		"format": "junit",
	})
	checkStatus(t, rr, http.StatusOK)
	body := rr.Body.String()
	if !strings.Contains(body, "<testsuites") {
		t.Errorf("expected JUnit XML with <testsuites>, got: %s", body[:min(200, len(body))])
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/xml") {
		t.Errorf("expected application/xml content-type, got %q", ct)
	}
}

// TestAnalyzeRaw_ReturnsCSV verifies the format=csv path returns CSV text.
func TestAnalyzeRaw_ReturnsCSV(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze-raw", map[string]any{
		"files":  map[string]string{"Main.txt": rawSampleFlow},
		"name":   "raw-sample",
		"format": "csv",
	})
	checkStatus(t, rr, http.StatusOK)
	body := rr.Body.String()
	if !strings.Contains(body, "Rule,Severity") {
		t.Errorf("expected CSV with header row, got: %s", body[:min(200, len(body))])
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("expected text/csv content-type, got %q", ct)
	}
}

// TestAnalyzeRaw_NoFilesReturns400 confirms a missing body is a clean 400.
func TestAnalyzeRaw_NoFilesReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze-raw", map[string]any{
		"name": "empty",
	})
	checkStatus(t, rr, http.StatusBadRequest)
}

// TestAnalyzeRaw_GarbageIsGraceful confirms unparseable text doesn't crash.
// The parser is lenient by design (produces a doc with parse errors rather than
// erroring). With the parse-error rule, garbage input now correctly surfaces
// parse-error findings — the endpoint returns a clean 200 with those findings.
func TestAnalyzeRaw_GarbageIsGraceful(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze-raw", map[string]any{
		"files": map[string]string{"Main.txt": "\x00\x01 not a flow"},
	})
	checkStatus(t, rr, http.StatusOK)
	var report models.AnalysisReport
	decodeJSON(t, rr, &report)
	// Parse-error findings are expected (and correct) for unparseable input.
	// The point of this test is that the endpoint doesn't crash.
	for _, f := range report.Findings {
		if f.RuleID != "parse-error" {
			t.Errorf("expected only parse-error findings for garbage input, got %q", f.RuleID)
		}
	}
}
