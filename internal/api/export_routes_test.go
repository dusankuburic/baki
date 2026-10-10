package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"testing"
	"time"

	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/analyzer"
	"pad-core/export"
	"pad-core/models"
)

// exportWire is what every surface sends per format: the Content-Type and the attachment name are wire
// contract, pinned here literally rather than read back from the registry.
var exportWire = map[string]struct{ mime, filename string }{
	"sarif":    {"application/sarif+json", "pad-analysis.sarif"},
	"junit":    {"application/xml; charset=utf-8", "pad-analysis.xml"},
	"csv":      {"text/csv; charset=utf-8", "pad-analysis.csv"},
	"markdown": {"text/markdown; charset=utf-8", "pad-analysis.md"},
	"html":     {"text/html; charset=utf-8", "pad-analysis.html"},
	"pdf":      {"application/pdf", "pad-analysis.pdf"},
}

func checkDownload(t *testing.T, rr *httptest.ResponseRecorder, id string) {
	t.Helper()
	checkStatus(t, rr, http.StatusOK)
	want := exportWire[id]
	if ct := rr.Header().Get("Content-Type"); ct != want.mime {
		t.Errorf("%s: Content-Type %q, want %q", id, ct, want.mime)
	}
	if cd, wantCD := rr.Header().Get("Content-Disposition"), `attachment; filename="`+want.filename+`"`; cd != wantCD {
		t.Errorf("%s: Content-Disposition %q, want %q", id, cd, wantCD)
	}
}

// registryBytes is the format's rendering of report as the server renders it (the build's tool version).
func registryBytes(t *testing.T, id string, doc *models.FlowDocument, report *models.AnalysisReport) []byte {
	t.Helper()
	b, err := export.Bytes(id, export.Input{Report: report, Doc: doc}, export.Options{ToolVersion: service.Version})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// sarifLog is the part of a SARIF document the route tests read.
type sarifLog struct {
	Schema  string `json:"$schema"`
	Version string `json:"version"`
	Runs    []struct {
		Tool struct {
			Driver struct {
				Name    string            `json:"name"`
				Version string            `json:"version"`
				Rules   []json.RawMessage `json:"rules"`
			} `json:"driver"`
		} `json:"tool"`
		Results []struct {
			RuleID              string            `json:"ruleId"`
			Level               string            `json:"level"`
			PartialFingerprints map[string]string `json:"partialFingerprints"`
			Locations           []struct {
				PhysicalLocation struct {
					Region *struct {
						StartLine int `json:"startLine"`
					} `json:"region"`
				} `json:"physicalLocation"`
				LogicalLocations []json.RawMessage `json:"logicalLocations"`
			} `json:"locations"`
			Properties map[string]any `json:"properties"`
		} `json:"results"`
	} `json:"runs"`
}

func decodeSARIF(t *testing.T, body []byte) sarifLog {
	t.Helper()
	var log sarifLog
	if err := json.Unmarshal(body, &log); err != nil {
		t.Fatalf("not SARIF JSON: %v", err)
	}
	if log.Version != "2.1.0" || len(log.Runs) != 1 {
		t.Fatalf("SARIF version %q with %d runs", log.Version, len(log.Runs))
	}
	return log
}

// analyze-raw: json (the default, compact report) plus its three registry formats, each with the format's
// MIME and an attachment name; anything else (the documents included: the route does not offer them) is a
// 400 that names the accepted set.
func TestExportRoutes_AnalyzeRaw(t *testing.T) {
	for _, mode := range []struct {
		name string
		rt   *Router
		auth func(rt *Router) string
	}{
		{"desktop", newTestRouter(nil, false), func(*Router) string { return "Bearer " + testToken }},
		{"cloud", newJWTTestRouter(t), func(rt *Router) string { return jwtBearer(t, rt, "alice", "alice@example.com") }},
	} {
		t.Run(mode.name, func(t *testing.T) {
			post := func(format string) *httptest.ResponseRecorder {
				body := map[string]any{"files": map[string]string{"Main.txt": rawSampleFlow}, "name": "raw-sample"}
				if format != "-" {
					body["format"] = format
				}
				return doRequestWithAuth(t, mode.rt, http.MethodPost, "/api/analysis/analyze-raw", mode.auth(mode.rt), body)
			}
			for _, format := range []string{"-", "", "json"} {
				rr := post(format)
				checkStatus(t, rr, http.StatusOK)
				if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
					t.Errorf("format %q: Content-Type %q", format, ct)
				}
				if cd := rr.Header().Get("Content-Disposition"); cd != "" {
					t.Errorf("format %q: the JSON report is no attachment: %q", format, cd)
				}
				var report models.AnalysisReport
				decodeJSON(t, rr, &report)
				if len(report.Findings) == 0 {
					t.Errorf("format %q: no findings", format)
				}
			}

			for _, id := range analyzeRawFormats {
				rr := post(id)
				checkDownload(t, rr, id)
				body := rr.Body.Bytes()
				if !bytes.HasSuffix(body, []byte("\n")) {
					t.Errorf("%s: body does not end with a newline", id)
				}
				switch id {
				case "sarif":
					log := decodeSARIF(t, body)
					if d := log.Runs[0].Tool.Driver; d.Name != "PAD Analyzer" || d.Version != service.Version || len(d.Rules) == 0 {
						t.Errorf("sarif driver = %s %s with %d rules", d.Name, d.Version, len(d.Rules))
					}
				case "junit":
					if !bytes.HasPrefix(body, []byte("<?xml")) || !bytes.Contains(body, []byte("<testsuites")) {
						t.Errorf("junit body: %.80s", body)
					}
				case "csv":
					if !bytes.Contains(body, []byte("\nRule,Severity,")) {
						t.Errorf("csv body: %.120s", body)
					}
				}
			}

			for _, bad := range []string{"html", "markdown", "pdf", "text", "SARIF", "docx"} {
				rr := post(bad)
				checkStatus(t, rr, http.StatusBadRequest)
				var env struct {
					Message string `json:"message"`
				}
				decodeJSON(t, rr, &env)
				if want := `unknown format "` + bad + `" (must be json, sarif, junit, or csv)`; env.Message != want {
					t.Errorf("format %q: message %q, want %q", bad, env.Message, want)
				}
			}
		})
	}
}

// The CI use of analyze-raw posts the same unchanged flow on every run: each post gets the same results with
// their line numbers and fingerprints (the report cache once served the first parse's block ids to the second,
// whose document has none of them, so the SARIF lost every line). Only the block ids, fresh per parse, differ.
func TestExportRoutes_AnalyzeRawRepeatIsStable(t *testing.T) {
	for _, mode := range []struct {
		name string
		rt   *Router
		auth string
	}{
		{"desktop", newTestRouter(nil, false), "Bearer " + testToken},
		{"cloud", newJWTTestRouter(t), ""},
	} {
		t.Run(mode.name, func(t *testing.T) {
			auth := mode.auth
			if auth == "" {
				auth = jwtBearer(t, mode.rt, "alice", "alice@example.com")
			}
			var first []string
			for run := 1; run <= 3; run++ {
				rr := doRequestWithAuth(t, mode.rt, http.MethodPost, "/api/analysis/analyze-raw", auth, map[string]any{
					"files": map[string]string{"Main.txt": rawSampleFlow}, "name": "raw-sample", "format": "sarif",
				})
				checkDownload(t, rr, "sarif")
				log := decodeSARIF(t, rr.Body.Bytes())
				if len(log.Runs[0].Results) == 0 {
					t.Fatalf("run %d: no results", run)
				}
				var stable []string
				for _, r := range log.Runs[0].Results {
					if len(r.Locations) == 0 || r.Locations[0].PhysicalLocation.Region == nil || r.Locations[0].PhysicalLocation.Region.StartLine == 0 {
						t.Errorf("run %d: result %s has no start line", run, r.RuleID)
						continue
					}
					stable = append(stable, r.RuleID+"@"+strconv.Itoa(r.Locations[0].PhysicalLocation.Region.StartLine)+"#"+r.PartialFingerprints["padFindingId/v1"])
				}
				if first == nil {
					first = stable
				} else if !slices.Equal(stable, first) {
					t.Errorf("run %d: results %v, first run %v", run, stable, first)
				}
			}
		})
	}
}

// Desktop: the flow exports render the report through the registry, byte for byte. SARIF and JUnit analyze on
// demand and come back raw as attachments; Markdown, HTML and PDF need a current report (409 before one) and
// come back base64 in the {status, data} envelope, also written to the chosen path.
func TestExportRoutes_DesktopFlowExports(t *testing.T) {
	rt := newTestRouter(nil, false)
	file := filepath.Join(t.TempDir(), "flow.txt")
	if err := os.WriteFile(file, []byte(rawSampleFlow), 0o600); err != nil {
		t.Fatal(err)
	}
	checkStatus(t, doRequest(t, rt, http.MethodPost, "/api/flow/load-path", map[string]string{"path": file}), http.StatusOK)

	exp := moduleOf[*ExportHandler](t, rt)
	doc, err := exp.flowSvc.GetAuthorized(context.Background(), "", service.FlowRead)
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"markdown", "html", "pdf"} {
		rr := doRequest(t, rt, http.MethodPost, "/api/export/"+id, map[string]string{})
		checkStatus(t, rr, http.StatusConflict)
	}

	for _, id := range []string{"sarif", "junit"} {
		rr := doRequest(t, rt, http.MethodPost, "/api/analysis/export/"+id, nil)
		checkDownload(t, rr, id)
		report, err := exp.analysisSvc.AnalyzeFlow(context.Background(), doc) // the cached report the route rendered
		if err != nil {
			t.Fatal(err)
		}
		if want := registryBytes(t, id, doc, report); !bytes.Equal(rr.Body.Bytes(), want) {
			t.Errorf("%s: route body differs from the registry's rendering", id)
		}
	}

	report, _ := exp.analysisSvc.CurrentReport(doc)
	if report == nil {
		t.Fatal("no current report after the exports analyzed")
	}
	for _, id := range []string{"markdown", "html", "pdf"} {
		path := filepath.Join(t.TempDir(), exportWire[id].filename)
		rr := doRequest(t, rt, http.MethodPost, "/api/export/"+id, map[string]string{"path": path})
		checkStatus(t, rr, http.StatusOK)
		if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("%s: Content-Type %q, want the JSON envelope", id, ct)
		}
		var env map[string]string
		decodeJSON(t, rr, &env)
		data, err := base64.StdEncoding.DecodeString(env["data"])
		if err != nil || env["status"] != "ok" || len(env) != 2 {
			t.Fatalf("%s: envelope %v (decode: %v)", id, env, err)
		}
		if id == "pdf" { // fpdf stamps the render time
			if !bytes.HasPrefix(data, []byte("%PDF-")) {
				t.Errorf("pdf: data starts %q", data[:min(8, len(data))])
			}
		} else if want := registryBytes(t, id, doc, report); !bytes.Equal(data, want) {
			t.Errorf("%s: envelope data differs from the registry's rendering", id)
		}
		written, err := os.ReadFile(path) // #nosec G304 -- test temp dir
		if err != nil || !bytes.Equal(written, data) {
			t.Errorf("%s: the desktop write differs from the response data (err %v)", id, err)
		}
	}
}

// Cloud: every flow export answers with its format, drops the server path, and records a flow.export audit
// row whose Meta["format"] is the format id (stored data: sarif, junit, markdown, html, pdf).
func TestExportRoutes_CloudFlowExportsAudit(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "alice", "alice@example.com", "member")
	const flowID = "flow-export-routes"
	seedCloudFlowWithSource(t, rt, flowID, "alice", rawSampleFlow)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	ch := captureAuditEvents(t, 8)
	exp := moduleOf[*ExportHandler](t, rt)
	doc, err := exp.flowSvc.GetAuthorized(userActorCtx("alice"), flowID, service.FlowRead)
	if err != nil {
		t.Fatal(err)
	}

	for _, id := range []string{"sarif", "junit"} {
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/export/"+id, bearer, map[string]string{"flowId": flowID})
		checkDownload(t, rr, id)
		if id == "sarif" { // SARIF has no time in it: any analysis of the flow renders the same bytes
			report, err := exp.analysisSvc.AnalyzeFlow(userActorCtx("alice"), doc)
			if err != nil {
				t.Fatal(err)
			}
			if want := registryBytes(t, id, doc, report); !bytes.Equal(rr.Body.Bytes(), want) {
				t.Errorf("sarif: route body differs from the registry's rendering")
			}
		}
		checkExportAudit(t, nextAuditEvent(t, ch), flowID, id)
	}
	for _, id := range []string{"markdown", "html", "pdf"} {
		path := filepath.Join(t.TempDir(), exportWire[id].filename)
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/export/"+id, bearer, map[string]string{"flowId": flowID, "path": path})
		checkStatus(t, rr, http.StatusOK)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("%s: cloud export wrote the caller's path (stat: %v)", id, err)
		}
		checkExportAudit(t, nextAuditEvent(t, ch), flowID, id)
	}
}

func checkExportAudit(t *testing.T, ev *storageif.AuditEvent, flowID, format string) {
	t.Helper()
	if ev.Action != audit.ActionFlowExport || ev.ResourceType != "flow" || ev.ResourceID != flowID || ev.Meta["format"] != format || len(ev.Meta) != 1 {
		t.Errorf("%s: audit row = %s %s/%s meta %v", format, ev.Action, ev.ResourceType, ev.ResourceID, ev.Meta)
	}
}

// signedCIWebhook is a signed CI webhook request.
func signedCIWebhook(t *testing.T, secret string, body map[string]string) *http.Request {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Unix()
	req := httptest.NewRequest(http.MethodPost, "/api/integrations/ci", bytes.NewReader(raw))
	req.Header.Set("X-Baki-Signature", signBodyWithTimestamp(secret, raw, ts))
	req.Header.Set("X-Baki-Timestamp", strconv.FormatInt(ts, 10))
	return req
}

// postCIWebhook sends a signed CI webhook request.
func postCIWebhook(t *testing.T, rt *Router, secret string, body map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, signedCIWebhook(t, secret, body))
	return rr
}

// Through the production chain the webhook's SARIF is compressed like its JSON answers (and like its SARIF was
// while it went out as application/json): application/sarif+json is on Compress's allowlist.
func TestCIWebhook_SARIFIsCompressed(t *testing.T) {
	rt := newJWTTestRouter(t)
	const secret = "topsecret"
	moduleOf[*AnalysisHandler](t, rt).ciSecret = secret
	const flowID = "flow-ci-gzip"
	seedCloudFlowWithSource(t, rt, flowID, "alice", rawSampleFlow)
	h := fullChain(t, rt, nil)

	for _, format := range []string{"report", "sarif"} {
		plain := postCIWebhook(t, rt, secret, map[string]string{"flowId": flowID, "format": format})
		checkStatus(t, plain, http.StatusOK)
		req := signedCIWebhook(t, secret, map[string]string{"flowId": flowID, "format": format})
		req.Header.Set("Accept-Encoding", "gzip")
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)
		checkStatus(t, rr, http.StatusOK)
		if ce, ct := rr.Header().Get("Content-Encoding"), rr.Header().Get("Content-Type"); ce != "gzip" {
			t.Errorf("%s: %s answer of %d bytes has Content-Encoding %q, want gzip", format, ct, plain.Body.Len(), ce)
			continue
		}
		zr, err := gzip.NewReader(rr.Body)
		if err != nil {
			t.Fatalf("%s: %v", format, err)
		}
		if got, err := io.ReadAll(zr); err != nil || !bytes.Equal(got, plain.Body.Bytes()) {
			t.Errorf("%s: decompressed body differs from the plain answer (err %v)", format, err)
		}
	}
}

// The CI webhook: summary and report keep their JSON shape; sarif is the core SARIF document (the one
// analyze-raw and /api/analysis/export/sarif send) as application/sarif+json, not an attachment. Its old
// hand-written SARIF (driver "baki", a singular logicalLocation the spec does not define) is gone.
func TestCIWebhook_Formats(t *testing.T) {
	rt := newJWTTestRouter(t)
	const secret = "topsecret"
	ah := moduleOf[*AnalysisHandler](t, rt)
	ah.ciSecret = secret
	const flowID = "flow-ci-formats"
	seeded := seedCloudFlowWithSource(t, rt, flowID, "alice", rawSampleFlow)

	type verdict struct {
		FlowID   string           `json:"flowId"`
		FlowName string           `json:"flowName"`
		Passed   bool             `json:"passed"`
		Gate     string           `json:"gate"`
		Errors   int              `json:"errors"`
		Warnings int              `json:"warnings"`
		Info     int              `json:"info"`
		Preview  *int             `json:"preview"`
		Findings []models.Finding `json:"findings"`
	}
	summary := func(format string) verdict {
		t.Helper()
		rr := postCIWebhook(t, rt, secret, map[string]string{"flowId": flowID, "format": format})
		checkStatus(t, rr, http.StatusOK)
		if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("format %q: Content-Type %q", format, ct)
		}
		var v verdict
		decodeJSON(t, rr, &v)
		return v
	}

	s := summary("")
	if s.FlowID != flowID || s.FlowName != seeded.Name || s.Gate != "error" || s.Passed || s.Errors == 0 || s.Preview != nil || s.Findings != nil {
		t.Errorf("summary = %+v", s)
	}
	if r := summary("report"); len(r.Findings) != r.Errors+r.Warnings+r.Info || len(r.Findings) == 0 {
		t.Errorf("report: %d findings for %d/%d/%d", len(r.Findings), r.Errors, r.Warnings, r.Info)
	}

	rr := postCIWebhook(t, rt, secret, map[string]string{"flowId": flowID, "format": "sarif"})
	checkStatus(t, rr, http.StatusOK)
	if ct := rr.Header().Get("Content-Type"); ct != "application/sarif+json" {
		t.Errorf("sarif: Content-Type %q", ct)
	}
	if cd := rr.Header().Get("Content-Disposition"); cd != "" {
		t.Errorf("sarif: the webhook answer is no attachment: %q", cd)
	}
	doc, err := ah.flowSvc.DocProvider().ResolveDoc(context.Background(), flowID)
	if err != nil {
		t.Fatal(err)
	}
	report, err := ah.analysisSvc.AnalyzeFlow(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if want := registryBytes(t, "sarif", doc, report); !bytes.Equal(rr.Body.Bytes(), want) {
		t.Errorf("sarif: webhook body differs from the registry's rendering")
	}
	log := decodeSARIF(t, rr.Body.Bytes())
	if d := log.Runs[0].Tool.Driver; d.Name != "PAD Analyzer" || d.Version != service.Version {
		t.Errorf("sarif driver = %s %s", d.Name, d.Version)
	}
	for _, r := range log.Runs[0].Results {
		if r.PartialFingerprints["padFindingId/v1"] == "" || len(r.Locations) == 0 || len(r.Locations[0].LogicalLocations) == 0 {
			t.Errorf("result %s lacks fingerprints or logicalLocations", r.RuleID)
		}
	}
	if bytes.Contains(rr.Body.Bytes(), []byte(`"logicalLocation"`)) {
		t.Error("sarif still carries the singular logicalLocation")
	}
}

// A preview rule's findings never fail the webhook's gate (D3): they stay in errors/warnings/info, which count
// every finding, and are counted in preview.
func TestCIWebhook_PreviewNeverGates(t *testing.T) {
	rt := newJWTTestRouter(t)
	const secret = "topsecret"
	moduleOf[*AnalysisHandler](t, rt).ciSecret = secret
	const flowID = "flow-ci-preview"
	seedCloudFlowWithSource(t, rt, flowID, "alice", rawSampleFlow)
	verdict := func() (passed bool, errs, preview int) {
		t.Helper()
		rr := postCIWebhook(t, rt, secret, map[string]string{"flowId": flowID, "failOn": "error"})
		checkStatus(t, rr, http.StatusOK)
		var v struct {
			Passed  bool `json:"passed"`
			Errors  int  `json:"errors"`
			Preview int  `json:"preview"`
		}
		decodeJSON(t, rr, &v)
		return v.Passed, v.Errors, v.Preview
	}

	passed, errs, preview := verdict()
	if passed || errs == 0 || preview != 0 {
		t.Fatalf("stable: passed %v, errors %d, preview %d; the sample's credential must fail the error gate", passed, errs, preview)
	}

	// Every error-severity rule of the sample goes preview; the report misses the cache (the digest folds stages).
	m, _ := analyzer.LookupMeta("hardcoded-credential")
	m.Stage = analyzer.StagePreview
	defer analyzer.SetRuleMetaForTesting("hardcoded-credential", m)()
	passed, errs2, preview := verdict()
	if !passed || errs2 != errs || preview != errs {
		t.Errorf("preview: passed %v, errors %d (was %d), preview %d; want a pass with the errors counted as preview", passed, errs2, errs, preview)
	}
}

// analyze-raw's formats are registered text formats; the server's surfaces render nothing outside the registry.
func TestAnalyzeRawFormats_AreRegisteredText(t *testing.T) {
	for _, id := range analyzeRawFormats {
		f, ok := export.Lookup(id)
		if !ok || f.Binary {
			t.Errorf("%s: registered %v, binary %v", id, ok, f.Binary)
		}
		if w, ok := exportWire[id]; !ok || w.mime != f.MIME || w.filename != exportBaseName+f.Ext {
			t.Errorf("%s: registry MIME/Ext %q/%q, wire %+v", id, f.MIME, f.Ext, w)
		}
	}
	for id, w := range exportWire {
		if f, ok := export.Lookup(id); !ok || f.MIME != w.mime || exportBaseName+f.Ext != w.filename {
			t.Errorf("%s: the registry no longer matches the wire pinned here", id)
		}
	}
}
