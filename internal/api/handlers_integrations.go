package api

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/api/render"
	"pad-core/logger"
	"pad-core/models"
)

// ciWebhookMaxSkew is the freshness window for an inbound CI webhook: a request whose
// X-Baki-Timestamp is older (or more future) than this is rejected, so a captured request can't
// be replayed after the window elapses.
const ciWebhookMaxSkew = 5 * time.Minute

// maxCIWebhookBodyBytes bounds the CI webhook body: verifySignature reads the WHOLE body (HMAC over
// the raw bytes) before any of it is parsed — without a cap an unauthenticated caller could feed
// gigabytes straight into memory.
const maxCIWebhookBodyBytes = 1 << 20

// ciWebhookUnconfiguredWarn gates the one WARN a process logs when the CI webhook is called without
// PAD_CI_WEBHOOK_SECRET: the operator learns of it once, repeated calls cost nothing in the log.
var ciWebhookUnconfiguredWarn sync.Once

// CIWebhookSecret is the HMAC key an admin configures (PAD_CI_WEBHOOK_SECRET); the AnalysisHandler
// holds it so the inbound CI endpoint can verify authenticity without going through jwtAuth.
type CIWebhookSecret string

// verifySignature is the inbound mirror of notify's outbound X-Baki-Signature signing: recompute
// HMAC-SHA256 over the raw body and compare in constant time; it consumes the body and restores it.
// Replay protection: X-Baki-Timestamp is validated against ±ciWebhookMaxSkew AND folded into the
// signed payload (as "<ts>." prefix), so a forged timestamp invalidates the signature.
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

// ciWebhookRequest is the body a CI runner POSTs: FlowID identifies a stored flow; FailOn (mirrors
// bakicli) sets the severity at which the response reports passed=false, so a pipeline can gate.
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

// handleCIWebhook analyzes a stored flow on demand from a CI runner. Auth is a pre-shared HMAC key
// (X-Baki-Signature), NOT a user session — the endpoint is in publicRoutes so jwtAuth skips it, and
// this handler does its own signature check. The flow is loaded without per-user authz (like the
// public share viewer): the runner needs only the flow ID + the secret.
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
		// The endpoint is public, so render.Error's per-request ERROR log for
		// this 503 let any unauthenticated caller flood the error log. Same
		// status and body; the misconfiguration is reported once per process.
		ciWebhookUnconfiguredWarn.Do(func() {
			logger.Warn("CI webhook called but not configured; POST /api/integrations/ci answers 503 until PAD_CI_WEBHOOK_SECRET is set")
		})
		render.ErrorUnlogged(w, fmt.Errorf("CI webhook not configured: set PAD_CI_WEBHOOK_SECRET"), http.StatusServiceUnavailable)
		return
	}
	// verifySignature reads the entire body for the HMAC; bound it before the
	// first read (the router's 10 MiB global cap never applied here — this
	// endpoint is in publicRoutes with no per-route cap until now).
	r.Body = http.MaxBytesReader(w, r.Body, maxCIWebhookBodyBytes)
	ok, err := verifySignature(r, string(h.ciSecret))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			render.Error(w, fmt.Errorf("CI webhook body exceeds %d bytes", maxCIWebhookBodyBytes), http.StatusRequestEntityTooLarge)
			return
		}
		render.Error(w, err, http.StatusInternalServerError)
		return
	}
	if !ok {
		render.Error(w, fmt.Errorf("invalid or missing X-Baki-Signature"), http.StatusUnauthorized)
		return
	}
	// The header is MANDATORY: a legacy-signed body (which loads any flow by ID and returns full
	// findings) could be replayed forever without a timestamp. An env escape hatch exists for
	// senders that genuinely cannot upgrade yet.
	if r.Header.Get("X-Baki-Timestamp") == "" {
		if os.Getenv("PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP") == "1" {
			logger.Warn("CI webhook accepted WITHOUT X-Baki-Timestamp (PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP=1; replay-vulnerable) — update the sender")
		} else {
			render.Error(w, fmt.Errorf("missing X-Baki-Timestamp (replay protection); include it in the signed payload or set PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP=1 on the server to re-enable legacy senders"), http.StatusUnauthorized)
			return
		}
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
		// 4xx bodies pass through unmasked — a wrapped driver error
		// (SQL state, blob details) must not reach an HMAC-authenticated but
		// external caller. Static message; detail stays server-side.
		logger.Warn("ci webhook: flow resolve failed", "flowId", req.FlowID, "err", err)
		render.Error(w, fmt.Errorf("flow not found"), http.StatusNotFound)
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
