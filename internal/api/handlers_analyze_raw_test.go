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

// TestAnalyzeRaw_TooManyFilesRejected verifies the file-count guard: an
// oversized request is rejected before parsing so a single call can't burn
// seconds of CPU on the shared backend (CPU-DoS mitigation).
func TestAnalyzeRaw_TooManyFilesRejected(t *testing.T) {
	rt := newTestRouter(nil, false)
	files := make(map[string]string, 51)
	for i := 0; i < 51; i++ {
		files["f"+itoa(i)+".txt"] = "#Region \"Main\"\n#EndRegion\n"
	}
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze-raw", map[string]any{
		"files": files,
	})
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for too many files, got %d", rr.Code)
	}
}

// TestAnalyzeRaw_OversizedPayloadRejected verifies the total-size guard: a
// single large file beyond the KB cap is rejected before parsing.
func TestAnalyzeRaw_OversizedPayloadRejected(t *testing.T) {
	rt := newTestRouter(nil, false)
	// ~3 MB of padding in one file — over the 2 MB total cap.
	big := "#Region \"Main\"\n" + strings.Repeat("x", 3*1024*1024) + "\n#EndRegion\n"
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/analyze-raw", map[string]any{
		"files": map[string]string{"Big.txt": big},
	})
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("expected 413 for oversized payload, got %d", rr.Code)
	}
}

// itoa avoids pulling strconv into a small test just for integer formatting.
func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	neg := i < 0
	if neg {
		i = -i
	}
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
