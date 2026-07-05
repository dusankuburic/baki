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

// TestAnalyzeRaw_NoFilesReturns400 confirms a missing body is a clean 400.
func TestAnalyzeRaw_NoFilesReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze-raw", map[string]any{
		"name": "empty",
	})
	checkStatus(t, rr, http.StatusBadRequest)
}

// TestAnalyzeRaw_GarbageIsGraceful confirms unparseable text doesn't crash —
// the parser is lenient by design (produces an empty doc, 0 findings) rather
// than erroring, so the endpoint returns a clean 200 with an empty report.
func TestAnalyzeRaw_GarbageIsGraceful(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze-raw", map[string]any{
		"files": map[string]string{"Main.txt": "\x00\x01 not a flow"},
	})
	checkStatus(t, rr, http.StatusOK)
	var report models.AnalysisReport
	decodeJSON(t, rr, &report)
	if len(report.Findings) != 0 {
		t.Errorf("expected 0 findings for garbage input, got %d", len(report.Findings))
	}
}
