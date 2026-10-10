package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCLI_ReportSARIFIsPrintedVerbatim: the CI webhook answers format=sarif
// with the core SARIF document (application/sarif+json, indented); bakicli
// prints it as it came and does not look for a gate verdict in it.
func TestCLI_ReportSARIFIsPrintedVerbatim(t *testing.T) {
	const sarif = "{\n  \"$schema\": \"https://json.schemastore.org/sarif-2.1.0.json\",\n  \"version\": \"2.1.0\",\n  \"runs\": []\n}\n"
	var format string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		format = string(body)
		w.Header().Set("Content-Type", "application/sarif+json")
		_, _ = io.WriteString(w, sarif)
	}))
	t.Cleanup(srv.Close)

	stdout, stderr, code := runBakicli(t, "report", "-endpoint", srv.URL, "-secret", "s", "-flow-id", "f", "-format", "sarif")
	if code != 0 || stdout != strings.TrimSpace(sarif)+"\n" {
		t.Errorf("exit %d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(format, `"format":"sarif"`) {
		t.Errorf("request body = %s", format)
	}
}

// TestCLI_ReportGatesEveryNonSARIFFormat: the server lowercases and trims the
// format and answers everything but sarif with the verdict, so a failed gate
// must exit 1 whatever the spelling (exit 0 here was a fail-open gate).
func TestCLI_ReportGatesEveryNonSARIFFormat(t *testing.T) {
	srv, _ := ciWebhookServer(t, "s", false)
	for _, format := range []string{"summary", "Summary", " REPORT ", "json", ""} {
		_, stderr, code := runBakicli(t, "report", "-endpoint", srv.URL, "-secret", "s", "-flow-id", "f", "-format", format)
		if code != 1 || !strings.Contains(stderr, "gate FAILED") {
			t.Errorf("-format %q: exit %d, want 1 (stderr=%s)", format, code, stderr)
		}
	}
}

// TestCLI_ReportSARIFLargerThanTheVerdictCap: a SARIF answer is streamed
// whole, however large (it used to be cut at 16 MiB, exit 0, invalid JSON).
func TestCLI_ReportSARIFLargerThanTheVerdictCap(t *testing.T) {
	head := "{\"runs\":[{\"results\":[\n"
	pad := strings.Repeat("{\"ruleId\":\"x\"},\n", (maxVerdictBytes/15)+1000)
	sarif := head + pad + "{\"ruleId\":\"last\"}]}]}"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/sarif+json")
		_, _ = io.WriteString(w, sarif)
	}))
	t.Cleanup(srv.Close)

	stdout, stderr, code := runBakicli(t, "report", "-endpoint", srv.URL, "-secret", "s", "-flow-id", "f", "-format", "sarif")
	if code != 0 || len(stdout) != len(sarif)+1 || !strings.HasSuffix(stdout, "\"last\"}]}]}\n") {
		t.Errorf("exit %d, stdout %d bytes (want %d), stderr=%q", code, len(stdout), len(sarif)+1, stderr)
	}
}

// TestCLI_ReportTruncatedOrOversizedAnswerExits2: a body cut short by the
// connection, or a verdict past the cap, is an error, never a printed
// fragment with exit 0.
func TestCLI_ReportTruncatedOrOversizedAnswerExits2(t *testing.T) {
	cut := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = io.WriteString(w, "{\"runs\":[")
	}))
	t.Cleanup(cut.Close)
	huge := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, "{\"passed\":true,\"pad\":\""+strings.Repeat("x", maxVerdictBytes)+"\"}")
	}))
	t.Cleanup(huge.Close)

	for _, c := range []struct{ name, url, format string }{
		{"truncated sarif", cut.URL, "sarif"},
		{"truncated verdict", cut.URL, "summary"},
		{"oversized verdict", huge.URL, "summary"},
	} {
		_, stderr, code := runBakicli(t, "report", "-endpoint", c.url, "-secret", "s", "-flow-id", "f", "-format", c.format)
		if code != 2 {
			t.Errorf("%s: exit %d, want 2 (stderr=%s)", c.name, code, stderr)
		}
	}
}
