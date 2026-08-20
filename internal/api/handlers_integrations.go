package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pad-analyzer/internal/api/render"
	"pad-core/logger"
	"pad-core/models"
)

// ciWebhookMaxSkew is the freshness window for an inbound CI webhook. A request
// whose X-Baki-Timestamp is older (or further in the future) than this is
// rejected, so a captured request can't be replayed after the window elapses.
const ciWebhookMaxSkew = 5 * time.Minute

// CIWebhookSecret is the HMAC key an admin configures (PAD_CI_WEBHOOK_SECRET).
// The AnalysisHandler holds it so the inbound CI endpoint can verify request
// authenticity without going through jwtAuth (the endpoint is in publicRoutes).
type CIWebhookSecret string

// verifySignature is the inbound mirror of notify's outbound X-Baki-Signature
// signing: recompute HMAC-SHA256 over the raw body and compare in constant time.
// It consumes the request body and restores it for the downstream handler.
//
// Replay protection: when the sender includes X-Baki-Timestamp (unix seconds),
// it is validated against a ±ciWebhookMaxSkew window AND folded into the signed
// payload (as "<ts>." prefix) so a forged timestamp invalidates the signature.
// Senders that omit the header are accepted for backward compatibility (logged
// as deprecated on a successful match) — the header will become mandatory in a
// future release.
func verifySignature(r *http.Request, secret string) (bool, error) {
	if secret == "" {
		return false, fmt.Errorf("CI webhook secret not configured")
	}
	sent := r.Header.Get("X-Baki-Signature")
	if sent == "" {
		return false, nil
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		return false, err
	}
	r.Body.Close()
	// Restore the body so the handler can JSON-decode it.
	r.Body = io.NopCloser(bytes.NewReader(body))

	mac := hmac.New(sha256.New, []byte(secret))
	if ts := r.Header.Get("X-Baki-Timestamp"); ts != "" {
		sec, terr := strconv.ParseInt(ts, 10, 64)
		if terr != nil {
			return false, nil
		}
		if skew := time.Since(time.Unix(sec, 0)); skew > ciWebhookMaxSkew || skew < -ciWebhookMaxSkew {
			return false, nil
		}
		// Fold the timestamp into the signed payload so a tampered timestamp
		// (e.g. refreshing an old request) breaks the signature.
		mac.Write([]byte(ts + "."))
	}
	mac.Write(body)
	expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	return hmac.Equal([]byte(sent), []byte(expected)), nil
}

// ciWebhookRequest is the body a CI runner POSTs. FlowID identifies a stored
// flow to analyze; FailOn (optional, mirrors bakicli) sets the severity at which
// the response reports passed=false, letting a pipeline gate without parsing the
// report.
type ciWebhookRequest struct {
	FlowID string `json:"flowId"`
	FailOn string `json:"failOn,omitempty"` // "error" (default) | "warning" | "info" | "none"
	Format string `json:"format,omitempty"` // "summary" (default) | "report" | "sarif"
}

// ciWebhookResponse is the summary shape returned for format="summary". It
// carries enough for a CI step to gate AND to print a compact job log.
type ciWebhookResponse struct {
	FlowID   string `json:"flowId"`
	FlowName string `json:"flowName,omitempty"`
	Passed   bool   `json:"passed"`
	Gate     string `json:"gate"`
	Errors   int    `json:"errors"`
	Warnings int    `json:"warnings"`
	Info     int    `json:"info"`
	// Findings is omitted unless format="report".
	Findings []models.Finding `json:"findings,omitempty"`
}

// handleCIWebhook analyzes a stored flow on demand from a CI runner. Auth is a
// pre-shared HMAC key (X-Baki-Signature), NOT a user session — the endpoint is
// in publicRoutes so jwtAuth skips it, and this handler does its own signature
// check. The flow is loaded without per-user authz (like the public share
// viewer), so the runner only needs the flow ID + the secret.
// @Summary      CI webhook
// @Description  Token-authenticated CI integration endpoint.
// @Tags         analysis
// @Accept       json
// @Produce      json
// @Success      200 {object} map[string]interface{} "OK"
// @Failure      400 {object} map[string]string "Bad Request"
// @Failure      401 {object} map[string]string "Unauthorized"
// @Failure      500 {object} map[string]string "Error"
// @Router       /api/integrations/ci [post]
func (h *AnalysisHandler) handleCIWebhook(w http.ResponseWriter, r *http.Request) {
	if h.ciSecret == "" {
		render.Error(w, fmt.Errorf("CI webhook not configured: set PAD_CI_WEBHOOK_SECRET"), http.StatusServiceUnavailable)
		return
	}
	ok, err := verifySignature(r, string(h.ciSecret))
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if !ok {
		render.Error(w, fmt.Errorf("invalid or missing X-Baki-Signature"), http.StatusUnauthorized)
		return
	}
	// Backward-compat nudge: a request without X-Baki-Timestamp was accepted
	// (legacy signer) but is replay-vulnerable. Surface it so operators upgrade
	// their CI senders; the header will become mandatory in a future release.
	if r.Header.Get("X-Baki-Timestamp") == "" {
		logger.Warn("CI webhook accepted a request without X-Baki-Timestamp (deprecated; replay-vulnerable) — update the sender to include the timestamp")
	}

	var req ciWebhookRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if req.FlowID == "" {
		render.Error(w, fmt.Errorf("flowId is required"), http.StatusBadRequest)
		return
	}
	gate := strings.ToLower(strings.TrimSpace(req.FailOn))
	if gate == "" {
		gate = "error"
	}
	format := strings.ToLower(strings.TrimSpace(req.Format))
	if format == "" {
		format = "summary"
	}

	// Load the flow WITHOUT per-user authz — the webhook authenticated via HMAC,
	// not as a user. This mirrors handleViewShared's resolve path.
	doc, err := h.flowSvc.DocProvider().ResolveDoc(r.Context(), req.FlowID)
	if err != nil {
		render.Error(w, fmt.Errorf("flow not found: %w", err), http.StatusNotFound)
		return
	}

	report, err := h.analysisSvc.AnalyzeFlow(r.Context(), doc)
	if err != nil {
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	// Persist the rollup so the dashboard/portfolio reflect the CI run (best-effort).
	if h.dashboard != nil {
		h.dashboard.RecordAnalysis(r.Context(), doc, report)
	}

	// SARIF format short-circuits the summary envelope.
	if format == "sarif" {
		render.JSON(w, sarifFor(report))
		return
	}

	passed := gatePasses(report, gate)
	resp := ciWebhookResponse{
		FlowID:   doc.ID,
		FlowName: doc.Name,
		Passed:   passed,
		Gate:     gate,
		Errors:   report.Stats.Errors,
		Warnings: report.Stats.Warnings,
		Info:     report.Stats.Info,
	}
	if format == "report" {
		resp.Findings = report.Findings
	}
	// The CI gate verdict rides in the body (passed/reason), not the status —
	// CI callers want the structured result even on a failed gate.
	render.JSON(w, resp)
}

// gatePasses reports whether the report clears the requested severity gate.
// gate="none" always passes (report-only); "error" passes with zero errors;
// "warning" requires zero errors AND zero warnings; "info" requires a clean report.
func gatePasses(report *models.AnalysisReport, gate string) bool {
	if report == nil {
		return true
	}
	switch gate {
	case "none":
		return true
	case "warning":
		return report.Stats.Errors == 0 && report.Stats.Warnings == 0
	case "info":
		return report.Stats.Errors == 0 && report.Stats.Warnings == 0 && report.Stats.Info == 0
	case "error", "":
		return report.Stats.Errors == 0
	default:
		return report.Stats.Errors == 0
	}
}

// sarifFor builds a minimal SARIF 2.1.0 document from a report. CI runners that
// already consume bakicli SARIF can point at this endpoint with format="sarif".
func sarifFor(report *models.AnalysisReport) map[string]any {
	results := make([]map[string]any, 0, len(report.Findings))
	for _, f := range report.Findings {
		entry := map[string]any{
			"ruleId": f.RuleID,
			"level":  sarifLevel(string(f.Severity)),
			"message": map[string]string{
				"text": f.Title,
			},
		}
		// Finding carries no source line, so mark the location logically by the
		// subflow + block rather than a physical region.
		if f.SubflowID != "" || f.BlockID != "" {
			entry["locations"] = []map[string]any{
				{
					"logicalLocation": map[string]string{
						"name": f.BlockID,
					},
				},
			}
		}
		results = append(results, entry)
	}
	return map[string]any{
		"$schema": "https://docs.oasis-open.org/sarif/sarif/v2.1.0/cs01/schemas/sarif-schema-2.1.0.json",
		"version": "2.1.0",
		"runs": []map[string]any{
			{
				"tool": map[string]any{
					"driver": map[string]any{
						"name":    "baki",
						"version": "ci-webhook",
					},
				},
				"results": results,
			},
		},
	}
}

func sarifLevel(severity string) string {
	switch severity {
	case "error":
		return "error"
	case "warning":
		return "warning"
	default:
		return "note"
	}
}
